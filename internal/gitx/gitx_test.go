package gitx

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestHashPinnedAgainstConfigAndClone(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "1\n2\n")
	write(t, r, "tests/b.bin", "a\x00b")
	base := commit(t, r, "base")
	write(t, r, "tests/a.py", "1\n")
	write(t, r, "tests/b.bin", "a\x00c")
	head := commit(t, r, "head")
	pat := []string{"tests/**"}
	d1, err := ProtectedDiff(r, base, head, pat)
	if err != nil {
		t.Fatal(err)
	}
	git(t, r, "config", "core.abbrev", "4")
	git(t, r, "config", "diff.algorithm", "patience")
	d2, _ := ProtectedDiff(r, base, head, pat)
	if d1.Hash != d2.Hash {
		t.Errorf("hash depends on config: %s vs %s", d1.Hash, d2.Hash)
	}
	clone := filepath.Join(t.TempDir(), "c")
	git(t, r, "clone", "-q", r, clone)
	d3, err := ProtectedDiff(clone, base, head, pat)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Hash != d3.Hash {
		t.Errorf("hash differs in clone: %s vs %s", d1.Hash, d3.Hash)
	}
	// binary content change alters hash
	write(t, r, "tests/b.bin", "a\x00d")
	head2 := commit(t, r, "head2")
	d4, _ := ProtectedDiff(r, base, head2, pat)
	if d4.Hash == d1.Hash {
		t.Error("binary content change must change hash")
	}
}

func TestAddedMatching(t *testing.T) {
	r := newRepo(t)
	write(t, r, "README", "r\n")
	base := commit(t, r, "base")
	for _, f := range []string{"tests/a.py", "a.py", "b.py", "conftest.py", "x/y/conftest.py", "docs/내 파일 x.md"} {
		write(t, r, f, "x\n")
	}
	head := commit(t, r, "head")
	cases := []struct {
		globs []string
		want  []string
	}{
		{[]string{"tests"}, []string{"tests/a.py"}},
		{[]string{"tests/"}, []string{"tests/a.py"}},
		{[]string{"[!a]*.py"}, []string{"b.py", "conftest.py"}},
		{[]string{"**/conftest.py"}, []string{"conftest.py", "x/y/conftest.py"}},
		{[]string{"docs/*.md"}, []string{"docs/내 파일 x.md"}},
		{nil, nil},
	}
	for _, c := range cases {
		got, err := AddedMatching(r, base, head, c.globs)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q err %v want %q", c.globs, got, err, c.want)
		}
	}
	if _, err := AddedMatching(r, base, head, []string{"../outside"}); err == nil {
		t.Error("invalid pathspec must error")
	}
}

// C1: attributes from the main worktree (.gitattributes of the agent's head)
// or .git/info/attributes must not hide removed lines behind "Binary files
// differ".
func TestProtectedDiffIgnoresDiffAttributes(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/test_a.py", "def test_a():\n    assert add(1,2)==3\n")
	base := commit(t, r, "base")
	write(t, r, "tests/test_a.py", "def test_a():\n    assert True\n")
	write(t, r, ".gitattributes", "tests/** -diff\n")
	head := commit(t, r, "head")
	write(t, r, ".git/info/attributes", "tests/** binary\n")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.RemovedLines, []string{"    assert add(1,2)==3"}) {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
	if !reflect.DeepEqual(d.FilesWithRemovals, []string{"tests/test_a.py"}) {
		t.Errorf("FilesWithRemovals=%q", d.FilesWithRemovals)
	}
	if !strings.Contains(d.Text, "-    assert add(1,2)==3\n") || strings.Contains(d.Text, "Binary files") {
		t.Errorf("Text=%q", d.Text)
	}
}

func TestProtectedDiffDeletedEmptyFile(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/__init__.py", "")
	write(t, r, "tests/a.py", "x\n")
	base := commit(t, r, "base")
	if err := os.Remove(filepath.Join(r, "tests/__init__.py")); err != nil {
		t.Fatal(err)
	}
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.RemovedLines) != 0 {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
	if !reflect.DeepEqual(d.DeletedFiles, []string{"tests/__init__.py"}) {
		t.Errorf("DeletedFiles=%q", d.DeletedFiles)
	}
	if !reflect.DeepEqual(d.Changed(), []string{"tests/__init__.py"}) {
		t.Errorf("Changed=%q", d.Changed())
	}
}

func TestProtectedDiffBinaryAppendIsNonText(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/golden.bin", "a\x00b\n")
	base := commit(t, r, "base")
	write(t, r, "tests/golden.bin", "a\x00b\nc\x00d\n") // only an added "line"
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.RemovedLines) != 0 {
		t.Errorf("RemovedLines=%q", d.RemovedLines)
	}
	if !reflect.DeepEqual(d.ModifiedNonText, []string{"tests/golden.bin"}) {
		t.Errorf("ModifiedNonText=%q", d.ModifiedNonText)
	}
}

func TestProtectedDiffModeAndTypeChange(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/run.sh", "echo hi\n")
	write(t, r, "tests/data.txt", "d\n")
	base := commit(t, r, "base")
	if err := os.Chmod(filepath.Join(r, "tests/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r, "tests/data.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(r, "tests/data.txt")); err != nil {
		t.Fatal(err)
	}
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.ModifiedNonText, []string{"tests/data.txt", "tests/run.sh"}) {
		t.Errorf("ModifiedNonText=%q", d.ModifiedNonText)
	}
	if !strings.Contains(d.Text, "new mode 100755") {
		t.Errorf("mode change not in hashed text: %q", d.Text)
	}
}

func TestProtectedDiffPureTextAdditionIsNotAChange(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "one\n")
	base := commit(t, r, "base")
	write(t, r, "tests/a.py", "one\ntwo\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.RemovedLines) != 0 || len(d.Changed()) != 0 || len(d.FilesWithRemovals) != 0 {
		t.Errorf("pure addition flagged: %+v", d)
	}
	if !reflect.DeepEqual(d.TouchedFiles, []string{"tests/a.py"}) {
		t.Errorf("TouchedFiles=%q", d.TouchedFiles)
	}
}

func TestProtectedDiffHashedTextMatchesHash(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "one\n")
	base := commit(t, r, "base")
	write(t, r, "tests/a.py", "ONE\nadded\n")
	write(t, r, "tests/conftest.py", "import os\n")
	head := commit(t, r, "head")
	d, err := ProtectedDiff(r, base, head, []string{"tests/**"})
	if err != nil {
		t.Fatal(err)
	}
	ht := d.HashedText()
	sum := sha256.Sum256([]byte(ht))
	if hex.EncodeToString(sum[:])[:16] != d.Hash {
		t.Errorf("hash %s is not sha256(HashedText)", d.Hash)
	}
	for _, want := range []string{"+added\n", "+import os\n", "\nA tests/conftest.py\n"} {
		if !strings.Contains("\n"+ht, want) {
			t.Errorf("HashedText lacks %q:\n%s", want, ht)
		}
	}
}

func TestHashPinnedAgainstDiffConfig(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/a.py", "def f():\n    a\n\n    b\n\ndef g():\n    c\n")
	write(t, r, "tests/b.py", "1\n")
	base := commit(t, r, "base")
	write(t, r, "tests/a.py", "def f():\n    A\n\n    b\n\ndef g():\n    C\n")
	write(t, r, "tests/b.py", "2\n")
	head := commit(t, r, "head")
	pat := []string{"tests/**"}
	d1, err := ProtectedDiff(r, base, head, pat)
	if err != nil {
		t.Fatal(err)
	}
	order := filepath.Join(t.TempDir(), "order")
	if err := os.WriteFile(order, []byte("tests/b.py\ntests/a.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"diff.noprefix", "true"}, {"diff.mnemonicPrefix", "true"}, {"diff.interHunkContext", "10"},
		{"diff.indentHeuristic", "true"}, {"diff.orderFile", order}, {"diff.submodule", "log"}, {"diff.ignoreSubmodules", "all"}} {
		git(t, r, "config", kv[0], kv[1])
	}
	d2, err := ProtectedDiff(r, base, head, pat)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Hash != d2.Hash || d1.Text != d2.Text {
		t.Errorf("hash depends on diff config:\n%s\nvs\n%s", d1.Text, d2.Text)
	}
}

func TestWithWorktreeRunsNoHooks(t *testing.T) {
	r := newRepo(t)
	write(t, r, "f.txt", "v1\n")
	sha := commit(t, r, "c1")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	write(t, r, ".git/hooks/post-checkout", hook)
	if err := os.Chmod(filepath.Join(r, ".git/hooks/post-checkout"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WithWorktree(r, sha, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("post-checkout hook ran during worktree add")
	}
}

func TestWithWorktreeNotInsideRepoEvenIfTMPDIRIs(t *testing.T) {
	r, err := filepath.EvalSymlinks(newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	write(t, r, "f.txt", "v1\n")
	sha := commit(t, r, "c1")
	inside := filepath.Join(r, "tmp")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", inside)
	var dir string
	if err := WithWorktree(r, sha, func(d string) error { dir = d; return nil }); err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(r, dir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("worktree %s is inside the repo %s", dir, r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir remains: %v", err)
	}
}
