package keys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func advStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	root, _ := filepath.EvalSymlinks(d)
	now := time.Unix(1_800_000_000, 0)
	return &Store{Root: root, Now: func() time.Time { return now }}, &now
}

// The sweeps remove only what this store made: with an expired key of its own
// next to hostile neighbours in keys-staging (a symlink to a directory outside
// holding a precious file, a plain file, a directory with a non-staging name,
// a staging-shaped directory that is a symlink, and a promoted key under
// keys/), only the own expired directory goes. Nothing
// outside the tree, nothing under keys/, no neighbour is touched or followed.
func TestSweepTouchesOnlyOwnDirectoriesW3e(t *testing.T) {
	s, now := advStore(t)
	own, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(own.SHA256, "kept-one"); err != nil {
		t.Fatal(err)
	}
	k2, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious")
	os.WriteFile(precious, []byte("do not delete"), 0o600)
	staging := filepath.Join(s.Root, stagingDirName)
	hex32 := strings.Repeat("ab", 16)
	hex32b := strings.Repeat("cd", 16)
	hex32c := strings.Repeat("ef", 16)
	neighbours := map[string]func(string){
		"symlink-to-outside": func(p string) { os.Symlink(outside, p) },
		"plain-file":         func(p string) { os.WriteFile(p, []byte("x"), 0o600) },
		"not-a-staging-name": func(p string) { os.Mkdir(p, 0o700); os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o600) },
		hex32:                func(p string) { os.Symlink(outside, p) },                                                            // staging-shaped but a symlink
		hex32b:               func(p string) { os.Mkdir(p, 0o755) },                                                                // staging-shaped, group/world readable
		hex32c:               func(p string) { os.Mkdir(p, 0o700); os.WriteFile(filepath.Join(p, "foreign"), []byte("x"), 0o600) }, // staging-shaped, never made by this store
	}
	for name, mk := range neighbours {
		mk(filepath.Join(staging, name))
	}
	*now = now.Add(StagingTTL + time.Minute)
	s.SweepExpired()
	s.SweepAll()
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("a symlink was followed: %v", err)
	}
	for _, name := range []string{"symlink-to-outside", "plain-file", "not-a-staging-name", hex32} {
		if _, err := os.Lstat(filepath.Join(staging, name)); err != nil {
			t.Errorf("neighbour %s was removed: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(s.KeysDir(), "kept-one", PrivateName)); err != nil {
		t.Fatalf("a promoted key was swept: %v", err)
	}
	// (staging-shaped REAL directories of our own user, hex32b and hex32c, are what SweepAll
	// exists to clear at start; everything that is not one is left alone)
	if ents, _ := os.ReadDir(staging); len(ents) != 4 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("staging holds %v: the expired own key must be gone and every foreign neighbour kept", names)
	}
	if _, err := s.Staged(k2.SHA256); err == nil {
		t.Fatal("an expired staged key can still be applied")
	}
}

// Symlink swaps are refused: a keys/ or keys-staging/ that is a symlink (to an
// attacker's directory), a keys/<id> that is a symlink, and a staging
// directory swapped for a symlink between Stage and Promote. In every case
// nothing is written outside the store, nothing is replaced, and the private
// key never lands in the foreign place.
func TestSymlinkSwapsInKeysAndStagingRefusedW3e(t *testing.T) {
	// keys-staging is a symlink: Stage refuses and writes nothing behind it
	s, _ := advStore(t)
	foreign := t.TempDir()
	if err := os.Symlink(foreign, filepath.Join(s.Root, stagingDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(); err == nil {
		t.Fatal("Stage worked through a symlinked keys-staging")
	}
	if ents, _ := os.ReadDir(foreign); len(ents) != 0 {
		t.Fatalf("something was written behind the symlink: %v", ents)
	}
	// keys is a symlink: Promote refuses, the staged key stays where it was
	s2, _ := advStore(t)
	k, err := s2.Stage()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(s2.Root, keysDirName)); err != nil {
		t.Fatal(err)
	}
	if err := s2.Promote(k.SHA256, "proj-one"); err == nil {
		t.Fatal("Promote worked through a symlinked keys dir")
	}
	if ents, _ := os.ReadDir(foreign); len(ents) != 0 {
		t.Fatalf("a key was moved behind the symlink: %v", ents)
	}
	// keys/<id> exists as a symlink to a foreign dir (or a real dir): never replaced, never entered
	s3, _ := advStore(t)
	k3, err := s3.Stage()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(s3.KeysDir(), 0o700)
	if err := os.Symlink(foreign, filepath.Join(s3.KeysDir(), "proj-two")); err != nil {
		t.Fatal(err)
	}
	if err := s3.Promote(k3.SHA256, "proj-two"); err == nil {
		t.Fatal("Promote replaced a symlink")
	}
	if ents, _ := os.ReadDir(foreign); len(ents) != 0 {
		t.Fatalf("a key was moved into the symlink target: %v", ents)
	}
	if fi, err := os.Lstat(filepath.Join(s3.KeysDir(), "proj-two")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	// the staged directory swapped for a symlink after Stage: Promote refuses and follows nothing
	s4, _ := advStore(t)
	k4, err := s4.Stage()
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Join(s4.Root, stagingDirName))
	if len(ents) != 1 {
		t.Fatal("staging layout")
	}
	dir := filepath.Join(s4.Root, stagingDirName, ents[0].Name())
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, dir); err != nil {
		t.Fatal(err)
	}
	if err := s4.Promote(k4.SHA256, "proj-three"); err == nil {
		t.Fatal("Promote followed a swapped staging directory")
	}
	if _, err := os.Lstat(filepath.Join(s4.KeysDir(), "proj-three")); err == nil {
		t.Fatal("a key appeared under keys/")
	}
	if _, err := s4.Staged(k4.SHA256); err == nil {
		t.Fatal("a swapped staging directory is still offered as a staged key")
	}
	if ents, _ := os.ReadDir(foreign); len(ents) != 0 {
		t.Fatalf("something was written behind the swapped symlink: %v", ents)
	}
	// every key is new and none can be applied twice or after its expiry
	s5, now := advStore(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		kk, err := s5.Stage()
		if err != nil {
			t.Fatal(err)
		}
		if seen[kk.SHA256] || seen[kk.Public] {
			t.Fatal("a key was offered twice")
		}
		seen[kk.SHA256], seen[kk.Public] = true, true
		if i%2 == 0 {
			if err := s5.Promote(kk.SHA256, "pj-"+strings.Repeat("a", 1+i)); err != nil {
				t.Fatal(err)
			}
			if _, err := s5.Staged(kk.SHA256); err == nil {
				t.Fatal("a promoted key is still staged (it could be applied to a second project)")
			}
		}
	}
	*now = now.Add(StagingTTL + time.Second)
	for h := range seen {
		if _, err := s5.Staged(h); err == nil {
			t.Fatalf("a key is stageable after expiry")
		}
	}
}
