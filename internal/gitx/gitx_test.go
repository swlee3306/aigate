package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	return dir
}

func write(t *testing.T, repo, name, content string) {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, msg string) string {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", msg)
	return git(t, repo, "rev-parse", "HEAD")
}

func TestProtectedDiffModify(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a_test.py", "one\ntwo\nthree\n")
	write(t, r, "src/x.py", "x\n")
	base := commit(t, r, "base")
	write(t, r, "tests/a_test.py", "one\nthree\nfour\n")
	write(t, r, "src/x.py", "y\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.RemovedLines, []string{"two"}) {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
	if !reflect.DeepEqual(d.FilesWithRemovals, []string{"tests/a_test.py"}) {
		t.Errorf("FilesWithRemovals=%q", d.FilesWithRemovals)
	}
	if !reflect.DeepEqual(d.TouchedFiles, []string{"tests/a_test.py"}) {
		t.Errorf("TouchedFiles=%q", d.TouchedFiles)
	}
	if len(d.AddedFiles) != 0 {
		t.Errorf("AddedFiles=%q", d.AddedFiles)
	}
}

func TestProtectedDiffAddOnly(t *testing.T) {
	r := newRepo(t)
	write(t, r, "README", "r\n")
	base := commit(t, r, "base")
	write(t, r, "tests/new_test.py", "a\nb\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.RemovedLines) != 0 || len(d.FilesWithRemovals) != 0 {
		t.Errorf("unexpected removals %+v", d)
	}
	if !reflect.DeepEqual(d.AddedFiles, []string{"tests/new_test.py"}) {
		t.Errorf("AddedFiles=%q", d.AddedFiles)
	}
}

func TestProtectedDiffDeletedFile(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "l1\nl2\n")
	base := commit(t, r, "base")
	if err := os.Remove(filepath.Join(r, "tests/a.py")); err != nil {
		t.Fatal(err)
	}
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.RemovedLines, []string{"l1", "l2"}) {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
	if !reflect.DeepEqual(d.FilesWithRemovals, []string{"tests/a.py"}) {
		t.Errorf("FilesWithRemovals=%q", d.FilesWithRemovals)
	}
}

func TestProtectedDiffRemovedLineStartingWithDashes(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "keep\n-- sql comment\n")
	base := commit(t, r, "base")
	write(t, r, "tests/a.py", "keep\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.RemovedLines, []string{"-- sql comment"}) {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
}

func TestProtectedDiffRenameIsDeleteAndAdd(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/old.py", "a\nb\nc\nd\n")
	base := commit(t, r, "base")
	git(t, r, "mv", "tests/old.py", "tests/new.py")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.RemovedLines, []string{"a", "b", "c", "d"}) {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
	if !reflect.DeepEqual(d.AddedFiles, []string{"tests/new.py"}) {
		t.Errorf("AddedFiles=%q", d.AddedFiles)
	}
	if !reflect.DeepEqual(d.TouchedFiles, []string{"tests/new.py", "tests/old.py"}) {
		t.Errorf("TouchedFiles=%q", d.TouchedFiles)
	}
}

func TestProtectedDiffSpacesAndKorean(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/my file.py", "a\nb\n")
	write(t, r, "tests/한글 테스트.py", "가\n나\n")
	base := commit(t, r, "base")
	write(t, r, "tests/my file.py", "a\n")
	write(t, r, "tests/한글 테스트.py", "가\n")
	write(t, r, "tests/새 파일.py", "x\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tests/my file.py", "tests/한글 테스트.py"}
	if !reflect.DeepEqual(d.FilesWithRemovals, want) {
		t.Errorf("FilesWithRemovals=%q", d.FilesWithRemovals)
	}
	if !reflect.DeepEqual(d.AddedFiles, []string{"tests/새 파일.py"}) {
		t.Errorf("AddedFiles=%q", d.AddedFiles)
	}
	if !reflect.DeepEqual(d.RemovedLines, []string{"b", "나"}) && !reflect.DeepEqual(d.RemovedLines, []string{"나", "b"}) {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
}

func TestProtectedDiffBinaryRemoval(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/data.bin", "a\x00b\x00c")
	base := commit(t, r, "base")
	write(t, r, "tests/data.bin", "z\x00y")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.FilesWithRemovals, []string{"tests/data.bin"}) {
		t.Errorf("FilesWithRemovals=%q", d.FilesWithRemovals)
	}
}

func TestProtectedDiffGlobStarStar(t *testing.T) {
	r := newRepo(t)
	write(t, r, "conftest.py", "a\n")
	write(t, r, "pkg/sub/conftest.py", "a\n")
	write(t, r, "pkg/other.py", "a\n")
	base := commit(t, r, "base")
	write(t, r, "conftest.py", "b\n")
	write(t, r, "pkg/sub/conftest.py", "b\n")
	write(t, r, "pkg/other.py", "b\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"**/conftest.py"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"conftest.py", "pkg/sub/conftest.py"}
	if !reflect.DeepEqual(d.TouchedFiles, want) {
		t.Errorf("TouchedFiles=%q", d.TouchedFiles)
	}
}

func TestProtectedDiffHash(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "1\n2\n")
	base := commit(t, r, "base")
	write(t, r, "tests/a.py", "1\n")
	h1 := commit(t, r, "h1")
	pat := []string{"tests/**"}
	a, err := ProtectedDiff(r, base, h1, pat)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ProtectedDiff(r, base, h1, pat)
	if a.Hash != b.Hash || len(a.Hash) != 16 {
		t.Errorf("hash unstable or wrong length: %q %q", a.Hash, b.Hash)
	}
	// different diff -> different hash
	git(t, r, "checkout", "-q", base)
	write(t, r, "tests/a.py", "2\n")
	h2 := commit(t, r, "h2")
	c, _ := ProtectedDiff(r, base, h2, pat)
	if c.Hash == a.Hash {
		t.Error("hash should differ for different diff")
	}
	// adding only an empty file changes hash
	git(t, r, "checkout", "-q", h1)
	write(t, r, "tests/empty.py", "")
	h3 := commit(t, r, "h3")
	d1, _ := ProtectedDiff(r, base, h1, pat)
	d2, err := ProtectedDiff(r, base, h3, pat)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Hash == d2.Hash {
		t.Error("adding an empty file must change the hash")
	}
	if !reflect.DeepEqual(d2.AddedFiles, []string{"tests/empty.py"}) {
		t.Errorf("AddedFiles=%q", d2.AddedFiles)
	}
}

func TestDirty(t *testing.T) {
	r := newRepo(t)
	write(t, r, "a.txt", "a\n")
	write(t, r, ".gitignore", "ignored.log\n")
	commit(t, r, "base")
	if s, err := Dirty(r); err != nil || s != "" {
		t.Fatalf("clean repo: %q %v", s, err)
	}
	write(t, r, "ignored.log", "x")
	write(t, r, ".aigate/state.json", "{}")
	if s, err := Dirty(r); err != nil || s != "" {
		t.Fatalf("ignored/.aigate should not be dirty: %q %v", s, err)
	}
	write(t, r, "new.txt", "n")
	if s, _ := Dirty(r); !strings.Contains(s, "new.txt") {
		t.Errorf("untracked not flagged: %q", s)
	}
	os.Remove(filepath.Join(r, "new.txt"))
	write(t, r, "a.txt", "changed\n")
	if s, _ := Dirty(r); !strings.Contains(s, "a.txt") {
		t.Errorf("modified not flagged: %q", s)
	}
}

func TestWithWorktree(t *testing.T) {
	r := newRepo(t)
	write(t, r, "f.txt", "v1\n")
	sha := commit(t, r, "c1")
	var dir string
	err := WithWorktree(r, sha, func(d string) error {
		dir = d
		b, err := os.ReadFile(filepath.Join(d, "f.txt"))
		if err != nil || string(b) != "v1\n" {
			t.Errorf("content %q %v", b, err)
		}
		if strings.HasPrefix(d, r) {
			t.Errorf("worktree inside repo: %s", d)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir remains: %v", err)
	}
	if out := git(t, r, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktree registered: %s", out)
	}
}

func TestWithWorktreeErrorAndPanic(t *testing.T) {
	r := newRepo(t)
	write(t, r, "f.txt", "v1\n")
	sha := commit(t, r, "c1")
	var dir string
	sentinel := os.ErrDeadlineExceeded
	err := WithWorktree(r, sha, func(d string) error { dir = d; return sentinel })
	if err != sentinel {
		t.Errorf("err=%v", err)
	}
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Error("dir remains after error")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic not re-raised")
			}
		}()
		_ = WithWorktree(r, sha, func(d string) error { dir = d; panic("boom") })
	}()
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Error("dir remains after panic")
	}
	if out := git(t, r, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktree registered: %s", out)
	}
}

func TestResolveMergeBaseAncestorShow(t *testing.T) {
	r := newRepo(t)
	write(t, r, "cfg.yml", "k: 1\n")
	c1 := commit(t, r, "c1")
	write(t, r, "cfg.yml", "k: 2\n")
	c2 := commit(t, r, "c2")
	got, err := Resolve(r, "HEAD")
	if err != nil || got != c2 {
		t.Fatalf("Resolve=%q %v", got, err)
	}
	if _, err := Resolve(r, "nope-ref"); err == nil || !strings.Contains(err.Error(), "nope-ref") {
		t.Errorf("want error with ref, got %v", err)
	}
	tree := git(t, r, "rev-parse", "HEAD^{tree}")
	if _, err := Resolve(r, tree); err == nil {
		t.Error("tree object must be rejected")
	}
	if _, err := Resolve(r, "--all"); err == nil {
		t.Error("option-like ref must be rejected")
	}
	mb, err := MergeBase(r, c1, c2)
	if err != nil || mb != c1 {
		t.Errorf("MergeBase=%q %v", mb, err)
	}
	if ok, err := IsAncestor(r, c1, c2); !ok || err != nil {
		t.Errorf("IsAncestor(c1,c2)=%v %v", ok, err)
	}
	if ok, err := IsAncestor(r, c2, c1); ok || err != nil {
		t.Errorf("IsAncestor(c2,c1)=%v %v", ok, err)
	}
	if _, err := IsAncestor(r, "bogus", c1); err == nil {
		t.Error("bad ref should error")
	}
	b, err := Show(r, c1, "cfg.yml")
	if err != nil || string(b) != "k: 1\n" {
		t.Errorf("Show=%q %v", b, err)
	}
	if _, err := Show(r, c1, "missing.yml"); err == nil {
		t.Error("missing file should error")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"tests/**", "tests/a/b.py", true},
		{"tests/**", "tests/a.py", true},
		{"tests/**", "tests", false},
		{"tests/**", "testsx/a", false},
		{"**/conftest.py", "conftest.py", true},
		{"**/conftest.py", "a/b/conftest.py", true},
		{"**/conftest.py", "a/conftest.pyc", false},
		{"*.py", "a.py", true},
		{"*.py", "a/b.py", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"**", "x/y", true},
		{"src/*/x.go", "src/a/x.go", true},
		{"src/*/x.go", "src/a/b/x.go", false},
		{"tests/한글*.py", "tests/한글 테스트.py", true},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pat, c.path); got != c.want {
			t.Errorf("MatchGlob(%q,%q)=%v want %v", c.pat, c.path, got, c.want)
		}
	}
}
