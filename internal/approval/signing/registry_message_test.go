package signing

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func regMsg(issued time.Time) RegistryMessage {
	return RegistryMessage{
		Action:  RegistryActionRegisterProject,
		Subject: "my-proj",
		Change:  strings.Repeat("c", 64),
		Request: strings.Repeat("1", 32),
		Issued:  issued,
	}
}

func (a fakeAuthenticator) assertRegistry(t *testing.T, signerID string, msg RegistryMessage) Signature {
	t.Helper()
	d := msg.Digest()
	return a.assertFor(t, signerID, d[:], a.rpID, a.origin)
}

func TestRegistryMessageTextRoundTrip(t *testing.T) {
	m := regMsg(testNow)
	want := "aigate-registry-v1\naction: register_project\nsubject: my-proj\nchange: " +
		strings.Repeat("c", 64) + "\nrequest: " + strings.Repeat("1", 32) + "\nissued: 2026-11-09T12:00:00Z\n"
	if m.Text() != want {
		t.Fatalf("Text = %q, want %q", m.Text(), want)
	}
	got, err := ParseRegistryMessage(m.Text())
	if err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Fatalf("round trip: %+v != %+v", got, m)
	}
	m.Action = RegistryActionSetFlag
	if got, err := ParseRegistryMessage(m.Text()); err != nil || got != m {
		t.Fatalf("set_flag round trip: %+v %v", got, err)
	}
	if m.Digest() == ([32]byte{}) {
		t.Fatal("digest of a valid message must not be zero")
	}
}

func TestRegistryMessageRejectsBadFields(t *testing.T) {
	cases := map[string]func(*RegistryMessage){
		"action unknown":     func(m *RegistryMessage) { m.Action = "delete_project" },
		"action empty":       func(m *RegistryMessage) { m.Action = "" },
		"subject uppercase":  func(m *RegistryMessage) { m.Subject = "Proj" },
		"subject leading -":  func(m *RegistryMessage) { m.Subject = "-proj" },
		"subject newline":    func(m *RegistryMessage) { m.Subject = "a\nb" },
		"subject too long":   func(m *RegistryMessage) { m.Subject = "a" + strings.Repeat("b", 40) },
		"subject empty":      func(m *RegistryMessage) { m.Subject = "" },
		"change short":       func(m *RegistryMessage) { m.Change = strings.Repeat("c", 63) },
		"change uppercase":   func(m *RegistryMessage) { m.Change = strings.Repeat("C", 64) },
		"change non hex":     func(m *RegistryMessage) { m.Change = strings.Repeat("g", 64) },
		"request short":      func(m *RegistryMessage) { m.Request = strings.Repeat("1", 31) },
		"request 64":         func(m *RegistryMessage) { m.Request = strings.Repeat("1", 64) },
		"request uppercase":  func(m *RegistryMessage) { m.Request = strings.Repeat("A", 32) },
		"issued zero":        func(m *RegistryMessage) { m.Issued = time.Time{} },
		"issued nanoseconds": func(m *RegistryMessage) { m.Issued = testNow.Add(time.Nanosecond) },
		"issued non UTC": func(m *RegistryMessage) {
			m.Issued = testNow.In(time.FixedZone("KST", 9*3600))
		},
	}
	for name, mut := range cases {
		m := regMsg(testNow)
		mut(&m)
		if m.Validate() == nil {
			t.Errorf("%s: Validate accepted", name)
		}
		if m.Text() != "" {
			t.Errorf("%s: Text not empty", name)
		}
		if m.Digest() != ([32]byte{}) {
			t.Errorf("%s: Digest not zero", name)
		}
	}
	// Strict parsing: only byte-exact canonical text.
	good := regMsg(testNow).Text()
	for name, text := range map[string]string{
		"no trailing newline": strings.TrimSuffix(good, "\n"),
		"extra trailing line": good + "\n",
		"crlf":                strings.ReplaceAll(good, "\n", "\r\n"),
		"swapped lines":       strings.Replace(good, "action: register_project\nsubject: my-proj", "subject: my-proj\naction: register_project", 1),
		"offset time":         strings.Replace(good, "12:00:00Z", "21:00:00+09:00", 1),
		"extra space":         strings.Replace(good, "subject: my-proj", "subject:  my-proj", 1),
		"empty":               "",
	} {
		if _, err := ParseRegistryMessage(text); err == nil {
			t.Errorf("%s: parse accepted", name)
		}
	}
}

func TestApprovalAndRegistryMessagesRefuseEachOther(t *testing.T) {
	if _, err := ParseMessage(regMsg(testNow).Text()); err == nil {
		t.Fatal("ParseMessage accepted a registry message")
	}
	if _, err := ParseRegistryMessage(approvalMsg(testNow).Text()); err == nil {
		t.Fatal("ParseRegistryMessage accepted an approval message")
	}
}

func TestVerifyRegistrySSH(t *testing.T) {
	f := newSSHFixture(t, "alice")
	reg := Registry{Signers: []Signer{f.signer}}
	msg := regMsg(testNow)
	if err := VerifyRegistry(reg, msg, f.sign(t, NamespaceRegistry, msg.Text()), fixedOpts); err != nil {
		t.Fatalf("VerifyRegistry: %v", err)
	}
	wantErr(t, VerifyRegistry(reg, msg, f.sign(t, NamespaceApproval, msg.Text()), fixedOpts), ErrBadSignature)
	wantErr(t, VerifyRegistry(reg, msg, f.sign(t, NamespaceLogin, msg.Text()), fixedOpts), ErrBadSignature)
	other := msg
	other.Subject = "other"
	wantErr(t, VerifyRegistry(reg, msg, f.sign(t, NamespaceRegistry, other.Text()), fixedOpts), ErrBadSignature)
	// A registry-namespace signature is no approval signature.
	am := approvalMsg(testNow)
	wantErr(t, Verify(reg, am, f.sign(t, NamespaceRegistry, am.Text()), fixedOpts), ErrBadSignature)
	// Invalid message is refused before anything else.
	bad := msg
	bad.Change = ""
	wantErr(t, VerifyRegistry(Registry{}, bad, Signature{Signer: "nobody"}, fixedOpts), ErrInvalidMessage)
}

func TestVerifyRegistryWebAuthn(t *testing.T) {
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{a.signer(t, "phone")}}
	msg := regMsg(testNow)
	if err := VerifyRegistry(reg, msg, a.assertRegistry(t, "phone", msg), fixedOpts); err != nil {
		t.Fatalf("VerifyRegistry: %v", err)
	}
	// Challenge for a different message.
	other := msg
	other.Request = strings.Repeat("2", 32)
	wantErr(t, VerifyRegistry(reg, msg, a.assertRegistry(t, "phone", other), fixedOpts), ErrBadSignature)
	// Challenge is the digest of the approval-style text, not Digest().
	text := msg.Text()
	raw := sha256Sum([]byte(text + "x"))
	wantErr(t, VerifyRegistry(reg, msg, a.assertFor(t, "phone", raw[:], a.rpID, a.origin), fixedOpts), ErrBadSignature)
	// Wrong origin / missing webauthn data.
	d := msg.Digest()
	wantErr(t, VerifyRegistry(reg, msg, a.assertFor(t, "phone", d[:], a.rpID, "https://evil.example"), fixedOpts), ErrBadSignature)
	sig := a.assertRegistry(t, "phone", msg)
	sig.WebAuthn = nil
	wantErr(t, VerifyRegistry(reg, msg, sig, fixedOpts), ErrBadSignature)
}

func TestVerifyRegistryFreshness(t *testing.T) {
	f := newSSHFixture(t, "alice")
	reg := Registry{Signers: []Signer{f.signer}}
	for _, tc := range []struct {
		off time.Duration
		ok  bool
	}{
		{0, true}, {5 * time.Minute, true}, {-5 * time.Minute, true},
		{5*time.Minute + time.Second, false}, {-5*time.Minute - time.Second, false},
	} {
		msg := regMsg(testNow.Add(tc.off))
		err := VerifyRegistry(reg, msg, f.sign(t, NamespaceRegistry, msg.Text()), fixedOpts)
		if tc.ok && err != nil {
			t.Errorf("offset %v: %v", tc.off, err)
		}
		if !tc.ok {
			wantErr(t, err, ErrStale)
		}
	}
	// There is no SkipFreshness escape hatch for registry changes.
	old := regMsg(testNow.Add(-time.Hour))
	wantErr(t, VerifyRegistry(reg, old, f.sign(t, NamespaceRegistry, old.Text()), VerifyOptions{Now: testNow, SkipFreshness: true}), ErrStale)
	// Zero options use the real clock and the 5 minute default.
	live := regMsg(time.Now().UTC().Truncate(time.Second))
	if err := VerifyRegistry(reg, live, f.sign(t, NamespaceRegistry, live.Text()), VerifyOptions{}); err != nil {
		t.Fatalf("default options: %v", err)
	}
}

func TestVerifyRegistryUnknownSignerAndAlgMismatch(t *testing.T) {
	f := newSSHFixture(t, "alice")
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{f.signer, a.signer(t, "phone")}}
	msg := regMsg(testNow)
	sig := f.sign(t, NamespaceRegistry, msg.Text())
	sig.Signer = "mallory"
	wantErr(t, VerifyRegistry(reg, msg, sig, fixedOpts), ErrUnknownSigner)

	sig = f.sign(t, NamespaceRegistry, msg.Text())
	sig.Alg = TypeWebAuthnES256
	wantErr(t, VerifyRegistry(reg, msg, sig, fixedOpts), ErrBadSignature)

	sig = a.assertRegistry(t, "phone", msg)
	sig.Alg = TypeSSHEd25519
	wantErr(t, VerifyRegistry(reg, msg, sig, fixedOpts), ErrBadSignature)

	// An ssh signature presented under a webauthn signer id.
	sig = f.sign(t, NamespaceRegistry, msg.Text())
	sig.Signer = "phone"
	wantErr(t, VerifyRegistry(reg, msg, sig, fixedOpts), ErrBadSignature)
}

// TestExistingVerifyUnchanged pins the public behaviour of Verify next to the
// registry verifier; the full existing suite in verify_test.go must also pass.
func TestExistingVerifyUnchanged(t *testing.T) {
	f := newSSHFixture(t, "alice")
	a := newFakeAuthenticator(t)
	reg := Registry{Signers: []Signer{f.signer, a.signer(t, "phone")}}
	am := approvalMsg(testNow)
	if err := Verify(reg, am, f.sign(t, NamespaceApproval, am.Text()), fixedOpts); err != nil {
		t.Fatal(err)
	}
	if err := Verify(reg, am, a.assert(t, "phone", am), fixedOpts); err != nil {
		t.Fatal(err)
	}
	wantErr(t, Verify(reg, am, f.sign(t, NamespaceLogin, am.Text()), fixedOpts), ErrBadSignature)
	// Registry-namespace and registry-challenge signatures never approve.
	rm := regMsg(testNow)
	wantErr(t, Verify(reg, am, f.sign(t, NamespaceRegistry, am.Text()), fixedOpts), ErrBadSignature)
	wantErr(t, Verify(reg, am, a.assertRegistry(t, "phone", rm), fixedOpts), ErrBadSignature)
	stale := approvalMsg(testNow.Add(-time.Hour))
	wantErr(t, Verify(reg, stale, f.sign(t, NamespaceApproval, stale.Text()), fixedOpts), ErrStale)
	if err := Verify(reg, stale, f.sign(t, NamespaceApproval, stale.Text()), VerifyOptions{SkipFreshness: true}); err != nil {
		t.Fatal(err)
	}
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
