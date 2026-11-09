package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/orch/api"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
)

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func put(t *testing.T, repo, name, content string) {
	t.Helper()
	p := filepath.Join(repo, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const toyYAML = "version: 1\nprotected:\n  - \"tests/**\"\n  - \".aigate.yml\"\nchecks:\n" +
	"  - id: unit\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"test -f ok.flag\"]\n    timeout: 20s\n"

type fakeAgent struct {
	head  string
	onRun func() // optional: runs while the agent "works" (between prepare and publish)
}

func (fakeAgent) Prepare(context.Context, string, string, string) error { return nil }
func (f fakeAgent) Run(context.Context, string, string, string, time.Duration) (vm.RunResult, error) {
	if f.onRun != nil {
		f.onRun()
	}
	return vm.RunResult{}, nil
}
func (f fakeAgent) Collect(context.Context, string, string, string) (string, int, error) {
	return f.head, 1, nil
}

type localTest struct{}

func (localTest) Restore(context.Context) error    { return nil }
func (localTest) Executor() (gate.Executor, error) { return &gate.LocalExecutor{}, nil }

// e2eBaseCache, when a test sets it, is handed to the steps of the next newE2E*.
var e2eBaseCache *steps.BaseCache

// e2eAPIConfig, when a test sets it, adjusts the daemon config of the next
// newE2E* (e.g. to turn the HTTP listener on).
var e2eAPIConfig func(*api.Config)

type e2e struct {
	t          *testing.T
	st         *store.Store
	sock       string
	home       string // gate home
	repo       string
	base       string
	head       string
	policyPath string
	httpAddr   string // the daemon's HTTP listener, when e2eAPIConfig enabled it
	cleanups   []func()
}

// newE2E runs a real daemon with the real steps and the real gate (local
// executor) over a toy repo whose agent commit changes the given files.
func newE2E(t *testing.T, files map[string]string, pub steps.Publisher) *e2e {
	t.Helper()
	return newE2EPolicy(t, files, pub, nil, nil)
}

// newE2EPolicy is newE2E with a live policy in force; onRun runs while the
// agent works, so a test can change the policy file mid-task.
func newE2EPolicy(t *testing.T, files map[string]string, pub steps.Publisher, live *policy.Live, onRun func()) *e2e {
	t.Helper()
	dir, _ := os.MkdirTemp("/tmp", "orche2e")
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, _ = filepath.EvalSymlinks(dir)
	e := &e2e{t: t, sock: filepath.Join(dir, "s"), home: filepath.Join(dir, "gate-home"), repo: filepath.Join(dir, "repos", "proj")}
	os.MkdirAll(e.repo, 0o755)
	git(t, e.repo, "init", "-q", "-b", "main")
	put(t, e.repo, ".aigate.yml", toyYAML)
	put(t, e.repo, "ok.flag", "")
	put(t, e.repo, "tests/spec.txt", "line1\n")
	put(t, e.repo, "src/a.txt", "a\n")
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "base")
	e.base = git(t, e.repo, "rev-parse", "HEAD")
	git(t, e.repo, "checkout", "-q", "-b", "agent")
	for n, c := range files {
		put(t, e.repo, n, c)
	}
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "agent work")
	e.head = git(t, e.repo, "rev-parse", "HEAD")
	git(t, e.repo, "checkout", "-q", "main")

	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	t.Cleanup(func() { st.Close() })
	cfg := steps.DefaultConfig()
	cfg.Agent, cfg.Test = fakeAgent{head: e.head, onRun: onRun}, localTest{}
	cfg.Policy = live
	cfg.BaseCache = e2eBaseCache
	cfg.ReportDir, cfg.GateHome = filepath.Join(dir, "reports"), e.home
	cfg.Publisher = pub
	p, err := steps.NewProvider(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	acfg := api.Config{Socket: e.sock, AllowedRepos: []string{filepath.Join(dir, "repos")},
		GateHome: e.home, PollInterval: 20 * time.Millisecond}
	if live != nil {
		acfg.PolicyHash = func() string { return live.Current().Hash() }
	}
	if e2eAPIConfig != nil {
		e2eAPIConfig(&acfg)
	}
	d, err := api.NewDaemon(acfg, st, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	if a := d.HTTPAddr(); a != nil {
		e.httpAddr = a.String()
	}
	t.Cleanup(func() { cancel(); <-done })
	return e
}

func (e *e2e) orch(in io.Reader, args ...string) (int, string, string) {
	e.t.Helper()
	old := approveIn
	approveIn = in
	defer func() { approveIn = old }()
	var o, er bytes.Buffer
	code := run(append([]string{"--socket", e.sock, "--gate-home", e.home}, args...), &o, &er)
	return code, o.String(), er.String()
}

func (e *e2e) waitState(id int64, want store.State) store.Task {
	e.t.Helper()
	var tk store.Task
	for i := 0; i < 600; i++ {
		tk, _ = e.st.GetTask(context.Background(), id)
		if tk.State == want {
			return tk
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("task %d is %s (error %q), want %s", id, tk.State, tk.ErrorText, want)
	return tk
}

func (e *e2e) submit() int64 {
	e.t.Helper()
	code, out, errS := e.orch(nil, "--json", "submit", "--repo", e.repo, "--base", "main", "--prompt", "x", "--class", "docs", "--agent", "claude")
	var v api.TaskView
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil {
		e.t.Fatalf("submit %d %q %q", code, out, errS)
	}
	return v.ID
}

func fakeTTY(t *testing.T) {
	t.Helper()
	old := requireTTY
	requireTTY = func(io.Reader, io.Writer) error { return nil }
	t.Cleanup(func() { requireTTY = old })
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func approvalsFile(e *e2e) string { return gate.ApprovalsPath(e.home) }

// needs_approval -> grant written by the TTY path -> reverify -> verifying ->
// the real gate sees the grant -> passed -> done.
func TestApproveFlowEndToEnd(t *testing.T) {
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	id := e.submit()
	tk := e.waitState(id, store.StateNeedsApproval)
	if tk.Verdict != "check_weakened" {
		t.Fatalf("verdict %q", tk.Verdict)
	}
	// The queue lists it, in Korean and as JSON.
	code, out, _ := e.orch(nil, "approvals")
	if code != 0 || !strings.Contains(out, "protected_modified") || !strings.Contains(out, "종류") {
		t.Fatalf("approvals %d %q", code, out)
	}
	code, out, _ = e.orch(nil, "--json", "approvals")
	var list []api.ApprovalView
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list) != 1 || list[0].Head != e.head || list[0].Base != e.base {
		t.Fatalf("json approvals %d %q", code, out)
	}
	// A process that only calls reverify, without a grant, changes nothing.
	if code, _, errS := e.orch(nil, "reverify", "1"); code != 1 || !strings.Contains(errS, "grant") {
		t.Fatalf("reverify without grant: %d %q", code, errS)
	}
	if got, _ := e.st.GetTask(context.Background(), id); got.State != store.StateNeedsApproval {
		t.Fatalf("task moved to %s", got.State)
	}
	if _, err := os.Stat(approvalsFile(e)); err == nil {
		t.Fatal("grant file exists before any approval")
	}
	// The human approves in a terminal: same screen as `aigate approve`.
	fakeTTY(t)
	code, out, errS := e.orch(strings.NewReader("yes\n"), "approve", "1")
	if code != 0 {
		t.Fatalf("approve %d out %q err %q", code, out, errS)
	}
	for _, want := range []string{"BEGIN HASHED DIFF", "+line2", "hash: " + list[0].Hash, "approved.", "재검증"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if m := mode(t, approvalsFile(e)); m != 0o600 {
		t.Errorf("approvals.json mode %04o", m)
	}
	if m := mode(t, filepath.Dir(approvalsFile(e))); m != 0o700 {
		t.Errorf("approvals dir mode %04o", m)
	}
	done := e.waitState(id, store.StateDone)
	if done.Verdict != "ok" {
		t.Fatalf("verdict after approval %q", done.Verdict)
	}
	if code, out, _ = e.orch(nil, "approvals"); !strings.Contains(out, "없습니다") {
		t.Fatalf("queue after approval: %q", out)
	}
	var path []store.State
	evs, _ := e.st.Events(context.Background(), id)
	for _, ev := range evs {
		path = append(path, ev.To)
	}
	var sawNeeds, sawBack bool
	for i, s := range path {
		if s == store.StateNeedsApproval {
			sawNeeds = true
		}
		if sawNeeds && s == store.StateVerifying && i > 0 {
			sawBack = true
		}
	}
	if !sawBack {
		t.Fatalf("path %v", path)
	}
}

func TestApprovalRequiresTTY(t *testing.T) {
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	// The real check, with stdin/stdout that are not terminals (buffers, a pipe).
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	for name, in := range map[string]io.Reader{"buffer": strings.NewReader("yes\n"), "pipe": pr} {
		code, out, errS := e.orch(in, "approve", "1")
		if code != 64 || !strings.Contains(errS, "터미널") || strings.Contains(out, "BEGIN HASHED DIFF") {
			t.Fatalf("%s: code %d out %q err %q", name, code, out, errS)
		}
	}
	if _, err := os.Stat(approvalsFile(e)); err == nil {
		t.Fatal("a grant was written without a terminal")
	}
	if got, _ := e.st.GetTask(context.Background(), id); got.State != store.StateNeedsApproval {
		t.Fatalf("task moved to %s", got.State)
	}
	// Even with a terminal, anything but "yes" grants nothing.
	fakeTTY(t)
	for _, ans := range []string{"no\n", "\n", "y\n", ""} {
		if code, _, _ := e.orch(strings.NewReader(ans), "approve", "1"); code != 64 {
			t.Fatalf("answer %q: code %d", ans, code)
		}
	}
	if _, err := os.Stat(approvalsFile(e)); err == nil {
		t.Fatal("a grant was written without a yes")
	}
	if got, _ := e.st.GetTask(context.Background(), id); got.State != store.StateNeedsApproval {
		t.Fatalf("task moved to %s", got.State)
	}
}

// An entry whose hash is not the hash of the task's current diff is never granted.
func TestApproveHashMismatchRejected(t *testing.T) {
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	// A stale/forged entry: same task and head, another hash (it supersedes the real one).
	if _, err := e.st.AddApproval(context.Background(), store.Approval{TaskID: id, Kind: store.KindProtectedModified,
		Hash: "deadbeef", Base: e.base, Head: e.head, Reason: "forged"}); err != nil {
		t.Fatal(err)
	}
	fakeTTY(t)
	code, out, errS := e.orch(strings.NewReader("yes\n"), "approve", "2")
	if code != 64 || !strings.Contains(errS, "해시") || strings.Contains(out, "approved.") {
		t.Fatalf("code %d out %q err %q", code, out, errS)
	}
	if _, err := os.Stat(approvalsFile(e)); err == nil {
		t.Fatal("grant written for a mismatching hash")
	}
	// The superseded entry cannot be approved either.
	if code, _, errS = e.orch(strings.NewReader("yes\n"), "approve", "1"); code != 64 || !strings.Contains(errS, "대기 중인 항목이 아닙니다") {
		t.Fatalf("superseded: %d %q", code, errS)
	}
}

// TestOldShortHashPendingEntry: a pending row written by a build before
// 2026-11-09 carries the 16-hex prefix of the diff hash, and approvals.json may
// hold a grant of that short hash. Nothing migrates them: listing and the
// approve screen handle the row without crashing, and it can never be granted
// or count as approved (the gate's full 64-hex hash does not match it).
func TestOldShortHashPendingEntry(t *testing.T) {
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	real, err := e.st.GetApproval(context.Background(), 1)
	if err != nil || len(real.Hash) != 64 {
		t.Fatalf("the gate's entry must carry a 64-hex hash: %+v %v", real, err)
	}
	old := real.Hash[:16]
	if _, err := e.st.AddApproval(context.Background(), store.Approval{TaskID: id, Kind: store.KindProtectedModified,
		Hash: old, Base: e.base, Head: e.head, Reason: "from an old build"}); err != nil {
		t.Fatal(err)
	}
	// An old build's grant of the short hash in approvals.json.
	rid, _, _ := gate.RepoID(e.repo)
	if err := (approval.Store{Path: approvalsFile(e)}).Grant(rid, e.base, old, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := (approvals.Grants{Home: e.home}).Granted(e.repo, real); err != nil || ok {
		t.Fatalf("an old 16-hex grant must not count for the full hash: %v %v", ok, err)
	}
	code, out, errS := e.orch(nil, "approvals")
	if code != 0 || !strings.Contains(out, old[:12]) || !strings.Contains(out, "from an old build") {
		t.Fatalf("approvals listing: %d %q %q", code, out, errS)
	}
	code, out, errS = e.orch(nil, "--json", "approvals")
	var list []api.ApprovalView
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list) != 1 || list[0].Hash != old {
		t.Fatalf("json approvals: %d %q %q", code, out, errS)
	}
	fakeTTY(t)
	code, out, errS = e.orch(strings.NewReader("yes\n"), "approve", "2")
	if code != 64 || !strings.Contains(errS, "해시") || strings.Contains(out, "approved.") {
		t.Fatalf("approve of a 16-hex entry: code %d out %q err %q", code, out, errS)
	}
	if got, _ := e.st.GetTask(context.Background(), id); got.State != store.StateNeedsApproval {
		t.Fatalf("task moved to %s", got.State)
	}
	// Legacy mode: the old grant matches the old row, so reverify lets the
	// gate re-judge; the gate's full hash is not granted, so the task comes
	// back to needs_approval with the 64-hex entry pending, and a human
	// approves that one.
	if code, out, errS = e.orch(nil, "reverify", "2"); code != 0 {
		t.Fatalf("reverify of the old row: %d %q %q", code, out, errS)
	}
	e.waitState(id, store.StateNeedsApproval)
	var again []api.ApprovalView
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, out, _ = e.orch(nil, "--json", "approvals")
		again = nil
		_ = json.Unmarshal([]byte(out), &again)
		if len(again) == 1 && again[0].Hash == real.Hash || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(again) != 1 || again[0].Hash != real.Hash {
		t.Fatalf("after re-judging, want the 64-hex entry pending: %q", out)
	}
	if code, out, errS = e.orch(strings.NewReader("yes\n"), "approve", strconv.FormatInt(again[0].ID, 10)); code != 0 {
		t.Fatalf("re-approval: %d %q %q", code, out, errS)
	}
	if done := e.waitState(id, store.StateDone); done.Verdict != "ok" {
		t.Fatalf("verdict after re-approval %q", done.Verdict)
	}
}

func TestDestructiveBlockedNeverApprovableViaCLI(t *testing.T) {
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	if _, err := e.st.AddApproval(context.Background(), store.Approval{TaskID: id, Kind: store.KindDestructiveBlocked,
		Hash: "d", Base: e.base, Head: e.head, Reason: "blocked"}); err != nil {
		t.Fatal(err)
	}
	fakeTTY(t)
	code, _, errS := e.orch(strings.NewReader("yes\n"), "approve", "2")
	if code != 64 || !strings.Contains(errS, "destructive_blocked") {
		t.Fatalf("approve: %d %q", code, errS)
	}
	if code, _, _ = e.orch(nil, "reverify", "2"); code == 0 {
		t.Fatal("reverify of destructive_blocked succeeded")
	}
	if _, err := os.Stat(approvalsFile(e)); err == nil {
		t.Fatal("grant written")
	}
}

// publishing parks outside the allowlist; the human approves that exact action
// from the terminal; the task then pushes and finishes.
func TestPublishOutsideAllowlistApproveFlow(t *testing.T) {
	bare, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, bare, "init", "-q", "--bare")
	// The publisher must read grants from the daemon's gate home, which
	// newE2E creates, so it is built right after and plugged in lazily.
	lazy := &lazyPublisher{}
	e := newE2E(t, map[string]string{"src/new.txt": "n\n"}, lazy)
	pub, err := publish.New(publish.Config{Allow: policy.NewAllowlist(), Remote: bare, Grants: approvals.Grants{Home: e.home}})
	if err != nil {
		t.Fatal(err)
	}
	lazy.set(pub)
	id := e.submit()
	tk := e.waitState(id, store.StateNeedsApproval)
	pend, _ := e.st.PendingApprovals(context.Background())
	if len(pend) != 1 || pend[0].Kind != store.KindPublishOutsideAllowlist || tk.Verdict != "ok" {
		t.Fatalf("pending %+v verdict %q", pend, tk.Verdict)
	}
	fakeTTY(t)
	code, out, errS := e.orch(strings.NewReader("yes\n"), "approve", "1")
	if code != 0 || !strings.Contains(out, "push_branch") {
		t.Fatalf("approve %d %q %q", code, out, errS)
	}
	e.waitState(id, store.StateDone)
	if got := git(t, bare, "for-each-ref", "--format=%(refname) %(objectname)"); got != "refs/heads/orch/1 "+e.head {
		t.Fatalf("remote refs %q", got)
	}
}

// lazyPublisher lets the test build the publisher after the daemon's gate home exists.
type lazyPublisher struct {
	mu sync.Mutex
	p  *publish.Publisher
}

func (l *lazyPublisher) set(p *publish.Publisher) { l.mu.Lock(); l.p = p; l.mu.Unlock() }
func (l *lazyPublisher) get() *publish.Publisher  { l.mu.Lock(); defer l.mu.Unlock(); return l.p }
func (l *lazyPublisher) Publish(ctx context.Context, t store.Task) error {
	return l.get().Publish(ctx, t)
}
func (l *lazyPublisher) PublishNote() string { return l.get().PublishNote() }
func (l *lazyPublisher) Plan() []string      { return l.get().Plan() }
func (l *lazyPublisher) Granted(t store.Task, kind string) bool {
	return l.get().Granted(t, kind)
}
func (l *lazyPublisher) Permit(kind string) error { return l.get().Permit(kind) }
