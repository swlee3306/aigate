package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/store"
)

// None of these tests may use t.Parallel: they change the process-wide signers
// registry path (useTestRegistry) and swap handler fields before requests.

const (
	regRPID   = "console.example.test"
	regOrigin = "https://console.example.test"
)

// ---- test-only CBOR encoder and synthetic authenticator (independent of
// internal/webauthn's test helpers) ----

func tcHead(major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return []byte{m | byte(n)}
	case n <= 0xff:
		return []byte{m | 24, byte(n)}
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16([]byte{m | 25}, uint16(n))
	default:
		return binary.BigEndian.AppendUint32([]byte{m | 26}, uint32(n))
	}
}

func tcInt(v int64) []byte {
	if v < 0 {
		return tcHead(1, uint64(-1-v))
	}
	return tcHead(0, uint64(v))
}

func tcBytes(b []byte) []byte { return append(tcHead(2, uint64(len(b))), b...) }
func tcText(s string) []byte  { return append(tcHead(3, uint64(len(s))), s...) }
func tcMap(kv ...[]byte) []byte {
	out := tcHead(5, uint64(len(kv)/2))
	for _, b := range kv {
		out = append(out, b...)
	}
	return out
}

type passkey struct {
	key    *ecdsa.PrivateKey
	credID []byte
}

func newPasskey(t *testing.T) *passkey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &passkey{key: k, credID: id}
}

func (p *passkey) point() []byte {
	b, err := p.key.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return b
}

func (p *passkey) cose() []byte {
	pt := p.point()
	return tcMap(tcInt(1), tcInt(2), tcInt(3), tcInt(-7), tcInt(-1), tcInt(1),
		tcInt(-2), tcBytes(pt[1:33]), tcInt(-3), tcBytes(pt[33:65]))
}

// regOpts shapes one synthetic navigator.credentials.create() answer.
type regOpts struct {
	rpID, origin, typ string
	flags             byte
	challenge         string // base64url as sent in clientData
	fmt               string
	credID            []byte // in authData (nil: the passkey's)
	extraCD           map[string]any
}

func (p *passkey) authData(o regOpts) []byte {
	h := sha256.Sum256([]byte(o.rpID))
	b := append([]byte{}, h[:]...)
	b = append(b, o.flags, 0, 0, 0, 0)
	b = append(b, make([]byte, 16)...) // AAGUID
	id := o.credID
	if id == nil {
		id = p.credID
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(id)))
	b = append(b, id...)
	return append(b, p.cose()...)
}

func (p *passkey) attestation(o regOpts) []byte {
	return tcMap(tcText("fmt"), tcText(o.fmt), tcText("attStmt"), tcMap(), tcText("authData"), tcBytes(p.authData(o)))
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// credential is PublicKeyCredential.toJSON() for a registration (with the
// extra members a browser sends, which the server ignores).
func (p *passkey) credential(t *testing.T, o regOpts) map[string]any {
	t.Helper()
	cd := map[string]any{"type": o.typ, "challenge": o.challenge, "origin": o.origin}
	for k, v := range o.extraCD {
		cd[k] = v
	}
	cdj, err := json.Marshal(cd)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64u(p.credID), "rawId": b64u(p.credID), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON": b64u(cdj), "attestationObject": b64u(p.attestation(o)),
			"transports": []string{"internal"},
		},
	}
}

func goodReg(challenge string) regOpts {
	return regOpts{rpID: regRPID, origin: regOrigin, typ: "webauthn.create", flags: 0x45,
		challenge: challenge, fmt: "none"}
}

// assert signs a WebAuthn assertion over challenge for rpID/origin.
func (p *passkey) assert(t *testing.T, challenge []byte, rpID, origin string) (authData, clientData, sig []byte) {
	t.Helper()
	h := sha256.Sum256([]byte(rpID))
	authData = append(append([]byte{}, h[:]...), 0x05, 0, 0, 0, 1)
	cdj, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": b64u(challenge), "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	ch := sha256.Sum256(cdj)
	d := sha256.Sum256(append(append([]byte{}, authData...), ch[:]...))
	sig, err = ecdsa.SignASN1(rand.Reader, p.key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return authData, cdj, sig
}

// ---- fixture ----

type regFixture struct {
	e       *env
	srv     *httptest.Server
	clock   *testClock
	reg     string // registry path
	pending string // pending file path
	alice   testSigner
}

func withWebAuthn(pending string) func(*Config) {
	return func(c *Config) { c.RPID, c.Origin, c.PendingFile = regRPID, regOrigin, pending }
}

func newRegFixture(t *testing.T) *regFixture {
	t.Helper()
	alice := newTestSigner(t, "alice")
	reg := useTestRegistry(t, []signing.Signer{alice.signer})
	pending := filepath.Join(t.TempDir(), "state", "signers.pending.json")
	e := newEnv(t, noSteps{}, 1, withWebAuthn(pending))
	e.d.pauseWorkers()
	f := &regFixture{e: e, reg: reg, pending: pending, clock: &testClock{}, alice: alice}
	e.d.handler.now = f.clock.now // before the first request
	f.srv = e.httpServer(t)
	return f
}

type enrollResp struct {
	Code, ID, Label, Expires string
}

// enroll asks for a code over the socket (the socket principal).
func (f *regFixture) enroll(t *testing.T, body string) enrollResp {
	t.Helper()
	code, _, b := f.e.sockDo(t, "POST", "/api/v1/admin/enroll", body, hdr{"Content-Type": "application/json"})
	if code != http.StatusCreated {
		t.Fatalf("enroll %d %s", code, b)
	}
	var r enrollResp
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *regFixture) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, _, rb := httpDo(t, f.srv, "POST", path, string(b), hdr{"Content-Type": "application/json"})
	return code, rb
}

type beginResp struct {
	ChallengeID string `json:"challenge_id"`
	Expires     string `json:"expires"`
	Options     struct {
		RP               struct{ ID, Name string } `json:"rp"`
		User             struct{ ID, Name, DisplayName string }
		Challenge        string
		PubKeyCredParams []struct {
			Type string
			Alg  int
		}
		Timeout                int64
		Attestation            string
		AuthenticatorSelection struct {
			ResidentKey, UserVerification string
		}
	} `json:"options"`
}

func (f *regFixture) begin(t *testing.T, code string) beginResp {
	t.Helper()
	c, b := f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": code})
	if c != http.StatusOK {
		t.Fatalf("begin %d %s", c, b)
	}
	var r beginResp
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *regFixture) finish(t *testing.T, challengeID string, cred any) (int, []byte) {
	t.Helper()
	return f.post(t, "/api/v1/auth/webauthn/register/finish", map[string]any{"challenge_id": challengeID, "credential": cred})
}

// fresh runs enroll + begin and returns the challenge.
func (f *regFixture) fresh(t *testing.T, body string) beginResp {
	t.Helper()
	return f.begin(t, f.enroll(t, body).Code)
}

func (f *regFixture) pendingList(t *testing.T) []PendingCredential {
	t.Helper()
	ps, err := ReadPending(f.pending)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func fileBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var codeRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)

// ---- tests named in the brief ----

func TestEnrollCodeSingleUseAndExpiry(t *testing.T) {
	f := newRegFixture(t)
	r := f.enroll(t, `{"id":"phone","label":"iPhone"}`)
	if !codeRE.MatchString(r.Code) || r.ID != "phone" || r.Label != "iPhone" {
		t.Fatalf("enroll %+v", r)
	}
	exp, err := time.Parse(time.RFC3339, r.Expires)
	if err != nil || exp.Sub(time.Now()) > EnrollCodeTTL+time.Second || exp.Sub(time.Now()) < EnrollCodeTTL-5*time.Second {
		t.Fatalf("expires %q", r.Expires)
	}
	// The printed form, lower case and without the hyphen, is the same code.
	f.begin(t, strings.ToLower(strings.ReplaceAll(r.Code, "-", "")))
	// Single use.
	code, b := f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": r.Code})
	wantCode(t, "reused code", code, b, http.StatusUnauthorized, "bad_enroll_code")

	// Expiry (handler clock): a code is refused after EnrollCodeTTL, and the
	// refused attempt consumed it.
	r2 := f.enroll(t, `{}`)
	f.clock.set(EnrollCodeTTL + time.Second)
	code, b = f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": r2.Code})
	wantCode(t, "expired code", code, b, http.StatusUnauthorized, "bad_enroll_code")
	f.clock.set(0)
	code, b = f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": r2.Code})
	wantCode(t, "expired code consumed", code, b, http.StatusUnauthorized, "bad_enroll_code")
	// Just inside the TTL still works.
	r3 := f.enroll(t, `{}`)
	f.clock.set(EnrollCodeTTL - 5*time.Second)
	f.begin(t, r3.Code)
	f.clock.set(0)
	if !regexp.MustCompile(`^pk-[0-9a-f]{8}$`).MatchString(r3.ID) || r3.Label != r3.ID {
		t.Fatalf("default id/label %+v", r3)
	}
}

func TestRegisterBeginNeedsValidCode(t *testing.T) {
	f := newRegFixture(t)
	good := f.enroll(t, `{"id":"phone","label":"iPhone"}`)
	var first []byte
	for _, c := range []string{"", "ABCD-EFGH", "ABCD-EFG", "ABCD-EFGHJ", "ABCD-EFGU", "<img onerror=EVIL>", strings.Repeat("A", 40), "АВСD-EFGH"} {
		code, b := f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": c})
		wantCode(t, "code "+c, code, b, http.StatusUnauthorized, "bad_enroll_code")
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Fatalf("refusals differ: %s vs %s", first, b)
		}
		if strings.Contains(string(b), "EVIL") {
			t.Fatalf("echoed: %s", b)
		}
	}
	for _, body := range []string{``, `{}x`, `{"code":"x","extra":1}`, `[]`} {
		code, _, b := httpDo(t, f.srv, "POST", "/api/v1/auth/webauthn/register/begin", body, hdr{"Content-Type": "application/json"})
		wantCode(t, "body "+body, code, b, http.StatusBadRequest, "bad_request")
	}
	// The failures did not consume the real code.
	r := f.begin(t, good.Code)
	o := r.Options
	ch, err := base64.RawURLEncoding.Strict().DecodeString(o.Challenge)
	uid, err2 := base64.RawURLEncoding.Strict().DecodeString(o.User.ID)
	if err != nil || err2 != nil || len(ch) != 32 || len(uid) != 16 {
		t.Fatalf("challenge/user id: %q %q", o.Challenge, o.User.ID)
	}
	if o.RP.ID != regRPID || o.RP.Name == "" || o.User.DisplayName != "iPhone" || !strings.Contains(o.User.Name, "phone") ||
		len(o.PubKeyCredParams) != 1 || o.PubKeyCredParams[0].Type != "public-key" || o.PubKeyCredParams[0].Alg != -7 ||
		o.Attestation != "none" || o.AuthenticatorSelection.UserVerification != "required" ||
		o.AuthenticatorSelection.ResidentKey != "preferred" || o.Timeout != RegisterChallengeTTL.Milliseconds() ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(r.ChallengeID) {
		t.Fatalf("options %+v", r)
	}
	ctx := context.Background()
	if n, _ := f.e.st.CountChallenges(ctx, ChallengePurposeRegister); n != 1 {
		t.Fatalf("register challenges %d", n)
	}
	if n, _ := f.e.st.CountChallenges(ctx, ChallengePurposeLogin); n != 0 {
		t.Fatalf("login challenges %d", n)
	}
	// Two ceremonies get different challenges.
	if r2 := f.fresh(t, `{}`); r2.Options.Challenge == o.Challenge || r2.ChallengeID == r.ChallengeID {
		t.Fatal("challenge reused")
	}
}

func TestRegisterFinishStoresPendingOnly(t *testing.T) {
	f := newRegFixture(t)
	regBefore := fileBytes(t, f.reg)
	pk := newPasskey(t)
	r := f.fresh(t, `{"id":"phone","label":"iPhone"}`)
	code, b := f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge)))
	if code != http.StatusCreated {
		t.Fatalf("finish %d %s", code, b)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pk.point())
	wantFP := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	if out["id"] != "phone" || out["label"] != "iPhone" || out["fingerprint"] != wantFP ||
		out["rp_id"] != regRPID || out["origin"] != regOrigin {
		t.Fatalf("finish body %s", b)
	}
	ps := f.pendingList(t)
	if len(ps) != 1 {
		t.Fatalf("pending %+v", ps)
	}
	p := ps[0]
	if p.ID != "phone" || p.Label != "iPhone" || p.CredentialID != b64u(pk.credID) || p.PublicKey != b64u(pk.point()) ||
		p.RPID != regRPID || p.Origin != regOrigin || time.Since(p.Created) > time.Minute || p.Created.Location() != time.UTC {
		t.Fatalf("pending entry %+v", p)
	}
	// It is activatable as is (the registry rules) and has the same
	// fingerprint as `orch admin signers pending` shows.
	s := signing.Signer{ID: p.ID, Type: signing.TypeWebAuthnES256, Label: p.Label, PublicKey: p.PublicKey,
		CredentialID: p.CredentialID, RPID: p.RPID, Origin: p.Origin}
	if fp, err := signing.Fingerprint(s); err != nil || fp != wantFP {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	fi, err := os.Stat(f.pending)
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("pending mode %v %v", fi.Mode(), err)
	}
	if left := leftovers(t, filepath.Dir(f.pending)); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	// Not a signer: the registry is byte-identical and does not know it.
	if !bytes.Equal(fileBytes(t, f.reg), regBefore) {
		t.Fatal("registry changed")
	}
	reg, err := signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Find("phone"); ok {
		t.Fatal("pending credential is a signer")
	}
	// Login is impossible: the challenge step answers (no oracle) but no
	// signature of this passkey yields a token.
	lf := &loginFixture{e: f.e, srv: f.srv}
	c := lf.challenge(t, "phone")
	_, _, sig := pk.assert(t, []byte(c.Message), regRPID, regOrigin)
	code, b = lf.token(t, c.ChallengeID, b64u(sig))
	wantCode(t, "pending login", code, b, http.StatusUnauthorized, "bad_signature")
}

func TestRegisterRejectsWrongOriginOrRPID(t *testing.T) {
	f := newRegFixture(t)
	pk := newPasskey(t)
	for _, origin := range []string{"https://evil.example.test", regOrigin + ":8443", "http://console.example.test",
		regOrigin + "/", "https://sub." + regRPID, "<img onerror=EVIL>"} {
		r := f.fresh(t, `{}`)
		o := goodReg(r.Options.Challenge)
		o.origin = origin
		code, b := f.finish(t, r.ChallengeID, pk.credential(t, o))
		wantCode(t, "origin "+origin, code, b, http.StatusBadRequest, "bad_origin")
		if strings.Contains(string(b), "EVIL") || strings.Contains(string(b), "evil") {
			t.Fatalf("echoed: %s", b)
		}
	}
	for _, rp := range []string{"evil.example.test", "example.test", "sub." + regRPID, ""} {
		r := f.fresh(t, `{}`)
		o := goodReg(r.Options.Challenge)
		o.rpID = rp
		code, b := f.finish(t, r.ChallengeID, pk.credential(t, o))
		wantCode(t, "rp "+rp, code, b, http.StatusBadRequest, "bad_attestation")
	}
	if ps := f.pendingList(t); len(ps) != 0 {
		t.Fatalf("refused registrations stored: %+v", ps)
	}
	// The same passkey with the right origin and rpId is accepted.
	r := f.fresh(t, `{}`)
	if code, b := f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge))); code != http.StatusCreated {
		t.Fatalf("control %d %s", code, b)
	}
}

func TestPendingNotUsableForLoginOrApproval(t *testing.T) {
	gf := newGrantFixture(t)
	pending := filepath.Join(t.TempDir(), "signers.pending.json")
	h := gf.e.d.handler
	h.rpID, h.origin, h.pendingFile = regRPID, regOrigin, pending // before the first request
	f := &regFixture{e: gf.e, srv: gf.e.httpServer(t), reg: gf.reg, pending: pending}
	regBefore := fileBytes(t, f.reg)

	pk := newPasskey(t)
	r := f.fresh(t, `{"id":"phone"}`)
	if code, b := f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge))); code != http.StatusCreated {
		t.Fatalf("finish %d %s", code, b)
	}
	p := f.pendingList(t)[0]

	grantBody := func(issued time.Time, aid int64) map[string]any {
		m := gf.message(t, aid, issued)
		d := m.Digest()
		ad, cd, sig := pk.assert(t, d[:], regRPID, regOrigin)
		return map[string]any{"signer": "phone", "alg": signing.TypeWebAuthnES256, "sig": b64u(sig),
			"issued":   issued.Format(time.RFC3339),
			"webauthn": map[string]string{"authenticator_data": b64u(ad), "client_data_json": b64u(cd)}}
	}
	// Approval: the pending passkey's correct assertion is an unknown signer.
	tid, aid, _ := gf.protectedTask(t, "line1\nCHANGED\n")
	code, b := gf.post(t, aid, grantBody(nowSec(), aid))
	wantCode(t, "pending grant", code, b, http.StatusForbidden, "unknown_signer")
	gf.unchanged(t, tid, aid)
	if !bytes.Equal(fileBytes(t, f.reg), regBefore) {
		t.Fatal("registry changed")
	}
	// Login: same answer as any wrong signature.
	lf := &loginFixture{e: f.e, srv: f.srv}
	c := lf.challenge(t, "phone")
	_, _, sig := pk.assert(t, []byte(c.Message), regRPID, regOrigin)
	code, b = lf.token(t, c.ChallengeID, b64u(sig))
	wantCode(t, "pending login", code, b, http.StatusUnauthorized, "bad_signature")

	// Control: once root activates exactly this entry, the same kind of
	// assertion is accepted (so the refusal above was the pending state).
	active := signing.Signer{ID: p.ID, Type: signing.TypeWebAuthnES256, Label: p.Label, PublicKey: p.PublicKey,
		CredentialID: p.CredentialID, RPID: p.RPID, Origin: p.Origin, AddedAt: time.Now().UTC().Truncate(time.Second)}
	rb, err := json.Marshal(map[string]any{"version": 1, "signers": []signing.Signer{gf.alice.signer, active}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.reg, rb, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, b := gf.post(t, aid, grantBody(nowSec(), aid)); code != http.StatusOK {
		t.Fatalf("activated grant %d %s", code, b)
	}
}

// ---- further coverage ----

// Enrollment codes are issued to socket principals only.
func TestEnrollSocketOnly(t *testing.T) {
	f := newRegFixture(t)
	tok, csrf := f.e.newSession(t, "alice")
	bearer := f.e.newToken(t, "alice")
	for name, h := range map[string]hdr{
		"session+csrf": {"Cookie": cookie(tok), CSRFHeader: csrf, "Content-Type": "application/json"},
		"token":        {"Authorization": "Bearer " + bearer, "Content-Type": "application/json"},
	} {
		code, _, b := httpDo(t, f.srv, "POST", "/api/v1/admin/enroll", `{}`, h)
		wantCode(t, name, code, b, http.StatusForbidden, "socket_only")
	}
	code, _, b := httpDo(t, f.srv, "POST", "/api/v1/admin/enroll", `{}`, hdr{"Content-Type": "application/json"})
	wantCode(t, "anonymous", code, b, http.StatusUnauthorized, "unauthenticated")
	if n, _ := f.e.st.CountChallenges(context.Background(), ChallengePurposeEnroll); n != 0 {
		t.Fatalf("refused enrolls stored %d codes", n)
	}
	f.enroll(t, `{}`)
	if n, _ := f.e.st.CountChallenges(context.Background(), ChallengePurposeEnroll); n != 1 {
		t.Fatalf("codes %d", n)
	}
	// Bad id / label / body / taken id.
	for _, body := range []string{`{"id":"Phone"}`, `{"id":"a\nb"}`, `{"label":"x\u001b[2J"}`,
		`{"label":"` + strings.Repeat("x", 65) + `"}`, `{"nope":1}`, ``} {
		code, _, b := f.e.sockDo(t, "POST", "/api/v1/admin/enroll", body, nil)
		wantCode(t, "body "+body, code, b, http.StatusBadRequest, "bad_request")
		if strings.Contains(string(b), "\x1b") {
			t.Fatalf("echoed: %q", b)
		}
	}
	code, _, b = f.e.sockDo(t, "POST", "/api/v1/admin/enroll", `{"id":"alice"}`, nil)
	wantCode(t, "registry id", code, b, http.StatusConflict, "id_taken")
}

// The raw code is never stored: only its hash is the challenge row's id.
func TestEnrollCodeNeverStored(t *testing.T) {
	f := newRegFixture(t)
	r := f.enroll(t, `{"id":"phone"}`)
	raw := strings.ReplaceAll(r.Code, "-", "")
	ctx := context.Background()
	// The hashed id is there (the scan looks at the right data).
	ch, err := f.e.st.TakeChallenge(ctx, enrollChallengeID(raw))
	if err != nil || ch.Purpose != ChallengePurposeEnroll {
		t.Fatalf("hashed row: %+v %v", ch, err)
	}
	if err := f.e.st.SaveChallenge(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ch.ID+ch.Data, raw) {
		t.Fatalf("row holds the code: %+v", ch)
	}
	var all []byte
	for _, suf := range []string{"", "-wal", "-shm"} {
		if b, err := os.ReadFile(f.e.st.Path() + suf); err == nil {
			all = append(all, b...)
		}
	}
	if !bytes.Contains(all, []byte(store.HashSecret(raw))) {
		t.Fatal("hash not found in the database files: scan looked at the wrong data")
	}
	if bytes.Contains(all, []byte(raw)) || bytes.Contains(all, []byte(r.Code)) {
		t.Fatal("raw enrollment code in the database files")
	}
}

// CF14: login challenges are refused by the registration steps and
// registration challenges by the login step; each refusal consumes the row.
func TestRegisterChallengePurposeIsolation(t *testing.T) {
	f := newRegFixture(t)
	ctx := context.Background()
	pk := newPasskey(t)
	// A login challenge at register/finish.
	lf := &loginFixture{e: f.e, srv: f.srv}
	lc := lf.challenge(t, "alice")
	code, b := f.finish(t, lc.ChallengeID, pk.credential(t, goodReg(b64u([]byte(lc.Message)))))
	wantCode(t, "login challenge at finish", code, b, http.StatusUnauthorized, "bad_challenge")
	if _, err := f.e.st.TakeChallenge(ctx, lc.ChallengeID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("login row not consumed: %v", err)
	}
	// A registration challenge at /auth/token.
	r := f.fresh(t, `{}`)
	code, b = lf.token(t, r.ChallengeID, "x")
	wantCode(t, "register challenge at token", code, b, http.StatusUnauthorized, "bad_challenge")
	code, b = f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge)))
	wantCode(t, "register challenge consumed by token", code, b, http.StatusUnauthorized, "bad_challenge")
	// An enrollment code row is no registration challenge either.
	e := f.enroll(t, `{}`)
	code, b = f.finish(t, enrollChallengeID(strings.ReplaceAll(e.Code, "-", "")), pk.credential(t, goodReg(b64u([]byte("x")))))
	wantCode(t, "enroll row at finish", code, b, http.StatusUnauthorized, "bad_challenge")
	// A login row carrying registration-shaped data is still refused at
	// finish (the purpose decides, not the data's shape).
	chal := bytes.Repeat([]byte{9}, 32)
	data, _ := json.Marshal(map[string]string{"id": "phone", "label": "p", "challenge": b64u(chal), "user_id": b64u(chal[:16])})
	forged := strings.Repeat("f", 64)
	if err := f.e.st.SaveChallenge(ctx, store.Challenge{ID: forged, Purpose: ChallengePurposeLogin,
		Data: string(data), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	code, b = f.finish(t, forged, pk.credential(t, goodReg(b64u(chal))))
	wantCode(t, "login row with register data", code, b, http.StatusUnauthorized, "bad_challenge")
	// And a registration row carrying login-shaped data, correctly signed,
	// is refused at /auth/token.
	msg := LoginMessage("alice", strings.Repeat("a", 64), time.Now().UTC().Truncate(time.Second))
	ld, _ := json.Marshal(map[string]string{"signer": "alice", "message": msg})
	forged2 := strings.Repeat("g", 64)
	if err := f.e.st.SaveChallenge(ctx, store.Challenge{ID: forged2, Purpose: ChallengePurposeRegister,
		Data: string(ld), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	code, b = lf.token(t, forged2, f.alice.signNS(t, signing.NamespaceLogin, msg))
	wantCode(t, "register row with login data", code, b, http.StatusUnauthorized, "bad_challenge")
	// A row under a code's id with another purpose is no code.
	raw := "ABCDEFGH"
	if err := f.e.st.SaveChallenge(ctx, store.Challenge{ID: enrollChallengeID(raw), Purpose: ChallengePurposeLogin,
		Data: `{"id":"phone","label":"p"}`, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	code, b = f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": raw})
	wantCode(t, "login row as code", code, b, http.StatusUnauthorized, "bad_enroll_code")
	if ps := f.pendingList(t); len(ps) != 0 {
		t.Fatalf("stored %+v", ps)
	}
}

// Malformed registration answers are refused without a panic, nothing is
// stored, and each failure consumes the challenge.
func TestRegisterMalformedAttestation(t *testing.T) {
	f := newRegFixture(t)
	pk := newPasskey(t)
	type variant struct {
		name, want string
		cred       func(ch string) any
	}
	mod := func(fn func(*regOpts)) func(string) any {
		return func(ch string) any { o := goodReg(ch); fn(&o); return pk.credential(t, o) }
	}
	edit := func(fn func(m map[string]any)) func(string) any {
		return func(ch string) any { m := pk.credential(t, goodReg(ch)); fn(m); return m }
	}
	resp := func(m map[string]any) map[string]any { return m["response"].(map[string]any) }
	variants := []variant{
		{"null", "bad_attestation", func(string) any { return nil }},
		{"string", "bad_attestation", func(string) any { return "EVIL" }},
		{"array", "bad_attestation", func(string) any { return []int{1} }},
		{"type", "bad_attestation", edit(func(m map[string]any) { m["type"] = "password" })},
		{"padded-b64", "bad_attestation", edit(func(m map[string]any) { resp(m)["clientDataJSON"] = resp(m)["clientDataJSON"].(string) + "=" })},
		{"std-b64", "bad_attestation", edit(func(m map[string]any) { resp(m)["attestationObject"] = "+/+/" })},
		{"newline-b64", "bad_attestation", edit(func(m map[string]any) {
			s := resp(m)["attestationObject"].(string)
			resp(m)["attestationObject"] = s[:4] + "\n" + s[4:]
		})},
		{"empty-att", "bad_attestation", edit(func(m map[string]any) { resp(m)["attestationObject"] = "" })},
		{"garbage-cbor", "bad_attestation", edit(func(m map[string]any) { resp(m)["attestationObject"] = b64u([]byte{0xbf, 0xff, 0x00, 0x9f}) })},
		{"truncated-cbor", "bad_attestation", edit(func(m map[string]any) {
			a := pk.attestation(goodReg("x"))
			resp(m)["attestationObject"] = b64u(a[:len(a)/2])
		})},
		{"huge-att", "bad_attestation", edit(func(m map[string]any) { resp(m)["attestationObject"] = b64u(bytes.Repeat([]byte{0x41}, 9000)) })},
		{"fmt-packed", "bad_attestation", mod(func(o *regOpts) { o.fmt = "packed" })},
		{"no-uv", "bad_attestation", mod(func(o *regOpts) { o.flags = 0x41 })},
		{"no-up", "bad_attestation", mod(func(o *regOpts) { o.flags = 0x44 })},
		{"no-at", "bad_attestation", mod(func(o *regOpts) { o.flags = 0x05 })},
		{"short-cred-id", "bad_attestation", func(ch string) any {
			o := goodReg(ch)
			o.credID = []byte("12345678")
			m := pk.credential(t, o)
			m["id"], m["rawId"] = b64u(o.credID), b64u(o.credID)
			return m
		}},
		{"id-mismatch", "bad_attestation", edit(func(m map[string]any) { m["id"] = b64u(bytes.Repeat([]byte{7}, 32)) })},
		{"rawid-mismatch", "bad_attestation", edit(func(m map[string]any) { m["rawId"] = b64u(bytes.Repeat([]byte{7}, 32)) })},
		{"no-id", "bad_attestation", edit(func(m map[string]any) { delete(m, "id") })},
		{"get-type", "bad_attestation", mod(func(o *regOpts) { o.typ = "webauthn.get" })},
		{"cross-origin", "bad_attestation", mod(func(o *regOpts) { o.extraCD = map[string]any{"crossOrigin": true} })},
		{"top-origin", "bad_attestation", mod(func(o *regOpts) { o.extraCD = map[string]any{"topOrigin": regOrigin} })},
		{"wrong-challenge", "bad_challenge", mod(func(o *regOpts) { o.challenge = b64u(bytes.Repeat([]byte{1}, 32)) })},
		{"cd-not-json", "bad_attestation", edit(func(m map[string]any) { resp(m)["clientDataJSON"] = b64u([]byte("<img onerror=EVIL>")) })},
	}
	for _, v := range variants {
		r := f.fresh(t, `{}`)
		code, b := f.finish(t, r.ChallengeID, v.cred(r.Options.Challenge))
		wantStatus := http.StatusBadRequest
		if v.want == "bad_challenge" {
			wantStatus = http.StatusUnauthorized
		}
		wantCode(t, v.name, code, b, wantStatus, v.want)
		if strings.Contains(string(b), "EVIL") {
			t.Fatalf("%s echoed: %s", v.name, b)
		}
		// Single use, also on failure.
		code, b = f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge)))
		wantCode(t, v.name+" reuse", code, b, http.StatusUnauthorized, "bad_challenge")
	}
	for _, body := range []string{`{"challenge_id":"x"}x`, `{"challenge_id":"x","credential":{},"more":1}`, `[]`} {
		code, _, b := httpDo(t, f.srv, "POST", "/api/v1/auth/webauthn/register/finish", body, hdr{"Content-Type": "application/json"})
		wantCode(t, "body "+body, code, b, http.StatusBadRequest, "bad_request")
	}
	// Expired challenge (handler clock).
	r := f.fresh(t, `{}`)
	f.clock.set(RegisterChallengeTTL + time.Second)
	code, b := f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge)))
	f.clock.set(0)
	wantCode(t, "expired", code, b, http.StatusUnauthorized, "bad_challenge")
	if ps := f.pendingList(t); len(ps) != 0 {
		t.Fatalf("stored %+v", ps)
	}
	if _, err := os.Stat(f.pending); !os.IsNotExist(err) {
		t.Fatalf("pending file written: %v", err)
	}
}

// Duplicates and a full pending list are refused; a full list refuses begin
// before the code is spent.
func TestRegisterPendingDedupeAndCap(t *testing.T) {
	f := newRegFixture(t)
	pk := newPasskey(t)
	r := f.fresh(t, `{"id":"phone"}`)
	if code, b := f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge))); code != http.StatusCreated {
		t.Fatalf("finish %d %s", code, b)
	}
	before := fileBytes(t, f.pending)
	// The same passkey again (another id).
	r = f.fresh(t, `{"id":"phone2"}`)
	code, b := f.finish(t, r.ChallengeID, pk.credential(t, goodReg(r.Options.Challenge)))
	wantCode(t, "same credential", code, b, http.StatusConflict, "already_pending")
	// The id is taken at enroll already.
	code, _, b = f.e.sockDo(t, "POST", "/api/v1/admin/enroll", `{"id":"phone"}`, nil)
	wantCode(t, "pending id", code, b, http.StatusConflict, "id_taken")
	if !bytes.Equal(fileBytes(t, f.pending), before) {
		t.Fatal("pending file changed by refused registrations")
	}
	// Fill to the cap.
	for i := len(f.pendingList(t)); i < MaxPendingCredentials; i++ {
		r := f.fresh(t, `{}`)
		if code, b := f.finish(t, r.ChallengeID, newPasskey(t).credential(t, goodReg(r.Options.Challenge))); code != http.StatusCreated {
			t.Fatalf("fill %d: %d %s", i, code, b)
		}
	}
	e := f.enroll(t, `{}`)
	code, b = f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": e.Code})
	wantCode(t, "full at begin", code, b, http.StatusServiceUnavailable, "busy")
	// The code was not spent: free a place (as activation pruning would) and retry.
	ps := f.pendingList(t)
	raw, _ := json.Marshal(pendingFileJSON{Version: 1, Pending: ps[1:]})
	if err := os.WriteFile(f.pending, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	f.begin(t, e.Code)
	if n := len(f.pendingList(t)); n != MaxPendingCredentials-1 {
		t.Fatalf("pending %d", n)
	}
}

// Live enrollment codes and registration challenges are capped (503 busy);
// begin checks the cap before spending the code.
func TestEnrollChallengeCap(t *testing.T) {
	f := newRegFixture(t)
	f.e.d.handler.enrollChallengeCap = 1 // before the next request
	e := f.enroll(t, `{}`)
	code, _, b := f.e.sockDo(t, "POST", "/api/v1/admin/enroll", `{}`, nil)
	wantCode(t, "enroll at cap", code, b, http.StatusServiceUnavailable, "busy")
	f.begin(t, e.Code) // frees the enroll place, takes the register place
	e2 := f.enroll(t, `{}`)
	code, b = f.post(t, "/api/v1/auth/webauthn/register/begin", map[string]string{"code": e2.Code})
	wantCode(t, "begin at cap", code, b, http.StatusServiceUnavailable, "busy")
	f.e.d.handler.enrollChallengeCap = 2
	f.begin(t, e2.Code) // not spent by the busy answer
	if DefaultEnrollChallengeCap != 200 {
		t.Fatalf("default cap %d", DefaultEnrollChallengeCap)
	}
}

// Without --rp-id/--origin the enrollment routes refuse with one fixed code.
func TestRegisterUnconfigured(t *testing.T) {
	useTestRegistry(t, nil)
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	code, _, b := e.sockDo(t, "POST", "/api/v1/admin/enroll", `{}`, nil)
	wantCode(t, "enroll", code, b, http.StatusServiceUnavailable, "webauthn_unconfigured")
	for _, p := range []string{"begin", "finish"} {
		body := `{"code":"ABCD-EFGH"}`
		if p == "finish" {
			body = `{"challenge_id":"x","credential":{}}`
		}
		code, _, b := httpDo(t, srv, "POST", "/api/v1/auth/webauthn/register/"+p, body, hdr{"Content-Type": "application/json"})
		wantCode(t, p, code, b, http.StatusServiceUnavailable, "webauthn_unconfigured")
	}
}

func TestValidateWebAuthnConfig(t *testing.T) {
	for _, ok := range [][2]string{
		{"", ""}, {regRPID, regOrigin}, {regRPID, regOrigin + ":8443"}, {"example.test", "https://console.example.test"},
		{"localhost", "http://localhost"}, {"localhost", "http://localhost:8080"},
	} {
		if err := ValidateWebAuthnConfig(ok[0], ok[1]); err != nil {
			t.Errorf("%q %q: %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]string{
		{regRPID, ""}, {"", regOrigin}, {"https://" + regRPID, regOrigin}, {regRPID + ":443", regOrigin},
		{"Console.example.test", regOrigin}, {regRPID, "https://evil.example.test"}, {regRPID, "https://xconsole.example.test"},
		{regRPID, "http://console.example.test"}, {regRPID, regOrigin + "/"}, {regRPID, regOrigin + "/x"},
		{regRPID, "https://user@console.example.test"}, {regRPID, regOrigin + ":0"}, {regRPID, regOrigin + ":99999"},
		{"localhost", "http://127.0.0.1"}, {regRPID, "http://localhost"}, {"localhost", "ftp://localhost"},
		{regRPID, "https://CONSOLE.example.test"}, {regRPID + "/x", regOrigin},
	} {
		if err := ValidateWebAuthnConfig(bad[0], bad[1]); err == nil {
			t.Errorf("%q %q accepted", bad[0], bad[1])
		}
	}
	if _, err := NewDaemon(Config{Socket: "/tmp/x.sock", RPID: regRPID, Origin: "https://evil.test"}, nil, noSteps{}); err == nil {
		t.Fatal("NewDaemon accepted a bad origin")
	}
}

// AppendPending: atomic replace with mode 0640, dedupe by decoded credential
// id and by id, a cap, pruning of activated ids, and refusal of a malformed
// or symlinked file (left untouched).
func TestAppendPendingFile(t *testing.T) {
	useTestRegistry(t, []signing.Signer{newTestSigner(t, "alice").signer})
	dir := t.TempDir()
	path := filepath.Join(dir, "p.json")
	entry := func(id string, cred byte) PendingCredential {
		return PendingCredential{ID: id, Label: id, CredentialID: b64u(bytes.Repeat([]byte{cred}, 16)),
			PublicKey: b64u(newPasskey(t).point()), RPID: regRPID, Origin: regOrigin, Created: time.Now().UTC().Truncate(time.Second)}
	}
	if err := AppendPending(path, entry("a", 1), 3); err != nil {
		t.Fatal(err)
	}
	ino := func() uint64 {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v %v", fi, err)
		}
		return inodeOf(fi)
	}
	first := ino()
	if err := AppendPending(path, entry("b", 1), 3); !errors.Is(err, errPendingDup) {
		t.Fatalf("dup credential: %v", err)
	}
	if err := AppendPending(path, entry("a", 2), 3); !errors.Is(err, errPendingDup) {
		t.Fatalf("dup id: %v", err)
	}
	if err := AppendPending(path, entry("b", 2), 3); err != nil {
		t.Fatal(err)
	}
	if ino() == first {
		t.Fatal("file rewritten in place, not renamed")
	}
	if err := AppendPending(path, entry("alice", 3), 3); err != nil { // stored; pruned next time
		t.Fatal(err)
	}
	if err := AppendPending(path, entry("c", 4), 3); err != nil {
		t.Fatalf("pruning activated id should make room: %v", err)
	}
	ps, err := ReadPending(path)
	if err != nil || len(ps) != 3 || ps[0].ID != "a" || ps[1].ID != "b" || ps[2].ID != "c" {
		t.Fatalf("list %+v %v", ps, err)
	}
	if err := AppendPending(path, entry("d", 5), 3); !errors.Is(err, errPendingFull) {
		t.Fatalf("cap: %v", err)
	}
	if left := leftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files: %v", left)
	}
	// Malformed: refused, untouched.
	bad := filepath.Join(dir, "bad.json")
	for _, content := range []string{`{"version":1,"pending":[],"x":1}`, `{"version":2,"pending":[]}`, `nope`} {
		if err := os.WriteFile(bad, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := AppendPending(bad, entry("z", 9), 3); err == nil {
			t.Fatalf("%s accepted", content)
		}
		if string(fileBytes(t, bad)) != content {
			t.Fatal("malformed file changed")
		}
	}
	// Symlink: refused, target untouched.
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, path)
	if err := AppendPending(link, entry("z", 9), 30); err == nil {
		t.Fatal("symlink accepted")
	}
	if !bytes.Equal(fileBytes(t, path), before) {
		t.Fatal("symlink target changed")
	}
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

func inodeOf(fi os.FileInfo) uint64 { return uint64(fi.Sys().(*syscall.Stat_t).Ino) }
