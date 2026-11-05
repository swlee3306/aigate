package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
)

type env struct {
	sock   string
	root   string
	repo   string
	sha    string
	st     *store.Store
	client *http.Client
	d      *Daemon
}

type noSteps struct{}

func (noSteps) Steps() []machine.Step { return nil }

// shortDir returns a short temp dir: unix socket paths are limited to ~104
// bytes and t.TempDir() can exceed that.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "orchd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	r, _ := filepath.EvalSymlinks(d)
	return r
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newEnv(t *testing.T, p StepProvider, conc int) *env {
	t.Helper()
	dir := shortDir(t)
	e := &env{sock: filepath.Join(dir, "o.sock"), root: filepath.Join(dir, "repos")}
	e.repo = filepath.Join(e.root, "proj")
	if err := os.MkdirAll(e.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, e.repo, "init", "-q")
	git(t, e.repo, "commit", "-q", "--allow-empty", "-m", "init")
	e.sha = git(t, e.repo, "rev-parse", "HEAD")

	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	d, err := NewDaemon(Config{Socket: e.sock, Concurrency: conc, AllowedRepos: []string{e.root}}, st, p)
	if err != nil {
		t.Fatal(err)
	}
	e.d = d
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("daemon exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	e.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.sock)
		}}}
	return e
}

func (e *env) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://orchd"+path, strings.NewReader(body))
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) submit(t *testing.T, repo, base, prompt, class, agent string) (int, []byte) {
	b, _ := json.Marshal(map[string]string{"repo": repo, "base": base, "prompt": prompt, "class": class, "agent": agent})
	return e.do(t, "POST", "/tasks", string(b))
}

func errText(b []byte) string {
	var m struct{ Error string }
	_ = json.Unmarshal(b, &m)
	return m.Error
}

func TestSubmitGetRoundTrip(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	code, b := e.submit(t, e.repo, "HEAD", "do the thing", "버그수정", "claude")
	if code != http.StatusCreated {
		t.Fatalf("submit %d %s", code, b)
	}
	var v TaskView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.ID == 0 || v.Repo != e.repo || v.Base != e.sha || v.Agent != "claude" || v.Class != "버그수정" || v.Prompt != "do the thing" {
		t.Fatalf("bad view %+v", v)
	}
	code, b = e.do(t, "GET", fmt.Sprintf("/tasks/%d", v.ID), "")
	if code != 200 {
		t.Fatalf("get %d %s", code, b)
	}
	var g TaskView
	json.Unmarshal(b, &g)
	if g.ID != v.ID || g.Prompt != "do the thing" {
		t.Fatalf("got %+v", g)
	}
	code, b = e.do(t, "GET", "/tasks", "")
	var list []TaskView
	if code != 200 || json.Unmarshal(b, &list) != nil || len(list) != 1 || list[0].ID != v.ID {
		t.Fatalf("list %d %s", code, b)
	}
	if code, _ = e.do(t, "GET", "/tasks/9999", ""); code != 404 {
		t.Fatalf("missing task => %d", code)
	}
	if code, _ = e.do(t, "GET", "/tasks/abc", ""); code != 400 {
		t.Fatalf("bad id => %d", code)
	}
	code, b = e.do(t, "GET", "/approvals", "")
	if code != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("approvals %d %s", code, b)
	}
}

func TestInvalidInputsRejected(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	outside := shortDir(t)
	git(t, outside, "init", "-q")
	git(t, outside, "commit", "-q", "--allow-empty", "-m", "x")
	plain := filepath.Join(e.root, "plain")
	os.MkdirAll(plain, 0o755)
	link := filepath.Join(e.root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", MaxPromptBytes+1)

	cases := []struct {
		name                           string
		repo, base, prompt, cls, agent string
	}{
		{"empty prompt", e.repo, "HEAD", "", "x", "claude"},
		{"blank prompt", e.repo, "HEAD", "  \n", "x", "claude"},
		{"long prompt", e.repo, "HEAD", long, "x", "claude"},
		{"unknown agent", e.repo, "HEAD", "p", "x", "gpt"},
		{"empty agent", e.repo, "HEAD", "p", "x", ""},
		{"bad class", e.repo, "HEAD", "p", "Bad Class!", "claude"},
		{"empty class", e.repo, "HEAD", "p", "", "claude"},
		{"long class", e.repo, "HEAD", "p", strings.Repeat("a", 33), "claude"},
		{"repo outside roots", outside, "HEAD", "p", "x", "claude"},
		{"relative repo", "proj", "HEAD", "p", "x", "claude"},
		{"traversal", e.repo + "/../../etc", "HEAD", "p", "x", "claude"},
		{"traversal back inside", e.root + "/x/../proj", "HEAD", "p", "x", "claude"},
		{"trailing slash", e.repo + "/", "HEAD", "p", "x", "claude"},
		{"symlink escape", link, "HEAD", "p", "x", "claude"},
		{"non-git dir", plain, "HEAD", "p", "x", "claude"},
		{"missing dir", filepath.Join(e.root, "nope"), "HEAD", "p", "x", "claude"},
		{"base not found", e.repo, "deadbeefdeadbeef", "p", "x", "claude"},
		{"base option-like", e.repo, "--output=/tmp/x", "p", "x", "claude"},
		{"base with space", e.repo, "HEAD foo", "p", "x", "claude"},
		{"base empty", e.repo, "", "p", "x", "claude"},
	}
	for _, c := range cases {
		code, b := e.submit(t, c.repo, c.base, c.prompt, c.cls, c.agent)
		if code < 400 || code >= 500 || errText(b) == "" {
			t.Errorf("%s: want 4xx with error, got %d %s", c.name, code, b)
		}
	}
	// outside root is a policy refusal.
	if code, _ := e.submit(t, outside, "HEAD", "p", "x", "claude"); code != http.StatusForbidden {
		t.Errorf("outside root should be 403, got %d", code)
	}
	if l, _ := e.st.ListTasks(context.Background(), 0); len(l) != 0 {
		t.Fatalf("rejected input created %d tasks", len(l))
	}

	raw := []struct{ name, body string }{
		{"unknown field", `{"repo":"` + e.repo + `","base":"HEAD","prompt":"p","class":"x","agent":"claude","extra":1}`},
		{"not json", `hello`},
		{"trailing data", `{"repo":"a"} {"repo":"b"}`},
		{"empty body", ``},
		{"wrong type", `{"repo":5}`},
	}
	for _, c := range raw {
		code, b := e.do(t, "POST", "/tasks", c.body)
		if code != 400 || errText(b) == "" {
			t.Errorf("%s: got %d %s", c.name, code, b)
		}
	}
	big := `{"prompt":"` + strings.Repeat("a", MaxBodyBytes+10) + `"}`
	if code, b := e.do(t, "POST", "/tasks", big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: got %d %s", code, b)
	}
	if l, _ := e.st.ListTasks(context.Background(), 0); len(l) != 0 {
		t.Fatalf("rejected input created %d tasks", len(l))
	}
}

func TestDefaultNoRepoRootsRefusesAll(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	st := e.st
	// a daemon with no allowed repos refuses every path
	d, err := NewDaemon(Config{Socket: filepath.Join(shortDir(t), "s")}, st, noSteps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.handler.validate(submitRequest{Repo: e.repo, Base: "HEAD", Prompt: "p", Class: "x", Agent: "claude"}); err == nil {
		t.Fatal("expected refusal with no allowed roots")
	}
}

func TestSocketModeAndStaleHandling(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	fi, err := os.Lstat(e.sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}

	// A second daemon must refuse while the first answers.
	d2, _ := NewDaemon(Config{Socket: e.sock}, e.st, noSteps{})
	if err := d2.Run(context.Background()); err == nil {
		t.Fatal("second daemon started on a live socket")
	}

	// A regular file is never removed.
	dir := shortDir(t)
	f := filepath.Join(dir, "file")
	os.WriteFile(f, []byte("x"), 0o600)
	d3, _ := NewDaemon(Config{Socket: f}, e.st, noSteps{})
	if err := d3.Run(context.Background()); err == nil {
		t.Fatal("started over a non-socket")
	}
	if b, _ := os.ReadFile(f); string(b) != "x" {
		t.Fatal("non-socket file was touched")
	}
}

func TestStaleSocketRemoved(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close() // file stays, nobody answers
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("setup: stale socket missing")
	}
	st, _ := store.Open(filepath.Join(dir, "db", "o.db"))
	defer st.Close()
	d, _ := NewDaemon(Config{Socket: sock}, st, noSteps{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("stale socket not replaced: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	cancel()
	<-done
	if _, err := os.Lstat(sock); err == nil {
		t.Fatal("socket left behind after shutdown")
	}
}

func TestSocketParentDirCreated0700(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "run", "o.sock")
	st, _ := store.Open(filepath.Join(dir, "db", "o.db"))
	defer st.Close()
	d, _ := NewDaemon(Config{Socket: sock}, st, noSteps{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	fi, err := os.Stat(filepath.Dir(sock))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("parent: %v %v", fi, err)
	}
	cancel()
	<-done
}

func TestConcurrentSubmissions(t *testing.T) {
	e := newEnv(t, noSteps{}, 2)
	const n = 24
	var wg sync.WaitGroup
	ids := make(chan int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, b := e.submit(t, e.repo, "HEAD", fmt.Sprintf("p%d", i), "x", "codex")
			if code != 201 {
				t.Errorf("submit %d: %d %s", i, code, b)
				return
			}
			var v TaskView
			json.Unmarshal(b, &v)
			ids <- v.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	_, b := e.do(t, "GET", "/tasks", "")
	var list []TaskView
	json.Unmarshal(b, &list)
	if len(seen) != n || len(list) != n {
		t.Fatalf("ids=%d list=%d", len(seen), len(list))
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	_, b := e.submit(t, e.repo, "HEAD", "p", "x", "claude")
	var v TaskView
	json.Unmarshal(b, &v)
	path := fmt.Sprintf("/tasks/%d/cancel", v.ID)
	code, b := e.do(t, "POST", path, "")
	if code != 200 {
		t.Fatalf("cancel %d %s", code, b)
	}
	var c TaskView
	json.Unmarshal(b, &c)
	if c.State != "cancelled" {
		t.Fatalf("state %s", c.State)
	}
	if code, _ = e.do(t, "POST", path, ""); code != http.StatusConflict {
		t.Fatalf("second cancel => %d", code)
	}
	if code, _ = e.do(t, "POST", "/tasks/9999/cancel", ""); code != 404 {
		t.Fatalf("cancel missing => %d", code)
	}
	if code, _ = e.do(t, "POST", "/tasks/x/cancel", ""); code != 400 {
		t.Fatalf("cancel bad id => %d", code)
	}
}

type recStep struct {
	name machine.State
	next machine.State
}

func (r recStep) Name() machine.State { return r.name }
func (r recStep) Run(context.Context, store.Task) (machine.Outcome, error) {
	return machine.Outcome{Next: r.next}, nil
}

type provider []machine.Step

func (p provider) Steps() []machine.Step { return p }

func TestDaemonRunsTasksThroughMachine(t *testing.T) {
	e := newEnv(t, provider{
		recStep{machine.StatePreparing, machine.StateAgentRunning},
		recStep{machine.StateAgentRunning, machine.StateCollecting},
		recStep{machine.StateCollecting, machine.StateVerifying},
		recStep{machine.StateVerifying, machine.StatePassed},
		recStep{machine.StatePublishing, machine.StateDone},
	}, 1)
	_, b := e.submit(t, e.repo, "HEAD", "p", "x", "claude")
	var v TaskView
	json.Unmarshal(b, &v)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, b = e.do(t, "GET", fmt.Sprintf("/tasks/%d", v.ID), "")
		var g TaskView
		json.Unmarshal(b, &g)
		if g.State == "done" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task never reached done")
}

func TestUnknownRouteJSON(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	code, b := e.do(t, "GET", "/nope", "")
	if code != 404 || errText(b) == "" {
		t.Fatalf("%d %s", code, b)
	}
	code, b = e.do(t, "DELETE", "/tasks", "")
	if code != 405 || errText(b) == "" {
		t.Fatalf("%d %s", code, b)
	}
	_ = bytes.MinRead
}

// The daemon's own environment must not redirect git away from the repo.
func TestGitEnvNotInherited(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	other := shortDir(t)
	git(t, other, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if code, b := e.submit(t, e.repo, "HEAD", "p", "x", "claude"); code != 201 {
		t.Fatalf("%d %s", code, b)
	}
}
