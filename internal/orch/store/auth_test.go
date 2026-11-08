package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock makes expiry tests deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openClock(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	s, path := openTemp(t)
	c := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	s.clock = c.Now
	return s, c, path
}

func TestMigrateFromV5KeepsTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (5)`,
		`INSERT INTO tasks(repo, base, prompt, state, created_at, updated_at) VALUES ('/r','b','keepme','awaiting',1,2)`,
		`INSERT INTO approvals_pending(task_id, kind, hash, base, head, reason, created_at, status) VALUES (1,'protected_modified','h','b','hd','why',1,'pending')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := s.GetTask(ctx, 1)
	if err != nil || got.Prompt != "keepme" {
		t.Fatalf("task: %+v %v", got, err)
	}
	if a, err := s.GetApproval(ctx, 1); err != nil || a.Hash != "h" {
		t.Fatalf("approval: %+v %v", a, err)
	}
	var v int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 6 {
		t.Fatalf("version %d", v)
	}
	for _, tbl := range []string{"sessions", "challenges", "idempotency_keys"} {
		if _, err := s.db.Exec(`SELECT count(*) FROM ` + tbl); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
	}
	s.Close()
	// Reopening a v6 database is a no-op.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestSessionExpiry(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	made, err := s.CreateSession(ctx, "tok-1", "csrf-1", "alice", "passkey", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if made.Principal != "alice" || made.Kind != "passkey" || !made.ExpiresAt.Equal(c.Now().Add(time.Hour)) {
		t.Fatalf("created %+v", made)
	}
	got, err := s.GetSession(ctx, "tok-1")
	if err != nil || got.IDHash != HashSecret("tok-1") || got.CSRFHash != HashSecret("csrf-1") || got.Principal != "alice" {
		t.Fatalf("get %+v %v", got, err)
	}
	if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	// Touch slides the expiry.
	c.Advance(50 * time.Minute)
	if err := s.TouchSession(ctx, "tok-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	c.Advance(30 * time.Minute) // 80 min after creation, past the original expiry
	got, err = s.GetSession(ctx, "tok-1")
	if err != nil || !got.LastSeen.Equal(c.Now().Add(-30*time.Minute)) {
		t.Fatalf("after touch: %+v %v", got, err)
	}
	c.Advance(31 * time.Minute)
	if _, err := s.GetSession(ctx, "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session returned: %v", err)
	}
	if err := s.TouchSession(ctx, "tok-1", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch of expired session: %v", err)
	}
	// Delete is idempotent.
	if _, err := s.CreateSession(ctx, "tok-2", "c", "bob", "ssh", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "tok-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session: %v", err)
	}
	// A token collision is refused, not overwritten.
	if _, err := s.CreateSession(ctx, "tok-3", "c", "a", "k", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, "tok-3", "c", "evil", "k", time.Hour); err == nil {
		t.Fatal("duplicate session id accepted")
	}
}

func TestSessionStoresHashNotSecret(t *testing.T) {
	s, _, path := openClock(t)
	ctx := context.Background()
	const tok = "RAW-SESSION-TOKEN-0123456789abcdef"
	const csrf = "RAW-CSRF-TOKEN-fedcba9876543210"
	const idem = "RAW-IDEMPOTENCY-KEY-777"
	id := newTask(t, s)
	if _, err := s.CreateSession(ctx, tok, csrf, "alice", "passkey", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIdempotent(ctx, idem, "bodyhash", id, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Dump every table through SQL.
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var dump strings.Builder
	for _, tbl := range tables {
		r, err := s.db.Query(`SELECT * FROM "` + tbl + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			r.Scan(ptrs...)
			fmt.Fprintf(&dump, "%s %v\n", tbl, vals)
			for _, v := range vals {
				if b, ok := v.([]byte); ok {
					dump.Write(b)
				}
			}
		}
		r.Close()
	}
	for _, secret := range []string{tok, csrf, idem} {
		if strings.Contains(dump.String(), secret) {
			t.Fatalf("table dump contains raw secret %q", secret)
		}
	}
	if !strings.Contains(dump.String(), HashSecret(tok)) {
		t.Fatal("hash of the token not stored")
	}
	// And the raw file bytes (including the WAL) while the store is open, then after close.
	check := func(when string) {
		for _, suffix := range []string{"", "-wal"} {
			b, err := os.ReadFile(path + suffix)
			if err != nil {
				continue
			}
			for _, secret := range []string{tok, csrf, idem} {
				if bytes.Contains(b, []byte(secret)) {
					t.Fatalf("%s: file %q contains raw secret %q", when, path+suffix, secret)
				}
			}
		}
	}
	check("open")
	s.Close()
	check("closed")
}

func TestChallengeSingleUse(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	ch := Challenge{ID: "c1", Purpose: "login", Data: `{"x":1}`, ExpiresAt: c.Now().Add(time.Minute)}
	if err := s.SaveChallenge(ctx, ch); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeChallenge(ctx, "c1")
	if err != nil || got.Purpose != "login" || got.Data != `{"x":1}` || !got.ExpiresAt.Equal(ch.ExpiresAt) {
		t.Fatalf("take: %+v %v", got, err)
	}
	if _, err := s.TakeChallenge(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second take: %v", err)
	}
	// Concurrent double take: exactly one wins.
	for round := 0; round < 20; round++ {
		id := fmt.Sprintf("race-%d", round)
		if err := s.SaveChallenge(ctx, Challenge{ID: id, Purpose: "p", ExpiresAt: c.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		var won, lost atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.TakeChallenge(ctx, id)
				switch {
				case err == nil:
					won.Add(1)
				case errors.Is(err, ErrNotFound):
					lost.Add(1)
				default:
					t.Errorf("take: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if won.Load() != 1 || lost.Load() != 7 {
			t.Fatalf("round %d: won %d lost %d", round, won.Load(), lost.Load())
		}
	}
	// Saving the same id twice is refused (no silent overwrite of a live challenge).
	if err := s.SaveChallenge(ctx, Challenge{ID: "dup", Purpose: "a", ExpiresAt: c.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChallenge(ctx, Challenge{ID: "dup", Purpose: "b", ExpiresAt: c.Now().Add(time.Minute)}); err == nil {
		t.Fatal("duplicate challenge id accepted")
	}
}

func TestChallengeExpiry(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	if err := s.SaveChallenge(ctx, Challenge{ID: "e", Purpose: "p", ExpiresAt: c.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute) // expiry instant counts as expired
	if _, err := s.TakeChallenge(ctx, "e"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired challenge taken: %v", err)
	}
	if _, err := s.TakeChallenge(ctx, "never"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestCountChallenges(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	count := func(purpose string) int {
		t.Helper()
		n, err := s.CountChallenges(ctx, purpose)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("login"); n != 0 {
		t.Fatalf("empty: %d", n)
	}
	for _, ch := range []Challenge{
		{ID: "a", Purpose: "login", ExpiresAt: c.Now().Add(time.Minute)},
		{ID: "b", Purpose: "login", ExpiresAt: c.Now().Add(2 * time.Minute)},
		{ID: "c", Purpose: "other", ExpiresAt: c.Now().Add(time.Minute)},
	} {
		if err := s.SaveChallenge(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	if count("login") != 2 || count("other") != 1 {
		t.Fatalf("counts %d %d", count("login"), count("other"))
	}
	if _, err := s.TakeChallenge(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if n := count("login"); n != 1 {
		t.Fatalf("after take: %d", n)
	}
	c.Advance(2 * time.Minute) // expired rows are not counted, even before a purge
	if n := count("login"); n != 0 {
		t.Fatalf("after expiry: %d", n)
	}
}

func TestIdempotencyKeyTTL(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	id := newTask(t, s)
	if _, _, ok, err := s.GetIdempotent(ctx, "k"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	if err := s.PutIdempotent(ctx, "k", "body-A", id, time.Hour); err != nil {
		t.Fatal(err)
	}
	tid, bh, ok, err := s.GetIdempotent(ctx, "k")
	if err != nil || !ok || tid != id || bh != "body-A" {
		t.Fatalf("get: %d %q %v %v", tid, bh, ok, err)
	}
	// A live key is not overwritten (a different body must be detected by the caller).
	if err := s.PutIdempotent(ctx, "k", "body-B", id, time.Hour); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("second put: %v", err)
	}
	if _, bh, _, _ := s.GetIdempotent(ctx, "k"); bh != "body-A" {
		t.Fatalf("body hash overwritten: %q", bh)
	}
	c.Advance(time.Hour)
	if _, _, ok, err := s.GetIdempotent(ctx, "k"); err != nil || ok {
		t.Fatalf("expired key visible: ok=%v err=%v", ok, err)
	}
	// An expired key can be reused.
	if err := s.PutIdempotent(ctx, "k", "body-B", id, time.Hour); err != nil {
		t.Fatalf("reuse after expiry: %v", err)
	}
	if _, bh, ok, _ := s.GetIdempotent(ctx, "k"); !ok || bh != "body-B" {
		t.Fatalf("reused: %q %v", bh, ok)
	}
}

func TestPurgeExpired(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	id := newTask(t, s)
	mustS := func(tok string, ttl time.Duration) {
		if _, err := s.CreateSession(ctx, tok, "c", "p", "k", ttl); err != nil {
			t.Fatal(err)
		}
	}
	mustS("s-short", time.Minute)
	mustS("s-long", time.Hour)
	for id, d := range map[string]time.Duration{"c-short": time.Minute, "c-long": time.Hour} {
		if err := s.SaveChallenge(ctx, Challenge{ID: id, Purpose: "p", ExpiresAt: c.Now().Add(d)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutIdempotent(ctx, "i-short", "b", id, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIdempotent(ctx, "i-long", "b", id, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeExpired(ctx); err != nil || n != 0 {
		t.Fatalf("nothing expired yet: %d %v", n, err)
	}
	c.Advance(2 * time.Minute)
	// Concurrent readers while purging.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s.GetSession(ctx, "s-long")
				s.GetIdempotent(ctx, "i-long")
			}
		}()
	}
	n, err := s.PurgeExpired(ctx)
	wg.Wait()
	if err != nil || n != 3 {
		t.Fatalf("purged %d %v", n, err)
	}
	if n, _ := s.PurgeExpired(ctx); n != 0 {
		t.Fatalf("second purge %d", n)
	}
	var cnt int
	for _, tbl := range []string{"sessions", "challenges", "idempotency_keys"} {
		s.db.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&cnt)
		if cnt != 1 {
			t.Fatalf("%s has %d rows, want 1", tbl, cnt)
		}
	}
	if _, err := s.GetSession(ctx, "s-long"); err != nil {
		t.Fatal(err)
	}
}

func TestRotateSessionCSRF(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	made, err := s.CreateSession(ctx, "tok-r", "csrf-old", "alice", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(10 * time.Minute)
	got, err := s.RotateSessionCSRF(ctx, "tok-r", "csrf-new")
	if err != nil {
		t.Fatal(err)
	}
	// Only the CSRF hash changes; the returned row is the stored one.
	if got.CSRFHash != HashSecret("csrf-new") || got.IDHash != HashSecret("tok-r") || got.Principal != "alice" ||
		got.Kind != "session" || !got.CreatedAt.Equal(made.CreatedAt) || !got.ExpiresAt.Equal(made.ExpiresAt) {
		t.Fatalf("rotated %+v (made %+v)", got, made)
	}
	stored, err := s.GetSession(ctx, "tok-r")
	if err != nil || stored != got {
		t.Fatalf("stored %+v %v, returned %+v", stored, err, got)
	}
	// Unknown and expired sessions are ErrNotFound and nothing is created.
	if _, err := s.RotateSessionCSRF(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	c.Advance(time.Hour)
	if _, err := s.RotateSessionCSRF(ctx, "tok-r", "csrf-late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := s.RotateSessionCSRF(ctx, "", "x"); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestSetClock(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	s.SetClock(func() time.Time { return at })
	made, err := s.CreateSession(ctx, "t", "c", "p", "session", time.Hour)
	if err != nil || !made.CreatedAt.Equal(at) {
		t.Fatalf("%+v %v", made, err)
	}
}

func TestTouchSessionCapped(t *testing.T) {
	s, c, _ := openClock(t)
	ctx := context.Background()
	made, err := s.CreateSession(ctx, "tok-c", "c", "alice", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	limit := made.CreatedAt.Add(90 * time.Minute)
	c.Advance(30 * time.Minute)
	if err := s.TouchSessionCapped(ctx, "tok-c", time.Hour, limit); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, "tok-c")
	if !got.ExpiresAt.Equal(limit) {
		t.Fatalf("capped expiry %v, want %v", got.ExpiresAt, limit)
	}
	// Under the cap the plain sliding window applies.
	if err := s.TouchSessionCapped(ctx, "tok-c", 10*time.Minute, limit); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, "tok-c"); !got.ExpiresAt.Equal(c.Now().Add(10 * time.Minute)) {
		t.Fatalf("uncapped expiry %v", got.ExpiresAt)
	}
	// A cap already passed refuses the touch and leaves the row alone.
	if err := s.TouchSessionCapped(ctx, "tok-c", time.Hour, c.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("passed cap: %v", err)
	}
	if got, _ := s.GetSession(ctx, "tok-c"); !got.ExpiresAt.Equal(c.Now().Add(10 * time.Minute)) {
		t.Fatalf("row changed: %v", got.ExpiresAt)
	}
}
