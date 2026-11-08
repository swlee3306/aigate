package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Web console auth state (schema v6). Secrets are never stored: session tokens,
// CSRF tokens and idempotency keys are kept only as SHA-256 hex (HashSecret).
// Callers always pass the RAW value; the store hashes it. Structs returned by
// the store carry hashes (IDHash, CSRFHash), never the raw secret.

var (
	// ErrKeyExists is returned by PutIdempotent when a live (unexpired) key is already stored.
	ErrKeyExists = errors.New("store: idempotency key already exists")
	// ErrExists is returned when a live session or challenge with the same id is already stored.
	ErrExists = errors.New("store: already exists")
)

// HashSecret returns the SHA-256 hex digest the store keeps instead of a secret.
// Callers compare a presented CSRF token with Session.CSRFHash via HashSecret.
func HashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// nowNanos is the store's notion of now (injectable through clock in tests).
func (s *Store) nowNanos() int64 {
	if s.clock != nil {
		return s.clock().UTC().UnixNano()
	}
	return now()
}

// Session is a logged-in browser or CLI session. IDHash is the SHA-256 hex of
// the raw session token; CSRFHash is the SHA-256 hex of the CSRF token.
type Session struct {
	IDHash    string
	Principal string
	Kind      string
	CSRFHash  string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
}

// CreateSession stores a session for the raw token and returns it. A token that
// is already stored (live or expired-but-unpurged) is refused with ErrExists.
func (s *Store) CreateSession(ctx context.Context, token, csrf, principal, kind string, ttl time.Duration) (Session, error) {
	if token == "" || ttl <= 0 {
		return Session{}, errors.New("store: session needs a token and a positive ttl")
	}
	n := s.nowNanos()
	sess := Session{
		IDHash: HashSecret(token), Principal: principal, Kind: kind, CSRFHash: HashSecret(csrf),
		CreatedAt: ts(n), LastSeen: ts(n), ExpiresAt: ts(n + int64(ttl)),
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id_hash, principal, kind, csrf_hash, created_at, last_seen, expires_at)
		 VALUES (?,?,?,?,?,?,?) ON CONFLICT(id_hash) DO NOTHING`,
		sess.IDHash, principal, kind, sess.CSRFHash, n, n, n+int64(ttl))
	if err != nil {
		return Session{}, err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return Session{}, ErrExists
	}
	return sess, nil
}

// GetSession returns the live session for the raw token, or ErrNotFound when it
// is unknown or expired.
func (s *Store) GetSession(ctx context.Context, token string) (Session, error) {
	var (
		sess                  Session
		created, seen, expiry int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id_hash, principal, kind, csrf_hash, created_at, last_seen, expires_at
		 FROM sessions WHERE id_hash = ? AND expires_at > ?`, HashSecret(token), s.nowNanos()).
		Scan(&sess.IDHash, &sess.Principal, &sess.Kind, &sess.CSRFHash, &created, &seen, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.CreatedAt, sess.LastSeen, sess.ExpiresAt = ts(created), ts(seen), ts(expiry)
	return sess, nil
}

// TouchSession sets last_seen to now and the expiry to now+ttl (sliding window).
// An unknown or expired session is ErrNotFound. Absolute lifetime caps are the caller's job.
func (s *Store) TouchSession(ctx context.Context, token string, ttl time.Duration) error {
	n := s.nowNanos()
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE id_hash = ? AND expires_at > ?`,
		n, n+int64(ttl), HashSecret(token), n)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSession removes the session; an unknown token is not an error.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, HashSecret(token))
	return err
}

// Challenge is a single-use login or registration challenge. ID is a random
// nonce chosen by the caller (it is sent to the client, so it is stored as is);
// Data is caller-defined context (JSON).
type Challenge struct {
	ID        string
	Purpose   string
	Data      string
	ExpiresAt time.Time
}

// SaveChallenge stores a challenge. A live challenge with the same id is
// refused with ErrExists; an expired one is replaced.
func (s *Store) SaveChallenge(ctx context.Context, c Challenge) error {
	if c.ID == "" {
		return errors.New("store: challenge needs an id")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		n := s.nowNanos()
		if _, err := tx.ExecContext(ctx, `DELETE FROM challenges WHERE id = ? AND expires_at <= ?`, c.ID, n); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO challenges(id, purpose, data, expires_at) VALUES (?,?,?,?) ON CONFLICT(id) DO NOTHING`,
			c.ID, c.Purpose, c.Data, c.ExpiresAt.UTC().UnixNano())
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			return ErrExists
		}
		return nil
	})
}

// CountChallenges returns the number of live (unexpired) challenges of the
// given purpose.
func (s *Store) CountChallenges(ctx context.Context, purpose string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM challenges WHERE purpose = ? AND expires_at > ?`, purpose, s.nowNanos()).Scan(&n)
	return n, err
}

// TakeChallenge atomically removes and returns the challenge: a second take, an
// unknown id and an expired challenge are all ErrNotFound.
func (s *Store) TakeChallenge(ctx context.Context, id string) (Challenge, error) {
	var (
		c      Challenge
		expiry int64
	)
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM challenges WHERE id = ? AND expires_at > ? RETURNING id, purpose, data, expires_at`,
		id, s.nowNanos()).Scan(&c.ID, &c.Purpose, &c.Data, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return Challenge{}, ErrNotFound
	}
	if err != nil {
		return Challenge{}, err
	}
	c.ExpiresAt = ts(expiry)
	return c, nil
}

// PutIdempotent remembers that the raw client key produced taskID for a request
// body with bodyHash, for ttl. A live key is never overwritten: ErrKeyExists
// (the caller then calls GetIdempotent and compares body hashes: same body =>
// replay the task, different body => 409 conflict). An expired key is replaced.
func (s *Store) PutIdempotent(ctx context.Context, key, bodyHash string, taskID int64, ttl time.Duration) error {
	if key == "" || ttl <= 0 {
		return errors.New("store: idempotency key needs a value and a positive ttl")
	}
	kh := HashSecret(key)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		n := s.nowNanos()
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE key_hash = ? AND expires_at <= ?`, kh, n); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys(key_hash, body_hash, task_id, expires_at) VALUES (?,?,?,?) ON CONFLICT(key_hash) DO NOTHING`,
			kh, bodyHash, taskID, n+int64(ttl))
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			return ErrKeyExists
		}
		return nil
	})
}

// GetIdempotent returns the task and body hash recorded for a live key.
func (s *Store) GetIdempotent(ctx context.Context, key string) (taskID int64, bodyHash string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT task_id, body_hash FROM idempotency_keys WHERE key_hash = ? AND expires_at > ?`,
		HashSecret(key), s.nowNanos()).Scan(&taskID, &bodyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return taskID, bodyHash, true, nil
}

// PurgeExpired deletes expired sessions, challenges and idempotency keys in one
// transaction and returns how many rows went away. Safe next to concurrent reads
// (readers already ignore expired rows).
func (s *Store) PurgeExpired(ctx context.Context) (int64, error) {
	var total int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n := s.nowNanos()
		for _, q := range []string{
			`DELETE FROM sessions WHERE expires_at <= ?`,
			`DELETE FROM challenges WHERE expires_at <= ?`,
			`DELETE FROM idempotency_keys WHERE expires_at <= ?`,
		} {
			res, err := tx.ExecContext(ctx, q, n)
			if err != nil {
				return err
			}
			c, _ := res.RowsAffected()
			total += c
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// inTx runs fn in an immediate transaction (the DSN sets _txlock=immediate).
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
