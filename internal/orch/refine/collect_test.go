package refine

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a repository whose first commit holds files (path -> content).
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	d, err := os.MkdirTemp("", "refine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	gitIn(t, d, nil, "init", "-q", "-b", "main")
	for p, c := range files {
		writeFile(t, d, p, c)
	}
	commitAll(t, d, "c1")
	return d
}

func writeFile(t *testing.T, root, p, c string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, d, msg string) {
	t.Helper()
	gitIn(t, d, nil, "add", "-A", "-f")
	gitIn(t, d, nil, "commit", "-q", "--allow-empty", "-m", msg)
}

func collect(t *testing.T, d, req string, o Options) Selection {
	t.Helper()
	sel, err := Collect(context.Background(), d, req, o)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func paths(s Selection) []string {
	var out []string
	for _, f := range s.Files {
		out = append(out, f.Path)
	}
	return out
}

func excludedReason(s Selection, p string) string {
	for _, e := range s.Excluded {
		if e.Path == p {
			return e.Reason
		}
	}
	return ""
}

func TestCollectUsesHeadCommitNotWorkingTree(t *testing.T) {
	d := newRepo(t, map[string]string{"src/auth.go": "package auth // HEAD-CONTENT\n", "README.md": "readme at head\n"})
	// Work tree and index differ from HEAD; an untracked file matches too.
	writeFile(t, d, "src/auth.go", "package auth // WORKTREE-CHANGE\n")
	writeFile(t, d, "README.md", "readme STAGED-CHANGE\n")
	gitIn(t, d, nil, "add", "README.md")
	writeFile(t, d, "src/auth_untracked.go", "UNTRACKED-CONTENT\n")
	sel := collect(t, d, "change the auth flow", Options{})
	var all strings.Builder
	for _, f := range sel.Contents {
		all.WriteString(f.Path + "=" + f.Content + "\n")
	}
	got := all.String()
	if !strings.Contains(got, "HEAD-CONTENT") || !strings.Contains(got, "readme at head") {
		t.Fatalf("HEAD content missing: %q", got)
	}
	for _, bad := range []string{"WORKTREE-CHANGE", "STAGED-CHANGE", "UNTRACKED-CONTENT", "auth_untracked"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%s leaked from the work tree/index: %q", bad, got)
		}
	}
	for _, p := range paths(sel) {
		if strings.Contains(p, "untracked") {
			t.Fatalf("untracked path selected: %v", paths(sel))
		}
	}
	// README comes first.
	if sel.Files[0].Path != "README.md" || sel.Files[0].Why != WhyReadme {
		t.Fatalf("files %+v", sel.Files)
	}
}

func TestCollectExcludesSecretsAndBinaries(t *testing.T) {
	const pk = "-----BEGIN RSA PRIVATE KEY-----"
	files := map[string]string{
		"main.go":            "package main // ok\n",
		".env":               "DB=plain-value\n",
		"id_rsa":             "not really a key\n",
		"server.pem":         "pem body\n",
		"blob.bin":           "ab\x00cd",
		"private_notes.txt":  pk + "\nMIIE...\n",
		"aws_notes.txt":      "id AKIAABCDEFGHIJKLMNOP\n",
		"password_notes.txt": "password = \"hunter2hunter2hunter2\"\n",
		"latin1.txt":         "caf\xe9\n",
	}
	d := newRepo(t, files)
	if err := os.Symlink("main.go", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	commitAll(t, d, "link")
	sel := collect(t, d, "main env rsa server blob link private aws password latin", Options{})
	want := map[string]string{
		".env": ReasonSecretName, "id_rsa": ReasonSecretName, "server.pem": ReasonSecretName,
		"blob.bin": ReasonBinary, "link": ReasonSymlink, "private_notes.txt": ReasonLooksLikeSecret,
		"aws_notes.txt": ReasonLooksLikeSecret, "password_notes.txt": ReasonLooksLikeSecret,
		"latin1.txt": ReasonNotUTF8,
	}
	for p, reason := range want {
		if got := excludedReason(sel, p); got != reason {
			t.Errorf("%s: reason %q, want %q (excluded %+v)", p, got, reason, sel.Excluded)
		}
	}
	if fmt.Sprint(paths(sel)) != "[main.go]" {
		t.Fatalf("files %v", paths(sel))
	}
	// No excluded content anywhere: not in the JSON view, not in Contents.
	view, _ := json.Marshal(sel)
	var all strings.Builder
	all.Write(view)
	for _, f := range sel.Contents {
		all.WriteString(f.Content)
	}
	for _, leak := range []string{"hunter2", "AKIAABCDEFGH", "PRIVATE KEY", "MIIE", "pem body", "plain-value", "not really a key", "ab\x00"} {
		if strings.Contains(all.String(), leak) {
			t.Fatalf("%q leaked into the selection", leak)
		}
	}
}

func TestCollectCapsFilesAndBytes(t *testing.T) {
	if MaxFiles != 40 || MaxFileBytes != 32*1024 || MaxTotalBytes != 240*1024 {
		t.Fatalf("limits %d %d %d", MaxFiles, MaxFileBytes, MaxTotalBytes)
	}
	files := map[string]string{}
	for i := 0; i < 45; i++ {
		files[fmt.Sprintf("feature_%02d.txt", i)] = "x"
	}
	d := newRepo(t, files)
	sel := collect(t, d, "feature", Options{})
	if len(sel.Files) != 40 {
		t.Fatalf("%d files, want 40", len(sel.Files))
	}
	n := 0
	for _, e := range sel.Excluded {
		if e.Reason == ReasonFileCap {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("file_cap exclusions %d, want 5", n)
	}

	// Per-file and total byte caps (the real caps, with real sizes).
	big := map[string]string{
		"data_big.txt": strings.Repeat("a", MaxFileBytes+1),
		"data_ok.txt":  strings.Repeat("b", MaxFileBytes),
	}
	for i := 0; i < 8; i++ {
		big[fmt.Sprintf("data_%d.txt", i)] = strings.Repeat("c", MaxFileBytes)
	}
	d2 := newRepo(t, big)
	sel2 := collect(t, d2, "data", Options{})
	if got := excludedReason(sel2, "data_big.txt"); got != ReasonTooLarge {
		t.Fatalf("big file reason %q", got)
	}
	if sel2.TotalBytes > MaxTotalBytes || sel2.TotalBytes <= MaxTotalBytes-MaxFileBytes {
		t.Fatalf("total %d (cap %d)", sel2.TotalBytes, MaxTotalBytes)
	}
	total := 0
	for _, e := range sel2.Excluded {
		if e.Reason == ReasonTotalCap {
			total++
		}
	}
	if len(sel2.Files) != 7 || total != 2 {
		t.Fatalf("files %d, total_cap exclusions %d (want 7 and 2): %+v", len(sel2.Files), total, sel2.Excluded)
	}
	// Overridable limits.
	sel3 := collect(t, d, "feature", Options{MaxFiles: 3})
	if len(sel3.Files) != 3 {
		t.Fatalf("%d", len(sel3.Files))
	}
}

// rawTree writes a tree object holding entries literally (git mktree would
// refuse some names) and returns a commit on it, made HEAD.
func rawCommit(t *testing.T, d string, entries []rawEntry) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range entries {
		sha, _ := hex.DecodeString(e.sha)
		fmt.Fprintf(&buf, "%s %s\x00", e.mode, e.name)
		buf.Write(sha)
	}
	tree := gitIn(t, d, buf.Bytes(), "hash-object", "-t", "tree", "-w", "--literally", "--stdin")
	c := gitIn(t, d, nil, "commit-tree", "-m", "raw", tree)
	gitIn(t, d, nil, "update-ref", "HEAD", c)
}

type rawEntry struct{ mode, name, sha string }

func TestCollectNeverReadsOutsideRepo(t *testing.T) {
	outside, err := os.MkdirTemp("", "outside")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside)
	if err := os.WriteFile(filepath.Join(outside, "passwd.txt"), []byte("OUTSIDE-SECRET-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newRepo(t, map[string]string{"notes.txt": "inside notes\n"})
	// Symlinks out of the repository: a file and a directory.
	os.Symlink(filepath.Join(outside, "passwd.txt"), filepath.Join(d, "passwd_link.txt"))
	os.Symlink(outside, filepath.Join(d, "outdir"))
	// A submodule (gitlink) whose commit does not exist here.
	gitIn(t, d, nil, "update-index", "--add", "--cacheinfo", "160000,1234567890123456789012345678901234567890,vendor_sub")
	commitAll(t, d, "links")
	blob := gitIn(t, d, nil, "rev-parse", "HEAD:notes.txt")
	// A hostile tree: ".." and absolute-looking names, as a crafted commit
	// could carry them.
	rawCommit(t, d, []rawEntry{
		{"100644", "notes.txt", blob},
		{"120000", "passwd_link.txt", gitIn(t, d, []byte(filepath.Join(outside, "passwd.txt")), "hash-object", "-w", "--stdin")},
		{"160000", "vendor_sub", "1234567890123456789012345678901234567890"},
		{"100644", "..", blob},
		{"100644", "../notes_escape.txt", blob},
		{"100644", "/abs_escape_notes.txt", blob},
		{"100644", "notes_ok.txt", blob},
	})
	sel := collect(t, d, "notes passwd link outdir vendor sub escape", Options{})
	for _, f := range sel.Contents {
		if strings.Contains(f.Content, "OUTSIDE-SECRET") {
			t.Fatalf("outside content read: %s", f.Path)
		}
		if f.Path == ".." || strings.Contains(f.Path, "escape") {
			t.Fatalf("unsafe path selected: %s", f.Path)
		}
	}
	if got := excludedReason(sel, "passwd_link.txt"); got != ReasonSymlink {
		t.Fatalf("symlink reason %q: %+v", got, sel.Excluded)
	}
	if got := excludedReason(sel, "vendor_sub"); got != ReasonSubmodule {
		t.Fatalf("submodule reason %q: %+v", got, sel.Excluded)
	}
	unsafe := 0
	for _, e := range sel.Excluded {
		if e.Reason == ReasonUnsafePath {
			unsafe++
		}
	}
	// The tree is small, so ".." (no name match) is listed too, as an
	// anonymous "<unsafe path>".
	if unsafe != 3 {
		t.Fatalf("unsafe exclusions %d, want 3: %+v", unsafe, sel.Excluded)
	}
	view, _ := json.Marshal(sel)
	if strings.Contains(string(view), "OUTSIDE-SECRET") || strings.Contains(string(view), "escape") {
		t.Fatal("outside content or an unsafe path in the view")
	}
}

func TestCollectUnsafePathListedWithoutName(t *testing.T) {
	d := newRepo(t, map[string]string{"notes.txt": "n\n"})
	blob := gitIn(t, d, nil, "rev-parse", "HEAD:notes.txt")
	rawCommit(t, d, []rawEntry{{"100644", "bad\nname_notes.txt", blob}, {"100644", "notes.txt", blob}})
	sel := collect(t, d, "notes name", Options{})
	if fmt.Sprint(paths(sel)) != "[notes.txt]" {
		t.Fatalf("files %v", paths(sel))
	}
	if len(sel.Excluded) != 1 || sel.Excluded[0].Reason != ReasonUnsafePath || strings.ContainsAny(sel.Excluded[0].Path, "\n") {
		t.Fatalf("excluded %+v", sel.Excluded)
	}
}

func TestCollectEmptyRepoAndNoCommit(t *testing.T) {
	d, _ := os.MkdirTemp("", "refine")
	defer os.RemoveAll(d)
	gitIn(t, d, nil, "init", "-q", "-b", "main")
	if _, err := Collect(context.Background(), d, "anything", Options{}); err != ErrNoCommit {
		t.Fatalf("err %v", err)
	}
}

func TestRereadSameHeadSameHashAndNewHeadDiffers(t *testing.T) {
	d := newRepo(t, map[string]string{"auth.go": "v1\n", "README.md": "r\n"})
	sel := collect(t, d, "auth", Options{})
	again, err := Reread(context.Background(), d, paths(sel), Options{})
	if err != nil || again.Hash != sel.Hash {
		t.Fatalf("hash %q vs %q (%v)", again.Hash, sel.Hash, err)
	}
	writeFile(t, d, "auth.go", "v2\n")
	commitAll(t, d, "c2")
	changed, err := Reread(context.Background(), d, paths(sel), Options{})
	if err != nil || changed.Hash == sel.Hash {
		t.Fatalf("hash unchanged after a new commit: %v", err)
	}
	// An empty commit moves HEAD, so the hash changes too.
	gitIn(t, d, nil, "commit", "-q", "--allow-empty", "-m", "empty")
	again2, _ := Reread(context.Background(), d, paths(sel), Options{})
	if again2.Hash == changed.Hash {
		t.Fatal("HEAD moved but the hash did not")
	}
	// A deleted file cannot be re-read.
	gitIn(t, d, nil, "rm", "-q", "auth.go")
	gitIn(t, d, nil, "commit", "-q", "-m", "rm")
	if _, err := Reread(context.Background(), d, paths(sel), Options{}); err != ErrChanged {
		t.Fatalf("err %v", err)
	}
}

func TestCheckRequirement(t *testing.T) {
	ok := []string{"add a health check", "여러 줄\n두 번째 줄\t탭", strings.Repeat("가", 2000)}
	bad := []string{"", "   \n", strings.Repeat("a", 2001), "a\x00b", "a\x1bb", "a\rb", "a\u200bb", "a\u202eb", "a\u2028b",
		"a\u00adb", "a\ufeffb", "a\x7fb", "a\u0085b", "a\xffb", "a\U000e0041b", "a\ufe0fb"}
	for _, s := range ok {
		if !CheckRequirement(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range bad {
		if CheckRequirement(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestStoreTTLActorAndCap(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewStore(func() time.Time { return now })
	sel := Selection{Hash: "h", Files: []FileEntry{{Path: "a"}}}
	id, err := s.Put("alice", "/r", "text", sel)
	if err != nil || len(id) != 32 {
		t.Fatalf("id %q %v", id, err)
	}
	if h, ok := s.Get(id, "alice"); !ok || h.Hash != "h" || h.Paths[0] != "a" {
		t.Fatalf("%+v %v", h, ok)
	}
	if _, ok := s.Get(id, "bob"); ok {
		t.Fatal("another actor got the selection")
	}
	now = now.Add(TTL - time.Second)
	if _, ok := s.Get(id, "alice"); !ok {
		t.Fatal("expired early")
	}
	now = now.Add(2 * time.Second)
	if _, ok := s.Get(id, "alice"); ok {
		t.Fatal("not expired after the TTL")
	}
}

// A refs/replace entry must not change what Reread (hence the hash) sees.
func TestRereadIgnoresReplaceRefs(t *testing.T) {
	d := newRepo(t, map[string]string{"auth.go": "ORIGINAL\n"})
	sel := collect(t, d, "auth", Options{})
	orig := gitIn(t, d, nil, "rev-parse", "HEAD:auth.go")
	fake := gitIn(t, d, []byte("REPLACED\n"), "hash-object", "-w", "--stdin")
	gitIn(t, d, nil, "replace", orig, fake)
	again, err := Reread(context.Background(), d, paths(sel), Options{})
	if err != nil || again.Hash != sel.Hash || strings.Contains(again.Contents[0].Content, "REPLACED") {
		t.Fatalf("hash %q vs %q: %v", again.Hash, sel.Hash, err)
	}
}

func TestStoreFairness(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewStore(func() time.Time { return now })
	sel := Selection{Hash: "h", Files: []FileEntry{{Path: "a"}}}
	bobID, err := s.Put("bob", "/r", "t", sel)
	if err != nil {
		t.Fatal(err)
	}
	var firstA, lastA string
	for i := 0; i < MaxHeld+20; i++ {
		now = now.Add(time.Second)
		id, err := s.Put("alice", "/r", "t", sel)
		if err != nil {
			t.Fatalf("alice put %d: %v", i, err)
		}
		if i == 0 {
			firstA = id
		}
		lastA = id
	}
	if _, ok := s.Get(bobID, "bob"); !ok {
		t.Fatal("alice evicted bob's selection")
	}
	if _, ok := s.Get(lastA, "alice"); !ok {
		t.Fatal("alice's newest is gone")
	}
	if _, ok := s.Get(firstA, "alice"); ok {
		t.Fatal("alice's oldest was kept past her cap")
	}
	s.mu.Lock()
	n := 0
	for _, h := range s.m {
		if h.Actor == "alice" {
			n++
		}
	}
	s.mu.Unlock()
	if n != MaxHeldPerActor {
		t.Fatalf("alice holds %d, want %d", n, MaxHeldPerActor)
	}
	// The store as a whole is full: a newcomer is refused, nobody is evicted.
	for a := 0; ; a++ {
		if _, err := s.Put(fmt.Sprintf("actor%d", a), "/r", "t", sel); err != nil {
			if err != ErrStoreFull {
				t.Fatalf("err %v", err)
			}
			break
		}
		if a > 100 {
			t.Fatal("the global cap never applied")
		}
	}
	if _, ok := s.Get(bobID, "bob"); !ok {
		t.Fatal("a full store evicted bob")
	}
	s.mu.Lock()
	total := len(s.m)
	s.mu.Unlock()
	if total != MaxHeld {
		t.Fatalf("holding %d, want %d", total, MaxHeld)
	}
	// Expiry frees room.
	now = now.Add(TTL + time.Second)
	if _, err := s.Put("late", "/r", "t", sel); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func toyFiles() map[string]string {
	return map[string]string{
		"calc.go":      "package toy\nfunc Add(a, b int) int { return a + b }\n",
		"calc_test.go": "package toy\n",
		"go.mod":       "module toy\n",
		".gitignore":   "bin/\n",
		".aigate.yml":  "checks: []\n",
	}
}

func TestCollectSmallRepoSendsAllFiles(t *testing.T) {
	const req = "계산기에 빼기 기능을 추가해 주세요"
	d := newRepo(t, toyFiles())
	sel := collect(t, d, req, Options{})
	if len(sel.Files) != 5 {
		t.Fatalf("files %+v excluded %+v", sel.Files, sel.Excluded)
	}
	for _, f := range sel.Files {
		if f.Why != WhySmallRepo {
			t.Fatalf("why %+v", f)
		}
	}
	if WhySmallRepo != "small_repo" {
		t.Fatal(WhySmallRepo)
	}
	// README first when present; then the rest by path (shortest first).
	files := toyFiles()
	files["README.md"] = "# toy\n"
	d2 := newRepo(t, files)
	sel = collect(t, d2, req, Options{})
	want := []string{"README.md", "go.mod", "calc.go", ".gitignore", ".aigate.yml", "calc_test.go"}
	if got := paths(sel); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v", got)
	}
	if sel.Files[0].Why != WhyReadme || sel.Files[1].Why != WhySmallRepo {
		t.Fatalf("%+v", sel.Files)
	}
	// Name matches keep name_match and come before the rest.
	sel = collect(t, d2, "calc 기능 수정", Options{})
	if sel.Files[0].Path != "README.md" || sel.Files[1].Path != "calc.go" || sel.Files[1].Why != WhyNameMatch {
		t.Fatalf("%+v", sel.Files)
	}
	if sel.Files[2].Path != "calc_test.go" || sel.Files[2].Why != WhyNameMatch || sel.Files[3].Why != WhySmallRepo {
		t.Fatalf("%+v", sel.Files)
	}
}

func TestCollectSmallRepoStillExcludesSecretsAndBinaries(t *testing.T) {
	const pk = "-----BEGIN RSA PRIVATE KEY-----"
	files := toyFiles()
	files[".env"] = "DB=SECRET-ENV-VALUE\n"
	files["server.pem"] = "SECRET-PEM-BODY\n"
	files["id_rsa"] = "SECRET-RSA-BODY\n"
	files["blob.bin"] = "ab\x00cd"
	files["notes.txt"] = pk + "\nSECRET-KEY-BODY\n"
	d := newRepo(t, files)
	sel := collect(t, d, "계산기에 빼기 기능", Options{})
	got := strings.Join(paths(sel), ",")
	for _, p := range []string{".env", "server.pem", "id_rsa", "blob.bin", "notes.txt"} {
		if strings.Contains(got, p) {
			t.Fatalf("%s selected: %s", p, got)
		}
	}
	for p, r := range map[string]string{".env": ReasonSecretName, "server.pem": ReasonSecretName, "id_rsa": ReasonSecretName,
		"blob.bin": ReasonBinary, "notes.txt": ReasonLooksLikeSecret} {
		if g := excludedReason(sel, p); g != r {
			t.Errorf("%s: reason %q want %q", p, g, r)
		}
	}
	var all strings.Builder
	for _, f := range sel.Contents {
		all.WriteString(f.Content)
	}
	if strings.Contains(all.String(), "SECRET-") || len(sel.Files) != 5 {
		t.Fatalf("files %v", paths(sel))
	}
}

func TestCollectLargeRepoKeepsOldRule(t *testing.T) {
	check := func(name string, sel Selection) {
		t.Helper()
		for _, f := range sel.Files {
			if f.Why == WhySmallRepo {
				t.Errorf("%s: small_repo file %+v", name, f)
			}
		}
		if got := strings.Join(paths(sel), ","); got != "README.md,auth.go" {
			t.Errorf("%s: files %s", name, got)
		}
	}
	// More entries than MaxFiles.
	files := map[string]string{"README.md": "# r\n", "auth.go": "package a\n"}
	for i := 0; i < 5; i++ {
		files["f"+strconv.Itoa(i)+".txt"] = "x\n"
	}
	d := newRepo(t, files)
	check("count", collect(t, d, "change auth flow", Options{MaxFiles: 6}))
	// Exactly at the limit is still small.
	if sel := collect(t, d, "change auth flow", Options{MaxFiles: 7}); len(sel.Files) != 7 {
		t.Errorf("at the limit: %v", paths(sel))
	}
	// Total size over the cap.
	files = map[string]string{"README.md": "# r\n", "auth.go": "package a\n", "aaa.txt": strings.Repeat("x", 200), "bbb.txt": strings.Repeat("z", 200)}
	d = newRepo(t, files)
	check("bytes", collect(t, d, "change auth flow", Options{MaxTotalBytes: 300, MaxFileBytes: 300}))
}

func TestCollectSmallRepoRespectsCaps(t *testing.T) {
	files := toyFiles()
	files["huge.txt"] = strings.Repeat("y", 300)
	d := newRepo(t, files)
	sel := collect(t, d, "계산기에 빼기 기능", Options{MaxFileBytes: 200})
	if excludedReason(sel, "huge.txt") != ReasonTooLarge {
		t.Fatalf("excluded %+v", sel.Excluded)
	}
	if len(sel.Files) != 5 {
		t.Fatalf("files %v", paths(sel))
	}
}
