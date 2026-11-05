package gitx

import (
	"reflect"
	"testing"

	"aigate/internal/gittest"
)

// plumbCommit builds a child commit of parent holding files via git
// plumbing (gittest.Plumb).
func plumbCommit(t *testing.T, repo, parent string, files map[string]string) string {
	t.Helper()
	return gittest.Plumb(t, repo, parent, files)
}

func TestTreePaths(t *testing.T) {
	r := newRepo(t)
	write(t, r, "tests/Run.js", "strong\n")
	write(t, r, "dir with space/한글.txt", "x\n")
	base := commit(t, r, "base")
	head := plumbCommit(t, r, base, map[string]string{"tests/run.js": "weak\n", "x/\x1b[31mZ": "c\n", "node_module\u017f/t/index.js": "\n"})
	got, err := TreePaths(r, head)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dir with space/한글.txt", "node_module\u017f/t/index.js", "tests/Run.js", "tests/run.js", "x/\x1b[31mZ"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TreePaths: got %q want %q", got, want)
	}
	if _, err := TreePaths(r, "nosuchref"); err == nil {
		t.Fatal("unknown sha must error")
	}
}

// Built-in guards are matched case-insensitively (":(glob,icase)"), user
// globs stay case-sensitive.
func TestPathspecICase(t *testing.T) {
	r := newRepo(t)
	write(t, r, "README", "r\n")
	base := commit(t, r, "base")
	added := []string{".NPMRC", "Package-Lock.json", "Pytest.ini", ".Gitattributes", "Node_Modules/x/i.js", "Tests/x.py", "tests/y.py"}
	files := map[string]string{}
	for _, f := range added {
		files[f] = "x\n"
	}
	head := plumbCommit(t, r, base, files)
	icase := ICaseGlobs(".npmrc", "package-lock.json", "pytest.ini", ".gitattributes", "**/node_modules/**")
	got, err := AddedMatching(r, base, head, icase)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".Gitattributes", ".NPMRC", "Node_Modules/x/i.js", "Package-Lock.json", "Pytest.ini"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("icase AddedMatching: got %q want %q", got, want)
	}
	d, err := ProtectedDiff(r, base, head, append(Globs("tests/**"), icase...))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".Gitattributes", ".NPMRC", "Node_Modules/x/i.js", "Package-Lock.json", "Pytest.ini", "tests/y.py"}; !reflect.DeepEqual(d.AddedFiles, want) {
		t.Fatalf("ProtectedDiff added: got %q want %q (user glob tests/** must not match Tests/x.py)", d.AddedFiles, want)
	}
	// the case-sensitive variant of the same globs does not see them
	cs, err := AddedMatching(r, base, head, Globs(".npmrc", "package-lock.json", "pytest.ini", ".gitattributes", "**/node_modules/**"))
	if err != nil || len(cs) != 0 {
		t.Fatalf("case-sensitive: got %q err %v", cs, err)
	}
}

// Switching a glob to icase does not change the hash of a diff that has no
// case variants: the hashed text depends only on the files matched.
func TestICaseKeepsHashWithoutVariants(t *testing.T) {
	r := newRepo(t)
	write(t, r, "pytest.ini", "[pytest]\n")
	write(t, r, "tests/a.py", "1\n2\n")
	base := commit(t, r, "base")
	write(t, r, "pytest.ini", "[pytest]\naddopts = -q\n")
	write(t, r, "tests/a.py", "1\n")
	write(t, r, "node_modules/x/i.js", "x\n")
	head := commit(t, r, "head")
	a, err := ProtectedDiff(r, base, head, Globs("tests/**", "pytest.ini", "**/node_modules/**"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProtectedDiff(r, base, head, append(Globs("tests/**"), ICaseGlobs("pytest.ini", "**/node_modules/**")...))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash || a.HashedText() != b.HashedText() {
		t.Fatalf("icase changed the hash of a diff without case variants: %s vs %s", a.Hash, b.Hash)
	}
}
