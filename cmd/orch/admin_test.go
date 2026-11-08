package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"aigate/internal/approval/signing"
)

func fakeEUID(t *testing.T, uid int) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = old })
}

// adminDir is a registry directory the test user owns (the tests cannot be
// root; LoadRegistry is told to trust the test user's uid).
func adminDir(t *testing.T) (dir, reg string) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	dir = filepath.Join(root, "aigate")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "signers.json")
}

func selfOwned() signing.RegistryOptions {
	me := uint32(os.Geteuid())
	return signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == me }}
}

// edPub writes an ssh-ed25519 public key file and returns its path and key.
func edPub(t *testing.T, comment string) (string, ssh.PublicKey) {
	t.Helper()
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ssh.NewPublicKey(pk)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment + "\n"
	p := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, pub
}

func admin(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	code := run(append([]string{"admin", "signers"}, args...), &o, &e)
	return code, o.String(), e.String()
}

func tmpLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var left []string
	for _, e := range ents {
		if e.Name() != "signers.json" {
			left = append(left, e.Name())
		}
	}
	return left
}

func TestSignersAddRequiresRoot(t *testing.T) {
	fakeEUID(t, 501)
	dir, reg := adminDir(t)
	key, _ := edPub(t, "me@mac")
	code, _, errS := admin("add", "--signers-file", reg, "--id", "mac", "--key", key)
	if code == 0 || !strings.Contains(errS, "root") {
		t.Fatalf("code %d err %q", code, errS)
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatalf("registry touched: %v", err)
	}
	if left := tmpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
	// Pending activation is a write too.
	if code, _, errS = admin("add", "--signers-file", reg, "--pending", "p1", "--fingerprint", "SHA256:x"); code == 0 || !strings.Contains(errS, "root") {
		t.Fatalf("pending add: code %d err %q", code, errS)
	}
}

func TestSignersAddWritesAtomicallyAndIdempotent(t *testing.T) {
	fakeEUID(t, 0)
	dir, reg := adminDir(t)
	key, pub := edPub(t, "me@mac")
	code, out, errS := admin("add", "--signers-file", reg, "--id", "mac", "--label", "Mac SSH", "--key", key)
	if code != 0 {
		t.Fatalf("add %d %q %q", code, out, errS)
	}
	if !strings.Contains(out, ssh.FingerprintSHA256(pub)) {
		t.Errorf("fingerprint not shown: %q", out)
	}
	fi, err := os.Stat(reg)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %04o", fi.Mode().Perm())
	}
	if left := tmpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	r, err := signing.LoadRegistry(reg, selfOwned())
	if err != nil {
		t.Fatalf("LoadRegistry rejects what admin wrote: %v", err)
	}
	s, ok := r.Find("mac")
	if !ok || s.Type != signing.TypeSSHEd25519 || s.Label != "Mac SSH" || s.AddedAt.IsZero() ||
		s.PublicKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) {
		t.Fatalf("signer %+v", s)
	}
	before, _ := os.ReadFile(reg)
	ino := fi.Sys().(*syscall.Stat_t).Ino

	// Same id, same key: success, file untouched.
	if code, out, errS = admin("add", "--signers-file", reg, "--id", "mac", "--key", key); code != 0 || !strings.Contains(out, "변경 없음") {
		t.Fatalf("repeat add %d %q %q", code, out, errS)
	}
	if after, _ := os.ReadFile(reg); !bytes.Equal(before, after) {
		t.Fatal("idempotent add rewrote the file")
	}
	// Same id, different key: refused, file untouched.
	other, _ := edPub(t, "other")
	if code, _, errS = admin("add", "--signers-file", reg, "--id", "mac", "--key", other); code == 0 || !strings.Contains(errS, "mac") {
		t.Fatalf("conflicting add %d %q", code, errS)
	}
	if after, _ := os.ReadFile(reg); !bytes.Equal(before, after) {
		t.Fatal("refused add changed the file")
	}
	// A not-ed25519 / garbage key is refused.
	bad := filepath.Join(t.TempDir(), "bad.pub")
	os.WriteFile(bad, []byte("ssh-rsa AAAA garbage\n"), 0o644)
	if code, _, _ = admin("add", "--signers-file", reg, "--id", "bad", "--key", bad); code == 0 {
		t.Fatal("garbage key accepted")
	}
	// A second signer replaces the file by rename (new inode), mode kept.
	if code, _, errS = admin("add", "--signers-file", reg, "--id", "backup", "--key", other); code != 0 {
		t.Fatalf("second add %d %q", code, errS)
	}
	fi2, _ := os.Stat(reg)
	if fi2.Sys().(*syscall.Stat_t).Ino == ino {
		t.Fatal("registry rewritten in place, not renamed over")
	}
	if fi2.Mode().Perm() != 0o644 {
		t.Fatalf("mode after rewrite %04o", fi2.Mode().Perm())
	}
	if r, err = signing.LoadRegistry(reg, selfOwned()); err != nil || len(r.Signers) != 2 {
		t.Fatalf("after second add: %v %+v", err, r)
	}
	if left := tmpLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// A registry the trust checks reject (group-writable) is never written to.
func TestSignersAddRefusesUntrustedRegistry(t *testing.T) {
	fakeEUID(t, 0)
	dir, reg := adminDir(t)
	os.Chmod(dir, 0o777)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	key, _ := edPub(t, "x")
	if code, _, errS := admin("add", "--signers-file", reg, "--id", "mac", "--key", key); code == 0 || !strings.Contains(errS, "쓰기") {
		t.Fatalf("code %d err %q", code, errS)
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatal("registry written into an untrusted directory")
	}
}

// writePending writes a pending-enrollment file with one valid passkey.
func writePending(t *testing.T) (path string, p pendingCredential) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ek, _ := k.PublicKey.ECDH()
	p = pendingCredential{ID: "phone", Label: "iPhone passkey",
		CredentialID: base64.RawURLEncoding.EncodeToString([]byte("cred-1")),
		PublicKey:    base64.RawURLEncoding.EncodeToString(ek.Bytes()),
		RPID:         "orch-vm.tailnet.ts.net", Origin: "https://orch-vm.tailnet.ts.net",
		Created: time.Date(2026, 11, 9, 1, 2, 3, 0, time.UTC)}
	b, _ := json.Marshal(pendingFile{Version: 1, Pending: []pendingCredential{p}})
	path = filepath.Join(t.TempDir(), "signers.pending.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, p
}

func TestSignersPendingListAndActivate(t *testing.T) {
	_, reg := adminDir(t)
	pf, p := writePending(t)
	fp, err := signing.Fingerprint(p.signer())
	if err != nil {
		t.Fatal(err)
	}
	fakeEUID(t, 501) // listing pending needs no root
	code, out, errS := admin("pending", "--pending-file", pf)
	if code != 0 || !strings.Contains(out, "phone") || !strings.Contains(out, fp) || !strings.Contains(out, p.Origin) {
		t.Fatalf("pending %d %q %q", code, out, errS)
	}
	fakeEUID(t, 0)
	// The fingerprint the admin checked out of band is required and must match.
	if code, _, errS = admin("add", "--signers-file", reg, "--pending-file", pf, "--pending", "phone"); code == 0 || !strings.Contains(errS, "--fingerprint") {
		t.Fatalf("without fingerprint %d %q", code, errS)
	}
	if code, _, errS = admin("add", "--signers-file", reg, "--pending-file", pf, "--pending", "phone", "--fingerprint", "SHA256:wrong"); code == 0 || !strings.Contains(errS, "지문") {
		t.Fatalf("wrong fingerprint %d %q", code, errS)
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatal("registry written on refusal")
	}
	if code, out, errS = admin("add", "--signers-file", reg, "--pending-file", pf, "--pending", "phone", "--fingerprint", fp); code != 0 {
		t.Fatalf("activate %d %q %q", code, out, errS)
	}
	r, err := signing.LoadRegistry(reg, selfOwned())
	if err != nil {
		t.Fatal(err)
	}
	s, ok := r.Find("phone")
	if !ok || s.Type != signing.TypeWebAuthnES256 || s.RPID != p.RPID || s.Origin != p.Origin || s.CredentialID != p.CredentialID || s.Label != p.Label {
		t.Fatalf("signer %+v", s)
	}
	if code, _, _ = admin("add", "--signers-file", reg, "--pending", "nope", "--pending-file", pf, "--fingerprint", fp); code == 0 {
		t.Fatal("unknown pending id accepted")
	}
}

func TestSignersListShowsFingerprints(t *testing.T) {
	fakeEUID(t, 0)
	_, reg := adminDir(t)
	key, pub := edPub(t, "me@mac")
	if code, _, errS := admin("add", "--signers-file", reg, "--id", "mac", "--label", "Mac SSH", "--key", key); code != 0 {
		t.Fatal(errS)
	}
	pf, p := writePending(t)
	wfp, _ := signing.Fingerprint(p.signer())
	if code, _, errS := admin("add", "--signers-file", reg, "--pending-file", pf, "--pending", "phone", "--fingerprint", wfp); code != 0 {
		t.Fatal(errS)
	}
	fakeEUID(t, 501) // listing needs no root
	code, out, errS := admin("list", "--signers-file", reg)
	if code != 0 {
		t.Fatalf("list %d %q", code, errS)
	}
	for _, want := range []string{"mac", signing.TypeSSHEd25519, "Mac SSH", ssh.FingerprintSHA256(pub),
		"phone", signing.TypeWebAuthnES256, "iPhone passkey", wfp, p.Origin} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	// A missing registry is reported as legacy mode, not an error.
	if code, out, _ = admin("list", "--signers-file", filepath.Join(filepath.Dir(reg), "none.json")); code != 0 || !strings.Contains(out, "없") {
		t.Fatalf("absent list %d %q", code, out)
	}
}

func TestSignersRemoveRequiresRoot(t *testing.T) {
	fakeEUID(t, 0)
	_, reg := adminDir(t)
	key, _ := edPub(t, "me@mac")
	if code, _, errS := admin("add", "--signers-file", reg, "--id", "mac", "--key", key); code != 0 {
		t.Fatal(errS)
	}
	before, _ := os.ReadFile(reg)
	fakeEUID(t, 501)
	if code, _, errS := admin("remove", "--signers-file", reg, "mac"); code == 0 || !strings.Contains(errS, "root") {
		t.Fatalf("non-root remove %d %q", code, errS)
	}
	if after, _ := os.ReadFile(reg); !bytes.Equal(before, after) {
		t.Fatal("non-root remove changed the file")
	}
	fakeEUID(t, 0)
	if code, _, errS := admin("remove", "--signers-file", reg, "ghost"); code == 0 || !strings.Contains(errS, "ghost") {
		t.Fatalf("unknown remove %d %q", code, errS)
	}
	if code, _, errS := admin("remove", "--signers-file", reg, "mac"); code != 0 {
		t.Fatalf("remove %d %q", code, errS)
	}
	r, err := signing.LoadRegistry(reg, selfOwned())
	if err != nil || len(r.Signers) != 0 {
		t.Fatalf("after remove: %v %+v", err, r)
	}
	if mode(t, reg) != 0o644 {
		t.Fatalf("mode %04o", mode(t, reg))
	}
}
