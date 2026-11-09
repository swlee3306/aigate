package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// mkRepo makes a git repository with one commit at dir.
func mkRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return git(t, dir, "rev-parse", "HEAD")
}

// gitAt is git with a fixed committer/author date (orders for-each-ref).
func gitAt(t *testing.T, dir string, when time.Time, args ...string) {
	t.Helper()
	d := when.Format(time.RFC3339)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (e *env) repos(t *testing.T) []RepoView {
	t.Helper()
	code, b := e.do(t, "GET", "/api/v1/repos", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var out []RepoView
	if err := json.Unmarshal(b, &out); err != nil || out == nil {
		t.Fatalf("not a JSON array: %s", b)
	}
	return out
}

func repoPaths(rs []RepoView) []string {
	var p []string
	for _, r := range rs {
		p = append(p, r.Path)
	}
	return p
}

// Review focus 5: only git work trees under an allowed root, by their real
// path; nothing reached through a symlink to the outside, no plain folders,
// nothing deeper than two levels, directory symlinks never walked.
func TestReposListsOnlyUnderAllowedRoots(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	parent := filepath.Dir(e.root)
	outside := filepath.Join(parent, "outside")
	mkRepo(t, filepath.Join(outside, "x"))
	mkRepo(t, outside) // the outside dir itself is a repo too
	a := filepath.Join(e.root, "a")
	mkRepo(t, a)
	b := filepath.Join(e.root, "group", "b")
	mkRepo(t, b)
	mkRepo(t, filepath.Join(e.root, "group", "sub", "c"))       // depth 3: not listed
	mkRepo(t, filepath.Join(e.root, "g1", "g2", "deep"))        // depth 3: not listed
	os.MkdirAll(filepath.Join(e.root, "plain", "inner"), 0o755) // no .git
	os.WriteFile(filepath.Join(e.root, "file.txt"), []byte("x"), 0o644)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(outside, filepath.Join(e.root, "link-out")))                            // dir symlink to an outside repo
	must(os.Symlink(filepath.Join(outside, "x"), filepath.Join(e.root, "group", "lx")))     // depth 2 symlink out
	must(os.Symlink(a, filepath.Join(e.root, "link-in")))                                   // symlink to a repo inside: same real path
	must(os.Symlink(filepath.Join(e.root, "g1", "g2"), filepath.Join(e.root, "deep-link"))) // following it would reach "deep" at depth 2
	// Names with a newline or another control character are never listed.
	mkRepo(t, filepath.Join(e.root, "bad\nname"))
	mkRepo(t, filepath.Join(e.root, "ctl\x01x"))
	// A .git FILE (worktree/submodule form) counts, as in validate.
	wt := filepath.Join(e.root, "wt")
	git(t, e.repo, "worktree", "add", "-q", "--detach", wt)

	got := repoPaths(e.repos(t))
	want := []string{e.repo, a, b, wt}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range got {
		if strings.HasPrefix(p, outside) {
			t.Fatalf("outside path listed: %s", p)
		}
	}

	// The root itself is depth 0.
	e2 := newEnv(t, noSteps{}, 1, func(c *Config) { c.AllowedRepos = []string{outside} })
	e2.d.pauseWorkers()
	got = repoPaths(e2.repos(t))
	if strings.Join(got, ",") != outside+","+filepath.Join(outside, "x") {
		t.Fatalf("root as repo: %v", got)
	}

	// No roots: an empty array.
	e3 := newEnv(t, noSteps{}, 1, func(c *Config) { c.AllowedRepos = nil })
	if code, body := e3.do(t, "GET", "/api/v1/repos", ""); code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("no roots: %d %s", code, body)
	}

	// At most reposMax entries, the first ones by path.
	e.d.handler.reposMax = 2
	got = repoPaths(e.repos(t))
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cap: %v", got)
	}
	if DefaultReposMax != 200 {
		t.Fatalf("DefaultReposMax %d", DefaultReposMax)
	}
}

func TestReposHeadAndBranches(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	r := filepath.Join(e.root, "multi")
	if err := os.MkdirAll(r, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, r, "init", "-q", "-b", "main")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gitAt(t, r, t0, "commit", "-q", "--allow-empty", "-m", "init")
	// 12 more branches, each with a newer commit: b00 oldest ... b11 newest.
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("b%02d", i)
		if i == 11 {
			name = "기능/새것"
		}
		git(t, r, "checkout", "-q", "-b", name, "main")
		gitAt(t, r, t0.Add(time.Duration(i+1)*time.Hour), "commit", "-q", "--allow-empty", "-m", name)
	}
	git(t, r, "checkout", "-q", "b05")
	head := git(t, r, "rev-parse", "HEAD")

	det := filepath.Join(e.root, "detached")
	mkRepo(t, det)
	detHead := git(t, det, "rev-parse", "HEAD")
	git(t, det, "checkout", "-q", "--detach", "HEAD")

	unborn := filepath.Join(e.root, "unborn")
	os.MkdirAll(unborn, 0o755)
	git(t, unborn, "init", "-q", "-b", "trunk")

	byPath := map[string]RepoView{}
	for _, v := range e.repos(t) {
		byPath[v.Path] = v
	}
	m := byPath[r]
	if m.Head != head || m.Branch != "b05" {
		t.Fatalf("multi: %+v", m)
	}
	wantBr := []string{"기능/새것", "b10", "b09", "b08", "b07", "b06", "b05", "b04", "b03", "b02"}
	if strings.Join(m.Branches, ",") != strings.Join(wantBr, ",") {
		t.Fatalf("branches %v, want %v", m.Branches, wantBr)
	}
	if d := byPath[det]; d.Head != detHead || d.Branch != "" || strings.Join(d.Branches, ",") != "main" {
		t.Fatalf("detached: %+v", d)
	}
	u, ok := byPath[unborn]
	if !ok || u.Head != "" || u.Branches == nil || len(u.Branches) != 0 {
		t.Fatalf("unborn: %+v (listed %v)", u, ok)
	}
	if p := byPath[e.repo]; p.Head != e.sha || p.Branch == "" {
		t.Fatalf("proj: %+v", p)
	}
	// The listed path is what validate stores: submitting it pins the same repo.
	code, b := e.submitV1(t, "docs")
	var tv TaskView
	_ = json.Unmarshal(b, &tv)
	if code != 201 || byPath[tv.Repo].Path != tv.Repo {
		t.Fatalf("submit path %q not in the list (%d %s)", tv.Repo, code, b)
	}
}

// treeHash fingerprints every file under dir (name, mode, size, mtime, content).
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s|%v|%d|%d\n", p, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestReposReadOnlyAndAuthRequired(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	// A hook that would write if any git command ran one.
	hook := filepath.Join(e.repo, ".git", "hooks", "post-checkout")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch \"$GIT_DIR/../HOOK_RAN\"\n"), 0o755)
	git(t, e.repo, "config", "core.fsmonitor", "touch "+filepath.Join(e.repo, "FSMONITOR_RAN")+" #")
	time.Sleep(20 * time.Millisecond)
	before := treeHash(t, e.root)
	for i := 0; i < 3; i++ {
		if len(e.repos(t)) != 1 {
			t.Fatal("repo not listed")
		}
	}
	if after := treeHash(t, e.root); after != before {
		t.Fatal("GET /repos changed the repository on disk")
	}
	for _, f := range []string{"HOOK_RAN", "FSMONITOR_RAN"} {
		if _, err := os.Stat(filepath.Join(e.repo, f)); err == nil {
			t.Fatalf("%s: a hook or fsmonitor ran", f)
		}
	}

	srv := e.httpServer(t)
	code, _, b := httpDo(t, srv, "GET", "/api/v1/repos", "", nil)
	if code != 401 || v1Code(b) != "unauthenticated" || strings.Contains(string(b), e.root) {
		t.Fatalf("unauthenticated: %d %s", code, b)
	}
	tok, _ := e.newSession(t, "web")
	code, _, b = httpDo(t, srv, "GET", "/api/v1/repos", "", hdr{"Cookie": cookie(tok)})
	if code != 200 {
		t.Fatalf("session read: %d %s", code, b)
	}
	// Read only: POST is not a route.
	if code, _ := e.do(t, "POST", "/api/v1/repos", ""); code != 405 {
		t.Fatalf("POST /repos: %d", code)
	}
}

func TestReposNoPathLeakInError(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	e.d.handler.reposTimeout = time.Nanosecond
	code, b := e.do(t, "GET", "/api/v1/repos", "")
	if code != 503 {
		t.Fatalf("%d %s", code, b)
	}
	v := decodeV1Err(t, b)
	if v.Error.Code != "repos_unavailable" || v.Error.Message == "" {
		t.Fatalf("%s", b)
	}
	s := string(b)
	for _, bad := range []string{e.root, "/tmp", "git", "context", "deadline", "signal"} {
		if strings.Contains(s, bad) {
			t.Fatalf("error body leaks %q: %s", bad, s)
		}
	}
}

func TestSafeRefName(t *testing.T) {
	for _, ok := range []string{"main", "feature/x-1", "기능/새것", "a.b_c"} {
		if !safeRefName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "a\nb", "a\x00", "a\x1b[31m", "a‮b", strings.Repeat("a", 201), "\xff", "日本"} {
		if safeRefName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// outerSetup makes the directory around e.root an outer repository with a
// recognisable branch, and a repository X outside every root.
func outerSetup(t *testing.T, e *env) (outerSHA, x, xSHA string) {
	t.Helper()
	outer := filepath.Dir(e.root)
	git(t, outer, "init", "-q", "-b", "outersecretbranch")
	git(t, outer, "commit", "-q", "--allow-empty", "-m", "outer")
	outerSHA = git(t, outer, "rev-parse", "HEAD")
	x = filepath.Join(outer, "elsewhere", "x")
	if err := os.MkdirAll(x, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, x, "init", "-q", "-b", "xsecretbranch")
	git(t, x, "commit", "-q", "--allow-empty", "-m", "x")
	return outerSHA, x, git(t, x, "rev-parse", "HEAD")
}

func assertNotLeaked(t *testing.T, e *env, path string, secrets ...string) {
	t.Helper()
	code, b := e.do(t, "GET", "/api/v1/repos", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	for _, s := range append(secrets, path) {
		if strings.Contains(string(b), s) {
			t.Fatalf("response contains %q: %s", s, b)
		}
	}
	if got := repoPaths(e.repos(t)); len(got) != 1 || got[0] != e.repo {
		t.Fatalf("listed %v, want only %s", got, e.repo)
	}
}

// R8: an empty .git directory must not make git walk up to an enclosing
// repository and report ITS state.
func TestReposRefusesEmptyGitDirWalkUp(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	outerSHA, _, _ := outerSetup(t, e)
	p := filepath.Join(e.root, "emptygit")
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertNotLeaked(t, e, p, outerSHA, "outersecretbranch")
}

func TestReposRefusesGitFileOutsideRoots(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	outerSHA, x, xSHA := outerSetup(t, e)
	p := filepath.Join(e.root, "gf")
	os.MkdirAll(p, 0o755)
	if err := os.WriteFile(filepath.Join(p, ".git"), []byte("gitdir: "+filepath.Join(x, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertNotLeaked(t, e, p, xSHA, "xsecretbranch", outerSHA, "outersecretbranch")
}

func TestReposRefusesGitSymlinkOutsideRoots(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	outerSHA, x, xSHA := outerSetup(t, e)
	p := filepath.Join(e.root, "gs")
	os.MkdirAll(p, 0o755)
	if err := os.Symlink(filepath.Join(x, ".git"), filepath.Join(p, ".git")); err != nil {
		t.Fatal(err)
	}
	assertNotLeaked(t, e, p, xSHA, "xsecretbranch", outerSHA, "outersecretbranch")
}

// R8 for submit: an empty .git directory is refused (git would resolve the
// base in the enclosing repository and the task would bundle it).
func TestSubmitRefusesEmptyGitDirWalkUp(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	outerSHA, _, _ := outerSetup(t, e)
	p := filepath.Join(e.root, "emptygit")
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"repo": p, "base": "HEAD", "prompt": "p", "class": "docs", "agent": "claude"})
	code, body := e.do(t, "POST", "/api/v1/tasks", string(b))
	if code != 400 || v1Code(body) != "bad_request" {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(string(body), outerSHA) || strings.Contains(string(body), "outersecretbranch") {
		t.Fatalf("leak: %s", body)
	}
	if n := e.taskCount(t); n != 0 {
		t.Fatalf("%d tasks created", n)
	}
}

// A linked worktree (gitfile to the main repository) still submits, pinned
// to the worktree's own path and HEAD.
func TestSubmitAllowsLinkedWorktreeGitFile(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	wt := filepath.Join(e.root, "wt")
	git(t, e.repo, "worktree", "add", "-q", "-b", "wtbranch", wt)
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "wt")
	head := git(t, wt, "rev-parse", "HEAD")
	b, _ := json.Marshal(map[string]string{"repo": wt, "base": "HEAD", "prompt": "p", "class": "docs", "agent": "claude"})
	code, body := e.do(t, "POST", "/api/v1/tasks", string(b))
	var v TaskView
	_ = json.Unmarshal(body, &v)
	if code != 201 || v.Repo != wt || v.Base != head {
		t.Fatalf("%d %s (want repo %s base %s)", code, body, wt, head)
	}
}

// Pins the top-level check of ownRepo (and of validate): b's .git is a
// gitfile to a git directory under the root whose core.worktree names
// ANOTHER directory c under the root. Every git directory is inside the
// roots, so only "top level == listed path" keeps b (which would show c's
// state under b's path) out of /repos and out of submit.
func TestReposRefusesGitFileWithForeignWorktree(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	c := filepath.Join(e.root, "c")
	mkRepo(t, c)
	git(t, c, "commit", "-q", "--allow-empty", "-m", "c-only")
	git(t, c, "branch", "csecretbranch")
	cSHA := git(t, c, "rev-parse", "HEAD")
	gd := filepath.Join(e.root, "gd")
	if err := os.Rename(filepath.Join(c, ".git"), gd); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c, ".git"), []byte("gitdir: "+gd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, e.root, "--git-dir="+gd, "config", "core.worktree", c)
	b := filepath.Join(e.root, "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, ".git"), []byte("gitdir: "+gd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := repoPaths(e.repos(t))
	sort.Strings(got)
	if len(got) != 2 || got[0] != c || got[1] != e.repo {
		t.Fatalf("listed %v, want only %s and %s (never %s)", got, c, e.repo, b)
	}
	for _, r := range e.repos(t) {
		if r.Path == b || (r.Path != c && r.Head == cSHA) {
			t.Fatalf("b or c's state listed under another path: %+v", r)
		}
	}
	body, _ := json.Marshal(map[string]string{"repo": b, "base": "HEAD", "prompt": "p", "class": "docs", "agent": "claude"})
	code, rb := e.do(t, "POST", "/api/v1/tasks", string(body))
	if code != 400 || v1Code(rb) != "bad_request" || strings.Contains(string(rb), cSHA) {
		t.Fatalf("submit b: %d %s", code, rb)
	}
	if n := e.taskCount(t); n != 0 {
		t.Fatalf("%d tasks", n)
	}
}
