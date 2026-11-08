package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"aigate/internal/orch/store"
)

// Principal kinds: who a request acts as, and the path it came in on (the
// ledger's "via").
const (
	KindSocket  = "socket"  // local unix socket, peer uid on the allowlist
	KindSession = "session" // browser session cookie (passkey login, Task 20)
	KindToken   = "token"   // bearer token (SSH challenge login, Task 18)
)

// Principal is the authenticated caller of a request. ID is "uid:<n>" for a
// socket peer and the signer id the session/token was issued to otherwise.
type Principal struct {
	ID   string
	Kind string
}

const (
	// SessionCookieName carries the raw browser session token.
	SessionCookieName = "orch_session"
	// CSRFHeader carries the session's CSRF token on every write (design 7).
	CSRFHeader = "X-Orch-CSRF"

	// SessionIdleTTL is the sliding expiry of a browser session; each
	// authenticated request moves it forward (never past the absolute cap).
	SessionIdleTTL = 8 * time.Hour
	// SessionMaxLifetime caps a browser session from its creation (CF5).
	SessionMaxLifetime = 24 * time.Hour
	// TokenMaxLifetime caps a bearer token from its creation. Tokens are not
	// slid: the expiry set at issue (Task 18: 15 minutes) is final.
	TokenMaxLifetime = 15 * time.Minute

	// IdempotencyTTL is how long a submit's Idempotency-Key is remembered.
	IdempotencyTTL = 24 * time.Hour
	maxIdemKeyLen  = 128
)

// Authenticator resolves HTTP credentials. Bearer gets the raw token from
// "Authorization: Bearer <token>", Session the raw session cookie value. Both
// return ok=false for anything unknown, expired or of the wrong kind; they
// never return the credential in an error.
type Authenticator interface {
	Bearer(ctx context.Context, raw string) (Principal, bool)
	Session(ctx context.Context, raw string) (Principal, *store.Session, bool)
}

// StoreAuthenticator resolves sessions and bearer tokens from the store's
// sessions table: a browser session is a row of Kind "session", a bearer token
// a row of Kind "token" (Task 18 issues them with store.CreateSession). A
// credential presented on the other channel is refused.
type StoreAuthenticator struct {
	St  *store.Store
	Now func() time.Time // nil: time.Now (tests move it)
}

func (a *StoreAuthenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// lookup returns the live row of the wanted kind for raw, deleting it when it
// is older than maxLife.
func (a *StoreAuthenticator) lookup(ctx context.Context, raw, kind string, maxLife time.Duration) (store.Session, bool) {
	if raw == "" {
		return store.Session{}, false
	}
	s, err := a.St.GetSession(ctx, raw)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("orchd: auth: session lookup: %v", err)
		}
		return store.Session{}, false
	}
	// The row was found by its hash; compare again in constant time so the
	// decision never rests on a non-constant-time path (CF7).
	if subtle.ConstantTimeCompare([]byte(store.HashSecret(raw)), []byte(s.IDHash)) != 1 || s.Kind != kind {
		return store.Session{}, false
	}
	if a.now().Sub(s.CreatedAt) >= maxLife {
		if err := a.St.DeleteSession(ctx, raw); err != nil {
			log.Printf("orchd: auth: delete expired session: %v", err)
		}
		return store.Session{}, false
	}
	return s, true
}

// Bearer resolves a bearer token (a Kind "token" row within TokenMaxLifetime).
func (a *StoreAuthenticator) Bearer(ctx context.Context, raw string) (Principal, bool) {
	s, ok := a.lookup(ctx, raw, KindToken, TokenMaxLifetime)
	if !ok {
		return Principal{}, false
	}
	return Principal{ID: s.Principal, Kind: KindToken}, true
}

// Session resolves a browser session and slides its idle expiry, never past
// CreatedAt+SessionMaxLifetime.
func (a *StoreAuthenticator) Session(ctx context.Context, raw string) (Principal, *store.Session, bool) {
	s, ok := a.lookup(ctx, raw, KindSession, SessionMaxLifetime)
	if !ok {
		return Principal{}, nil, false
	}
	ttl := SessionIdleTTL
	if left := s.CreatedAt.Add(SessionMaxLifetime).Sub(a.now()); left < ttl {
		ttl = left
	}
	if ttl <= 0 {
		return Principal{}, nil, false
	}
	if err := a.St.TouchSession(ctx, raw, ttl); err != nil {
		return Principal{}, nil, false
	}
	return Principal{ID: s.Principal, Kind: KindSession}, &s, true
}

// Context plumbing.

type ctxKey int

const (
	keyConn ctxKey = iota
	keyPrincipal
	keySession
)

// connInfo is what ConnContext learned about a connection.
type connInfo struct {
	unix bool
	uid  uint32
	err  error // reading the peer credentials failed (unix only)
}

// peerUID is the seam tests swap; production reads the kernel's record.
var peerUID = peerCredUID

// ConnContext records, per connection, whether it is a unix socket and the
// peer's uid (or why it could not be read). Every http.Server serving the
// handler must set it as its ConnContext: a request without this information
// is treated as untrusted HTTP (credentials required), never as a socket peer.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	ci := connInfo{}
	if _, ok := c.(*net.UnixConn); ok {
		ci.unix = true
		ci.uid, ci.err = peerUID(c)
	}
	return context.WithValue(ctx, keyConn, ci)
}

// PrincipalFrom returns the request's authenticated principal.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(keyPrincipal).(Principal)
	return p, ok
}

// sessionFrom returns the browser session behind a session principal.
func sessionFrom(ctx context.Context) (*store.Session, bool) {
	s, ok := ctx.Value(keySession).(*store.Session)
	return s, ok && s != nil
}

func withPrincipal(ctx context.Context, p Principal, s *store.Session) context.Context {
	ctx = context.WithValue(ctx, keyPrincipal, p)
	if s != nil {
		ctx = context.WithValue(ctx, keySession, s)
	}
	return ctx
}

var (
	errUnauthenticated = apiErr(http.StatusUnauthorized, "unauthenticated", "인증이 필요합니다")
	errForbiddenUID    = apiErr(http.StatusForbidden, "forbidden_uid", "이 사용자는 소켓으로 접근할 수 없습니다")
	errCSRF            = apiErr(http.StatusForbidden, "csrf_failed", "CSRF 확인에 실패했습니다")
	errBadIdemKey      = apiErr(http.StatusBadRequest, "bad_idempotency_key", "Idempotency-Key는 출력 가능한 ASCII 1~128바이트 하나여야 합니다")
	errIdemConflict    = apiErr(http.StatusConflict, "idempotency_conflict", "같은 Idempotency-Key로 다른 요청이 이미 처리되었습니다")
)

// socketUIDSet builds the socket allowlist. Empty means the daemon's own uid
// only.
func socketUIDSet(uids []uint32) map[uint32]bool {
	set := map[uint32]bool{}
	for _, u := range uids {
		set[u] = true
	}
	if len(set) == 0 {
		set[uint32(os.Getuid())] = true
	}
	return set
}

// authenticate attaches the request's principal or refuses it before any
// handler runs. A unix socket peer must have an allowlisted uid (403
// forbidden_uid otherwise, also when the credentials cannot be read). Any
// other request, including one with no connection information, must carry a
// bearer token or a session cookie (401 unauthenticated). A malformed
// Authorization header is refused without falling back to the cookie.
func (h *handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ci, _ := r.Context().Value(keyConn).(connInfo)
		if ci.unix {
			if ci.err != nil || !h.socketUIDs[ci.uid] {
				if ci.err != nil {
					log.Printf("orchd: socket peer refused: %v", ci.err)
				} else {
					log.Printf("orchd: socket peer uid %d refused (not on the allowlist)", ci.uid)
				}
				writeErr(w, errForbiddenUID)
				return
			}
			p := Principal{ID: "uid:" + strconv.FormatUint(uint64(ci.uid), 10), Kind: KindSocket}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p, nil)))
			return
		}
		if h.auth == nil {
			writeErr(w, errUnauthenticated)
			return
		}
		var (
			p    Principal
			sess *store.Session
			ok   bool
		)
		if authz := r.Header.Values("Authorization"); len(authz) > 0 {
			scheme, tok, found := strings.Cut(authz[0], " ")
			if len(authz) == 1 && found && strings.EqualFold(scheme, "Bearer") && tok != "" {
				p, ok = h.auth.Bearer(r.Context(), tok)
			}
		} else if c, err := r.Cookie(SessionCookieName); err == nil {
			p, sess, ok = h.auth.Session(r.Context(), c.Value)
		}
		if !ok {
			writeErr(w, errUnauthenticated)
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p, sess)))
	})
}

// requireWrite admits the write tier: a socket peer, a bearer token (design 7:
// the CLI's token unlocks general writes, never an approval without a
// signature), or a browser session whose X-Orch-CSRF header matches the
// session's CSRF token.
func (h *handler) requireWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			writeErr(w, errUnauthenticated)
			return
		}
		switch p.Kind {
		case KindSocket, KindToken:
		case KindSession:
			s, ok := sessionFrom(r.Context())
			got := r.Header.Values(CSRFHeader)
			if !ok || len(got) != 1 || got[0] == "" ||
				subtle.ConstantTimeCompare([]byte(store.HashSecret(got[0])), []byte(s.CSRFHash)) != 1 {
				writeErr(w, errCSRF)
				return
			}
		default:
			writeErr(w, errUnauthenticated)
			return
		}
		next(w, r)
	}
}

// healthz is the only unauthenticated endpoint; it reveals nothing.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// idempotencyKey returns the request's Idempotency-Key ("" when absent): one
// header of 1..128 printable ASCII bytes.
func idempotencyKey(r *http.Request) (string, error) {
	vals := r.Header.Values("Idempotency-Key")
	if len(vals) == 0 {
		return "", nil
	}
	k := vals[0]
	if len(vals) != 1 || k == "" || len(k) > maxIdemKeyLen {
		return "", errBadIdemKey
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return "", errBadIdemKey
		}
	}
	return k, nil
}

// scopedIdemKey binds a client key to the principal, so one principal can
// neither replay nor probe another's keys. The store hashes the result.
func scopedIdemKey(p Principal, key string) string {
	return p.Kind + "\x00" + p.ID + "\x00" + key
}

func bodyHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
