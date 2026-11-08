package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/store"
)

// SSH challenge login (design 7, CLI over HTTPS). Two unauthenticated routes:
//
//	POST /api/v1/auth/challenge {"signer"}           -> {"challenge_id","message","expires"}
//	POST /api/v1/auth/token     {"challenge_id","sig"} -> {"token","expires"}
//
// The challenge step does not read the signers registry: a known and an
// unknown signer id get challenges of the same shape, so the endpoint is no
// registry oracle. Only the id's format is checked (it is embedded in the
// signed message, so a newline must be impossible). The token step consumes
// the challenge (single use, also on failure), re-reads the registry, and
// verifies an `ssh-keygen -Y sign` signature in the NamespaceLogin namespace
// over the exact message. An unknown signer, a signer that is not ssh-ed25519,
// a wrong key, a wrong namespace and a malformed signature all answer the same
// bad_signature (no unknown_signer code, so no oracle at this step either).
// The token is a bearer token principal (KindToken, ID = signer id) valid for
// TokenMaxLifetime from issue and never extended. A token permits general
// writes but never an approval: the grant endpoint needs its own approval
// signature.

const (
	// ChallengePurposeLogin is the store purpose of SSH login challenges.
	// Other challenges (WebAuthn) must use other purposes; the token step
	// refuses any challenge whose purpose differs.
	ChallengePurposeLogin = "login"
	// LoginChallengeTTL is how long a login challenge can be answered.
	LoginChallengeTTL = 2 * time.Minute
	// LoginMagic is the first line of a login message.
	LoginMagic = "aigate-login-v1"

	loginMaxFailures = 5           // failed token attempts per signer id ...
	loginFailWindow  = time.Minute // ... per window, then 429
	maxLimiterKeys   = 4096
	maxLoginBody     = 16 << 10
)

var (
	errLoginBadRequest  = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"signer\": 서명자 ID})")
	errLoginBadSigner   = apiErr(http.StatusBadRequest, "bad_request", "서명자 ID 형식이 올바르지 않습니다")
	errLoginTokenBody   = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"challenge_id\", \"sig\"})")
	errLoginTooLarge    = apiErr(http.StatusRequestEntityTooLarge, "too_large", "요청이 너무 큽니다")
	errBadChallenge     = apiErr(http.StatusUnauthorized, "bad_challenge", "로그인 챌린지가 없거나 만료되었거나 이미 사용되었습니다. 처음부터 다시 로그인하세요")
	errLoginBadSig      = apiErr(http.StatusUnauthorized, "bad_signature", "로그인 서명이 올바르지 않습니다")
	errTooManyAttempts  = apiErr(http.StatusTooManyRequests, "too_many_attempts", "로그인 실패가 너무 많습니다. 잠시 뒤 다시 시도하세요")
	errLoginSignersDown = apiErr(http.StatusServiceUnavailable, "signers_unavailable", "서명자 목록을 신뢰할 수 없어 로그인을 처리할 수 없습니다")
)

// LoginMessage is the exact text a login signature covers. Every line,
// the last included, ends with "\n".
func LoginMessage(signer, nonce string, issued time.Time) string {
	return LoginMagic + "\nsigner: " + signer + "\nnonce: " + nonce +
		"\nissued: " + issued.UTC().Format(time.RFC3339) + "\n"
}

// loginChallengeData is a login challenge's stored Data.
type loginChallengeData struct {
	Signer  string `json:"signer"`
	Message string `json:"message"`
}

func (h *handler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// decodeLoginBody strictly decodes a small JSON body into v.
func decodeLoginBody(w http.ResponseWriter, r *http.Request, v any, bad *httpError) *httpError {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errLoginTooLarge
		}
		return bad
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return bad
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad
	}
	return nil
}

// authChallenge serves POST /api/v1/auth/challenge.
func (h *handler) authChallenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Signer string `json:"signer"`
	}
	if herr := decodeLoginBody(w, r, &req, errLoginBadRequest); herr != nil {
		writeErr(w, herr)
		return
	}
	if !signing.ValidSignerID(req.Signer) {
		writeErr(w, errLoginBadSigner)
		return
	}
	id, err1 := randomHex(32)
	nonce, err2 := randomHex(32)
	if err1 != nil || err2 != nil {
		writeErr(w, errInternal)
		return
	}
	issued := h.clock().UTC().Truncate(time.Second)
	msg := LoginMessage(req.Signer, nonce, issued)
	data, err := json.Marshal(loginChallengeData{Signer: req.Signer, Message: msg})
	if err != nil {
		writeErr(w, errInternal)
		return
	}
	expires := issued.Add(LoginChallengeTTL)
	if err := h.st.SaveChallenge(r.Context(), store.Challenge{ID: id, Purpose: ChallengePurposeLogin,
		Data: string(data), ExpiresAt: expires}); err != nil {
		log.Printf("orchd: login challenge: %v", err)
		writeErr(w, errInternal)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"challenge_id": id, "message": msg, "expires": expires.Format(time.RFC3339)})
}

// authToken serves POST /api/v1/auth/token.
func (h *handler) authToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challenge_id"`
		Sig         string `json:"sig"`
	}
	if herr := decodeLoginBody(w, r, &req, errLoginTokenBody); herr != nil {
		writeErr(w, herr)
		return
	}
	ctx := r.Context()
	// Consumed here, whatever happens next (single use, also on failure).
	ch, err := h.st.TakeChallenge(ctx, req.ChallengeID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, errBadChallenge)
		return
	}
	if err != nil {
		log.Printf("orchd: login token: take challenge: %v", err)
		writeErr(w, errInternal)
		return
	}
	var data loginChallengeData
	// The store checks the expiry with its own clock; checking again with
	// the handler's keeps the TTL one decision with the issued time.
	if ch.Purpose != ChallengePurposeLogin || json.Unmarshal([]byte(ch.Data), &data) != nil ||
		!signing.ValidSignerID(data.Signer) || !h.clock().Before(ch.ExpiresAt) {
		writeErr(w, errBadChallenge)
		return
	}
	if !h.loginFails.allow(data.Signer) {
		writeErr(w, errTooManyAttempts)
		return
	}
	// The registry is read now, not at challenge time: a signer removed in
	// between cannot log in.
	reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	if err != nil {
		log.Printf("orchd: login token: signers registry unavailable: %v", err)
		writeErr(w, errLoginSignersDown)
		return
	}
	signer, ok := reg.Find(data.Signer)
	if !ok || signer.Type != signing.TypeSSHEd25519 ||
		signing.VerifySSHSig(signer.PublicKey, signing.NamespaceLogin, []byte(data.Message), req.Sig) != nil {
		h.loginFails.fail(data.Signer)
		log.Printf("orchd: login token: refused for signer %q (known=%v)", data.Signer, ok)
		writeErr(w, errLoginBadSig)
		return
	}
	tok, err := newBearerToken()
	if err != nil {
		writeErr(w, errInternal)
		return
	}
	s, err := h.st.CreateSession(ctx, tok, "", signer.ID, KindToken, TokenMaxLifetime)
	if err != nil {
		log.Printf("orchd: login token: create: %v", err)
		writeErr(w, errInternal)
		return
	}
	h.loginFails.reset(signer.ID)
	log.Printf("orchd: login token issued to signer %q", signer.ID)
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "expires": s.ExpiresAt.UTC().Format(time.RFC3339)})
}

// newBearerToken is 256 random bits, base64url without padding.
func newBearerToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// failLimiter counts failed login attempts per key in a sliding window. It is
// in memory (one orchd per database) and bounded: when it holds maxLimiterKeys
// keys (after dropping those with no failure inside the window), failures of
// new keys are counted in one shared overflow bucket, and while that bucket is
// over the limit every key is refused. A flood of made-up signer ids thus
// fails closed instead of evicting a real signer's count.
type failLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	fails map[string][]time.Time
}

const overflowKey = "\x00overflow"

func newFailLimiter(now func() time.Time) *failLimiter {
	if now == nil {
		now = time.Now
	}
	return &failLimiter{now: now, fails: map[string][]time.Time{}}
}

// recent prunes key's failures outside the window; the caller holds mu.
func (l *failLimiter) recent(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= loginFailWindow {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = ts
	return ts
}

func (l *failLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	return len(l.recent(key, now)) < loginMaxFailures && len(l.recent(overflowKey, now)) < loginMaxFailures
}

func (l *failLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, ok := l.fails[key]; !ok && len(l.fails) >= maxLimiterKeys {
		for k := range l.fails {
			l.recent(k, now)
		}
		if len(l.fails) >= maxLimiterKeys {
			key = overflowKey
		}
	}
	l.fails[key] = append(l.recent(key, now), now)
}

func (l *failLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func (l *failLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fails)
}
