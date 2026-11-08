package approval

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
)

// Helpers below are small copies of the signing package's test helpers (test
// helpers are not imported across packages). None of these tests may use
// t.Parallel: they change the process-wide registry path and options.

var tNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

const (
	tRepo = "0123456789ab"
	tTask = 7
)

var (
	tBase  = strings.Repeat("a", 40)
	tHashA = strings.Repeat("b", 16)
	tHashB = strings.Repeat("c", 16)
)

func msgFor(repo, base, hash string) signing.Message {
	return signing.Message{Kind: signing.KindProtectedModified, Repo: repo, Task: tTask, Base: base, Hash: hash, Issued: tNow}
}

func sshKeygen(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen이 없습니다")
	}
	return p
}

type sshSigner struct {
	id, key string
	signer  signing.Signer
}

func newSSHSigner(t *testing.T, id string) sshSigner {
	t.Helper()
	bin := sshKeygen(t)
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return sshSigner{id: id, key: key, signer: signing.Signer{
		ID: id, Type: signing.TypeSSHEd25519, Label: "L", PublicKey: strings.TrimSpace(string(pub)), AddedAt: tNow,
	}}
}

func (s sshSigner) sign(t *testing.T, msg signing.Message) signing.Signature {
	t.Helper()
	if err := msg.Validate(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sshKeygen(t), "-Y", "sign", "-f", s.key, "-n", signing.NamespaceApproval)
	cmd.Stdin = strings.NewReader(msg.Text())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v: %s", err, stderr.String())
	}
	return signing.Signature{Signer: s.id, Alg: signing.TypeSSHEd25519, Sig: stdout.String()}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type fakeAuthenticator struct {
	key          *ecdsa.PrivateKey
	rpID, origin string
}

func newFakeAuthenticator(t *testing.T) fakeAuthenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return fakeAuthenticator{key: k, rpID: "console.example.test", origin: "https://console.example.test:8443"}
}

func (a fakeAuthenticator) signer(t *testing.T, id string) signing.Signer {
	t.Helper()
	pub, err := a.key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return signing.Signer{ID: id, Type: signing.TypeWebAuthnES256, Label: "phone", PublicKey: b64u(pub),
		CredentialID: b64u([]byte("cred-1")), RPID: a.rpID, Origin: a.origin, AddedAt: tNow}
}

func (a fakeAuthenticator) assert(t *testing.T, signerID string, msg signing.Message) signing.Signature {
	t.Helper()
	challenge := msg.Digest()
	rpHash := sha256.Sum256([]byte(a.rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x05) // UP | UV
	authData = binary.BigEndian.AppendUint32(authData, 1)
	cd, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": b64u(challenge[:]), "origin": a.origin})
	if err != nil {
		t.Fatal(err)
	}
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	der, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing.Signature{Signer: signerID, Alg: signing.TypeWebAuthnES256, Sig: b64u(der),
		WebAuthn: &signing.WebAuthnData{AuthenticatorData: b64u(authData), ClientDataJSON: b64u(cd)}}
}

// useRegistry points the process-wide registry at a trusted-looking temp
// layout (dir 0755, file 0644 when signers != nil) owned by the current user,
// trusts the current uid, and restores both globals on cleanup. It returns the
// registry file path. signers == nil leaves the file absent (legacy mode).
func useRegistry(t *testing.T, signers []signing.Signer) string {
	t.Helper()
	oldPath, oldOpts := signing.RegistryPath(), signing.DefaultOptions
	t.Cleanup(func() {
		signing.SetRegistryPath(oldPath)
		signing.DefaultOptions = oldOpts
	})
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "signers.json")
	if signers != nil {
		writeRegistry(t, p, signers)
	}
	signing.SetRegistryPath(p)
	signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }}
	return p
}

func writeRegistry(t *testing.T, p string, signers []signing.Signer) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "signers": signers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
}

var verifyOpts = signing.VerifyOptions{}

// rawFile is the on-disk shape, decoded generically so tests can tamper with it.
type rawFile struct {
	Version   int              `json:"version"`
	Approvals []map[string]any `json:"approvals"`
}

func readRaw(t *testing.T, s Store) rawFile {
	t.Helper()
	b, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	var f rawFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeRaw(t *testing.T, s Store, f rawFile) {
	t.Helper()
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// signedSetup enables signed mode with one SSH signer and records a valid
// signed approval for (tRepo, tBase, tHashA).
func signedSetup(t *testing.T) (Store, sshSigner, signing.Message, signing.Signature) {
	t.Helper()
	alice := newSSHSigner(t, "alice")
	useRegistry(t, []signing.Signer{alice.signer})
	s := newStore(t)
	msg := msgFor(tRepo, tBase, tHashA)
	sig := alice.sign(t, msg)
	if err := s.GrantSigned(msg, sig, tNow, verifyOpts); err != nil {
		t.Fatal(err)
	}
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("signed approval must be valid")
	}
	return s, alice, msg, sig
}

func TestLegacyModeUnchanged(t *testing.T) {
	useRegistry(t, nil) // trusted dir, no registry file: legacy mode
	s := newStore(t)
	if err := s.Grant("r", "b", "h", t0); err != nil {
		t.Fatal(err)
	}
	if !s.Valid("r", "b", "h") || s.Valid("r", "b", "x") {
		t.Fatal("legacy triple matching changed")
	}
	data, _ := os.ReadFile(s.Path)
	want := "{\n  \"version\": 1,\n  \"approvals\": [\n    {\n      \"repo\": \"r\",\n      \"base\": \"b\",\n" +
		"      \"hash\": \"h\",\n      \"granted_at\": \"2026-11-04T01:02:03Z\"\n    }\n  ]\n}\n"
	if string(data) != want {
		t.Fatalf("legacy file format changed:\n%s", data)
	}
	// An entry carrying (even bogus) signature fields is still matched by its
	// plain triple when no registry exists.
	f := readRaw(t, s)
	f.Approvals = append(f.Approvals, map[string]any{"repo": "r2", "base": "b", "hash": "h", "granted_at": "x",
		"message": "garbage", "signer": "nobody", "alg": "ssh-ed25519", "sig": "nope"})
	writeRaw(t, s, f)
	if !s.Valid("r2", "b", "h") {
		t.Fatal("legacy mode must ignore signature fields")
	}
	// Signed grants need a registry to verify against.
	if err := s.GrantSigned(msgFor(tRepo, tBase, tHashA), signing.Signature{Signer: "a", Alg: signing.TypeSSHEd25519, Sig: "x"}, tNow, verifyOpts); err == nil {
		t.Fatal("GrantSigned without a registry must fail")
	}
}

func TestSignedModeRejectsUnsignedEntry(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	p := useRegistry(t, nil)
	s := newStore(t)
	if err := s.Grant(tRepo, tBase, tHashA, t0); err != nil {
		t.Fatal(err)
	}
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("legacy grant should be valid before the registry exists")
	}
	writeRegistry(t, p, []signing.Signer{alice.signer})
	if s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("unsigned entry must be invalid in signed mode")
	}
	// A hand-added entry with an empty signature is also unsigned.
	f := readRaw(t, s)
	msg := msgFor(tRepo, tBase, tHashB)
	f.Approvals = append(f.Approvals, map[string]any{"repo": tRepo, "base": tBase, "hash": tHashB, "granted_at": "x",
		"message": msg.Text(), "signer": "alice", "alg": signing.TypeSSHEd25519})
	writeRaw(t, s, f)
	if s.Valid(tRepo, tBase, tHashB) {
		t.Fatal("entry without sig must be invalid")
	}
}

func TestSignedModeAcceptsValidEntry(t *testing.T) {
	t.Run("ssh", func(t *testing.T) {
		s, _, msg, sig := signedSetup(t)
		f := readRaw(t, s)
		if f.Version != 1 || len(f.Approvals) != 1 {
			t.Fatalf("file: %+v", f)
		}
		e := f.Approvals[0]
		if e["repo"] != tRepo || e["base"] != tBase || e["hash"] != tHashA || e["message"] != msg.Text() ||
			e["signer"] != "alice" || e["alg"] != signing.TypeSSHEd25519 || e["sig"] != sig.Sig || e["granted_at"] != "2026-11-09T12:00:00Z" {
			t.Fatalf("entry: %+v", e)
		}
		if _, ok := e["webauthn"]; ok {
			t.Fatal("ssh entry must omit webauthn")
		}
		if s.Valid(tRepo, tBase, tHashB) {
			t.Fatal("other hash must not be valid")
		}
	})
	t.Run("webauthn", func(t *testing.T) {
		a := newFakeAuthenticator(t)
		useRegistry(t, []signing.Signer{a.signer(t, "phone")})
		s := newStore(t)
		msg := msgFor(tRepo, tBase, tHashA)
		if err := s.GrantSigned(msg, a.assert(t, "phone", msg), tNow, verifyOpts); err != nil {
			t.Fatal(err)
		}
		if !s.Valid(tRepo, tBase, tHashA) {
			t.Fatal("webauthn approval must be valid")
		}
		w, ok := readRaw(t, s).Approvals[0]["webauthn"].(map[string]any)
		if !ok || w["authenticator_data"] == "" || w["client_data_json"] == "" || len(w) != 2 {
			t.Fatalf("webauthn object: %#v", w)
		}
	})
}

// tamper rewrites approvals[0][key] in the approvals file.
func tamper(t *testing.T, s Store, key, val string) {
	t.Helper()
	f := readRaw(t, s)
	f.Approvals[0][key] = val
	writeRaw(t, s, f)
}

func TestSignedModeRejectsTamperedHash(t *testing.T) {
	s, _, msg, _ := signedSetup(t)
	tamper(t, s, "hash", tHashB)
	if s.Valid(tRepo, tBase, tHashB) || s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("entry with edited hash must not validate either triple")
	}
	// Editing the message's hash line too breaks the signature.
	tamper(t, s, "message", strings.Replace(msg.Text(), "hash: "+tHashA, "hash: "+tHashB, 1))
	if s.Valid(tRepo, tBase, tHashB) {
		t.Fatal("re-pointed message must not verify")
	}
}

func TestSignedModeRejectsTamperedBaseOrRepo(t *testing.T) {
	otherBase := strings.Repeat("d", 40)
	otherRepo := "ba9876543210"
	for _, c := range []struct{ key, val, line, repo, base string }{
		{"base", otherBase, "base: ", tRepo, otherBase},
		{"repo", otherRepo, "repo: ", otherRepo, tBase},
	} {
		t.Run(c.key, func(t *testing.T) {
			s, _, msg, _ := signedSetup(t)
			tamper(t, s, c.key, c.val)
			if s.Valid(c.repo, c.base, tHashA) || s.Valid(tRepo, tBase, tHashA) {
				t.Fatalf("edited %s must invalidate the entry", c.key)
			}
			orig := map[string]string{"base": tBase, "repo": tRepo}[c.key]
			tamper(t, s, "message", strings.Replace(msg.Text(), c.line+orig, c.line+c.val, 1))
			if s.Valid(c.repo, c.base, tHashA) {
				t.Fatalf("re-pointed %s in message must not verify", c.key)
			}
		})
	}
}

func TestSignedModeRejectsEditedMessage(t *testing.T) {
	s, _, msg, _ := signedSetup(t)
	edits := map[string]string{
		"task":          strings.Replace(msg.Text(), "task: 7", "task: 8", 1),
		"issued":        strings.Replace(msg.Text(), "12:00:00Z", "12:00:01Z", 1),
		"kind":          strings.Replace(msg.Text(), signing.KindProtectedModified, signing.KindPublishOutsideAllowlist, 1),
		"non-canonical": strings.Replace(msg.Text(), "task: 7", "task: 07", 1),
		"no-newline":    strings.TrimSuffix(msg.Text(), "\n"),
		"garbage":       "hello",
		"empty":         "",
	}
	for name, text := range edits {
		t.Run(name, func(t *testing.T) {
			tamper(t, s, "message", text)
			if s.Valid(tRepo, tBase, tHashA) {
				t.Fatalf("edited message (%s) must not validate", name)
			}
		})
	}
	tamper(t, s, "message", msg.Text())
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("restoring the message should restore validity")
	}
	tamper(t, s, "signer", "mallory")
	if s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("unknown signer must not validate")
	}
}

// Review point 1: a signature over item A must not approve item B.
func TestSignatureForItemANotValidForItemB(t *testing.T) {
	s, _, msgA, sigA := signedSetup(t)
	msgB := msgFor(tRepo, tBase, tHashB)
	entryB := func(message string) map[string]any {
		return map[string]any{"repo": tRepo, "base": tBase, "hash": tHashB, "granted_at": "x",
			"message": message, "signer": sigA.Signer, "alg": sigA.Alg, "sig": sigA.Sig}
	}
	for name, message := range map[string]string{
		"A's message and signature":  msgA.Text(),
		"B's message, A's signature": msgB.Text(),
	} {
		t.Run(name, func(t *testing.T) {
			f := readRaw(t, s)
			f.Approvals = []map[string]any{f.Approvals[0], entryB(message)}
			writeRaw(t, s, f)
			if s.Valid(tRepo, tBase, tHashB) {
				t.Fatal("A's signature must not approve B")
			}
			if !s.Valid(tRepo, tBase, tHashA) {
				t.Fatal("A must stay valid")
			}
		})
	}
	// GrantSigned refuses the same reuse.
	if err := s.GrantSigned(msgB, sigA, tNow, verifyOpts); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("GrantSigned with A's signature for B: %v", err)
	}
}

// Review point 3: a registry that exists but fails the trust checks disables
// every signed approval and every grant.
func TestLoosePermsRegistryFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name string
		mess func(p string) error
	}{
		{"file 0666", func(p string) error { return os.Chmod(p, 0o666) }},
		{"dir 0777", func(p string) error { return os.Chmod(filepath.Dir(p), 0o777) }},
		{"untrusted owner", func(string) error {
			signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uint32) bool { return false }}
			return nil
		}},
		{"corrupt", func(p string) error { return os.WriteFile(p, []byte("{"), 0o644) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, alice, msg, sig := signedSetup(t)
			if err := c.mess(signing.RegistryPath()); err != nil {
				t.Fatal(err)
			}
			if s.Valid(tRepo, tBase, tHashA) {
				t.Fatal("Valid must fail closed")
			}
			if err := s.GrantSigned(msg, sig, tNow, verifyOpts); err == nil {
				t.Fatal("GrantSigned must fail")
			}
			msgB := msgFor(tRepo, tBase, tHashB)
			if err := s.GrantSigned(msgB, alice.sign(t, msgB), tNow, verifyOpts); err == nil {
				t.Fatal("GrantSigned must fail")
			}
			if err := s.Grant(tRepo, tBase, tHashB, tNow); err == nil {
				t.Fatal("Grant must fail")
			}
			if len(readRaw(t, s).Approvals) != 1 {
				t.Fatal("file must be untouched")
			}
		})
	}
}

func TestGrantRefusedInSignedMode(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	useRegistry(t, []signing.Signer{alice.signer})
	s := newStore(t)
	if err := s.Grant(tRepo, tBase, tHashA, t0); !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("Grant in signed mode: %v", err)
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatal("file must not be created")
	}
	if s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("must not be valid")
	}
}

func TestGrantSignedIdempotentAndConcurrent(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	p := useRegistry(t, nil)
	s := newStore(t)
	// A legacy entry for the triple is upgraded in place, not duplicated.
	if err := s.Grant(tRepo, tBase, tHashA, t0); err != nil {
		t.Fatal(err)
	}
	writeRegistry(t, p, []signing.Signer{alice.signer})

	msg := msgFor(tRepo, tBase, tHashA)
	later := msg
	later.Issued = tNow.Add(time.Minute)
	sigs := []signing.Signature{alice.sign(t, msg), alice.sign(t, later)}
	msgs := []signing.Message{msg, later}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.GrantSigned(msgs[i], sigs[i], tNow, verifyOpts); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	// Same triple again with a fresh signature: still one entry (keyed by the
	// triple, never by the signature).
	if err := s.GrantSigned(msg, alice.sign(t, msg), tNow, verifyOpts); err != nil {
		t.Fatal(err)
	}
	f := readRaw(t, s)
	if len(f.Approvals) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(f.Approvals), f.Approvals)
	}
	if f.Approvals[0]["sig"] == "" || f.Approvals[0]["message"] == nil {
		t.Fatalf("entry not signed: %+v", f.Approvals[0])
	}
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("must be valid")
	}

	// Freshness is enforced against the passed now, and errors are sentinels.
	msgB := msgFor(tRepo, tBase, tHashB)
	sigB := alice.sign(t, msgB)
	if err := s.GrantSigned(msgB, sigB, tNow.Add(10*time.Minute), verifyOpts); !errors.Is(err, signing.ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	if err := s.GrantSigned(msgB, sigB, tNow.Add(10*time.Minute), signing.VerifyOptions{SkipFreshness: true}); !errors.Is(err, signing.ErrStale) {
		t.Fatalf("GrantSigned must not let callers skip freshness: %v", err)
	}
	if err := s.GrantSigned(msgB, sigB, tNow.Add(10*time.Minute), signing.VerifyOptions{MaxAge: time.Hour}); err != nil {
		t.Fatalf("MaxAge option ignored: %v", err)
	}
	if err := s.GrantSigned(signing.Message{}, sigB, tNow, verifyOpts); !errors.Is(err, signing.ErrInvalidMessage) {
		t.Fatalf("invalid message: %v", err)
	}
	if len(readRaw(t, s).Approvals) != 2 {
		t.Fatal("want 2 entries")
	}
}
