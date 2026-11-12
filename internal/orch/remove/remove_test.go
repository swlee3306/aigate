package remove

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "rm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mkClone(t *testing.T, root, id string) string {
	t.Helper()
	p := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(p, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestRemoveOnlyUnderRepoRootsOneLevel(t *testing.T) {
	root := realTemp(t)
	other := realTemp(t)
	deep := filepath.Join(root, "group")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	mkClone(t, deep, "nested")
	mkClone(t, other, "outside")
	mkClone(t, root, "good")
	mkClone(t, root, "other")
	cases := []struct {
		name, path, id string
	}{
		{"two levels deep", filepath.Join(deep, "nested"), "nested"},
		{"outside every root", filepath.Join(other, "outside"), "outside"},
		{"the root itself", root, filepath.Base(root)},
		{"name differs from id", filepath.Join(root, "other"), "good"},
		{"dotdot in path", root + "/good/../good", "good"},
		{"relative path", "good", "good"},
		{"trailing slash", filepath.Join(root, "good") + "/", "good"},
		{"empty id", filepath.Join(root, "good"), ""},
		{"id with slash", filepath.Join(root, "good"), "a/good"},
		{"id upper case", filepath.Join(root, "Good"), "Good"},
		{"staging name", filepath.Join(root, ".deleting-0123456789abcdef"), ".deleting-0123456789abcdef"},
		{"clone staging name", filepath.Join(root, ".clone-0123456789abcdef"), ".clone-0123456789abcdef"},
	}
	for _, c := range cases {
		if _, err := Clone([]string{root}, c.path, c.id); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%s: err = %v, want ErrUnsafe", c.name, err)
		}
	}
	for _, p := range []string{filepath.Join(deep, "nested"), filepath.Join(other, "outside"), filepath.Join(root, "good"), filepath.Join(root, "other")} {
		if !exists(p) {
			t.Errorf("%s was removed by a refused call", p)
		}
	}
	// a configured root that is not clean or not absolute removes nothing
	for _, roots := range [][]string{{}, {"relative"}, {root + "/"}, {root + "/../" + filepath.Base(root)}} {
		if _, err := Clone(roots, filepath.Join(root, "good"), "good"); !errors.Is(err, ErrUnsafe) {
			t.Errorf("roots %v: err = %v, want ErrUnsafe", roots, err)
		}
	}
	// the one accepted shape
	out, err := Clone([]string{other, root}, filepath.Join(root, "good"), "good")
	if err != nil || out != Removed {
		t.Fatalf("good: %v %v", out, err)
	}
	if exists(filepath.Join(root, "good")) || !exists(filepath.Join(root, "other")) {
		t.Fatal("the wrong folder was removed")
	}
}

func TestRemoveRefusesSymlinkAndSwap(t *testing.T) {
	root := realTemp(t)
	victim := realTemp(t)
	if err := os.WriteFile(filepath.Join(victim, "precious"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(victim, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 1. the project folder is a symlink to a directory that looks like a clone
	if err := os.Symlink(victim, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := Clone([]string{root}, filepath.Join(root, "linked"), "linked"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlink: %v", err)
	}
	// 2. a regular file, 3. no .git, 4. .git is a file (worktree pointer), 5. .git is a symlink
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nogit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "gitfile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gitfile", ".git"), []byte("gitdir: /elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "gitlink"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, ".git"), filepath.Join(root, "gitlink", ".git")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"afile", "nogit", "gitfile", "gitlink"} {
		if _, err := Clone([]string{root}, filepath.Join(root, id), id); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%s: %v", id, err)
		}
		if !exists(filepath.Join(root, id)) {
			t.Errorf("%s removed", id)
		}
	}
	// 6. swapped for a symlink between the checks and the rename: the symlink
	// is moved (never followed), the check after the rename notices, nothing
	// behind the link is touched, and the link is put back
	mkClone(t, root, "swap")
	s := &seams{afterCheck: func() {
		os.RemoveAll(filepath.Join(root, "swap"))
		os.Symlink(victim, filepath.Join(root, "swap"))
	}}
	if _, err := removeClone([]string{root}, filepath.Join(root, "swap"), "swap", s); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("swap: %v", err)
	}
	if !exists(filepath.Join(victim, "precious")) || !exists(filepath.Join(victim, ".git")) {
		t.Fatal("the swapped-in link target was touched")
	}
	if fi, err := os.Lstat(filepath.Join(root, "swap")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the swapped-in link was not put back")
	}
	// 7. replaced by ANOTHER real directory with a .git between check and rename:
	// SameFile differs, so it is not deleted
	mkClone(t, root, "swap2")
	s = &seams{afterCheck: func() {
		os.RemoveAll(filepath.Join(root, "swap2"))
		mkClone(t, root, "swap2")
		os.WriteFile(filepath.Join(root, "swap2", "new.txt"), []byte("n"), 0o644)
	}}
	if _, err := removeClone([]string{root}, filepath.Join(root, "swap2"), "swap2", s); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("swap2: %v", err)
	}
	if !exists(filepath.Join(root, "swap2", "new.txt")) {
		t.Fatal("the replacement directory was deleted or not put back")
	}
	for _, n := range names(t, root) {
		if strings.HasPrefix(n, StagePrefix) {
			t.Errorf("a staging directory %s was left behind", n)
		}
	}
	// the key area follows the same rule
	keys := realTemp(t)
	os.Chmod(keys, 0o700)
	os.Symlink(victim, filepath.Join(keys, "kl"))
	if _, err := KeyDir(keys, "kl"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("key symlink: %v", err)
	}
	if !exists(filepath.Join(victim, "precious")) {
		t.Fatal("key symlink target touched")
	}
}

func TestRemoveRenamesThenDeletes(t *testing.T) {
	root := realTemp(t)
	p := mkClone(t, root, "proj")
	// at the moment of deletion the project name is already free and the
	// content sits under a .deleting-<random> name
	var seen []string
	s := &seams{removeAll: func(path string) error {
		seen = names(t, root)
		if exists(p) {
			t.Error("the project name still exists when RemoveAll runs")
		}
		if filepath.Dir(path) != root || !strings.HasPrefix(filepath.Base(path), StagePrefix) {
			t.Errorf("RemoveAll was given %q", path)
		}
		return os.RemoveAll(path)
	}}
	out, err := removeClone([]string{root}, p, "proj", s)
	if err != nil || out != Removed {
		t.Fatalf("%v %v", out, err)
	}
	if len(seen) != 1 || !strings.HasPrefix(seen[0], StagePrefix) {
		t.Fatalf("directory listing at deletion time: %v", seen)
	}
	if got := names(t, root); len(got) != 0 {
		t.Fatalf("left behind: %v", got)
	}
	// idempotent: gone already is success, nothing else is touched
	other := mkClone(t, root, "other")
	out, err = Clone([]string{root}, p, "proj")
	if err != nil || out != AlreadyGone {
		t.Fatalf("second call: %v %v", out, err)
	}
	if !exists(other) {
		t.Fatal("an unrelated folder was touched")
	}
	// a failing delete after the rename is a partial state, reported as such,
	// and the project name stays free (no half-deleted folder under it)
	mkClone(t, root, "half")
	s = &seams{removeAll: func(string) error { return errors.New("boom") }}
	out, err = removeClone([]string{root}, filepath.Join(root, "half"), "half", s)
	if !errors.Is(err, ErrPartial) || out != Remaining {
		t.Fatalf("partial: %v %v", out, err)
	}
	if exists(filepath.Join(root, "half")) {
		t.Fatal("a partly deleted folder is still under the project name")
	}
	// a rename that fails changes nothing and says so
	mkClone(t, root, "stuck")
	ro := realTemp(t)
	mkClone(t, ro, "stuck")
	os.Chmod(ro, 0o500)
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	if os.Geteuid() != 0 {
		if _, err := Clone([]string{ro}, filepath.Join(ro, "stuck"), "stuck"); !errors.Is(err, ErrNotRemoved) {
			t.Fatalf("read-only root: %v", err)
		}
		if !exists(filepath.Join(ro, "stuck", ".git")) {
			t.Fatal("changed despite the failed rename")
		}
	}
}

func TestStartupSweepOnlyOwnDeleting(t *testing.T) {
	root := realTemp(t)
	keys := realTemp(t)
	victim := realTemp(t)
	os.WriteFile(filepath.Join(victim, "precious"), []byte("keep"), 0o644)
	own := StagePrefix + "0123456789abcdef"
	for _, d := range []string{root, keys} {
		if err := os.MkdirAll(filepath.Join(d, own, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, own, "sub", "f"), []byte("x"), 0o644)
	}
	// things that are NOT ours to remove
	os.Symlink(victim, filepath.Join(root, StagePrefix+"fedcba9876543210")) // symlink with a matching name
	os.WriteFile(filepath.Join(root, StagePrefix+"aaaaaaaaaaaaaaaa"), []byte("f"), 0o644)
	os.MkdirAll(filepath.Join(root, StagePrefix+"short"), 0o755)                       // wrong name shape
	os.MkdirAll(filepath.Join(root, StagePrefix+"0123456789abcdeg"), 0o755)            // non hex character
	os.MkdirAll(filepath.Join(root, "project", StagePrefix+"0123456789abcdef"), 0o755) // one level too deep
	os.MkdirAll(filepath.Join(root, "project2"), 0o755)                                // a normal folder
	os.MkdirAll(filepath.Join(root, ".clone-0123456789abcdef"), 0o755)                 // the clone sweep's, not ours
	os.MkdirAll(filepath.Join(root, "x"+StagePrefix+"0123456789abcdef"), 0o755)        // prefix not at the start
	n := Sweep([]string{root, keys, "", "relative", filepath.Join(root, "nonexistent")})
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	if exists(filepath.Join(root, own)) || exists(filepath.Join(keys, own)) {
		t.Fatal("an own .deleting directory was kept")
	}
	if !exists(filepath.Join(victim, "precious")) {
		t.Fatal("the symlink was followed")
	}
	for _, keep := range []string{StagePrefix + "fedcba9876543210", StagePrefix + "aaaaaaaaaaaaaaaa", StagePrefix + "short",
		StagePrefix + "0123456789abcdeg", filepath.Join("project", StagePrefix+"0123456789abcdef"), "project2",
		".clone-0123456789abcdef", "x" + StagePrefix + "0123456789abcdef"} {
		if !exists(filepath.Join(root, keep)) {
			t.Errorf("%s was removed by the sweep", keep)
		}
	}
}

func TestKeyDirRemoved(t *testing.T) {
	keys := realTemp(t)
	if err := os.Chmod(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"app", "app2"} {
		if err := os.MkdirAll(filepath.Join(keys, id), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(keys, id, "id_ed25519"), []byte("PRIVATE"), 0o600)
		os.WriteFile(filepath.Join(keys, id, "id_ed25519.pub"), []byte("ssh-ed25519 AAAA"), 0o644)
	}
	out, err := KeyDir(keys, "app")
	if err != nil || out != Removed {
		t.Fatalf("%v %v", out, err)
	}
	if exists(filepath.Join(keys, "app")) || !exists(filepath.Join(keys, "app2", "id_ed25519")) {
		t.Fatal("wrong key folder removed")
	}
	if got := names(t, keys); len(got) != 1 || got[0] != "app2" {
		t.Fatalf("left behind: %v", got)
	}
	// idempotent, also without any keys directory at all
	if out, err := KeyDir(keys, "app"); err != nil || out != AlreadyGone {
		t.Fatalf("again: %v %v", out, err)
	}
	if out, err := KeyDir(filepath.Join(keys, "missing"), "app"); err != nil || out != AlreadyGone {
		t.Fatalf("no keys dir: %v %v", out, err)
	}
	// refusals: not a plain id, keys dir not clean, a regular file named like a key folder
	for _, c := range [][2]string{{keys, ".."}, {keys, "a/b"}, {keys, ""}, {keys, "App"}, {"relative", "app"}, {keys + "/", "app"}} {
		if _, err := KeyDir(c[0], c[1]); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%v: %v", c, err)
		}
	}
	os.WriteFile(filepath.Join(keys, "file"), []byte("x"), 0o600)
	if _, err := KeyDir(keys, "file"); !errors.Is(err, ErrUnsafe) {
		t.Errorf("file: %v", err)
	}
	if !exists(filepath.Join(keys, "app2", "id_ed25519")) || !exists(filepath.Join(keys, "file")) {
		t.Fatal("refused call changed something")
	}
}

func TestCheckChangesNothing(t *testing.T) {
	root := realTemp(t)
	p := mkClone(t, root, "proj")
	have, err := CheckClone([]string{root}, p, "proj")
	if err != nil || !have {
		t.Fatalf("%v %v", have, err)
	}
	have, err = CheckClone([]string{root}, filepath.Join(root, "none"), "none")
	if err != nil || have {
		t.Fatalf("absent: %v %v", have, err)
	}
	os.MkdirAll(filepath.Join(root, "nogit"), 0o755)
	if _, err := CheckClone([]string{root}, filepath.Join(root, "nogit"), "nogit"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("nogit: %v", err)
	}
	if got := names(t, root); len(got) != 2 {
		t.Fatalf("check changed the directory: %v", got)
	}
}

func TestProductionCannotEnableRemoveSeams(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(e.Name())
		src := string(b)
		if strings.Contains(src, "&seams{") || strings.Contains(src, "seams{}") {
			t.Errorf("%s builds a seams value", e.Name())
		}
		// the exported entry points pass nil
		for _, call := range []string{"removeClone(", "removeKeyDir("} {
			for _, line := range strings.Split(src, "\n") {
				if strings.Contains(line, call) && strings.Contains(line, "return ") && !strings.Contains(line, ", nil)") {
					t.Errorf("%s: %s", e.Name(), strings.TrimSpace(line))
				}
			}
		}
	}
}
