package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const canaryValue = "canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"

func newTestSecretStore(t *testing.T) *SecretStore {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &SecretStore{Dir: dir}
}

func TestSecretStorePutGetDeleteModes(t *testing.T) {
	s := newTestSecretStore(t)
	if err := s.Put("gitlab-main", canaryValue); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(s.Dir, "gitlab-main"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o400 || !fi.Mode().IsRegular() {
		t.Fatalf("secret file mode %v, want regular 0400", fi.Mode())
	}
	di, _ := os.Lstat(s.Dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode %v", di.Mode().Perm())
	}
	got, ok := s.Get("gitlab-main")
	if !ok || got != canaryValue {
		t.Fatalf("Get = %q, %v: value must round-trip exactly", got, ok)
	}
	// replacing keeps the mode
	if err := s.Put("gitlab-main", canaryValue+"2"); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Lstat(filepath.Join(s.Dir, "gitlab-main"))
	if fi.Mode().Perm() != 0o400 {
		t.Fatalf("replaced file mode %v", fi.Mode().Perm())
	}
	if got, _ := s.Get("gitlab-main"); got != canaryValue+"2" {
		t.Fatal("replace did not take")
	}
	if refs := s.Refs(); len(refs) != 1 || refs[0] != "gitlab-main" {
		t.Fatalf("Refs = %v", refs)
	}
	if err := s.Delete("gitlab-main"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("gitlab-main"); ok {
		t.Fatal("deleted value still readable")
	}
	for _, bad := range []string{"", "../x", "A", "a/b", ".state-x", "-a"} {
		if s.Put(bad, canaryValue) == nil {
			t.Errorf("Put accepted the name %q", bad)
		}
		if _, ok := s.Get(bad); ok {
			t.Errorf("Get answered the name %q", bad)
		}
	}
}

func TestSecretStoreAtomicOnFailure(t *testing.T) {
	for _, stage := range []string{"temp-written", "synced"} {
		t.Run(stage, func(t *testing.T) {
			s := newTestSecretStore(t)
			if err := s.Put("openai", "old-value-0123456789012345678901234567"); err != nil {
				t.Fatal(err)
			}
			s.opts.failAt = func(st string) error {
				if st == stage {
					return errors.New("injected")
				}
				return nil
			}
			err := s.Put("openai", canaryValue)
			if err == nil {
				t.Fatal("Put hid the injected failure")
			}
			if strings.Contains(err.Error(), canaryValue) {
				t.Fatal("error carries the value")
			}
			s.opts.failAt = nil
			if got, ok := s.Get("openai"); !ok || got != "old-value-0123456789012345678901234567" {
				t.Fatalf("previous value not intact: %q %v", got, ok)
			}
			entries, _ := os.ReadDir(s.Dir)
			if len(entries) != 1 {
				t.Fatalf("temp file left behind: %v", entries)
			}
		})
	}
	// a first write that fails leaves nothing
	s := newTestSecretStore(t)
	s.opts.failAt = func(st string) error { return errors.New("injected") }
	if s.Put("openai", canaryValue) == nil {
		t.Fatal("no error")
	}
	if entries, _ := os.ReadDir(s.Dir); len(entries) != 0 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestSecretStoreRefusesSymlinkAndWrongOwnerOrMode(t *testing.T) {
	s := newTestSecretStore(t)
	for _, r := range []string{"good", "badmode", "linked", "other"} {
		if err := s.Put(r, canaryValue+r); err != nil {
			t.Fatal(err)
		}
	}
	// wrong mode
	if err := os.Chmod(filepath.Join(s.Dir, "badmode"), 0o440); err != nil {
		t.Fatal(err)
	}
	// symlink to a perfectly fine file elsewhere
	target := filepath.Join(filepath.Dir(s.Dir), "elsewhere")
	if err := os.WriteFile(target, []byte(canaryValue), 0o400); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(s.Dir, "linked"))
	if err := os.Symlink(target, filepath.Join(s.Dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("badmode"); ok {
		t.Error("0440 file was trusted")
	}
	if _, ok := s.Get("linked"); ok {
		t.Error("symlink was followed")
	}
	// the others keep working
	if v, ok := s.Get("good"); !ok || v != canaryValue+"good" {
		t.Error("an untrusted sibling broke a good token")
	}
	if v, ok := s.Get("other"); !ok || v != canaryValue+"other" {
		t.Error("an untrusted sibling broke another token")
	}
	if st := s.Lookup("badmode"); st != SecretUntrusted {
		t.Errorf("badmode state %v", st)
	}
	if st := s.Lookup("linked"); st != SecretUntrusted {
		t.Errorf("linked state %v", st)
	}
	if st := s.Lookup("nothere"); st != SecretAbsent {
		t.Errorf("absent state %v", st)
	}
	// wrong owner: a store that distrusts our own uid sees nothing trusted
	me := uint32(os.Geteuid())
	w := &SecretStore{Dir: s.Dir, opts: RegistryOptions{fileOwnerOK: func(uid uint32) bool { return uid != me }}}
	if _, ok := w.Get("good"); ok {
		t.Error("a file of an untrusted owner was trusted")
	}
	// writing over an untrusted target: Put replaces it by rename (the file is
	// the broker's to replace); afterwards it is trusted again.
	if err := s.Put("badmode", canaryValue+"fixed"); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Get("badmode"); !ok || v != canaryValue+"fixed" {
		t.Error("Put did not repair a bad-mode file")
	}
	// the symlink target is never written through
	if err := s.Put("linked", canaryValue+"new"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != canaryValue {
		t.Error("Put wrote through a symlink")
	}
	// an untrusted directory makes the whole store unavailable
	if err := os.Chmod(s.Dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("good"); ok {
		t.Error("group-accessible secrets dir was trusted")
	}
	if s.Put("good", canaryValue) == nil {
		t.Error("Put into an untrusted dir succeeded")
	}
	os.Chmod(s.Dir, 0o700)
	// a secrets dir that is a symlink is refused too
	link := s.Dir + "-link"
	os.Symlink(s.Dir, link)
	if _, ok := (&SecretStore{Dir: link}).Get("good"); ok {
		t.Error("symlinked secrets dir was trusted")
	}
	_ = syscall.O_NOFOLLOW
}

func TestSecretStoreDeleteOverwritesThenRemoves(t *testing.T) {
	s := newTestSecretStore(t)
	if err := s.Put("gitlab-a", canaryValue); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir, "gitlab-a")
	// keep a second name for the inode: it shows what was left in the old file
	keep := filepath.Join(filepath.Dir(s.Dir), "inode-keeper")
	if err := os.Link(p, keep); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("gitlab-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still there after delete: %v", err)
	}
	left, _ := os.ReadFile(keep)
	if strings.Contains(string(left), canaryValue) || strings.Contains(string(left), "canary") {
		t.Fatalf("old content survived in the inode: %q", left)
	}
	if err := s.Delete("gitlab-a"); err != nil {
		t.Fatalf("deleting an absent token must be a no-op: %v", err)
	}
	if err := s.Put("gitlab-a", canaryValue+"again"); err != nil {
		t.Fatalf("name cannot be recreated: %v", err)
	}
	if v, _ := s.Get("gitlab-a"); v != canaryValue+"again" {
		t.Fatal("recreated value wrong")
	}
	// a symlink is removed, never written through
	target := filepath.Join(filepath.Dir(s.Dir), "tgt")
	os.WriteFile(target, []byte("keepme"), 0o600)
	os.Symlink(target, filepath.Join(s.Dir, "lnk"))
	if err := s.Delete("lnk"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keepme" {
		t.Fatal("Delete wrote through a symlink")
	}
}
