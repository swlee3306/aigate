package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/store"
)

// None of these tests may use t.Parallel: they change the process-wide signers
// registry path (useTestRegistry) and swap handler fields before requests.

// signNS signs text with the signer's key in namespace ns (ssh-keygen -Y sign).
func (s testSigner) signNS(t *testing.T, ns, text string) string {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-f", s.key, "-n", ns)
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v: %s", err, stderr.String())
	}
	return stdout.String()
}

// testClock is a movable clock safe to read from server goroutines.
type testClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *testClock) set(d time.Duration) {
	c.mu.Lock()
	c.off = d
	c.mu.Unlock()
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// loginFixture: a registry with alice and bob (ssh-ed25519), a daemon served
// over TCP the way the HTTP listener serves it, and a movable handler clock.
type loginFixture struct {
	e          *env
	srv        *httptest.Server
	alice, bob testSigner
	reg        string
	clock      *testClock
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	alice, bob := newTestSigner(t, "alice"), newTestSigner(t, "bob")
	reg := useTestRegistry(t, []signing.Signer{alice.signer, bob.signer})
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	f := &loginFixture{e: e, alice: alice, bob: bob, reg: reg, clock: &testClock{}}
	// Set before the first request (same pattern as handler.grants).
	e.d.handler.now = f.clock.now
	f.srv = e.httpServer(t)
	return f
}

type challengeResp struct {
	ChallengeID string `json:"challenge_id"`
	Message     string `json:"message"`
	Expires     string `json:"expires"`
}

type tokenResp struct {
	Token   string `json:"token"`
	Expires string `json:"expires"`
}

func (f *loginFixture) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, _, rb := httpDo(t, f.srv, "POST", path, string(b), hdr{"Content-Type": "application/json"})
	return code, rb
}

func (f *loginFixture) challenge(t *testing.T, signer string) challengeResp {
	t.Helper()
	code, b := f.post(t, "/api/v1/auth/challenge", map[string]string{"signer": signer})
	if code != http.StatusOK {
		t.Fatalf("challenge %d %s", code, b)
	}
	var c challengeResp
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *loginFixture) token(t *testing.T, id, sig string) (int, []byte) {
	t.Helper()
	return f.post(t, "/api/v1/auth/token", map[string]string{"challenge_id": id, "sig": sig})
}

// login runs the whole exchange for s and returns the bearer token.
func (f *loginFixture) login(t *testing.T, s testSigner) string {
	t.Helper()
	c := f.challenge(t, s.id)
	code, b := f.token(t, c.ChallengeID, s.signNS(t, signing.NamespaceLogin, c.Message))
	if code != http.StatusOK {
		t.Fatalf("token %d %s", code, b)
	}
	var tr tokenResp
	if err := json.Unmarshal(b, &tr); err != nil || tr.Token == "" {
		t.Fatalf("token body %s (%v)", b, err)
	}
	return tr.Token
}

func wantCode(t *testing.T, what string, code int, b []byte, status int, v1 string) {
	t.Helper()
	if code != status || v1Code(b) != v1 {
		t.Fatalf("%s: got %d %s, want %d %s", what, code, b, status, v1)
	}
}

var loginMsgRE = regexp.MustCompile(`^aigate-login-v1\nsigner: ([a-z0-9._-]+)\nnonce: ([0-9a-f]{64})\nissued: (\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ)\n$`)

func TestLoginHappyPath(t *testing.T) {
	f := newLoginFixture(t)
	before := time.Now().UTC().Truncate(time.Second)
	c := f.challenge(t, "alice")
	m := loginMsgRE.FindStringSubmatch(c.Message)
	if m == nil || m[1] != "alice" {
		t.Fatalf("message %q", c.Message)
	}
	issued, err := time.Parse(time.RFC3339, m[3])
	if err != nil || issued.Before(before) || issued.After(time.Now().Add(time.Second)) {
		t.Fatalf("issued %q %v", m[3], err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(c.ChallengeID) || c.ChallengeID == m[2] {
		t.Fatalf("challenge id %q (nonce %q)", c.ChallengeID, m[2])
	}
	exp, err := time.Parse(time.RFC3339, c.Expires)
	if err != nil || exp.Sub(issued) != LoginChallengeTTL {
		t.Fatalf("expires %q vs issued %q (%v)", c.Expires, m[3], err)
	}
	// Two challenges never share a nonce or an id.
	if c2 := f.challenge(t, "alice"); c2.ChallengeID == c.ChallengeID || c2.Message == c.Message {
		t.Fatal("challenge repeated")
	}

	code, b := f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	if code != http.StatusOK {
		t.Fatalf("token %d %s", code, b)
	}
	var tr tokenResp
	if err := json.Unmarshal(b, &tr); err != nil || len(tr.Token) < 43 {
		t.Fatalf("token %s (%v)", b, err)
	}
	texp, err := time.Parse(time.RFC3339, tr.Expires)
	if err != nil || texp.Sub(time.Now()) > TokenMaxLifetime || texp.Sub(time.Now()) < TokenMaxLifetime-time.Minute {
		t.Fatalf("token expires %q (%v)", tr.Expires, err)
	}
	// The store keeps a token principal for alice, hashed.
	s, err := f.e.st.GetSession(context.Background(), tr.Token)
	if err != nil || s.Kind != KindToken || s.Principal != "alice" || s.IDHash == tr.Token {
		t.Fatalf("session %+v %v", s, err)
	}
	// The token authenticates as a bearer token.
	if code, _, b := httpDo(t, f.srv, "GET", "/api/v1/tasks", "", hdr{"Authorization": "Bearer " + tr.Token}); code != 200 {
		t.Fatalf("tasks with token: %d %s", code, b)
	}
	// ... and not as a session cookie.
	if code, _, _ := httpDo(t, f.srv, "GET", "/api/v1/tasks", "", hdr{"Cookie": cookie(tr.Token)}); code != http.StatusUnauthorized {
		t.Fatalf("token as cookie: %d", code)
	}
}

func TestChallengeSingleUse(t *testing.T) {
	f := newLoginFixture(t)
	// Reuse after success.
	c := f.challenge(t, "alice")
	sig := f.alice.signNS(t, signing.NamespaceLogin, c.Message)
	if code, b := f.token(t, c.ChallengeID, sig); code != 200 {
		t.Fatalf("first %d %s", code, b)
	}
	code, b := f.token(t, c.ChallengeID, sig)
	wantCode(t, "reuse after success", code, b, http.StatusUnauthorized, "bad_challenge")

	// A failed attempt also consumes the challenge.
	c = f.challenge(t, "alice")
	code, b = f.token(t, c.ChallengeID, f.bob.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "wrong key", code, b, http.StatusUnauthorized, "bad_signature")
	code, b = f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "retry after failure", code, b, http.StatusUnauthorized, "bad_challenge")

	// Unknown and malformed ids.
	for _, id := range []string{strings.Repeat("0", 64), "", "x", strings.Repeat("A", 64)} {
		code, b = f.token(t, id, sig)
		wantCode(t, "id "+id, code, b, http.StatusUnauthorized, "bad_challenge")
	}
	// A challenge of another purpose (e.g. a WebAuthn one) is not a login
	// challenge, even with a well-formed login message in its data.
	data, _ := json.Marshal(loginChallengeData{Signer: "alice", Message: c.Message})
	foreign := strings.Repeat("ab", 32)
	if err := f.e.st.SaveChallenge(context.Background(), store.Challenge{ID: foreign, Purpose: "webauthn-login",
		Data: string(data), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	code, b = f.token(t, foreign, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "foreign purpose", code, b, http.StatusUnauthorized, "bad_challenge")
}

func TestWrongSigner(t *testing.T) {
	f := newLoginFixture(t)
	// bob's valid signature over alice's challenge.
	c := f.challenge(t, "alice")
	code, b := f.token(t, c.ChallengeID, f.bob.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "bob for alice", code, b, http.StatusUnauthorized, "bad_signature")

	// An unknown signer gets a challenge of the same shape (no registry
	// oracle); the failure shows at the token step with the same code as a
	// wrong key.
	u := f.challenge(t, "mallory")
	if m := loginMsgRE.FindStringSubmatch(u.Message); m == nil || m[1] != "mallory" || len(u.ChallengeID) != 64 {
		t.Fatalf("unknown signer challenge %+v", u)
	}
	code, b = f.token(t, u.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, u.Message))
	wantCode(t, "unknown signer", code, b, http.StatusUnauthorized, "bad_signature")
	if strings.Contains(string(b), "mallory") || strings.Contains(string(b), "alice") {
		t.Fatalf("error echoes ids: %s", b)
	}

	// Malformed signer ids are refused before anything else and not echoed;
	// a newline would inject a line into the signed message.
	for _, id := range []string{"", "Alice", "a\nnonce: 00", "<img onerror=x>", strings.Repeat("a", 64)} {
		code, b := f.post(t, "/api/v1/auth/challenge", map[string]string{"signer": id})
		wantCode(t, fmt.Sprintf("signer %q", id), code, b, http.StatusBadRequest, "bad_request")
		if id != "" && strings.Contains(string(b), id) {
			t.Fatalf("echoed %q: %s", id, b)
		}
	}
	// Unknown fields and garbage bodies.
	for _, body := range []string{`{"signer":"alice","x":1}`, `not json`, ``, `{"signer":"alice"} {}`} {
		code, _, b := httpDo(t, f.srv, "POST", "/api/v1/auth/challenge", body, hdr{"Content-Type": "application/json"})
		wantCode(t, "body "+body, code, b, http.StatusBadRequest, "bad_request")
	}
	// Garbage signatures never echo.
	c = f.challenge(t, "alice")
	code, b = f.token(t, c.ChallengeID, "-----BEGIN SSH SIGNATURE-----\n<script>x</script>\n")
	wantCode(t, "garbage sig", code, b, http.StatusUnauthorized, "bad_signature")
	if strings.Contains(string(b), "script") {
		t.Fatalf("echoed: %s", b)
	}
}

// A registry entry removed (or broken) between challenge and token is honoured
// at the token step: the registry is re-read for every token.
func TestLoginSignerRecheckedAtToken(t *testing.T) {
	f := newLoginFixture(t)
	c := f.challenge(t, "alice")
	b, _ := json.Marshal(map[string]any{"version": 1, "signers": []signing.Signer{f.bob.signer}})
	if err := os.WriteFile(f.reg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, rb := f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "removed signer", code, rb, http.StatusUnauthorized, "bad_signature")

	// A WebAuthn signer cannot log in with an SSH signature.
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	wa := signing.Signer{ID: "alice", Type: signing.TypeWebAuthnES256, Label: "p",
		PublicKey: base64.RawURLEncoding.EncodeToString(pt), CredentialID: "AQID",
		RPID: "orch.example", Origin: "https://orch.example"}
	if err := signing.ValidateSigner(wa); err != nil {
		t.Fatalf("webauthn signer: %v", err)
	}
	b, _ = json.Marshal(map[string]any{"version": 1, "signers": []signing.Signer{wa}})
	if err := os.WriteFile(f.reg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	c = f.challenge(t, "alice")
	code, rb = f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "webauthn signer", code, rb, http.StatusUnauthorized, "bad_signature")

	// Broken registry: 503 signers_unavailable, nothing echoed.
	if err := os.WriteFile(f.reg, []byte(`{"version":1,"signers":[{"id":"<x>"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c = f.challenge(t, "alice")
	code, rb = f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "broken registry", code, rb, http.StatusServiceUnavailable, "signers_unavailable")
	if strings.Contains(string(rb), "<x>") {
		t.Fatalf("echoed: %s", rb)
	}
	// Absent registry (legacy mode): nobody can log in.
	if err := os.Remove(f.reg); err != nil {
		t.Fatal(err)
	}
	c = f.challenge(t, "alice")
	code, rb = f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "absent registry", code, rb, http.StatusServiceUnavailable, "signers_unavailable")
}

func TestExpiredChallenge(t *testing.T) {
	f := newLoginFixture(t)
	c := f.challenge(t, "alice")
	sig := f.alice.signNS(t, signing.NamespaceLogin, c.Message)
	f.clock.advance(LoginChallengeTTL + time.Second)
	code, b := f.token(t, c.ChallengeID, sig)
	wantCode(t, "expired", code, b, http.StatusUnauthorized, "bad_challenge")

	// Just inside the TTL still works.
	c = f.challenge(t, "alice")
	sig = f.alice.signNS(t, signing.NamespaceLogin, c.Message)
	f.clock.advance(LoginChallengeTTL - 5*time.Second)
	if code, b := f.token(t, c.ChallengeID, sig); code != 200 {
		t.Fatalf("inside ttl %d %s", code, b)
	}

	// Expired by the store's own (real) clock as well, while the handler's
	// clock still says it is fresh.
	f.clock.set(-2*LoginChallengeTTL - time.Minute)
	c = f.challenge(t, "alice")
	code, b = f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c.Message))
	wantCode(t, "store-expired", code, b, http.StatusUnauthorized, "bad_challenge")
}

func TestApprovalSignatureNotAcceptedAsLogin(t *testing.T) {
	f := newLoginFixture(t)
	c := f.challenge(t, "alice")
	// Right key, right message, approval namespace.
	code, b := f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceApproval, c.Message))
	wantCode(t, "approval namespace", code, b, http.StatusUnauthorized, "bad_signature")
	// A login-namespace signature over different text (an approval message).
	c = f.challenge(t, "alice")
	approvalText := "aigate-approval-v1\nkind: protected_modified\nrepo: 0123456789ab\ntask: 1\n" +
		"base: " + strings.Repeat("a", 40) + "\nhash: " + strings.Repeat("b", 16) + "\nissued: 2026-11-09T00:00:00Z\n"
	code, b = f.token(t, c.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, approvalText))
	wantCode(t, "other text", code, b, http.StatusUnauthorized, "bad_signature")
}

func TestLoginSignatureNotAcceptedAsApproval(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nLOGIN-NS\n")
	issued := nowSec()
	m := f.message(t, aid, issued)
	// The approval message signed in the login namespace.
	code, b := f.post(t, aid, f.body(f.alice.signNS(t, signing.NamespaceLogin, m.Text()), issued))
	if code != http.StatusForbidden || v1Code(b) != "bad_signature" {
		t.Fatalf("login-namespace grant: %d %s", code, b)
	}
	f.unchanged(t, tid, aid)
	// The same check at the signing layer.
	reg := signing.Registry{Signers: []signing.Signer{f.alice.signer}}
	sig := signing.Signature{Signer: "alice", Alg: signing.TypeSSHEd25519, Sig: f.alice.signNS(t, signing.NamespaceLogin, m.Text())}
	if err := signing.Verify(reg, m, sig, signing.VerifyOptions{}); err != signing.ErrBadSignature {
		t.Fatalf("signing.Verify: %v", err)
	}
	// A login message signed in the login namespace is no approval either.
	login := LoginMessage("alice", strings.Repeat("c", 64), issued)
	sig.Sig = f.alice.signNS(t, signing.NamespaceLogin, login)
	if err := signing.Verify(reg, m, sig, signing.VerifyOptions{}); err != signing.ErrBadSignature {
		t.Fatalf("login message as approval: %v", err)
	}
	if signing.NamespaceLogin == signing.NamespaceApproval {
		t.Fatal("namespaces equal")
	}
}

func TestTokenExpires(t *testing.T) {
	f := newLoginFixture(t)
	tok := f.login(t, f.alice)
	clock := &testClock{}
	f.e.d.handler.auth = &StoreAuthenticator{St: f.e.st, Now: clock.now}
	auth := hdr{"Authorization": "Bearer " + tok}
	if code, _, b := httpDo(t, f.srv, "GET", "/api/v1/tasks", "", auth); code != 200 {
		t.Fatalf("fresh %d %s", code, b)
	}
	// Use does not extend it: still refused after 15 minutes from issue.
	clock.advance(TokenMaxLifetime - time.Minute)
	if code, _, b := httpDo(t, f.srv, "GET", "/api/v1/tasks", "", auth); code != 200 {
		t.Fatalf("near end %d %s", code, b)
	}
	clock.advance(time.Minute + time.Second)
	if code, _, b := httpDo(t, f.srv, "GET", "/api/v1/tasks", "", auth); code != http.StatusUnauthorized {
		t.Fatalf("expired token %d %s", code, b)
	}
	if _, err := f.e.st.GetSession(context.Background(), tok); err == nil {
		t.Fatal("expired token row kept")
	}
	// The stored expiry itself is at most 15 minutes after creation.
	tok2 := f.login(t, f.alice)
	s, err := f.e.st.GetSession(context.Background(), tok2)
	if err != nil || s.ExpiresAt.Sub(s.CreatedAt) != TokenMaxLifetime {
		t.Fatalf("stored token lifetime %+v %v", s, err)
	}
}

func TestTokenUnlocksWriteNotApproval(t *testing.T) {
	f := newGrantFixture(t)
	f.e.d.handler.now = time.Now
	srv := f.e.httpServer(t)
	lf := &loginFixture{e: f.e, srv: srv, alice: f.alice}
	tok := lf.login(t, f.alice)
	auth := hdr{"Authorization": "Bearer " + tok, "Content-Type": "application/json"}

	// General writes work with the token alone.
	if code, _, b := httpDo(t, srv, "POST", "/api/v1/tasks", f.e.submitBody("from token"), auth); code != http.StatusCreated {
		t.Fatalf("submit with token %d %s", code, b)
	}

	tid, aid, _ := f.protectedTask(t, "line1\nTOKEN\n")
	path := fmt.Sprintf("/api/v1/approvals/%d/grant", aid)
	issued := nowSec()
	m := f.message(t, aid, issued)
	type want struct {
		body   any
		status int
		code   string
	}
	refused := map[string]want{
		// Malformed: no issued, so no message can be rebuilt.
		"empty body": {map[string]any{}, http.StatusBadRequest, "invalid_message"},
		// Well-formed requests whose signature is not an approval signature.
		"no signature":  {map[string]any{"signer": "alice", "alg": signing.TypeSSHEd25519, "issued": issued.Format(time.RFC3339)}, http.StatusForbidden, "bad_signature"},
		"garbage sig":   {f.body("not a signature", issued), http.StatusForbidden, "bad_signature"},
		"login sig":     {f.body(f.alice.signNS(t, signing.NamespaceLogin, m.Text()), issued), http.StatusForbidden, "bad_signature"},
		"the token":     {f.body(tok, issued), http.StatusForbidden, "bad_signature"},
		"login message": {f.body(f.alice.signNS(t, signing.NamespaceLogin, LoginMessage("alice", strings.Repeat("d", 64), issued)), issued), http.StatusForbidden, "bad_signature"},
	}
	for name, w := range refused {
		b, _ := json.Marshal(w.body)
		code, _, rb := httpDo(t, srv, "POST", path, string(b), auth)
		if code != w.status || v1Code(rb) != w.code {
			t.Fatalf("%s: got %d %s, want %d %s", name, code, rb, w.status, w.code)
		}
		f.unchanged(t, tid, aid)
	}
	// A session (with its CSRF header) cannot grant without a signature either.
	stok, csrf := f.e.newSession(t, "alice")
	b, _ := json.Marshal(f.body("not a signature", issued))
	if code, _, rb := httpDo(t, srv, "POST", path, string(b), hdr{"Cookie": cookie(stok), CSRFHeader: csrf}); code != http.StatusForbidden || v1Code(rb) != "bad_signature" {
		t.Fatalf("session without signature: %d %s", code, rb)
	}
	f.unchanged(t, tid, aid)

	// With the approval signature the same token grants.
	b, _ = json.Marshal(f.body(f.alice.signText(t, m.Text()), issued))
	if code, _, rb := httpDo(t, srv, "POST", path, string(b), auth); code != http.StatusOK {
		t.Fatalf("signed grant with token %d %s", code, rb)
	}
}

// Live login challenges are capped; at the cap /auth/challenge answers 503
// busy, and a taken or expired challenge frees its place.
func TestLoginChallengeCap(t *testing.T) {
	f := newLoginFixture(t)
	f.e.d.handler.loginChallengeCap = 2 // before the next request
	c1 := f.challenge(t, "alice")
	f.challenge(t, "mallory")
	code, b := f.post(t, "/api/v1/auth/challenge", map[string]string{"signer": "zed-<x>"})
	wantCode(t, "malformed id still checked first", code, b, http.StatusBadRequest, "bad_request")
	code, b = f.post(t, "/api/v1/auth/challenge", map[string]string{"signer": "bob"})
	wantCode(t, "at cap", code, b, http.StatusServiceUnavailable, "busy")
	if strings.Contains(string(b), "bob") {
		t.Fatalf("echoed: %s", b)
	}
	// Taking one (a login) frees a place.
	if code, b := f.token(t, c1.ChallengeID, f.alice.signNS(t, signing.NamespaceLogin, c1.Message)); code != 200 {
		t.Fatalf("token %d %s", code, b)
	}
	f.challenge(t, "bob")
	code, b = f.post(t, "/api/v1/auth/challenge", map[string]string{"signer": "bob"})
	wantCode(t, "at cap again", code, b, http.StatusServiceUnavailable, "busy")
	// Expired challenges do not count: with one expired row stored and the
	// cap raised to 4 (2 live), there is room for exactly two fresh ones.
	if err := f.e.st.SaveChallenge(context.Background(), store.Challenge{ID: strings.Repeat("e", 64),
		Purpose: ChallengePurposeLogin, Data: "{}", ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	f.e.d.handler.loginChallengeCap = 4
	f.challenge(t, "alice")
	f.challenge(t, "alice")
	code, b = f.post(t, "/api/v1/auth/challenge", map[string]string{"signer": "alice"})
	wantCode(t, "cap 4", code, b, http.StatusServiceUnavailable, "busy")
	if DefaultLoginChallengeCap != 1000 {
		t.Fatalf("default cap %d", DefaultLoginChallengeCap)
	}
}

// Only healthz and the two SSH login routes are reachable without
// credentials; every other route (and other methods on the login paths)
// needs them.
func TestOnlyHealthzAndLoginRoutesOpen(t *testing.T) {
	f := newLoginFixture(t)
	open := map[string]bool{
		"GET /api/v1/healthz":         true,
		"POST /api/v1/auth/challenge": true,
		"POST /api/v1/auth/token":     true,
	}
	routes := []string{
		"GET /api/v1/healthz", "POST /api/v1/auth/challenge", "POST /api/v1/auth/token",
		"GET /api/v1/auth/challenge", "GET /api/v1/auth/token", "PUT /api/v1/auth/token",
		"POST /api/v1/auth/session", "GET /api/v1/auth/session", "POST /api/v1/auth/logout",
		"POST /api/v1/auth/webauthn/login/begin", "POST /api/v1/healthz",
		"GET /api/v1/tasks", "POST /api/v1/tasks", "GET /api/v1/tasks/1", "GET /api/v1/tasks/1/events",
		"POST /api/v1/tasks/1/cancel", "POST /api/v1/tasks/1/retry", "GET /api/v1/tasks/1/report",
		"GET /api/v1/tasks/1/diff", "GET /api/v1/approvals", "GET /api/v1/approvals/1",
		"POST /api/v1/approvals/1/reverify", "POST /api/v1/approvals/1/grant", "GET /api/v1/stages",
		"GET /api/v1/overview", "GET /api/v1/projects", "GET /api/v1/policy", "GET /api/v1/ops",
		"GET /api/v1/nope", "GET /tasks", "POST /tasks", "GET /approvals", "GET /", "GET /api/v1/auth",
		"POST /api/v1/auth/challenge/", "POST /api/v1/auth/token/x",
	}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt, " ")
		code, _, b := httpDo(t, f.srv, method, path, `{"signer":"alice"}`, hdr{"Content-Type": "application/json"})
		if open[rt] {
			if code == http.StatusUnauthorized {
				t.Errorf("%s: open route refused: %d %s", rt, code, b)
			}
			continue
		}
		if code != http.StatusUnauthorized {
			t.Errorf("%s: got %d %s, want 401", rt, code, b)
		}
	}
}
