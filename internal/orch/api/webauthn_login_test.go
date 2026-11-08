package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/store"
)

// Passkey login tests. The synthetic authenticator here builds the assertion
// itself (authenticatorData, clientDataJSON, DER signature) with every field
// controllable; it reuses only the passkey key holder from
// webauthn_register_test.go (same package, test-only).
//
// None of these tests may use t.Parallel (process-wide registry globals).

const (
	// The active passkey's entry pins an RPID and Origin that differ from the
	// daemon's Config.RPID/Origin, so using the server settings instead of
	// the signer's (R8) is caught.
	loginRPID   = "example.test"
	loginOrigin = "https://login.example.test"
)

// waOpts shapes one synthetic navigator.credentials.get() answer.
type waOpts struct {
	challenge    []byte
	rpID, origin string
	typ          string // clientData type (default webauthn.get)
	flags        byte   // authenticator flags (default UP|UV)
	credID       []byte // credential.id/rawId (nil: the passkey's)
	rawID        []byte // rawId when different from id (nil: same as id)
	signWith     *ecdsa.PrivateKey
}

// waCredential is PublicKeyCredential.toJSON() for an assertion.
func (p *passkey) waCredential(t *testing.T, o waOpts) map[string]any {
	t.Helper()
	if o.typ == "" {
		o.typ = "webauthn.get"
	}
	if o.flags == 0 {
		o.flags = 0x05
	}
	h := sha256.Sum256([]byte(o.rpID))
	authData := append(append([]byte{}, h[:]...), o.flags, 0, 0, 0, 7)
	cdj, err := json.Marshal(map[string]any{"type": o.typ, "challenge": b64u(o.challenge), "origin": o.origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	cdh := sha256.Sum256(cdj)
	d := sha256.Sum256(append(append([]byte{}, authData...), cdh[:]...))
	key := p.key
	if o.signWith != nil {
		key = o.signWith
	}
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	id := o.credID
	if id == nil {
		id = p.credID
	}
	raw := o.rawID
	if raw == nil {
		raw = id
	}
	return map[string]any{
		"id": b64u(id), "rawId": b64u(raw), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON": b64u(cdj), "authenticatorData": b64u(authData),
			"signature": b64u(sig), "userHandle": b64u([]byte("u")),
		},
	}
}

func (p *passkey) signer(id, rpID, origin string) signing.Signer {
	return signing.Signer{ID: id, Type: signing.TypeWebAuthnES256, Label: "Phone " + id,
		PublicKey: b64u(p.point()), CredentialID: b64u(p.credID), RPID: rpID, Origin: origin,
		AddedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}
}

func writeRegistry(t *testing.T, path string, signers ...signing.Signer) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "signers": signers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// waFixture: a registry with alice (ssh-ed25519) and phone (webauthn-es256,
// pinned to loginRPID/loginOrigin), passkeys configured (regRPID/regOrigin),
// and ONE movable clock shared by the handler, the store and the
// authenticator.
type waFixture struct {
	e       *env
	srv     *httptest.Server
	clock   *testClock
	reg     string
	pending string
	alice   testSigner
	pk      *passkey
	phone   signing.Signer
}

func newWAFixture(t *testing.T) *waFixture {
	t.Helper()
	alice := newTestSigner(t, "alice")
	pk := newPasskey(t)
	phone := pk.signer("phone", loginRPID, loginOrigin)
	reg := useTestRegistry(t, []signing.Signer{alice.signer, phone})
	pending := filepath.Join(t.TempDir(), "state", "signers.pending.json")
	e := newEnv(t, noSteps{}, 1, withWebAuthn(pending))
	e.d.pauseWorkers()
	f := &waFixture{e: e, clock: &testClock{}, reg: reg, pending: pending, alice: alice, pk: pk, phone: phone}
	// Before the first request (same pattern as handler.grants).
	e.st.SetClock(f.clock.now)
	e.d.handler.now = f.clock.now
	e.d.handler.auth = &StoreAuthenticator{St: e.st, Now: f.clock.now}
	f.srv = e.httpServer(t)
	return f
}

func (f *waFixture) post(t *testing.T, path string, body any, h hdr) (int, http.Header, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		h = hdr{}
	}
	h["Content-Type"] = "application/json"
	return httpDo(t, f.srv, "POST", path, string(b), h)
}

type waBegin struct {
	ChallengeID string `json:"challenge_id"`
	Expires     string `json:"expires"`
	Options     struct {
		Challenge        string `json:"challenge"`
		RPID             string `json:"rpId"`
		AllowCredentials []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"allowCredentials"`
		UserVerification string `json:"userVerification"`
		Timeout          int64  `json:"timeout"`
	} `json:"options"`
	raw []byte
}

func (b waBegin) challenge(t *testing.T) []byte {
	t.Helper()
	c, err := base64.RawURLEncoding.Strict().DecodeString(b.Options.Challenge)
	if err != nil || len(c) != 32 {
		t.Fatalf("challenge %q: %v", b.Options.Challenge, err)
	}
	return c
}

func (f *waFixture) begin(t *testing.T, signer string) waBegin {
	t.Helper()
	code, _, b := f.post(t, "/api/v1/auth/webauthn/login/begin", map[string]string{"signer": signer}, nil)
	if code != http.StatusOK {
		t.Fatalf("begin %s: %d %s", signer, code, b)
	}
	var r waBegin
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	r.raw = b
	return r
}

func (f *waFixture) finish(t *testing.T, id string, cred any) (int, http.Header, []byte) {
	t.Helper()
	return f.post(t, "/api/v1/auth/webauthn/login/finish", map[string]any{"challenge_id": id, "credential": cred}, nil)
}

// good is a correct assertion for the phone passkey over b's challenge.
func (f *waFixture) good(t *testing.T, b waBegin) map[string]any {
	return f.pk.waCredential(t, waOpts{challenge: b.challenge(t), rpID: loginRPID, origin: loginOrigin})
}

type waSession struct {
	token, csrf, expires string
	setCookie            []string
}

// login runs begin+finish for the phone passkey.
func (f *waFixture) login(t *testing.T) waSession {
	t.Helper()
	b := f.begin(t, "phone")
	code, h, body := f.finish(t, b.ChallengeID, f.good(t, b))
	if code != http.StatusOK {
		t.Fatalf("finish %d %s", code, body)
	}
	var r struct{ CSRF, Expires, Signer string }
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	s := waSession{csrf: r.CSRF, expires: r.Expires, setCookie: h.Values("Set-Cookie")}
	if len(s.setCookie) != 1 {
		t.Fatalf("Set-Cookie %q", s.setCookie)
	}
	v, _, _ := strings.Cut(s.setCookie[0], ";")
	name, tok, _ := strings.Cut(v, "=")
	if name != SessionCookieName {
		t.Fatalf("cookie %q", s.setCookie[0])
	}
	s.token = tok
	return s
}

func (f *waFixture) get(t *testing.T, path string, h hdr) (int, http.Header, []byte) {
	t.Helper()
	return httpDo(t, f.srv, "GET", path, "", h)
}

func noCookie(t *testing.T, what string, h http.Header) {
	t.Helper()
	if v := h.Values("Set-Cookie"); len(v) != 0 {
		t.Fatalf("%s: Set-Cookie %q", what, v)
	}
}

// keySet lists the JSON object keys at every depth ("a", "a.b", "a.c[]").
func keySet(t *testing.T, b []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				out = append(out, prefix+k)
				walk(prefix+k+".", c)
			}
		case []any:
			out = append(out, fmt.Sprintf("%s#%d", prefix, len(x)))
			for _, c := range x {
				walk(prefix+"[].", c)
			}
		}
	}
	walk("", v)
	sort.Strings(out)
	return out
}

// ---- tests named in the brief ----

func TestLoginOKSetsCookieFlags(t *testing.T) {
	f := newWAFixture(t)
	b := f.begin(t, "phone")
	// Request options: the signer's pinned RPID (R8), its credential only,
	// UV required, the TTL as timeout, a 32-byte challenge.
	o := b.Options
	if o.RPID != loginRPID || len(o.AllowCredentials) != 1 || o.AllowCredentials[0].ID != b64u(f.pk.credID) ||
		o.AllowCredentials[0].Type != "public-key" || o.UserVerification != "required" ||
		o.Timeout != WebAuthnLoginChallengeTTL.Milliseconds() || len(b.ChallengeID) != 64 {
		t.Fatalf("options %s", b.raw)
	}
	b.challenge(t)
	if exp, err := time.Parse(time.RFC3339, b.Expires); err != nil || exp.Sub(time.Now()) > WebAuthnLoginChallengeTTL+time.Second {
		t.Fatalf("expires %q", b.Expires)
	}
	ch, err := f.e.st.CountChallenges(context.Background(), ChallengePurposeWebAuthnLogin)
	if err != nil || ch != 1 {
		t.Fatalf("webauthn-login rows %d %v", ch, err)
	}
	if n, _ := f.e.st.CountChallenges(context.Background(), ChallengePurposeLogin); n != 0 {
		t.Fatalf("login rows %d", n)
	}

	code, h, body := f.finish(t, b.ChallengeID, f.good(t, b))
	if code != http.StatusOK {
		t.Fatalf("finish %d %s", code, body)
	}
	if ks := keySet(t, body); !reflect.DeepEqual(ks, []string{"csrf", "expires", "signer"}) {
		t.Fatalf("body keys %v", ks)
	}
	var r struct{ CSRF, Expires, Signer string }
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	sc := h.Values("Set-Cookie")
	if len(sc) != 1 {
		t.Fatalf("Set-Cookie %q", sc)
	}
	v, _, _ := strings.Cut(sc[0], ";")
	_, tok, _ := strings.Cut(v, "=")
	// Every attribute, in Go's serialisation order; no Domain, no Expires.
	want := SessionCookieName + "=" + tok + "; Path=/; Max-Age=86400; HttpOnly; Secure; SameSite=Strict"
	if sc[0] != want {
		t.Fatalf("Set-Cookie\n got %q\nwant %q", sc[0], want)
	}
	if raw, err := base64.RawURLEncoding.Strict().DecodeString(tok); err != nil || len(raw) != 32 {
		t.Fatalf("session token %q", tok)
	}
	if raw, err := base64.RawURLEncoding.Strict().DecodeString(r.CSRF); err != nil || len(raw) != 32 {
		t.Fatalf("csrf %q", r.CSRF)
	}
	// The CSRF token is in the body only, never in the cookie.
	if r.CSRF == tok || strings.Contains(sc[0], r.CSRF) || r.Signer != "phone" {
		t.Fatalf("csrf %q cookie %q signer %q", r.CSRF, sc[0], r.Signer)
	}
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	exp, err := time.Parse(time.RFC3339, r.Expires)
	if err != nil || exp.Sub(f.clock.now()) > SessionIdleTTL+time.Second || exp.Sub(f.clock.now()) < SessionIdleTTL-time.Minute {
		t.Fatalf("expires %q", r.Expires)
	}
	// The store keeps hashes only; the row is a session of the signer.
	s, err := f.e.st.GetSession(context.Background(), tok)
	if err != nil || s.Principal != "phone" || s.Kind != KindSession || s.CSRFHash != store.HashSecret(r.CSRF) ||
		s.IDHash != store.HashSecret(tok) {
		t.Fatalf("session %+v %v", s, err)
	}
	// The cookie authenticates; the CSRF unlocks writes.
	if code, _, b := f.get(t, "/api/v1/tasks", hdr{"Cookie": cookie(tok)}); code != 200 {
		t.Fatalf("read %d %s", code, b)
	}
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/tasks", f.e.submitBody("p"), hdr{"Cookie": cookie(tok), CSRFHeader: r.CSRF}); code != http.StatusCreated {
		t.Fatalf("write %d %s", code, b)
	}
}

func TestLoginOnlyForActiveRegistrySigners(t *testing.T) {
	f := newWAFixture(t)
	regBefore := fileBytes(t, f.reg)

	// Unknown id: a fake credential is offered; the phone's assertion (or one
	// naming the fake id) fails at finish.
	b := f.begin(t, "ghost")
	code, h, body := f.finish(t, b.ChallengeID, f.good(t, b))
	wantCode(t, "unknown signer", code, body, http.StatusUnauthorized, "bad_assertion")
	noCookie(t, "unknown signer", h)
	fake, _ := base64.RawURLEncoding.DecodeString(b.Options.AllowCredentials[0].ID)
	b = f.begin(t, "ghost")
	code, _, body = f.finish(t, b.ChallengeID, f.pk.waCredential(t, waOpts{challenge: b.challenge(t), rpID: regRPID, origin: regOrigin, credID: fake}))
	wantCode(t, "unknown signer, fake id", code, body, http.StatusUnauthorized, "bad_assertion")

	// An ssh-ed25519 signer cannot log in with a passkey.
	b = f.begin(t, "alice")
	code, _, body = f.finish(t, b.ChallengeID, f.good(t, b))
	wantCode(t, "ssh signer", code, body, http.StatusUnauthorized, "bad_assertion")

	// A pending (enrolled, not activated) credential: begin offers the fake
	// id and its correct assertion is refused (Task 19 deferred item).
	tablet := newPasskey(t)
	pc := PendingCredential{ID: "tablet", Label: "Tablet", CredentialID: b64u(tablet.credID), PublicKey: b64u(tablet.point()),
		RPID: regRPID, Origin: regOrigin, Created: time.Now().UTC().Truncate(time.Second)}
	if err := AppendPending(f.pending, pc, MaxPendingCredentials, time.Now()); err != nil {
		t.Fatal(err)
	}
	b = f.begin(t, "tablet")
	if b.Options.AllowCredentials[0].ID == pc.CredentialID || b.Options.RPID != regRPID {
		t.Fatalf("pending credential offered: %s", b.raw)
	}
	pendingCred := tablet.waCredential(t, waOpts{challenge: b.challenge(t), rpID: regRPID, origin: regOrigin})
	code, h, body = f.finish(t, b.ChallengeID, pendingCred)
	wantCode(t, "pending credential", code, body, http.StatusUnauthorized, "bad_assertion")
	noCookie(t, "pending credential", h)
	if !bytes.Equal(fileBytes(t, f.reg), regBefore) {
		t.Fatal("registry changed")
	}

	// The public signer list shows active webauthn signers only.
	code, _, body = f.get(t, "/api/v1/auth/signers", nil)
	if code != 200 || string(body) != `{"signers":[{"id":"phone","credential_id":"`+f.phone.CredentialID+`","label":"Phone phone"}]}`+"\n" {
		t.Fatalf("signers %d %s", code, body)
	}

	// Control: once root activates the pending entry, the same flow logs in
	// (so the refusal above was the pending state).
	act := signing.Signer{ID: pc.ID, Type: signing.TypeWebAuthnES256, Label: pc.Label, PublicKey: pc.PublicKey,
		CredentialID: pc.CredentialID, RPID: "console.example.test", Origin: "https://console.example.test",
		AddedAt: time.Now().UTC().Truncate(time.Second)}
	writeRegistry(t, f.reg, f.alice.signer, f.phone, act)
	b = f.begin(t, "tablet")
	if b.Options.AllowCredentials[0].ID != pc.CredentialID {
		t.Fatalf("activated credential not offered: %s", b.raw)
	}
	code, _, body = f.finish(t, b.ChallengeID, tablet.waCredential(t, waOpts{challenge: b.challenge(t), rpID: regRPID, origin: regOrigin}))
	if code != http.StatusOK {
		t.Fatalf("activated login %d %s", code, body)
	}

	// A signer removed between begin and finish cannot log in.
	b = f.begin(t, "phone")
	writeRegistry(t, f.reg, f.alice.signer, act)
	code, _, body = f.finish(t, b.ChallengeID, f.good(t, b))
	wantCode(t, "removed signer", code, body, http.StatusUnauthorized, "bad_assertion")
}

func TestCSRFHeaderRequiredOnWrite(t *testing.T) {
	f := newWAFixture(t)
	s := f.login(t)
	ck := cookie(s.token)
	body := f.e.submitBody("p")
	for name, h := range map[string]hdr{
		"missing":        {"Cookie": ck},
		"wrong":          {"Cookie": ck, CSRFHeader: s.csrf + "x"},
		"empty":          {"Cookie": ck, CSRFHeader: ""},
		"cookie as csrf": {"Cookie": ck, CSRFHeader: s.token},
	} {
		code, _, b := httpDo(t, f.srv, "POST", "/api/v1/tasks", body, h)
		if code != http.StatusForbidden || v1Code(b) != "csrf_failed" || strings.Contains(string(b), s.csrf) {
			t.Fatalf("%s: %d %s", name, code, b)
		}
	}
	// Logout is a write too: without the CSRF it changes nothing.
	code, h, b := httpDo(t, f.srv, "POST", "/api/v1/auth/logout", "", hdr{"Cookie": ck})
	if code != http.StatusForbidden || v1Code(b) != "csrf_failed" {
		t.Fatalf("logout without csrf: %d %s", code, b)
	}
	noCookie(t, "refused logout", h)
	if n := f.e.taskCount(t); n != 0 {
		t.Fatalf("refused writes made %d tasks", n)
	}
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/tasks", body, hdr{"Cookie": ck, CSRFHeader: s.csrf}); code != http.StatusCreated {
		t.Fatalf("valid csrf: %d %s", code, b)
	}

	// GET /auth/session rotates the CSRF: the new one works, the old one is dead.
	code, h, b = f.get(t, "/api/v1/auth/session", hdr{"Cookie": ck})
	if code != 200 {
		t.Fatalf("session %d %s", code, b)
	}
	noCookie(t, "session", h)
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	var sv struct{ Principal, Kind, Expires, CSRF string }
	if err := json.Unmarshal(b, &sv); err != nil {
		t.Fatal(err)
	}
	if ks := keySet(t, b); !reflect.DeepEqual(ks, []string{"csrf", "expires", "kind", "principal"}) ||
		sv.Principal != "phone" || sv.Kind != KindSession || sv.CSRF == "" || sv.CSRF == s.csrf {
		t.Fatalf("session view %s", b)
	}
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/tasks", body, hdr{"Cookie": ck, CSRFHeader: s.csrf}); code != http.StatusForbidden {
		t.Fatalf("old csrf after rotation: %d %s", code, b)
	}
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/tasks", body, hdr{"Cookie": ck, CSRFHeader: sv.CSRF}); code != http.StatusCreated {
		t.Fatalf("rotated csrf: %d %s", code, b)
	}

	// Token principals: /auth/session answers without a CSRF; no CSRF needed.
	tok := f.e.newToken(t, "alice")
	code, _, b = f.get(t, "/api/v1/auth/session", hdr{"Authorization": "Bearer " + tok})
	if ks := keySet(t, b); code != 200 || !reflect.DeepEqual(ks, []string{"expires", "kind", "principal"}) {
		t.Fatalf("token session %d %s", code, b)
	}
	// Without credentials: 401.
	if code, _, b := f.get(t, "/api/v1/auth/session", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous session %d %s", code, b)
	}
}

func TestSessionIdleAndAbsoluteExpiry(t *testing.T) {
	f := newWAFixture(t)
	ctx := context.Background()
	alive := func(tok string) bool {
		code, _, _ := f.get(t, "/api/v1/tasks", hdr{"Cookie": cookie(tok)})
		return code == 200
	}
	// Idle: 8 hours without a request ends the session.
	s := f.login(t)
	f.clock.advance(SessionIdleTTL - time.Minute)
	if !alive(s.token) {
		t.Fatal("session dead before the idle limit")
	}
	f.clock.advance(SessionIdleTTL - time.Minute) // slid by the request above
	if !alive(s.token) {
		t.Fatal("activity did not slide the idle expiry")
	}
	f.clock.advance(SessionIdleTTL + time.Second)
	if alive(s.token) {
		t.Fatal("idle session accepted")
	}

	// Absolute: activity keeps a session at most 24 hours after login.
	f.clock.set(0)
	s = f.login(t)
	created, err := f.e.st.GetSession(ctx, s.token)
	if err != nil {
		t.Fatal(err)
	}
	limit := created.CreatedAt.Add(SessionMaxLifetime)
	for _, at := range []time.Duration{7 * time.Hour, 14 * time.Hour, 21 * time.Hour, 23*time.Hour + 59*time.Minute} {
		f.clock.set(at)
		if !alive(s.token) {
			t.Fatalf("session dead at +%v", at)
		}
		got, err := f.e.st.GetSession(ctx, s.token)
		if err != nil {
			t.Fatal(err)
		}
		// One clock for store and handler: without the cap, the touch at
		// +21h would set the expiry to +29h.
		if got.ExpiresAt.After(limit) {
			t.Fatalf("at +%v expiry %v is past the absolute limit %v", at, got.ExpiresAt, limit)
		}
	}
	f.clock.set(SessionMaxLifetime + time.Second)
	if alive(s.token) {
		t.Fatal("session past the absolute lifetime accepted")
	}

	// A row whose idle expiry reaches past the cap (as any older or
	// hand-made row could) is still refused at the cap, and deleted.
	f.clock.set(0)
	if _, err := f.e.st.CreateSession(ctx, "long-row", "c", "phone", KindSession, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	f.clock.set(SessionMaxLifetime + time.Minute)
	if alive("long-row") {
		t.Fatal("over-lifetime row accepted")
	}
	f.clock.set(0) // the row would be live again for the store
	if _, err := f.e.st.GetSession(ctx, "long-row"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("over-lifetime row not deleted: %v", err)
	}
}

func TestLogoutInvalidates(t *testing.T) {
	f := newWAFixture(t)
	s := f.login(t)
	ck := cookie(s.token)
	code, h, b := httpDo(t, f.srv, "POST", "/api/v1/auth/logout", "", hdr{"Cookie": ck, CSRFHeader: s.csrf})
	if code != 200 || string(b) != `{"ok":true}`+"\n" {
		t.Fatalf("logout %d %s", code, b)
	}
	if sc := h.Values("Set-Cookie"); len(sc) != 1 ||
		sc[0] != SessionCookieName+"=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict" {
		t.Fatalf("clearing cookie %q", sc)
	}
	if _, err := f.e.st.GetSession(context.Background(), s.token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session still stored: %v", err)
	}
	for _, p := range []string{"/api/v1/tasks", "/api/v1/auth/session"} {
		if code, _, b := f.get(t, p, hdr{"Cookie": ck}); code != http.StatusUnauthorized {
			t.Fatalf("GET %s after logout: %d %s", p, code, b)
		}
	}
	// A second logout with the dead cookie is refused (no session); nothing
	// is left to delete.
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/auth/logout", "", hdr{"Cookie": ck, CSRFHeader: s.csrf}); code != http.StatusUnauthorized {
		t.Fatalf("second logout %d %s", code, b)
	}
	// Another session of the same signer is untouched.
	other := f.login(t)
	tok := f.e.newToken(t, "alice")
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/auth/logout", "", hdr{"Authorization": "Bearer " + tok}); code != 200 {
		t.Fatalf("token logout %d %s", code, b)
	}
	if code, _, _ := f.get(t, "/api/v1/tasks", hdr{"Authorization": "Bearer " + tok}); code != http.StatusUnauthorized {
		t.Fatal("token alive after logout")
	}
	if code, _, _ := f.get(t, "/api/v1/tasks", hdr{"Cookie": cookie(other.token)}); code != 200 {
		t.Fatal("unrelated session ended")
	}
}

func TestApprovalAssertionNotAcceptedAsLogin(t *testing.T) {
	f := newWAFixture(t)
	msg := signing.Message{Kind: signing.KindProtectedModified, Repo: "0123456789ab", Task: 1,
		Base: strings.Repeat("a", 40), Hash: strings.Repeat("b", 16), Issued: time.Now().UTC().Truncate(time.Second)}
	if err := msg.Validate(); err != nil {
		t.Fatal(err)
	}
	digest := msg.Digest()
	// An approval assertion (challenge = message digest, correct key, RPID and
	// origin) is no login.
	b := f.begin(t, "phone")
	code, h, body := f.finish(t, b.ChallengeID, f.pk.waCredential(t, waOpts{challenge: digest[:], rpID: loginRPID, origin: loginOrigin}))
	wantCode(t, "approval assertion at login", code, body, http.StatusUnauthorized, "bad_assertion")
	noCookie(t, "approval assertion", h)
	// The challenge was consumed by the failure.
	code, _, body = f.finish(t, b.ChallengeID, f.good(t, b))
	wantCode(t, "challenge after failure", code, body, http.StatusUnauthorized, "bad_challenge")

	// And a login assertion is no approval signature.
	b = f.begin(t, "phone")
	lc := f.good(t, b)
	resp := lc["response"].(map[string]any)
	sig := signing.Signature{Signer: "phone", Alg: signing.TypeWebAuthnES256, Sig: resp["signature"].(string),
		WebAuthn: &signing.WebAuthnData{AuthenticatorData: resp["authenticatorData"].(string), ClientDataJSON: resp["clientDataJSON"].(string)}}
	reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.Verify(reg, msg, sig, signing.VerifyOptions{}); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("login assertion verified as approval: %v", err)
	}
	// Control: the same assertion is a valid login.
	if code, _, body := f.finish(t, b.ChallengeID, lc); code != http.StatusOK {
		t.Fatalf("control login %d %s", code, body)
	}
	// Control: an assertion over the digest is a valid approval signature.
	ac := f.pk.waCredential(t, waOpts{challenge: digest[:], rpID: loginRPID, origin: loginOrigin})
	ar := ac["response"].(map[string]any)
	asig := signing.Signature{Signer: "phone", Alg: signing.TypeWebAuthnES256, Sig: ar["signature"].(string),
		WebAuthn: &signing.WebAuthnData{AuthenticatorData: ar["authenticatorData"].(string), ClientDataJSON: ar["clientDataJSON"].(string)}}
	if err := signing.Verify(reg, msg, asig, signing.VerifyOptions{}); err != nil {
		t.Fatalf("control approval: %v", err)
	}
}

func TestSessionNeverUnlocksApprovalWithoutSignature(t *testing.T) {
	gf := newGrantFixture(t)
	pk := newPasskey(t)
	phone := pk.signer("phone", loginRPID, loginOrigin)
	writeRegistry(t, gf.reg, gf.alice.signer, phone)
	h := gf.e.d.handler
	h.rpID, h.origin = regRPID, regOrigin // before the first request
	f := &waFixture{e: gf.e, srv: gf.e.httpServer(t), clock: &testClock{}, reg: gf.reg, pk: pk, phone: phone, alice: gf.alice}
	s := f.login(t)
	auth := hdr{"Cookie": cookie(s.token), CSRFHeader: s.csrf}

	tid, aid, _ := gf.protectedTask(t, "line1\nSESSION\n")
	path := fmt.Sprintf("/api/v1/approvals/%d/grant", aid)
	issued := nowSec()
	m := gf.message(t, aid, issued)
	d := m.Digest()
	lb := f.begin(t, "phone")
	login := f.good(t, lb)
	lr := login["response"].(map[string]any)
	wa := func(sig, ad, cd string) map[string]any {
		return map[string]any{"signer": "phone", "alg": signing.TypeWebAuthnES256, "sig": sig, "issued": issued.Format(time.RFC3339),
			"webauthn": map[string]string{"authenticator_data": ad, "client_data_json": cd}}
	}
	other := newPasskey(t)
	oc := other.waCredential(t, waOpts{challenge: d[:], rpID: loginRPID, origin: loginOrigin})["response"].(map[string]any)
	for name, body := range map[string]any{
		"no signature":       map[string]any{"signer": "phone", "alg": signing.TypeWebAuthnES256, "issued": issued.Format(time.RFC3339)},
		"empty body":         map[string]any{},
		"login assertion":    wa(lr["signature"].(string), lr["authenticatorData"].(string), lr["clientDataJSON"].(string)),
		"other key":          wa(oc["signature"].(string), oc["authenticatorData"].(string), oc["clientDataJSON"].(string)),
		"session as signer":  map[string]any{"signer": "phone", "alg": signing.TypeWebAuthnES256, "sig": s.token, "issued": issued.Format(time.RFC3339)},
		"ssh sig wrong text": gf.body(gf.alice.signText(t, "not the message\n"), issued),
	} {
		bb, _ := json.Marshal(body)
		code, _, rb := httpDo(t, f.srv, "POST", path, string(bb), auth)
		if code == 200 || code < 400 {
			t.Fatalf("%s: session granted: %d %s", name, code, rb)
		}
		gf.unchanged(t, tid, aid)
	}
	// Control: the same session with a real approval signature grants.
	ac := pk.waCredential(t, waOpts{challenge: d[:], rpID: loginRPID, origin: loginOrigin})["response"].(map[string]any)
	bb, _ := json.Marshal(wa(ac["signature"].(string), ac["authenticatorData"].(string), ac["clientDataJSON"].(string)))
	if code, _, rb := httpDo(t, f.srv, "POST", path, string(bb), auth); code != 200 {
		t.Fatalf("signed grant via session: %d %s", code, rb)
	}
}

// ---- further coverage ----

// CF14: challenges of other purposes are refused at login/finish, and a
// webauthn-login challenge is refused everywhere else. Each refusal consumes
// the row.
func TestWebAuthnLoginChallengePurposeIsolation(t *testing.T) {
	f := newWAFixture(t)
	ctx := context.Background()
	consumed := func(what, id string) {
		t.Helper()
		if _, err := f.e.st.TakeChallenge(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: row not consumed: %v", what, err)
		}
	}
	// An SSH login challenge at webauthn login/finish.
	lf := &loginFixture{e: f.e, srv: f.srv}
	lc := lf.challenge(t, "phone")
	code, _, b := f.finish(t, lc.ChallengeID, f.pk.waCredential(t, waOpts{challenge: []byte(lc.Message), rpID: loginRPID, origin: loginOrigin}))
	wantCode(t, "login challenge at webauthn finish", code, b, http.StatusUnauthorized, "bad_challenge")
	consumed("login", lc.ChallengeID)

	// A registration challenge at webauthn login/finish.
	rf := &regFixture{e: f.e, srv: f.srv, pending: f.pending}
	r := rf.fresh(t, `{}`)
	rc, _ := base64.RawURLEncoding.DecodeString(r.Options.Challenge)
	code, _, b = f.finish(t, r.ChallengeID, f.pk.waCredential(t, waOpts{challenge: rc, rpID: loginRPID, origin: loginOrigin}))
	wantCode(t, "register challenge at webauthn finish", code, b, http.StatusUnauthorized, "bad_challenge")
	consumed("register", r.ChallengeID)

	// A webauthn-login challenge at /auth/token and at register/finish.
	wb := f.begin(t, "phone")
	code, b = lf.token(t, wb.ChallengeID, "x")
	wantCode(t, "webauthn-login at token", code, b, http.StatusUnauthorized, "bad_challenge")
	code, _, b = f.finish(t, wb.ChallengeID, f.good(t, wb))
	wantCode(t, "consumed by token", code, b, http.StatusUnauthorized, "bad_challenge")
	wb = f.begin(t, "phone")
	code, b = rf.finish(t, wb.ChallengeID, newPasskey(t).credential(t, goodReg(wb.Options.Challenge)))
	wantCode(t, "webauthn-login at register finish", code, b, http.StatusUnauthorized, "bad_challenge")
	consumed("webauthn-login", wb.ChallengeID)

	// Rows of another purpose carrying webauthn-login-shaped data with a
	// correct assertion: the purpose decides, not the data.
	chal := bytes.Repeat([]byte{7}, 32)
	data, _ := json.Marshal(map[string]string{"signer": "phone", "challenge": b64u(chal)})
	for i, purpose := range []string{ChallengePurposeLogin, ChallengePurposeRegister, ChallengePurposeEnroll, ""} {
		id := strings.Repeat(string(rune('a'+i)), 64)
		if err := f.e.st.SaveChallenge(ctx, store.Challenge{ID: id, Purpose: purpose, Data: string(data),
			ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		code, _, b = f.finish(t, id, f.pk.waCredential(t, waOpts{challenge: chal, rpID: loginRPID, origin: loginOrigin}))
		wantCode(t, "forged "+purpose, code, b, http.StatusUnauthorized, "bad_challenge")
	}
	// Control: the same data under the right purpose logs in.
	ok := strings.Repeat("z", 64)
	if err := f.e.st.SaveChallenge(ctx, store.Challenge{ID: ok, Purpose: ChallengePurposeWebAuthnLogin, Data: string(data),
		ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if code, _, b := f.finish(t, ok, f.pk.waCredential(t, waOpts{challenge: chal, rpID: loginRPID, origin: loginOrigin})); code != 200 {
		t.Fatalf("control %d %s", code, b)
	}
}

// begin gives unknown, ssh-only, pending and real signers the same status and
// the same JSON shape.
func TestWebAuthnLoginBeginNoOracle(t *testing.T) {
	f := newWAFixture(t)
	real := f.begin(t, "phone")
	want := keySet(t, real.raw)
	for _, id := range []string{"ghost", "alice", "zz", "pk-0000"} {
		b := f.begin(t, id)
		if got := keySet(t, b.raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: keys %v, real %v", id, got, want)
		}
		fake, err := base64.RawURLEncoding.Strict().DecodeString(b.Options.AllowCredentials[0].ID)
		if err != nil || len(fake) != fakeCredIDLen || b.Options.RPID != regRPID || b.Options.UserVerification != "required" {
			t.Fatalf("%s: %s", id, b.raw)
		}
		// Deterministic per id within the process, so repeating begin does
		// not tell a fake from a real one.
		if again := f.begin(t, id); again.Options.AllowCredentials[0].ID != b.Options.AllowCredentials[0].ID {
			t.Fatalf("%s: fake id changes between calls", id)
		}
	}
	if f.begin(t, "ghost").Options.AllowCredentials[0].ID == f.begin(t, "ghost2").Options.AllowCredentials[0].ID {
		t.Fatal("fake ids collide")
	}
	// A bad id format is a plain 400 (not an oracle: no registry involved).
	code, _, b := f.post(t, "/api/v1/auth/webauthn/login/begin", map[string]string{"signer": "Bad\nId"}, nil)
	wantCode(t, "bad id", code, b, http.StatusBadRequest, "bad_request")
	code, _, b = f.post(t, "/api/v1/auth/webauthn/login/begin", map[string]any{"signer": "phone", "x": 1}, nil)
	wantCode(t, "unknown field", code, b, http.StatusBadRequest, "bad_request")
	// An untrusted registry does not change begin either.
	if err := os.Chmod(f.reg, 0o666); err != nil {
		t.Fatal(err)
	}
	if got := keySet(t, f.begin(t, "phone").raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("untrusted registry: %v", got)
	}
}

// Finish refuses every wrong part of an assertion with bad_assertion, and each
// failure consumes the challenge (single use).
func TestWebAuthnLoginFinishRejects(t *testing.T) {
	f := newWAFixture(t)
	other := newPasskey(t)
	wrongID := append([]byte{}, f.pk.credID...)
	wrongID[0] ^= 1
	type variant struct {
		name string
		cred func(c []byte) any
		want string
	}
	o := func(c []byte) waOpts { return waOpts{challenge: c, rpID: loginRPID, origin: loginOrigin} }
	with := func(m func(*waOpts)) func(c []byte) any {
		return func(c []byte) any { x := o(c); m(&x); return f.pk.waCredential(t, x) }
	}
	edit := func(m func(map[string]any)) func(c []byte) any {
		return func(c []byte) any { x := f.pk.waCredential(t, o(c)); m(x); return x }
	}
	variants := []variant{
		{"server origin (not the signer's)", with(func(x *waOpts) { x.origin = regOrigin }), "bad_assertion"},
		{"other origin", with(func(x *waOpts) { x.origin = "https://evil.example.test" }), "bad_assertion"},
		{"server rpid (not the signer's)", with(func(x *waOpts) { x.rpID = regRPID }), "bad_assertion"},
		{"other rpid", with(func(x *waOpts) { x.rpID = "evil.test" }), "bad_assertion"},
		{"uv missing", with(func(x *waOpts) { x.flags = 0x01 }), "bad_assertion"},
		{"up missing", with(func(x *waOpts) { x.flags = 0x04 }), "bad_assertion"},
		{"webauthn.create", with(func(x *waOpts) { x.typ = "webauthn.create" }), "bad_assertion"},
		{"other challenge", with(func(x *waOpts) { x.challenge = bytes.Repeat([]byte{1}, 32) }), "bad_assertion"},
		{"other key", with(func(x *waOpts) { x.signWith = other.key }), "bad_assertion"},
		{"wrong credential id", with(func(x *waOpts) { x.credID = wrongID }), "bad_assertion"},
		{"rawId differs", with(func(x *waOpts) { x.rawID = wrongID }), "bad_assertion"},
		{"credential id padded", edit(func(m map[string]any) { m["id"] = m["id"].(string) + "=" }), "bad_assertion"},
		{"credential id with newline", edit(func(m map[string]any) {
			s := m["id"].(string)
			m["id"] = s[:4] + "\n" + s[4:]
		}), "bad_assertion"},
		{"no id", edit(func(m map[string]any) { delete(m, "id") }), "bad_assertion"},
		{"type", edit(func(m map[string]any) { m["type"] = "password" }), "bad_assertion"},
		{"no signature", edit(func(m map[string]any) { delete(m["response"].(map[string]any), "signature") }), "bad_assertion"},
		{"garbage signature", edit(func(m map[string]any) { m["response"].(map[string]any)["signature"] = b64u([]byte("xx")) }), "bad_assertion"},
		{"string credential", func([]byte) any { return "x" }, "bad_assertion"},
		{"null credential", func([]byte) any { return nil }, "bad_assertion"},
	}
	for _, v := range variants {
		b := f.begin(t, "phone")
		code, h, body := f.finish(t, b.ChallengeID, v.cred(b.challenge(t)))
		wantCode(t, v.name, code, body, http.StatusUnauthorized, v.want)
		noCookie(t, v.name, h)
		if strings.Contains(string(body), "evil") || strings.Contains(string(body), "password") {
			t.Fatalf("%s: echoed input %s", v.name, body)
		}
		// Single use, also on failure.
		code, _, body = f.finish(t, b.ChallengeID, f.good(t, b))
		wantCode(t, v.name+": reuse", code, body, http.StatusUnauthorized, "bad_challenge")
	}
	// Replay of a successful assertion: the challenge is gone.
	b := f.begin(t, "phone")
	cred := f.good(t, b)
	if code, _, body := f.finish(t, b.ChallengeID, cred); code != 200 {
		t.Fatalf("login %d %s", code, body)
	}
	code, _, body := f.finish(t, b.ChallengeID, cred)
	wantCode(t, "replay", code, body, http.StatusUnauthorized, "bad_challenge")
	// Unknown and expired challenges.
	code, _, body = f.finish(t, strings.Repeat("0", 64), cred)
	wantCode(t, "unknown challenge", code, body, http.StatusUnauthorized, "bad_challenge")
	b = f.begin(t, "phone")
	f.clock.advance(WebAuthnLoginChallengeTTL + time.Second)
	code, _, body = f.finish(t, b.ChallengeID, f.good(t, b))
	wantCode(t, "expired challenge", code, body, http.StatusUnauthorized, "bad_challenge")
	f.clock.set(0)
	// Body errors.
	code, _, body = f.post(t, "/api/v1/auth/webauthn/login/finish", map[string]any{"challenge_id": "x", "credential": cred, "extra": 1}, nil)
	wantCode(t, "unknown field", code, body, http.StatusBadRequest, "bad_request")
	// An untrusted registry fails closed at finish.
	b = f.begin(t, "phone")
	if err := os.Chmod(f.reg, 0o666); err != nil {
		t.Fatal(err)
	}
	code, _, body = f.finish(t, b.ChallengeID, f.good(t, b))
	wantCode(t, "untrusted registry", code, body, http.StatusServiceUnavailable, "signers_unavailable")
	if strings.Contains(string(body), filepath.Dir(f.reg)) || strings.Contains(string(body), "signers.json") {
		t.Fatalf("path leaked: %s", body)
	}
}

func TestWebAuthnLoginCapAndUnconfigured(t *testing.T) {
	f := newWAFixture(t)
	f.e.d.handler.webauthnLoginCap = 2
	f.begin(t, "phone")
	f.begin(t, "ghost")
	code, _, b := f.post(t, "/api/v1/auth/webauthn/login/begin", map[string]string{"signer": "phone"}, nil)
	wantCode(t, "cap", code, b, http.StatusServiceUnavailable, "busy")
	// Expired rows do not count.
	f.clock.advance(WebAuthnLoginChallengeTTL + time.Second)
	f.begin(t, "phone")
	if DefaultWebAuthnLoginChallengeCap != 1000 {
		t.Fatalf("default cap %d", DefaultWebAuthnLoginChallengeCap)
	}

	// Passkeys off: both login routes answer webauthn_unconfigured; the
	// signer list still works.
	g := newLoginFixture(t)
	code, b = g.post(t, "/api/v1/auth/webauthn/login/begin", map[string]string{"signer": "alice"})
	wantCode(t, "begin off", code, b, http.StatusServiceUnavailable, "webauthn_unconfigured")
	code, b = g.post(t, "/api/v1/auth/webauthn/login/finish", map[string]any{"challenge_id": "x", "credential": map[string]any{}})
	wantCode(t, "finish off", code, b, http.StatusServiceUnavailable, "webauthn_unconfigured")
	code, _, b = httpDo(t, g.srv, "GET", "/api/v1/auth/signers", "", nil)
	if code != 200 || string(b) != `{"signers":[]}`+"\n" {
		t.Fatalf("signers without webauthn signers: %d %s", code, b)
	}
}

func TestAuthSignersList(t *testing.T) {
	f := newWAFixture(t)
	code, _, b := f.get(t, "/api/v1/auth/signers", nil)
	if code != 200 || keySet(t, b)[0] != "signers" {
		t.Fatalf("%d %s", code, b)
	}
	var v struct {
		Signers []map[string]string `json:"signers"`
	}
	if err := json.Unmarshal(b, &v); err != nil || len(v.Signers) != 1 {
		t.Fatalf("%s %v", b, err)
	}
	if ks := keySet(t, b); !reflect.DeepEqual(ks, []string{"signers", "signers.#1", "signers.[].credential_id", "signers.[].id", "signers.[].label"}) {
		t.Fatalf("keys %v", ks)
	}
	if strings.Contains(string(b), f.phone.PublicKey) || strings.Contains(string(b), "alice") {
		t.Fatalf("leaks %s", b)
	}
	// Absent registry: empty list. Untrusted: 503 without the path.
	if err := os.Remove(f.reg); err != nil {
		t.Fatal(err)
	}
	if code, _, b := f.get(t, "/api/v1/auth/signers", nil); code != 200 || string(b) != `{"signers":[]}`+"\n" {
		t.Fatalf("absent: %d %s", code, b)
	}
	writeRegistry(t, f.reg, f.alice.signer, f.phone)
	if err := os.Chmod(f.reg, 0o666); err != nil {
		t.Fatal(err)
	}
	code, _, b = f.get(t, "/api/v1/auth/signers", nil)
	wantCode(t, "untrusted", code, b, http.StatusServiceUnavailable, "signers_unavailable")
	if strings.Contains(string(b), filepath.Dir(f.reg)) {
		t.Fatalf("path leaked: %s", b)
	}
	// Only GET is open.
	if code, _, b := httpDo(t, f.srv, "POST", "/api/v1/auth/signers", "{}", nil); code != http.StatusUnauthorized {
		t.Fatalf("POST signers %d %s", code, b)
	}
}
