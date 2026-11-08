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

	// DefaultLoginChallengeCap bounds the live (unexpired, untaken) login
	// challenges; at the cap /auth/challenge answers 503 busy. Rate limiting
	// of the auth routes is Task 21's middleware.
	DefaultLoginChallengeCap = 1000
	maxLoginBody             = 16 << 10
)

var (
	errLoginBadRequest  = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"signer\": 서명자 ID})")
	errLoginBadSigner   = apiErr(http.StatusBadRequest, "bad_request", "서명자 ID 형식이 올바르지 않습니다")
	errLoginTokenBody   = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"challenge_id\", \"sig\"})")
	errLoginTooLarge    = apiErr(http.StatusRequestEntityTooLarge, "too_large", "요청이 너무 큽니다")
	errBadChallenge     = apiErr(http.StatusUnauthorized, "bad_challenge", "로그인 챌린지가 없거나 만료되었거나 이미 사용되었습니다. 처음부터 다시 로그인하세요")
	errLoginBadSig      = apiErr(http.StatusUnauthorized, "bad_signature", "로그인 서명이 올바르지 않습니다")
	errLoginBusy        = apiErr(http.StatusServiceUnavailable, "busy", "진행 중인 로그인 요청이 너무 많습니다. 잠시 뒤 다시 시도하세요")
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
	// Soft cap (count, then save: concurrent requests may pass it by a few).
	switch n, err := h.st.CountChallenges(r.Context(), ChallengePurposeLogin); {
	case err != nil:
		log.Printf("orchd: login challenge: count: %v", err)
		writeErr(w, errInternal)
		return
	case n >= h.loginChallengeCap:
		log.Printf("orchd: login challenge refused: %d live challenges (cap %d)", n, h.loginChallengeCap)
		writeErr(w, errLoginBusy)
		return
	}
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
