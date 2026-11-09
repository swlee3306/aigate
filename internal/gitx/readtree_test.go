package gitx

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitStdin is git with stdin (hash-object --literally).
func gitStdin(t *testing.T, repo string, in []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@e.com"}, args...)...)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type rawEnt struct{ mode, name, sha string }

// rawHead makes a commit on a tree holding exactly ents (names written
// literally, so odd ones git mktree would refuse still go in) and moves HEAD.
func rawHead(t *testing.T, repo string, ents []rawEnt) string {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range ents {
		sha, _ := hex.DecodeString(e.sha)
		fmt.Fprintf(&buf, "%s %s\x00", e.mode, e.name)
		buf.Write(sha)
	}
	tree := gitStdin(t, repo, buf.Bytes(), "hash-object", "-t", "tree", "-w", "--literally", "--stdin")
	c := git(t, repo, "commit-tree", "-m", "raw", tree)
	git(t, repo, "update-ref", "HEAD", c)
	return c
}

func blobOf(t *testing.T, repo, content string) string {
	return gitStdin(t, repo, []byte(content), "hash-object", "-w", "--stdin")
}

func emptyRepo(t *testing.T) string {
	d, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, d, "init", "-q", "-b", "main")
	return d
}

func TestReadHeadCommit(t *testing.T) {
	d := emptyRepo(t)
	if _, err := ReadHeadCommit(context.Background(), d); !errors.Is(err, ErrNoHead) {
		t.Fatalf("unborn: %v", err)
	}
	git(t, d, "commit", "-q", "--allow-empty", "-m", "x")
	got, err := ReadHeadCommit(context.Background(), d)
	if err != nil || got != git(t, d, "rev-parse", "HEAD") || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(got) {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ReadHeadCommit(context.Background(), t.TempDir()); err == nil || errors.Is(err, ErrNoHead) {
		t.Fatalf("not a repo: %v", err)
	}
}

func TestListTreeEntriesAndOddNames(t *testing.T) {
	d := emptyRepo(t)
	b := blobOf(t, d, "hello\n")
	link := blobOf(t, d, "../outside")
	sub := "1234567890123456789012345678901234567890"
	names := []string{"a b", "tab\there", "new\nline", "-dash", "--output=x", "sp ace/ nested.txt", "unié"}
	var ents []rawEnt
	for _, n := range names {
		ents = append(ents, rawEnt{"100644", n, b})
	}
	ents = append(ents, rawEnt{"100755", "run.sh", b}, rawEnt{"120000", "link", link}, rawEnt{"160000", "vendor", sub})
	head := rawHead(t, d, ents)
	got, err := ListTree(context.Background(), d, head)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]TreeEntry{}
	for _, e := range got {
		by[e.Path] = e
	}
	if len(got) != len(ents) {
		t.Fatalf("%d entries, want %d: %+v", len(got), len(ents), got)
	}
	for _, n := range names {
		e, ok := by[n]
		if !ok || e.Mode != "100644" || e.Type != "blob" || e.SHA != b || e.Size != 6 {
			t.Errorf("%q: %+v %v", n, e, ok)
		}
	}
	if e := by["run.sh"]; e.Mode != "100755" {
		t.Errorf("exec: %+v", e)
	}
	if e := by["link"]; e.Mode != "120000" || e.Type != "blob" || e.Size != int64(len("../outside")) {
		t.Errorf("symlink: %+v", e)
	}
	if e := by["vendor"]; e.Mode != "160000" || e.Type != "commit" || e.Size != -1 || e.SHA != sub {
		t.Errorf("submodule: %+v", e)
	}
	// Blob content by id.
	c, err := ReadBlob(context.Background(), d, b, 100)
	if err != nil || string(c) != "hello\n" {
		t.Fatalf("%q %v", c, err)
	}
}

func TestReadersRejectNonHexIDs(t *testing.T) {
	d := emptyRepo(t)
	git(t, d, "commit", "-q", "--allow-empty", "-m", "x")
	good := git(t, d, "rev-parse", "HEAD")
	bad := []string{"", "--output=x", "--output=" + filepath.Join(d, "pwned"), "-h", "HEAD", "main", good[:39], good + "0",
		strings.ToUpper(good), good[:39] + "g", good + "\n", "HEAD^{tree}", good + " --stat", "../" + good}
	for _, id := range bad {
		if _, err := ListTree(context.Background(), d, id); err == nil {
			t.Errorf("ListTree accepted %q", id)
		}
		if _, err := ReadBlob(context.Background(), d, id, 10); err == nil {
			t.Errorf("ReadBlob accepted %q", id)
		}
	}
	for _, f := range []string{"x", "pwned"} {
		for _, dir := range []string{d, "."} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil && dir == d {
				t.Errorf("%s was created", f)
			}
		}
	}
}

func TestListTreeAndReadBlobCaps(t *testing.T) {
	d := emptyRepo(t)
	b := blobOf(t, d, strings.Repeat("x", 100))
	var ents []rawEnt
	for i := 0; i < 20; i++ {
		ents = append(ents, rawEnt{"100644", fmt.Sprintf("file_%02d.txt", i), b})
	}
	head := rawHead(t, d, ents)
	old := maxTreeListing
	defer func() { maxTreeListing = old }()
	maxTreeListing = 64 // far less than 20 entries
	if _, err := ListTree(context.Background(), d, head); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("listing cap: %v", err)
	}
	maxTreeListing = old
	if got, err := ListTree(context.Background(), d, head); err != nil || len(got) != 20 {
		t.Fatalf("%d %v", len(got), err)
	}
	if _, err := ReadBlob(context.Background(), d, b, 99); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("blob cap 99: %v", err)
	}
	if c, err := ReadBlob(context.Background(), d, b, 100); err != nil || len(c) != 100 {
		t.Fatalf("blob cap 100: %d %v", len(c), err)
	}
	if _, err := ReadBlob(context.Background(), d, "0123456789012345678901234567890123456789", 10); err == nil {
		t.Fatal("missing object read")
	}
}

func TestCapBufOverContract(t *testing.T) {
	var c capBuf
	c.max = 5
	c.Write([]byte("abc"))
	c.Write([]byte("de"))
	if c.over || c.b.String() != "abcde" {
		t.Fatalf("exactly max: over=%v %q", c.over, c.b.String())
	}
	n, err := c.Write([]byte("f"))
	if n != 1 || err != nil || !c.over || c.b.String() != "abcde" {
		t.Fatalf("past max: %d %v over=%v %q", n, err, c.over, c.b.String())
	}
	var d capBuf
	d.max = 4
	d.Write([]byte("abcdefgh"))
	if !d.over || d.b.String() != "abcd" {
		t.Fatalf("one big write: over=%v %q", d.over, d.b.String())
	}
}

// refs/replace must not change what the readers return (GIT_NO_REPLACE_OBJECTS).
func TestReadersIgnoreReplaceRefs(t *testing.T) {
	d := emptyRepo(t)
	orig := blobOf(t, d, "ORIGINAL\n")
	fake := blobOf(t, d, "REPLACED-CONTENT\n")
	head := rawHead(t, d, []rawEnt{{"100644", "f.txt", orig}})
	// Replace the blob and a second commit's tree for the same HEAD tree.
	git(t, d, "replace", orig, fake)
	if out := git(t, d, "cat-file", "blob", orig); !strings.Contains(out, "REPLACED") {
		t.Skip("this git does not apply replace refs here")
	}
	c, err := ReadBlob(context.Background(), d, orig, 100)
	if err != nil || string(c) != "ORIGINAL\n" {
		t.Fatalf("ReadBlob followed a replace ref: %q %v", c, err)
	}
	// A replaced commit: HEAD's tree swapped for another tree.
	other := rawHead(t, d, []rawEnt{{"100644", "g.txt", orig}})
	git(t, d, "replace", head, other)
	ents, err := ListTree(context.Background(), d, head)
	if err != nil || len(ents) != 1 || ents[0].Path != "f.txt" {
		t.Fatalf("ListTree followed a replace ref: %+v %v", ents, err)
	}
}
