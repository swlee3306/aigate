package keys

import (
	"crypto/ed25519"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// short temp dirs keep unix paths short on macOS.
func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return &Store{Root: d}
}

func stagedDirs(t *testing.T, s *Store) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Root, "keys-staging"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, filepath.Join(s.Root, "keys-staging", e.Name()))
	}
	return out
}

func TestKeyGenerationEd25519NoExternalProgram(t *testing.T) {
	// No program can be found: only the standard library may have made the key.
	t.Setenv("PATH", t.TempDir())
	s := newStore(t)
	k, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	dirs := stagedDirs(t, s)
	if len(dirs) != 1 {
		t.Fatalf("%v", dirs)
	}
	privPEM, err := os.ReadFile(filepath.Join(dirs[0], PrivateName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(privPEM), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("not an OpenSSH private key: %.40s", privPEM)
	}
	raw, err := ssh.ParseRawPrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	priv, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		t.Fatalf("%T", raw)
	}
	signer, err := ssh.NewSignerFromKey(*priv)
	if err != nil {
		t.Fatal(err)
	}
	if signer.PublicKey().Type() != "ssh-ed25519" || ssh.FingerprintSHA256(signer.PublicKey()) != k.Fingerprint {
		t.Fatalf("public key does not belong to the private key: %s", k.Fingerprint)
	}
	if !strings.HasPrefix(k.Public, "ssh-ed25519 ") || strings.ContainsAny(k.Public, "\n\r") || k.SHA256 != Hash(k.Public) {
		t.Fatalf("%+v", k)
	}
	pub, err := ReadPublic(filepath.Join(dirs[0], PublicName))
	if err != nil || pub != k {
		t.Fatalf("%v %+v", err, pub)
	}
	// AST: the package starts no program and imports no os/exec.
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "keys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range af.Imports {
		if imp.Path.Value == `"os/exec"` {
			t.Error("keys imports os/exec")
		}
	}
}

func TestEveryStageMakesANewKey(t *testing.T) {
	s := newStore(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		k, err := s.Stage()
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.SHA256] || seen[k.Public] {
			t.Fatal("a key was offered twice")
		}
		seen[k.SHA256], seen[k.Public] = true, true
	}
}

func TestStagingKeyModesAndOwner(t *testing.T) {
	s := newStore(t)
	if _, err := s.Stage(); err != nil {
		t.Fatal(err)
	}
	check := func(p string, wantDir bool, mode os.FileMode) {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.IsDir() != wantDir || fi.Mode().Perm() != mode || !ownedByUs(fi) {
			t.Errorf("%s: dir=%v mode=%o owner-ok=%v", p, fi.IsDir(), fi.Mode().Perm(), ownedByUs(fi))
		}
	}
	check(filepath.Join(s.Root, "keys-staging"), true, 0o700)
	d := stagedDirs(t, s)[0]
	check(d, true, 0o700)
	check(filepath.Join(d, PrivateName), false, 0o600)
	if fi, _ := os.Lstat(filepath.Join(d, PublicName)); fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("public key writable by others: %o", fi.Mode().Perm())
	}
	// a staging directory that group or others can reach is refused
	if err := os.Chmod(filepath.Join(s.Root, "keys-staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(); err == nil {
		t.Error("staged into a world-readable staging directory")
	}
}

func TestStagingNeedsRealRootAndNoSymlinks(t *testing.T) {
	s := newStore(t)
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(s.Root, "keys-staging")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(); err == nil {
		t.Fatal("staged through a symlinked keys-staging")
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Fatalf("something was written through the symlink: %v", ents)
	}
	if _, err := (&Store{Root: filepath.Join(s.Root, "missing")}).Stage(); err == nil {
		t.Fatal("staged under a missing root")
	}
	if _, err := (&Store{}).Stage(); err == nil {
		t.Fatal("staged without a root")
	}
}

func TestStagedChecksHashExpiryAndTampering(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	k, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Staged(strings.Repeat("a", 64)); err != ErrNotFound {
		t.Fatalf("unknown hash: %v", err)
	}
	got, err := s.Staged(k.SHA256)
	if err != nil || got != k {
		t.Fatalf("%v %+v", err, got)
	}
	s.Release(k.SHA256)
	// the public file is replaced by another valid key: the hash compare refuses it
	other := newStore(t)
	k2, _ := other.Stage()
	if err := os.WriteFile(filepath.Join(stagedDirs(t, s)[0], PublicName), []byte(k2.Public+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Staged(k.SHA256); err != ErrMismatch {
		t.Fatalf("swapped public key: %v", err)
	}
	// expiry: after 15 minutes the key is gone and answers ErrExpired
	k3, _ := s.Stage()
	now = now.Add(StagingTTL - time.Second)
	if _, err := s.Staged(k3.SHA256); err != nil {
		t.Fatalf("just before expiry: %v", err)
	}
	s.Release(k3.SHA256)
	now = now.Add(2 * time.Second)
	if _, err := s.Staged(k3.SHA256); err != ErrExpired && err != ErrNotFound { // swept by the sweep inside Staged
		t.Fatalf("after expiry: %v", err)
	}
	if _, err := s.Staged(k3.SHA256); err != ErrNotFound {
		t.Fatalf("expired key still known: %v", err)
	}
}

func TestStagingKeyExpiresAndSweepsOnlyOwn(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	old, _ := s.Stage()
	oldDir := stagedDirs(t, s)[0]
	held, _ := s.Stage()
	now = now.Add(StagingTTL - time.Minute)
	if _, err := s.Staged(held.SHA256); err != nil { // held by an apply (just before it expires)
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	fresh, _ := s.Stage() // lazy sweep runs here
	if _, err := os.Lstat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("the expired staging dir is still there: %v", err)
	}
	if _, err := s.Staged(old.SHA256); err != ErrNotFound {
		t.Fatalf("expired key known: %v", err)
	}
	if _, err := s.Staged(held.SHA256); err != ErrHeld { // still known, still held: a second apply is refused
		t.Fatalf("a held key was swept or handed out twice: %v", err)
	}
	s.Release(held.SHA256) // released after it expired: the next sweep takes it
	if n := s.SweepExpired(); n != 1 {
		t.Fatalf("released expired key swept: %d", n)
	}
	if _, err := s.Staged(fresh.SHA256); err != nil {
		t.Fatalf("fresh key swept: %v", err)
	}

	// SweepAll (orchd start) removes only real dirs with the staging name made by us.
	root := filepath.Join(s.Root, "keys-staging")
	outside := t.TempDir()
	victim := filepath.Join(outside, "precious")
	os.WriteFile(victim, []byte("x"), 0o600)
	link := filepath.Join(root, strings.Repeat("a", 32)) // staging-shaped name, but a symlink
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(root, "not-a-staging-name")
	os.Mkdir(stranger, 0o700)
	strangerFile := filepath.Join(stranger, "keep")
	os.WriteFile(strangerFile, []byte("x"), 0o600)
	plainFile := filepath.Join(root, strings.Repeat("b", 32))
	os.WriteFile(plainFile, []byte("x"), 0o600) // staging-shaped name, but a file
	n := s.SweepAll()
	if n != 1 { // the fresh one (the held one was swept above)
		t.Errorf("swept %d, want 1", n)
	}
	for _, p := range []string{victim, strangerFile, plainFile, link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was touched: %v", p, err)
		}
	}
	if _, err := s.Staged(fresh.SHA256); err != ErrNotFound {
		t.Errorf("a swept key is still applicable: %v", err)
	}
	// a directory swapped in under a recorded name is never deleted by the lazy sweep
	s2 := newStore(t)
	n2 := time.Now()
	s2.Now = func() time.Time { return n2 }
	k, _ := s2.Stage()
	dir := stagedDirs(t, s2)[0]
	moved := dir + ".moved"
	os.Rename(dir, moved)
	os.Symlink(outside, dir)
	n2 = n2.Add(time.Hour)
	s2.SweepExpired()
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("sweep followed a swapped symlink: %v", err)
	}
	if _, err := os.Lstat(moved); err != nil {
		t.Errorf("sweep removed something it did not recognise: %v", err)
	}
	_ = k
}

func TestPromoteIsAtomicRename(t *testing.T) {
	s := newStore(t)
	k, _ := s.Stage()
	if _, err := s.Staged(k.SHA256); err != nil {
		t.Fatal(err)
	}
	dir := stagedDirs(t, s)[0]
	before, err := os.Lstat(filepath.Join(dir, PrivateName))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(k.SHA256, "app-one"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(filepath.Join(s.KeysDir(), "app-one", PrivateName))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("not the same file (rename expected): %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("staging dir remains")
	}
	if fi, _ := os.Lstat(s.KeysDir()); fi.Mode().Perm() != 0o700 {
		t.Errorf("keys dir mode %o", fi.Mode().Perm())
	}
	if _, err := s.Staged(k.SHA256); err != ErrNotFound {
		t.Fatalf("promoted key still staged: %v", err)
	}
	// an existing target (dir or symlink) is never replaced
	k2, _ := s.Stage()
	if err := s.Promote(k2.SHA256, "app-one"); err != ErrExists {
		t.Fatalf("existing dir replaced: %v", err)
	}
	pub, _ := ReadPublic(filepath.Join(s.KeysDir(), "app-one", PublicName))
	if pub != k {
		t.Fatal("the promoted key was replaced")
	}
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, filepath.Join(s.KeysDir(), "app-two"))
	if err := s.Promote(k2.SHA256, "app-two"); err != ErrExists {
		t.Fatalf("symlink target replaced: %v", err)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatal("wrote through a symlink")
	}
	for _, bad := range []string{"", "../x", "a/b", "App", "x.y"} {
		if err := s.Promote(k2.SHA256, bad); err != ErrBadID {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if !s.Exists("app-one") || !s.Exists("app-two") || s.Exists("app-three") {
		t.Error("Exists")
	}
}

func TestPrivatePathRules(t *testing.T) {
	s := newStore(t)
	k, _ := s.Stage()
	s.Staged(k.SHA256)
	if err := s.Promote(k.SHA256, "app-one"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.KeysDir(), "app-one", PrivateName)
	if p, err := PrivatePath(s.KeysDir(), "app-one"); err != nil || p != want {
		t.Fatalf("%q %v", p, err)
	}
	// a decoy that ".." would reach if the id were not checked
	if err := os.WriteFile(filepath.Join(s.Root, PrivateName), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "..", "a/../b", "App", "nope-here", "../keys/app-one"} {
		if p, err := PrivatePath(s.KeysDir(), id); err == nil || p != "" {
			t.Errorf("%q accepted: %q", id, p)
		}
	}
	for name, mod := range map[string]func(){
		"group readable": func() { os.Chmod(want, 0o640) },
		"world readable": func() { os.Chmod(want, 0o604) },
		"executable":     func() { os.Chmod(want, 0o700) },
		"dir open":       func() { os.Chmod(want, 0o600); os.Chmod(filepath.Dir(want), 0o755) },
		"empty":          func() { os.Chmod(filepath.Dir(want), 0o700); os.Truncate(want, 0) },
		"missing":        func() { os.Remove(want) },
		"symlink": func() {
			os.Remove(want)
			real := filepath.Join(t.TempDir(), "k")
			os.WriteFile(real, []byte("x"), 0o600)
			os.Symlink(real, want)
		},
		"a directory": func() { os.Remove(want); os.Mkdir(want, 0o700) },
	} {
		// restore a good state first
		os.Chmod(filepath.Dir(want), 0o700)
		os.RemoveAll(want)
		os.WriteFile(want, []byte("key"), 0o600)
		mod()
		if p, err := PrivatePath(s.KeysDir(), "app-one"); err == nil {
			t.Errorf("%s: accepted %q", name, p)
		}
	}
	os.Chmod(filepath.Dir(want), 0o700)
	os.RemoveAll(want)
	os.WriteFile(want, []byte("key"), 0o400)
	if _, err := PrivatePath(s.KeysDir(), "app-one"); err != nil {
		t.Errorf("0400 refused: %v", err)
	}
	// the keys directory itself being a symlink
	real := t.TempDir()
	os.Mkdir(filepath.Join(real, "app-one"), 0o700)
	os.WriteFile(filepath.Join(real, "app-one", PrivateName), []byte("k"), 0o600)
	link := filepath.Join(t.TempDir(), "keys")
	os.Symlink(real, link)
	if _, err := PrivatePath(link, "app-one"); err == nil {
		t.Error("symlinked keys dir accepted")
	}
	if err := UsableFile("relative/path"); err == nil {
		t.Error("relative path")
	}
}

// Staging, promotion and the lookups never read the private key file: it can
// be made unreadable and everything still works.
func TestPrivateKeyIsNeverReadBack(t *testing.T) {
	s := newStore(t)
	k, _ := s.Stage()
	priv := filepath.Join(stagedDirs(t, s)[0], PrivateName)
	if err := os.Chmod(priv, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Staged(k.SHA256); err != nil {
		t.Fatalf("Staged read the private key: %v", err)
	}
	if err := s.Promote(k.SHA256, "app-one"); err != nil {
		t.Fatalf("Promote read the private key: %v", err)
	}
	os.Chmod(filepath.Join(s.KeysDir(), "app-one", PrivateName), 0o600)

	// AST: os.ReadFile and os.Open appear only in ReadPublic.
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "keys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if ce, ok := n.(*ast.CallExpr); ok {
				if se, ok := ce.Fun.(*ast.SelectorExpr); ok {
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "os" {
						switch se.Sel.Name {
						case "ReadFile", "Open":
							if fd.Name.Name != "ReadPublic" { // the one reader: the .pub file
								t.Errorf("%s calls os.%s", fd.Name.Name, se.Sel.Name)
							}
						case "OpenFile":
							if fd.Name.Name != "writeNew" {
								t.Errorf("%s calls os.OpenFile", fd.Name.Name)
							}
						}
					}
				}
			}
			return true
		})
	}
}

// One apply at a time per staged key: a double submit cannot make the second
// apply delete or move the key the first one is still using.
func TestStagedKeyIsHeldByOneApplyAtATime(t *testing.T) {
	s := newStore(t)
	k, _ := s.Stage()
	var wins, held int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := s.Staged(k.SHA256); err {
			case nil:
				atomic.AddInt32(&wins, 1)
			case ErrHeld:
				atomic.AddInt32(&held, 1)
			default:
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || held != 15 {
		t.Fatalf("wins %d held %d", wins, held)
	}
	s.Release(k.SHA256)
	if _, err := s.Staged(k.SHA256); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// A hold is bounded: a hung apply cannot keep an entry (and a MaxStaged slot) forever.
func TestHoldIsBoundedAndRetainedKeysSurviveExpiry(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	k, _ := s.Stage()
	if _, err := s.Staged(k.SHA256); err != nil {
		t.Fatal(err)
	}
	now = now.Add(HoldTTL - time.Second)
	if _, err := s.Staged(k.SHA256); err != ErrHeld {
		t.Fatalf("%v", err)
	}
	now = now.Add(2 * time.Second) // hold older than HoldTTL, entry older than StagingTTL? no: 10 min < 15 min
	if _, err := s.Staged(k.SHA256); err != nil {
		t.Fatalf("a stale hold must not block: %v", err)
	}
	// retained: survives expiry and sweeps, can be applied again, listed for reconcile
	if !s.Retain(k.SHA256, "app-one", "gh") {
		t.Fatal("retain")
	}
	now = now.Add(3 * StagingTTL)
	if n := s.SweepExpired(); n != 0 {
		t.Fatalf("a retained key was swept: %d", n)
	}
	rk := s.RetainedKeys()
	if len(rk) != 1 || rk[0].ID != "app-one" || rk[0].Provider != "gh" || rk[0].Age < 3*StagingTTL {
		t.Fatalf("%+v", rk)
	}
	if _, err := s.Staged(k.SHA256); err != nil {
		t.Fatalf("a retained key must stay applicable: %v", err)
	}
	s.Release(k.SHA256)
	if err := s.Promote(k.SHA256, "app-one"); err != nil {
		t.Fatalf("retained key not promotable: %v", err)
	}
	// the cap
	for i := 0; i < MaxRetained+1; i++ {
		kk, _ := s.Stage()
		s.Staged(kk.SHA256)
		ok := s.Retain(kk.SHA256, "app-x", "gh")
		if ok != (i < MaxRetained) {
			t.Fatalf("retain %d: %v", i, ok)
		}
	}
	// expiry is enforced inside Staged and Exists, not only by Stage
	s2 := newStore(t)
	n2 := time.Now()
	s2.Now = func() time.Time { return n2 }
	k2, _ := s2.Stage()
	dir := stagedDirs(t, s2)[0]
	n2 = n2.Add(StagingTTL + time.Second)
	s2.Exists("app-one")
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("Exists did not sweep the expired key")
	}
	if _, err := s2.Staged(k2.SHA256); err != ErrNotFound {
		t.Fatalf("%v", err)
	}
	// Promote refuses a directory that is not the one this store made
	s3 := newStore(t)
	k3, _ := s3.Stage()
	s3.Staged(k3.SHA256)
	d3 := stagedDirs(t, s3)[0]
	os.Rename(d3, d3+".moved")
	os.Mkdir(d3, 0o700)
	if err := s3.Promote(k3.SHA256, "app-one"); err != ErrNotFound {
		t.Fatalf("promoted a swapped directory: %v", err)
	}
}
