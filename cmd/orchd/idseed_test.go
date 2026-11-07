package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aigate/internal/orch/api"
	"aigate/internal/orch/store"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func shortTmp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "idseed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	return d
}

func newRepo(t *testing.T, root, name string, refs ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)
	gitT(t, dir, "init", "-q")
	gitT(t, dir, "commit", "-q", "--allow-empty", "-m", "i")
	for _, r := range refs {
		gitT(t, dir, "update-ref", r, "HEAD")
	}
	return dir
}

func newBare(t *testing.T, parent string, refs ...string) string {
	t.Helper()
	dir := filepath.Join(parent, "remote.git")
	os.MkdirAll(dir, 0o755)
	gitT(t, dir, "init", "-q", "--bare")
	// borrow a commit from a scratch repo
	src := newRepo(t, parent, "scratch")
	gitT(t, src, "push", "-q", dir, "HEAD:refs/heads/base")
	for _, r := range refs {
		gitT(t, src, "push", "-q", dir, "HEAD:"+r)
	}
	os.RemoveAll(src)
	return dir
}

type logs struct {
	mu sync.Mutex
	l  []string
}

func (l *logs) f(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.l = append(l.l, fmt.Sprintf(format, a...))
}
func (l *logs) text() string { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.l, "\n") }

func openT(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func nextID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.CreateTask(context.Background(), store.Task{Repo: "r", Base: "b", Prompt: "p", Class: "c", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestParseTaskRef(t *testing.T) {
	for _, c := range []struct {
		ref    string
		remote bool
		id     int64
		ok     bool
	}{
		{"refs/orch/7/head", false, 7, true},
		{"refs/orch/007/head", false, 7, true},
		{"refs/orch/x/head", false, 0, false},
		{"refs/orch/-3/head", false, 0, false},
		{"refs/orch/7/tail", false, 0, false},
		{"refs/orch/7/head/x", false, 0, false},
		{"refs/orch/1000000000001/head", false, 0, false},
		{"refs/orch/99999999999999999999/head", false, 0, false},
		{"refs/orch//head", false, 0, false},
		{"refs/heads/orch/20", true, 20, true},
		{"refs/heads/orch/20/x", true, 0, false},
		{"refs/heads/orch/abc", true, 0, false},
		{"refs/heads/orch/+5", true, 0, false},
		{"refs/heads/main", true, 0, false},
	} {
		id, ok := parseTaskRef(c.ref, c.remote)
		if id != c.id || ok != c.ok {
			t.Errorf("%q remote=%v: got %d,%v want %d,%v", c.ref, c.remote, id, ok, c.id, c.ok)
		}
	}
}

func TestSeedFromRepoAndRemote(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	newRepo(t, roots, "a", "refs/orch/7/head", "refs/orch/12/head", "refs/orch/abc/head", "refs/orch/-4/head",
		"refs/orch/99999999999999999/head", "refs/orch/30/tail", "refs/heads/orch/90")
	newRepo(t, roots, "noref")
	os.MkdirAll(filepath.Join(roots, "plain"), 0o755) // not a git repo: skipped silently
	remote := newBare(t, dir, "refs/heads/orch/20", "refs/heads/orch/zzz")
	st := openT(t, dir)
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots}, remote, lg.f)
	if id := nextID(t, st); id != 21 {
		t.Fatalf("first id %d, want 21; log:\n%s", id, lg.text())
	}
	if !strings.Contains(lg.text(), "task id sequence starts after 20 (highest id found in refs: "+remote+")") {
		t.Fatalf("log:\n%s", lg.text())
	}
	if strings.Contains(lg.text(), "WARNING") || strings.Contains(lg.text(), "warning") {
		t.Fatalf("unexpected warning:\n%s", lg.text())
	}
}

func TestSeedRepoOnly(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	newRepo(t, roots, "a", "refs/orch/7/head", "refs/orch/12/head")
	st := openT(t, dir)
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots}, "https://example.invalid/x.git", lg.f)
	if id := nextID(t, st); id != 13 {
		t.Fatalf("id %d, want 13", id)
	}
}

func TestSeedNothingToDo(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	newRepo(t, roots, "noref")
	st := openT(t, dir)
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots}, "", lg.f)
	if id := nextID(t, st); id != 1 {
		t.Fatalf("id %d, want 1", id)
	}
	if lg.text() != "" {
		t.Fatalf("log not empty:\n%s", lg.text())
	}
}

func TestSeedDoesNotLowerAndStaysQuiet(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	newRepo(t, roots, "a", "refs/orch/7/head")
	st := openT(t, dir)
	for i := 0; i < 30; i++ {
		nextID(t, st)
	}
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots}, "", lg.f)
	if id := nextID(t, st); id != 31 {
		t.Fatalf("id %d, want 31", id)
	}
	if lg.text() != "" {
		t.Fatalf("logged although nothing was raised:\n%s", lg.text())
	}
}

func TestSeedUnreadableSourcesAreWarningsOnly(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	newRepo(t, roots, "good", "refs/orch/5/head")
	// a "repo" whose .git is garbage
	bad := filepath.Join(roots, "bad")
	os.MkdirAll(filepath.Join(bad, ".git"), 0o755)
	st := openT(t, dir)
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots, filepath.Join(dir, "missing")}, filepath.Join(dir, "no-such-remote.git"), lg.f)
	if id := nextID(t, st); id != 6 {
		t.Fatalf("id %d, want 6; log:\n%s", id, lg.text())
	}
	out := lg.text()
	for _, want := range []string{bad, "missing", "no-such-remote.git"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "WARNING"); n != 1 {
		t.Errorf("%d loud lines, want 1:\n%s", n, out)
	}
}

func TestSeedIgnoresHostileGitEnv(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	newRepo(t, roots, "a", "refs/orch/7/head")
	evil := newRepo(t, dir, "evil", "refs/orch/999/head")
	t.Setenv("GIT_DIR", filepath.Join(evil, ".git"))
	t.Setenv("GIT_WORK_TREE", evil)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(evil, ".git", "objects"))
	t.Setenv("GIT_NAMESPACE", "x")
	st := openT(t, dir)
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots}, "", lg.f)
	if id := nextID(t, st); id != 8 {
		t.Fatalf("id %d, want 8; log:\n%s", id, lg.text())
	}
}

// Starting orchd with an empty database next to old refs makes the first
// submitted task id follow them.
func TestEmptyDBNextToOldRefsContinuesIDs(t *testing.T) {
	dir := shortTmp(t)
	roots := filepath.Join(dir, "repos")
	repo := newRepo(t, roots, "p", "refs/orch/7/head", "refs/orch/12/head")
	remote := newBare(t, dir, "refs/heads/orch/20")
	st := openT(t, dir)
	var lg logs
	seedTaskIDs(context.Background(), st, []string{roots}, remote, lg.f)

	sock := filepath.Join(dir, "s")
	d, err := api.NewDaemon(api.Config{Socket: sock, Concurrency: 1, AllowedRepos: []string{roots}}, st, placeholderSteps{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	defer func() { cancel(); <-done }()
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := cl.Post("http://orchd/tasks", "application/json", strings.NewReader(
		`{"repo":"`+repo+`","base":"HEAD","prompt":"p","class":"x","agent":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	var v api.TaskView
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if v.ID != 21 {
		t.Fatalf("first task id %d, want 21", v.ID)
	}
}
