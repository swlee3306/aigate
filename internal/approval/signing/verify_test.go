package signing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func approvalMsg(issued time.Time) Message {
	return Message{
		Kind:   KindProtectedModified,
		Repo:   "0123456789ab",
		Task:   7,
		Base:   strings.Repeat("a", 40),
		Hash:   strings.Repeat("b", 16),
		Issued: issued,
	}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// --- synthetic WebAuthn authenticator (built independently of internal/webauthn) ---

type fakeAuthenticator struct {
	key    *ecdsa.PrivateKey
	rpID   string
	origin string
}

func newFakeAuthenticator(t *testing.T) fakeAuthenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return fakeAuthenticator{key: k, rpID: "console.example.test", origin: "https://console.example.test:8443"}
}

func (a fakeAuthenticator) signer(t *testing.T, id string) Signer {
	t.Helper()
	pub, err := a.key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return Signer{
		ID: id, Type: TypeWebAuthnES256, PublicKey: b64u(pub),
		CredentialID: b64u([]byte("cred-1")), RPID: a.rpID, Origin: a.origin,
	}
}

// assertFor produces an assertion over challenge as if the browser ran at
// origin for relying party rpID.
func (a fakeAuthenticator) assertFor(t *testing.T, signerID string, challenge []byte, rpID, origin string) Signature {
	t.Helper()
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x05) // UP | UV
	authData = binary.BigEndian.AppendUint32(authData, 1)
	cd, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": b64u(challenge), "origin": origin})
	if err != nil {
		t.Fatal(err)
	}
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	der, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return Signature{
		Signer: signerID, Alg: TypeWebAuthnES256, Sig: b64u(der),
		WebAuthn: &WebAuthnData{AuthenticatorData: b64u(authData), ClientDataJSON: b64u(cd)},
	}
}

func (a fakeAuthenticator) assert(t *testing.T, signerID string, msg Message) Signature {
	t.Helper()
	d := msg.Digest()
	return a.assertFor(t, signerID, d[:], a.rpID, a.origin)
}

// --- ssh fixtures ---

type sshFixture struct {
	key    string
	signer Signer
}

func newSSHFixture(t *testing.T, id string) sshFixture {
	t.Helper()
	key, pub := genKey(t, "ed25519")
	return sshFixture{key: key, signer: Signer{ID: id, Type: TypeSSHEd25519, PublicKey: pub}}
}

func (f sshFixture) sign(t *testing.T, ns string, text string) Signature {
	t.Helper()
	return Signature{Signer: f.signer.ID, Alg: TypeSSHEd25519, Sig: sshSign(t, f.key, ns, []byte(text))}
}

var fixedOpts = VerifyOptions{Now: testNow}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestVerifySSH(t *testing.T) {
	f := newSSHFixture(t, "alice")
	reg := Registry{Signers: []Signer{f.signer}}
	msg := approvalMsg(testNow)
	sig := f.sign(t, NamespaceApproval, msg.Text())
	if err := Verify(reg, msg, sig, fixedOpts); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Zero Now/MaxAge use the real clock and the 5 minute default.
	live := approvalMsg(time.Now().UTC().Truncate(time.Second))
	if err := Verify(reg, live, f.sign(t, NamespaceApproval, live.Text()), VerifyOptions{}); err != nil {
		t.Fatalf("Verify with default options: %v", err)
	}
}

func TestVerifyWebAuthn(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msg := approvalMsg(testNow)
	d := msg.Digest()
	sig := a.assertFor(t, "phone", d[:], a.rpID, a.origin)
	if err := Verify(reg, msg, sig, fixedOpts); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The challenge is the message digest, not e.g. the text itself.
	bad := a.assertFor(t, "phone", []byte(msg.Text()), a.rpID, a.origin)
	wantErr(t, Verify(reg, msg, bad, fixedOpts), ErrBadSignature)
}

func TestUnknownSigner(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msg := approvalMsg(testNow)
	sig := a.assert(t, "phone", msg)
	sig.Signer = "mallory"
	wantErr(t, Verify(reg, msg, sig, fixedOpts), ErrUnknownSigner)
	wantErr(t, Verify(Registry{}, msg, a.assert(t, "phone", msg), fixedOpts), ErrUnknownSigner)
}

func TestAlgMismatchWithSignerType(t *testing.T) {
	f := newSSHFixture(t, "alice")
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{f.signer, a.signer(t, "phone")}}
	msg := approvalMsg(testNow)

	// ssh signer presented with the webauthn alg (otherwise valid ssh signature).
	sshSig := f.sign(t, NamespaceApproval, msg.Text())
	sshSig.Alg = TypeWebAuthnES256
	sshSig.WebAuthn = a.assert(t, "phone", msg).WebAuthn
	wantErr(t, Verify(reg, msg, sshSig, fixedOpts), ErrBadSignature)

	// webauthn signer presented with the ssh alg (otherwise valid assertion).
	waSig := a.assert(t, "phone", msg)
	waSig.Alg = TypeSSHEd25519
	wantErr(t, Verify(reg, msg, waSig, fixedOpts), ErrBadSignature)

	// Case or spacing variants are not the signer's type.
	for _, alg := range []string{"SSH-ED25519", "ssh-ed25519 ", "", "ed25519"} {
		s := f.sign(t, NamespaceApproval, msg.Text())
		s.Alg = alg
		wantErr(t, Verify(reg, msg, s, fixedOpts), ErrBadSignature)
	}
}

func TestIssuedTooOldAndTooNew(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	cases := []struct {
		name   string
		offset time.Duration
		ok     bool
	}{
		{"exactly 5m old", -5 * time.Minute, true},
		{"5m1s old", -5*time.Minute - time.Second, false},
		{"exactly 5m ahead", 5 * time.Minute, true},
		{"5m1s ahead", 5*time.Minute + time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := approvalMsg(testNow.Add(c.offset))
			err := Verify(reg, msg, a.assert(t, "phone", msg), fixedOpts)
			if c.ok && err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !c.ok {
				wantErr(t, err, ErrStale)
			}
		})
	}
	// A custom MaxAge replaces the default.
	msg := approvalMsg(testNow.Add(-2 * time.Minute))
	wantErr(t, Verify(reg, msg, a.assert(t, "phone", msg), VerifyOptions{Now: testNow, MaxAge: time.Minute}), ErrStale)
}

func TestSkipFreshnessIgnoresIssued(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	for _, issued := range []time.Time{testNow.AddDate(-1, 0, 0), testNow.AddDate(1, 0, 0)} {
		msg := approvalMsg(issued)
		sig := a.assert(t, "phone", msg)
		wantErr(t, Verify(reg, msg, sig, fixedOpts), ErrStale)
		if err := Verify(reg, msg, sig, VerifyOptions{Now: testNow, SkipFreshness: true}); err != nil {
			t.Fatalf("SkipFreshness: %v", err)
		}
	}
}

// SkipFreshness skips only the time window; the signature is still verified.
func TestSkipFreshnessStillVerifiesSignature(t *testing.T) {
	f := newSSHFixture(t, "alice")
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{f.signer, a.signer(t, "phone")}}
	skip := VerifyOptions{Now: testNow, SkipFreshness: true}
	msg := approvalMsg(testNow.AddDate(-1, 0, 0))
	other := msg
	other.Hash = strings.Repeat("c", 16)

	garbageWA := a.assert(t, "phone", msg)
	garbageWA.Sig = b64u([]byte("not a der signature"))
	cases := map[string]Signature{
		"ssh other message":      f.sign(t, NamespaceApproval, other.Text()),
		"ssh garbage":            {Signer: "alice", Alg: TypeSSHEd25519, Sig: "garbage"},
		"webauthn other message": a.assert(t, "phone", other),
		"webauthn garbage":       garbageWA,
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			wantErr(t, Verify(reg, msg, s, skip), ErrBadSignature)
		})
	}
}

// A valid signature by one registered signer is not valid under another's id.
func TestSignatureNotValidForOtherSigner(t *testing.T) {
	alice, bob := newSSHFixture(t, "alice"), newSSHFixture(t, "bob")
	pa, pb := newFakeAuthenticator(t), newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{alice.signer, bob.signer, pa.signer(t, "phone-a"), pb.signer(t, "phone-b")}}
	msg := approvalMsg(testNow)

	sshSig := alice.sign(t, NamespaceApproval, msg.Text())
	if err := Verify(reg, msg, sshSig, fixedOpts); err != nil {
		t.Fatalf("precondition ssh: %v", err)
	}
	sshSig.Signer = "bob"
	wantErr(t, Verify(reg, msg, sshSig, fixedOpts), ErrBadSignature)

	waSig := pa.assert(t, "phone-a", msg)
	if err := Verify(reg, msg, waSig, fixedOpts); err != nil {
		t.Fatalf("precondition webauthn: %v", err)
	}
	waSig.Signer = "phone-b"
	wantErr(t, Verify(reg, msg, waSig, fixedOpts), ErrBadSignature)
}

func TestWebAuthnSignerWithoutRPIDOrOrigin(t *testing.T) {
	a := newFakeAuthenticator(t)
	msg := approvalMsg(testNow)
	sig := a.assert(t, "phone", msg)
	noRP := a.signer(t, "phone")
	noRP.RPID = ""
	noOrigin := a.signer(t, "phone")
	noOrigin.Origin = ""
	for _, s := range []Signer{noRP, noOrigin} {
		wantErr(t, Verify(Registry{Signers: []Signer{s}}, msg, sig, fixedOpts), ErrBadSignature)
	}
	// Assertions made to match the empty values are refused too.
	d := msg.Digest()
	wantErr(t, Verify(Registry{Signers: []Signer{noRP}}, msg, a.assertFor(t, "phone", d[:], "", a.origin), fixedOpts), ErrBadSignature)
	wantErr(t, Verify(Registry{Signers: []Signer{noOrigin}}, msg, a.assertFor(t, "phone", d[:], a.rpID, ""), fixedOpts), ErrBadSignature)
}

func TestInvalidMessageRefusedBeforeAnything(t *testing.T) {
	f := newSSHFixture(t, "alice")
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{f.signer, a.signer(t, "phone")}}

	noHash := approvalMsg(testNow)
	noHash.Hash = ""
	badKind := approvalMsg(testNow)
	badKind.Kind = "anything"
	for _, msg := range []Message{noHash, badKind} {
		if msg.Text() != "" {
			t.Fatal("precondition: invalid message must render as empty text")
		}
		// Signatures that would verify over Text()=="" and Digest()==zero.
		sshSig := f.sign(t, NamespaceApproval, "")
		if err := VerifySSHSig(f.signer.PublicKey, NamespaceApproval, []byte(msg.Text()), sshSig.Sig); err != nil {
			t.Fatalf("precondition: ssh signature over empty text: %v", err)
		}
		zero := [32]byte{}
		waSig := a.assertFor(t, "phone", zero[:], a.rpID, a.origin)
		wantErr(t, Verify(reg, msg, sshSig, fixedOpts), ErrInvalidMessage)
		wantErr(t, Verify(reg, msg, waSig, fixedOpts), ErrInvalidMessage)
		wantErr(t, Verify(reg, msg, waSig, VerifyOptions{SkipFreshness: true}), ErrInvalidMessage)
		// Checked before the signer lookup.
		wantErr(t, Verify(Registry{}, msg, Signature{Signer: "nobody"}, fixedOpts), ErrInvalidMessage)
	}
}

func TestLoginSignatureNotAcceptedAsApproval(t *testing.T) {
	f := newSSHFixture(t, "alice")
	reg := Registry{Signers: []Signer{f.signer}}
	msg := approvalMsg(testNow)
	wantErr(t, Verify(reg, msg, f.sign(t, "aigate-login", msg.Text()), fixedOpts), ErrBadSignature)

	other := msg
	other.Hash = strings.Repeat("c", 16)
	wantErr(t, Verify(reg, msg, f.sign(t, NamespaceApproval, other.Text()), fixedOpts), ErrBadSignature)
	// A login-style text (different prefix) signed in the approval namespace.
	login := strings.Replace(msg.Text(), "aigate-approval-v1", "aigate-login-v1", 1)
	wantErr(t, Verify(reg, msg, f.sign(t, NamespaceApproval, login), fixedOpts), ErrBadSignature)
}

func TestWebAuthnPinnedOriginAndRPID(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msg := approvalMsg(testNow)
	d := msg.Digest()
	wantErr(t, Verify(reg, msg, a.assertFor(t, "phone", d[:], a.rpID, "https://evil.example.test:8443"), fixedOpts), ErrBadSignature)
	wantErr(t, Verify(reg, msg, a.assertFor(t, "phone", d[:], "evil.example.test", a.origin), fixedOpts), ErrBadSignature)
}

func TestWebAuthnWrongChallengeForOtherMessage(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msgA := approvalMsg(testNow)
	msgB := msgA
	msgB.Hash = strings.Repeat("c", 16)
	sigA := a.assert(t, "phone", msgA)
	if err := Verify(reg, msgA, sigA, fixedOpts); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	wantErr(t, Verify(reg, msgB, sigA, fixedOpts), ErrBadSignature)
}

func TestErrorsDoNotEchoInput(t *testing.T) {
	const evil = "ATTACKER<script>zz9"
	f := newSSHFixture(t, "alice")
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{f.signer, a.signer(t, "phone")}}
	msg := approvalMsg(testNow)
	d := msg.Digest()

	sigs := []Signature{
		a.assertFor(t, "phone", d[:], a.rpID, "https://"+evil),
		{Signer: "phone", Alg: TypeWebAuthnES256, Sig: evil, WebAuthn: a.assert(t, "phone", msg).WebAuthn},
		{Signer: "alice", Alg: TypeSSHEd25519, Sig: evil},
		{Signer: "alice", Alg: evil},
	}
	for i, s := range sigs {
		err := Verify(reg, msg, s, fixedOpts)
		wantErr(t, err, ErrBadSignature)
		if strings.Contains(err.Error(), "ATTACKER") || strings.Contains(err.Error(), "zz9") {
			t.Fatalf("case %d: error echoes input: %q", i, err)
		}
	}
	err := Verify(reg, msg, Signature{Signer: evil}, fixedOpts)
	wantErr(t, err, ErrUnknownSigner)
	if strings.Contains(err.Error(), "zz9") {
		t.Fatalf("unknown signer error echoes input: %q", err)
	}
	bad := msg
	bad.Kind = evil
	err = Verify(reg, bad, Signature{}, fixedOpts)
	wantErr(t, err, ErrInvalidMessage)
	if strings.Contains(err.Error(), "zz9") {
		t.Fatalf("invalid message error echoes input: %q", err)
	}
}

func TestNilWebAuthnData(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msg := approvalMsg(testNow)
	sig := a.assert(t, "phone", msg)
	sig.WebAuthn = nil
	wantErr(t, Verify(reg, msg, sig, fixedOpts), ErrBadSignature)
}

func TestMalformedBase64Variants(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msg := approvalMsg(testNow)

	// Find an assertion whose signature uses a URL-only character and whose
	// length is not a multiple of 3, so the standard-alphabet and padded
	// variants really differ from the canonical form.
	var sig Signature
	var der []byte
	for i := 0; ; i++ {
		sig = a.assert(t, "phone", msg)
		der, _ = base64.RawURLEncoding.DecodeString(sig.Sig)
		if strings.ContainsAny(sig.Sig, "-_") && len(der)%3 != 0 {
			break
		}
		if i > 200 {
			t.Fatal("could not produce a suitable signature")
		}
	}
	if err := Verify(reg, msg, sig, fixedOpts); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	auth, _ := base64.RawURLEncoding.DecodeString(sig.WebAuthn.AuthenticatorData)

	type field int
	const (
		fSig field = iota
		fAuth
		fClient
	)
	mutate := func(f field, v string) Signature {
		s := sig
		w := *sig.WebAuthn
		s.WebAuthn = &w
		switch f {
		case fSig:
			s.Sig = v
		case fAuth:
			w.AuthenticatorData = v
		case fClient:
			w.ClientDataJSON = v
		}
		return s
	}
	mid := func(s, ins string) string { return s[:len(s)/2] + ins + s[len(s)/2:] }
	cases := map[string]Signature{
		"sig padded":        mutate(fSig, base64.URLEncoding.EncodeToString(der)),
		"sig std alphabet":  mutate(fSig, base64.RawStdEncoding.EncodeToString(der)),
		"sig LF":            mutate(fSig, mid(sig.Sig, "\n")),
		"sig CRLF":          mutate(fSig, mid(sig.Sig, "\r\n")),
		"sig trailing LF":   mutate(fSig, sig.Sig+"\n"),
		"authdata padded":   mutate(fAuth, base64.URLEncoding.EncodeToString(auth)),
		"authdata CR":       mutate(fAuth, mid(sig.WebAuthn.AuthenticatorData, "\r")),
		"clientdata LF":     mutate(fClient, mid(sig.WebAuthn.ClientDataJSON, "\n")),
		"clientdata padded": mutate(fClient, sig.WebAuthn.ClientDataJSON+"=="),
		"sig empty":         mutate(fSig, ""),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			wantErr(t, Verify(reg, msg, s, fixedOpts), ErrBadSignature)
		})
	}
}

func TestWebAuthnSignerWithBadKey(t *testing.T) {
	a := newFakeAuthenticator(t)
	s := a.signer(t, "phone")
	msg := approvalMsg(testNow)
	sig := a.assert(t, "phone", msg)
	for _, pk := range []string{"", "AAAA", b64u(make([]byte, 65))} {
		s.PublicKey = pk
		wantErr(t, Verify(Registry{Signers: []Signer{s}}, msg, sig, fixedOpts), ErrBadSignature)
	}
}

// The wire form of a signature (API request bodies, CLI) is pinned: renaming a
// field silently drops it on decode.
func TestSignatureJSONShape(t *testing.T) {
	ssh := Signature{Signer: "alice", Alg: TypeSSHEd25519, Sig: "S"}
	b, err := json.Marshal(ssh)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"signer":"alice","alg":"ssh-ed25519","sig":"S"}`; got != want {
		t.Fatalf("ssh shape\n got %s\nwant %s", got, want)
	}
	wa := Signature{Signer: "phone", Alg: TypeWebAuthnES256, Sig: "D",
		WebAuthn: &WebAuthnData{AuthenticatorData: "A", ClientDataJSON: "C"}}
	b, err = json.Marshal(wa)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"signer":"phone","alg":"webauthn-es256","sig":"D","webauthn":{"authenticator_data":"A","client_data_json":"C"}}`
	if string(b) != want {
		t.Fatalf("webauthn shape\n got %s\nwant %s", b, want)
	}
	var back Signature
	if err := json.Unmarshal([]byte(want), &back); err != nil || back.WebAuthn == nil ||
		back.WebAuthn.ClientDataJSON != "C" || back.Signer != "phone" || back.Sig != "D" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}
