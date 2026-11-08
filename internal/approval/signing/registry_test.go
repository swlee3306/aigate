package signing

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func ownOpts() RegistryOptions {
	return RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }}
}

func notOwnOpts() RegistryOptions {
	return RegistryOptions{OwnerOK: func(uid uint32) bool { return uid != uint32(os.Getuid()) }}
}

func authLine(t *testing.T, pub any) string {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func edLine(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return authLine(t, pub)
}

func p256Point(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	return e.Bytes()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sshSigner(id, key string) map[string]any {
	return map[string]any{"id": id, "type": "ssh-ed25519", "label": "L", "public_key": key, "added_at": "2026-11-09T01:02:03Z"}
}

func webSigner(id string, point []byte) map[string]any {
	return map[string]any{"id": id, "type": "webauthn-es256", "label": "phone", "public_key": b64(point),
		"credential_id": b64([]byte("cred-1")), "rp_id": "console.example.com",
		"origin": "https://console.example.com:8443", "added_at": "2026-11-09T01:02:03Z"}
}

func regJSON(t *testing.T, signers ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "signers": signers})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeReg creates a trusted-looking layout (dir 0755, file 0644) and returns the path.
func writeReg(t *testing.T, content []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "signers.json")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustFail(t *testing.T, p string, o RegistryOptions) error {
	t.Helper()
	_, err := LoadRegistry(p, o)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrRegistryAbsent) {
		t.Fatalf("must not be ErrRegistryAbsent: %v", err)
	}
	return err
}

func TestAbsentIsErrRegistryAbsent(t *testing.T) {
	_, err := LoadRegistry(filepath.Join(t.TempDir(), "nope.json"), ownOpts())
	if !errors.Is(err, ErrRegistryAbsent) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadOK(t *testing.T) {
	point := p256Point(t)
	key := edLine(t) + " alice@laptop"
	p := writeReg(t, regJSON(t, sshSigner("alice", key), webSigner("alice-phone", point)))
	r, err := LoadRegistry(p, ownOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Signers) != 2 {
		t.Fatalf("signers: %d", len(r.Signers))
	}
	s, ok := r.Find("alice")
	if !ok || s.Type != "ssh-ed25519" || s.PublicKey != key || s.Label != "L" || s.AddedAt.IsZero() {
		t.Fatalf("alice: %+v %v", s, ok)
	}
	w, ok := r.Find("alice-phone")
	if !ok || w.Type != "webauthn-es256" || w.PublicKey != b64(point) || w.RPID != "console.example.com" ||
		w.Origin != "https://console.example.com:8443" || w.CredentialID != b64([]byte("cred-1")) {
		t.Fatalf("phone: %+v %v", w, ok)
	}
	if _, ok := r.Find("bob"); ok {
		t.Fatal("unexpected bob")
	}
}

func TestEmptySignersOK(t *testing.T) {
	p := writeReg(t, regJSON(t))
	r, err := LoadRegistry(p, ownOpts())
	if err != nil || len(r.Signers) != 0 {
		t.Fatalf("%v %v", r, err)
	}
}

func TestDefaultOwnerIsRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	p := writeReg(t, regJSON(t))
	if err := mustFail(t, p, RegistryOptions{}); !strings.Contains(err.Error(), "소유") {
		t.Fatalf("err: %v", err)
	}
}

func TestRefusesGroupOrWorldWritableFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o664, 0o646, 0o666, 0o620, 0o602} {
		p := writeReg(t, regJSON(t))
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		mustFail(t, p, ownOpts())
	}
}

func TestRefusesSymlink(t *testing.T) {
	p := writeReg(t, regJSON(t))
	link := filepath.Join(filepath.Dir(p), "link.json")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	mustFail(t, link, ownOpts())
}

func TestRefusesSymlinkedParent(t *testing.T) {
	p := writeReg(t, regJSON(t))
	linkDir := filepath.Join(t.TempDir(), "linkdir")
	if err := os.Symlink(filepath.Dir(p), linkDir); err != nil {
		t.Fatal(err)
	}
	mustFail(t, filepath.Join(linkDir, "signers.json"), ownOpts())
}

func TestRefusesNonDirectoryOrNonRegularFile(t *testing.T) {
	p := writeReg(t, regJSON(t))
	dirAsFile := filepath.Join(filepath.Dir(p), "sub")
	if err := os.Mkdir(dirAsFile, 0o755); err != nil {
		t.Fatal(err)
	}
	mustFail(t, dirAsFile, ownOpts())
}

func TestRefusesNonRootOwnerFile(t *testing.T) {
	p := writeReg(t, regJSON(t))
	mustFail(t, p, notOwnOpts())
}

func TestRefusesWritableParentDir(t *testing.T) {
	for _, mode := range []os.FileMode{0o775, 0o757, 0o777} {
		p := writeReg(t, regJSON(t))
		if err := os.Chmod(filepath.Dir(p), mode); err != nil {
			t.Fatal(err)
		}
		mustFail(t, p, ownOpts())
	}
}

func TestRefusesNonRootOwnedParentDir(t *testing.T) {
	p := writeReg(t, regJSON(t))
	calls := 0
	// accepts the file (first call), rejects the parent dir (second call)
	o := RegistryOptions{OwnerOK: func(uid uint32) bool {
		calls++
		return calls == 1
	}}
	err := mustFail(t, p, o)
	if calls != 2 {
		t.Fatalf("OwnerOK calls: %d (%v)", calls, err)
	}
}

func TestRejectsDuplicateIDsAndUnknownType(t *testing.T) {
	k1, k2 := edLine(t), edLine(t)
	mustFail(t, writeReg(t, regJSON(t, sshSigner("a", k1), sshSigner("a", k2))), ownOpts())
	bad := sshSigner("a", k1)
	bad["type"] = "ssh-rsa"
	mustFail(t, writeReg(t, regJSON(t, bad)), ownOpts())
	bad["type"] = ""
	mustFail(t, writeReg(t, regJSON(t, bad)), ownOpts())
}

func TestRejectsBadID(t *testing.T) {
	for _, id := range []string{"", "A", "-a", ".a", "a b", "a/b", "a\n", strings.Repeat("a", 64)} {
		mustFail(t, writeReg(t, regJSON(t, sshSigner(id, edLine(t)))), ownOpts())
	}
	if _, err := LoadRegistry(writeReg(t, regJSON(t, sshSigner(strings.Repeat("a", 63), edLine(t)))), ownOpts()); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsBadPublicKey(t *testing.T) {
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := edLine(t)
	cases := map[string]string{
		"rsa":      authLine(t, &rk.PublicKey),
		"ecdsa":    authLine(t, &ek.PublicKey),
		"garbage":  "not a key",
		"empty":    "",
		"options":  `command="x" ` + good,
		"two":      good + "\n" + edLine(t),
		"comment":  "# hi\n" + good,
		"trailing": good + "\n",
	}
	for name, key := range cases {
		if err := mustFail(t, writeReg(t, regJSON(t, sshSigner("a", key))), ownOpts()); err == nil {
			t.Fatal(name)
		}
	}
	// ssh signer must not carry webauthn fields
	s := sshSigner("a", good)
	s["rp_id"] = "x.example.com"
	mustFail(t, writeReg(t, regJSON(t, s)), ownOpts())
	s = sshSigner("a", good)
	s["credential_id"] = b64([]byte("x"))
	mustFail(t, writeReg(t, regJSON(t, s)), ownOpts())
	s = sshSigner("a", good)
	s["origin"] = "https://x.example.com"
	mustFail(t, writeReg(t, regJSON(t, s)), ownOpts())
}

func TestRejectsBadWebAuthnFields(t *testing.T) {
	point := p256Point(t)
	offCurve := append([]byte(nil), point...)
	offCurve[64] ^= 0x01
	badPrefix := append([]byte(nil), point...)
	badPrefix[0] = 0x02
	cases := map[string]func(m map[string]any){
		"off-curve":    func(m map[string]any) { m["public_key"] = b64(offCurve) },
		"short":        func(m map[string]any) { m["public_key"] = b64(point[:64]) },
		"long":         func(m map[string]any) { m["public_key"] = b64(append(append([]byte(nil), point...), 0)) },
		"prefix":       func(m map[string]any) { m["public_key"] = b64(badPrefix) },
		"padded":       func(m map[string]any) { m["public_key"] = base64.URLEncoding.EncodeToString(point) },
		"std alphabet": func(m map[string]any) { m["public_key"] = base64.StdEncoding.EncodeToString(point) },
		"empty cred":   func(m map[string]any) { m["credential_id"] = "" },
		"bad cred":     func(m map[string]any) { m["credential_id"] = "!!!" },
		"huge cred":    func(m map[string]any) { m["credential_id"] = b64(make([]byte, 1024)) },
		"no rp":        func(m map[string]any) { m["rp_id"] = "" },
		"upper rp":     func(m map[string]any) { m["rp_id"] = "Console.example.com" },
		"rp port":      func(m map[string]any) { m["rp_id"] = "example.com:443" },
		"no origin":    func(m map[string]any) { m["origin"] = "" },
		"http origin":  func(m map[string]any) { m["origin"] = "http://console.example.com" },
		"http local":   func(m map[string]any) { m["origin"] = "http://localhost" },
		"http 127":     func(m map[string]any) { m["origin"] = "http://127.0.0.1" },
		"path":         func(m map[string]any) { m["origin"] = "https://console.example.com/x" },
		"slash":        func(m map[string]any) { m["origin"] = "https://console.example.com/" },
		"query":        func(m map[string]any) { m["origin"] = "https://console.example.com?x=1" },
		"userinfo":     func(m map[string]any) { m["origin"] = "https://u:p@console.example.com" },
		"fragment":     func(m map[string]any) { m["origin"] = "https://console.example.com#x" },
		"bad port":     func(m map[string]any) { m["origin"] = "https://console.example.com:99999" },
		"upper origin": func(m map[string]any) { m["origin"] = "https://Console.example.com" },
	}
	for name, mut := range cases {
		m := webSigner("w", point)
		mut(m)
		if err := mustFail(t, writeReg(t, regJSON(t, m)), ownOpts()); err == nil {
			t.Fatal(name)
		}
	}
	// max-length credential id is fine
	m := webSigner("w", point)
	m["credential_id"] = b64(make([]byte, 1023))
	if _, err := LoadRegistry(writeReg(t, regJSON(t, m)), ownOpts()); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsMalformedJSON(t *testing.T) {
	good := string(regJSON(t, sshSigner("a", edLine(t))))
	cases := map[string]string{
		"unknown top field":    strings.Replace(good, `"version":1`, `"version":1,"extra":1`, 1),
		"unknown signer field": strings.Replace(good, `"id":"a"`, `"id":"a","extra":1`, 1),
		"trailing data":        good + ` {}`,
		"trailing garbage":     good + `x`,
		"version 2":            strings.Replace(good, `"version":1`, `"version":2`, 1),
		"no version":           strings.Replace(good, `,"version":1`, ``, 1),
		"not json":             `nope`,
		"empty":                ``,
		"bad time":             strings.Replace(good, `2026-11-09T01:02:03Z`, `yesterday`, 1),
	}
	for name, c := range cases {
		if c == good {
			t.Fatalf("%s: case did not modify the document", name)
		}
		if err := mustFail(t, writeReg(t, []byte(c)), ownOpts()); err == nil {
			t.Fatal(name)
		}
	}
	// trailing whitespace/newline is fine
	if _, err := LoadRegistry(writeReg(t, []byte(good+"\n")), ownOpts()); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsOversizedFile(t *testing.T) {
	pad := strings.Repeat(" ", 1<<20)
	mustFail(t, writeReg(t, append(regJSON(t), pad...)), ownOpts())
	// exactly at the cap is accepted
	b := regJSON(t)
	b = append(b, strings.Repeat(" ", (1<<20)-len(b))...)
	if _, err := LoadRegistry(writeReg(t, b), ownOpts()); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryPathProcessWide(t *testing.T) {
	old := RegistryPath()
	t.Cleanup(func() { SetRegistryPath(old) })
	if DefaultRegistryPath != "/etc/aigate/signers.json" {
		t.Fatal(DefaultRegistryPath)
	}
	SetRegistryPath("/x/y.json")
	if RegistryPath() != "/x/y.json" {
		t.Fatal(RegistryPath())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); SetRegistryPath("/x/y.json") }()
		go func() { defer wg.Done(); _ = RegistryPath() }()
	}
	wg.Wait()
}

// Absent must only be reported when the directory chain is trusted, otherwise
// an untrusted user could delete the file and switch signed mode off.
func TestAbsentRequiresTrustedParent(t *testing.T) {
	newDir := func(mode os.FileMode) string {
		d := filepath.Join(t.TempDir(), "etc")
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		return d
	}
	// (h) trusted parent, missing file: absent
	d := newDir(0o755)
	if _, err := LoadRegistry(filepath.Join(d, "signers.json"), ownOpts()); !errors.Is(err, ErrRegistryAbsent) {
		t.Fatalf("(h) %v", err)
	}
	// (a) writable parent
	d = newDir(0o777)
	mustFail(t, filepath.Join(d, "signers.json"), ownOpts())
	// (b) untrusted owner
	d = newDir(0o755)
	mustFail(t, filepath.Join(d, "signers.json"), notOwnOpts())
	// (c) dangling symlink parent
	base := t.TempDir()
	if err := os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	mustFail(t, filepath.Join(base, "link", "signers.json"), ownOpts())
	// (d) parent missing under trusted ancestor: absent (legacy mode)
	d = newDir(0o755)
	if _, err := LoadRegistry(filepath.Join(d, "aigate", "signers.json"), ownOpts()); !errors.Is(err, ErrRegistryAbsent) {
		t.Fatalf("(d) %v", err)
	}
	if _, err := LoadRegistry(filepath.Join(d, "a", "b", "signers.json"), ownOpts()); !errors.Is(err, ErrRegistryAbsent) {
		t.Fatalf("(d2) %v", err)
	}
	// (e) missing parent under writable ancestor
	d = newDir(0o777)
	mustFail(t, filepath.Join(d, "aigate", "signers.json"), ownOpts())
	mustFail(t, filepath.Join(d, "a", "b", "signers.json"), ownOpts())
	// (f) missing parent under ancestor with rejected owner
	d = newDir(0o755)
	mustFail(t, filepath.Join(d, "aigate", "signers.json"), notOwnOpts())
	// (g) symlink in the missing chain
	d = newDir(0o755)
	real := filepath.Join(d, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(d, "sym")); err != nil {
		t.Fatal(err)
	}
	mustFail(t, filepath.Join(d, "sym", "aigate", "signers.json"), ownOpts())
	if err := os.Symlink(filepath.Join(d, "nowhere"), filepath.Join(d, "dangling")); err != nil {
		t.Fatal(err)
	}
	mustFail(t, filepath.Join(d, "dangling", "aigate", "signers.json"), ownOpts())
}
