package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/store"
	"aigate/internal/webauthn"
)

// Passkey (WebAuthn) login, browser sessions and CSRF (design 7, R8, R17).
//
//	GET  /api/v1/auth/signers                       unauthenticated
//	    -> 200 {"signers":[{"id","credential_id","label"}]}   active webauthn-es256 signers
//	POST /api/v1/auth/webauthn/login/begin {"signer"}   unauthenticated
//	    -> 200 {"challenge_id","expires","options":{PublicKeyCredentialRequestOptionsJSON}}
//	POST /api/v1/auth/webauthn/login/finish {"challenge_id","credential"}   unauthenticated
//	    -> 200 {"csrf","expires","signer"} + Set-Cookie __Host-orch_session (HttpOnly; Secure; SameSite=Strict)
//	GET  /api/v1/auth/session                       session or token (socket too)
//	    -> 200 {"principal","kind","expires"?,"csrf"?}
//	POST /api/v1/auth/logout                        write tier (session: X-Orch-CSRF)
//	    -> 200 {"ok":true} + clearing Set-Cookie
//
// begin answers every well-formed signer id the same way, so it is no
// registry oracle: a real active webauthn signer gets its credential id and
// its pinned RPID; any other id (unknown, ssh-ed25519, pending, or an
// unreadable registry) gets a fake credential id, HMAC-SHA256(per-process
// random key, id), and Config.RPID. The failure appears at finish as
// bad_assertion. The challenge row's purpose is "webauthn-login", distinct
// from "login" (SSH), "enroll" and "webauthn-register" (CF14).
//
// finish consumes the challenge first (single use, also on failure), re-reads
// the registry, and requires the stored signer to still be an active
// webauthn-es256 signer whose credential id equals the presented id and rawId
// (decoded bytes, CF2). The assertion is verified with the stored challenge
// bytes and the SIGNER entry's RPID and Origin (R8), UV required. Signature
// counters are not checked (internal/webauthn does not, and the root-owned
// registry has nowhere to keep them). Success creates a store session (Kind
// session, principal = signer id; the raw 256-bit token and a separate random
// CSRF token are stored only as hashes) and sets the cookie; the CSRF token is
// returned only in this body and by GET /auth/session, never in a cookie.
//
// GET /auth/session ROTATES a session's CSRF token: the store keeps only its
// hash, so the raw value cannot be read back. The web shell calls it on load;
// the previous CSRF token (of this session, in any tab) stops working. Like
// the idle-expiry touch (R17-5) this writes only auth bookkeeping, no domain
// state.
//
// Errors are fixed codes and texts (CF1): bad_request, body_too_large,
// webauthn_unconfigured, busy, bad_challenge, bad_assertion,
// signers_unavailable. Rate limits, Origin/Host and JSON-only checks are the
// HTTP middleware's (Task 21).

const (
	// ChallengePurposeWebAuthnLogin is the store purpose of passkey login
	// challenges.
	ChallengePurposeWebAuthnLogin = "webauthn-login"
	// WebAuthnLoginChallengeTTL is how long a login ceremony can take.
	WebAuthnLoginChallengeTTL = 2 * time.Minute
	// DefaultWebAuthnLoginChallengeCap bounds the live passkey login
	// challenges (503 busy at the cap).
	DefaultWebAuthnLoginChallengeCap = 1000

	fakeCredIDLen = 32
)

var (
	errBadAssertion = apiErr(http.StatusUnauthorized, "bad_assertion", "패스키 로그인 응답이 올바르지 않습니다. 처음부터 다시 로그인하세요")
	errWALoginBody  = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바르지 않습니다({\"challenge_id\", \"credential\"})")
)

// signerView is one public entry of GET /auth/signers.
type signerView struct {
	ID           string `json:"id"`
	CredentialID string `json:"credential_id"`
	Label        string `json:"label"`
}

// authSigners serves GET /api/v1/auth/signers: the active passkey signers,
// for the login screen. An absent registry is an empty list; an untrusted or
// unreadable one is 503 signers_unavailable (the detail is logged only).
func (h *handler) authSigners(w http.ResponseWriter, r *http.Request) {
	out := []signerView{}
	reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	switch {
	case errors.Is(err, signing.ErrRegistryAbsent):
	case err != nil:
		log.Printf("orchd: auth signers: registry unavailable: %v", err)
		writeErr(w, errLoginSignersDown)
		return
	default:
		for _, s := range reg.Signers {
			if s.Type == signing.TypeWebAuthnES256 {
				out = append(out, signerView{ID: s.ID, CredentialID: s.CredentialID, Label: s.Label})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"signers": out})
}

// webauthnLoginData is a passkey login challenge's stored Data.
type webauthnLoginData struct {
	Signer    string `json:"signer"`
	Challenge string `json:"challenge"` // base64url, 32 bytes
}

// fakeCredentialID is the credential id begin offers for an id that is not an
// active passkey signer: stable per id for the life of the process.
func (h *handler) fakeCredentialID(signer string) string {
	m := hmac.New(sha256.New, h.credKey)
	m.Write([]byte("aigate-fake-credential\x00" + signer))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:fakeCredIDLen])
}

// activePasskey returns the active webauthn-es256 signer id names, if any.
func activePasskey(reg signing.Registry, id string) (signing.Signer, bool) {
	s, ok := reg.Find(id)
	return s, ok && s.Type == signing.TypeWebAuthnES256
}

// webauthnLoginBegin serves POST /api/v1/auth/webauthn/login/begin.
func (h *handler) webauthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Signer string `json:"signer"`
	}
	if herr := decodeLoginBody(w, r, &req, errLoginBadRequest); herr != nil {
		writeErr(w, herr)
		return
	}
	if h.rpID == "" {
		writeErr(w, errWebAuthnOff)
		return
	}
	if !signing.ValidSignerID(req.Signer) {
		writeErr(w, errLoginBadSigner)
		return
	}
	ctx := r.Context()
	// Soft cap (count, then save: concurrent requests may pass it by a few).
	switch n, err := h.st.CountChallenges(ctx, ChallengePurposeWebAuthnLogin); {
	case err != nil:
		log.Printf("orchd: passkey login begin: count: %v", err)
		writeErr(w, errInternal)
		return
	case n >= h.webauthnLoginCap:
		log.Printf("orchd: passkey login begin refused: %d live challenges (cap %d)", n, h.webauthnLoginCap)
		writeErr(w, errLoginBusy)
		return
	}
	credID, rpID := h.fakeCredentialID(req.Signer), h.rpID
	// The registry decides only what is offered, never the status or shape.
	if reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions); err == nil {
		if s, ok := activePasskey(reg, req.Signer); ok {
			if raw, err := rawURLStrict.DecodeString(s.CredentialID); err == nil && len(raw) > 0 {
				credID, rpID = base64.RawURLEncoding.EncodeToString(raw), s.RPID
			}
		}
	}
	id, err1 := randomHex(32)
	chal, err2 := randomB64(32)
	if err1 != nil || err2 != nil {
		writeErr(w, errInternal)
		return
	}
	data, err := json.Marshal(webauthnLoginData{Signer: req.Signer, Challenge: chal})
	if err != nil {
		writeErr(w, errInternal)
		return
	}
	expires := h.clock().UTC().Truncate(time.Second).Add(WebAuthnLoginChallengeTTL)
	if err := h.st.SaveChallenge(ctx, store.Challenge{ID: id, Purpose: ChallengePurposeWebAuthnLogin,
		Data: string(data), ExpiresAt: expires}); err != nil {
		log.Printf("orchd: passkey login begin: save: %v", err)
		writeErr(w, errInternal)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge_id": id,
		"expires":      expires.Format(time.RFC3339),
		"options": map[string]any{
			"challenge":        chal,
			"rpId":             rpID,
			"allowCredentials": []map[string]string{{"type": "public-key", "id": credID}},
			"userVerification": "required",
			"timeout":          WebAuthnLoginChallengeTTL.Milliseconds(),
		},
	})
}

// assertionCredential is the part of PublicKeyCredential.toJSON() (an
// assertion) that finish reads; other members are ignored.
type assertionCredential struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	} `json:"response"`
}

// webauthnLoginFinish serves POST /api/v1/auth/webauthn/login/finish.
func (h *handler) webauthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
	}
	if herr := decodeLoginBody(w, r, &req, errWALoginBody); herr != nil {
		writeErr(w, herr)
		return
	}
	if h.rpID == "" {
		writeErr(w, errWebAuthnOff)
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
		log.Printf("orchd: passkey login finish: take: %v", err)
		writeErr(w, errInternal)
		return
	}
	var d webauthnLoginData
	if ch.Purpose != ChallengePurposeWebAuthnLogin || json.Unmarshal([]byte(ch.Data), &d) != nil ||
		!signing.ValidSignerID(d.Signer) || !h.clock().Before(ch.ExpiresAt) {
		writeErr(w, errBadChallenge)
		return
	}
	challenge, ok := decodeB64URL(d.Challenge)
	if !ok || len(challenge) != 32 {
		writeErr(w, errBadChallenge)
		return
	}
	// The registry is read now: a signer removed since begin cannot log in.
	reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	if err != nil {
		log.Printf("orchd: passkey login finish: signers registry unavailable: %v", err)
		writeErr(w, errLoginSignersDown)
		return
	}
	signer, ok := activePasskey(reg, d.Signer)
	if !ok {
		log.Printf("orchd: passkey login refused: %q is not an active passkey signer", d.Signer)
		writeErr(w, errBadAssertion)
		return
	}
	if reason := h.checkAssertion(signer, challenge, req.Credential); reason != "" {
		log.Printf("orchd: passkey login refused for signer %q: %s", signer.ID, reason)
		writeErr(w, errBadAssertion)
		return
	}
	tok, err1 := newBearerToken()
	csrf, err2 := randomB64(32)
	if err1 != nil || err2 != nil {
		writeErr(w, errInternal)
		return
	}
	s, err := h.st.CreateSession(ctx, tok, csrf, signer.ID, KindSession, h.sessionTTL)
	if err != nil {
		log.Printf("orchd: passkey login: create session: %v", err)
		writeErr(w, errInternal)
		return
	}
	log.Printf("orchd: passkey login: session issued to signer %q", signer.ID)
	setSessionCookie(w, tok, int(SessionMaxLifetime/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"csrf": csrf,
		"expires": s.ExpiresAt.UTC().Format(time.RFC3339), "signer": signer.ID})
}

// checkAssertion verifies a login assertion for signer over challenge; ""
// means valid, anything else is the (server-log only) reason.
func (h *handler) checkAssertion(signer signing.Signer, challenge []byte, raw json.RawMessage) string {
	var cred assertionCredential
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &cred) != nil || cred.Type != "public-key" {
		return "malformed credential"
	}
	credID, ok1 := decodeB64URL(cred.ID)
	clientData, ok2 := decodeB64URL(cred.Response.ClientDataJSON)
	authData, ok3 := decodeB64URL(cred.Response.AuthenticatorData)
	sig, ok4 := decodeB64URL(cred.Response.Signature)
	if !(ok1 && ok2 && ok3 && ok4) {
		return "bad encoding"
	}
	want, err := rawURLStrict.DecodeString(signer.CredentialID)
	if err != nil || len(want) == 0 || subtle.ConstantTimeCompare(credID, want) != 1 {
		return "credential id differs from the signer's"
	}
	if cred.RawID != "" {
		if rawID, ok := decodeB64URL(cred.RawID); !ok || !bytes.Equal(rawID, credID) {
			return "rawId differs from id"
		}
	}
	point, err := rawURLStrict.DecodeString(signer.PublicKey)
	if err != nil {
		return "signer public key"
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return "signer public key"
	}
	if err := webauthn.VerifyAssertion(webauthn.AssertionInput{
		PublicKey:         pub,
		AuthenticatorData: authData,
		ClientDataJSON:    clientData,
		Signature:         sig,
		Challenge:         challenge,
		RPID:              signer.RPID,
		Origin:            signer.Origin,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// setSessionCookie sets (maxAge > 0) or clears (maxAge < 0) the session
// cookie. Host-only (no Domain), whole site, never readable by scripts, sent
// over HTTPS only and never on cross-site requests.
func setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

// currentSessionToken returns the raw cookie value behind the request's
// session principal, checked against the authenticated session row.
func currentSessionToken(r *http.Request) (string, bool) {
	sess, ok := sessionFrom(r.Context())
	c, err := r.Cookie(SessionCookieName)
	if !ok || err != nil || subtle.ConstantTimeCompare([]byte(store.HashSecret(c.Value)), []byte(sess.IDHash)) != 1 {
		return "", false
	}
	return c.Value, true
}

// authSession serves GET /api/v1/auth/session. A session principal gets a
// fresh CSRF token (rotated; see the file comment); a token principal its
// expiry; a socket principal only who it is.
func (h *handler) authSession(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		writeErr(w, errUnauthenticated)
		return
	}
	out := map[string]string{"principal": p.ID, "kind": p.Kind}
	switch p.Kind {
	case KindSession:
		tok, ok := currentSessionToken(r)
		if !ok {
			writeErr(w, errUnauthenticated)
			return
		}
		csrf, err := randomB64(32)
		if err != nil {
			writeErr(w, errInternal)
			return
		}
		s, err := h.st.RotateSessionCSRF(r.Context(), tok, csrf)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, errUnauthenticated)
			return
		}
		if err != nil {
			log.Printf("orchd: auth session: rotate csrf: %v", err)
			writeErr(w, errInternal)
			return
		}
		out["csrf"], out["expires"] = csrf, s.ExpiresAt.UTC().Format(time.RFC3339)
	case KindToken:
		tok, _ := bearerToken(r)
		s, err := h.st.GetSession(r.Context(), tok)
		if err != nil {
			writeErr(w, errUnauthenticated)
			return
		}
		out["expires"] = s.ExpiresAt.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// authLogout serves POST /api/v1/auth/logout (write tier): it deletes the
// session or bearer token server-side and clears the cookie. Deleting is
// idempotent; a second call with the dead cookie is simply unauthenticated.
func (h *handler) authLogout(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	var tok string
	switch p.Kind {
	case KindSession:
		tok, _ = currentSessionToken(r)
	case KindToken:
		tok, _ = bearerToken(r)
	}
	if tok != "" {
		if err := h.st.DeleteSession(r.Context(), tok); err != nil {
			log.Printf("orchd: logout: %v", err)
			writeErr(w, errInternal)
			return
		}
		log.Printf("orchd: logout: %s of %q ended", p.Kind, p.ID)
	}
	if p.Kind == KindSession {
		setSessionCookie(w, "", -1)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// newCredKey is the per-process HMAC key for fake credential ids.
func newCredKey() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("orchd: crypto/rand: " + err.Error())
	}
	return k
}
