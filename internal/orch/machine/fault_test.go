package machine_test

// Fault-injection scenarios, one named test per row of the failure table in
// docs/07-DESIGN-orchestrator.md section 9 (and the extra cases of plan Task
// 13 step 3). They run the REAL step set (internal/orch/steps) and the REAL
// store and machine; only the VM drivers, the gate and the publisher are fakes.
// An "orchd crash" is simulated by abandoning a Machine and its store handle
// while a task is still claimed, then opening a NEW handle on the same file.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
	"aigate/internal/report"
	"aigate/internal/rules"
)

const head1 = "0123456789abcdef0123456789abcdef01234567"
const head2 = "89abcdef0123456789abcdef0123456789abcdef"

// ---- fakes -----------------------------------------------------------------

type fAgent struct {
	mu                       sync.Mutex
	prepares, runs, collects int
	head                     string
	runErr                   func(n int) error // n is the 1-based call number
	onRun                    func()
	onCollect                func(n int)
}

func (f *fAgent) Prepare(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	return nil
}

func (f *fAgent) Run(context.Context, string, string, string, time.Duration) (vm.RunResult, error) {
	f.mu.Lock()
	f.runs++
	n, errf, hook := f.runs, f.runErr, f.onRun
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if errf != nil {
		return vm.RunResult{}, errf(n)
	}
	return vm.RunResult{}, nil
}

func (f *fAgent) Collect(context.Context, string, string) (string, int, error) {
	f.mu.Lock()
	f.collects++
	n, hook, head := f.collects, f.onCollect, f.head
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return head, 1, nil
}

func (f *fAgent) counts() (p, r, c int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.runs, f.collects
}

type fTest struct{ restores atomic.Int32 }

func (f *fTest) Restore(context.Context) error { f.restores.Add(1); return nil }
func (f *fTest) Executor() (gate.Executor, error) {
	return &gate.LocalExecutor{}, nil
}

// fPub is an idempotent remote: pushes are keyed by head, like a git push of
// the same commit. failAfterPush simulates the network dying after the push.
type fPub struct {
	mu            sync.Mutex
	calls         int
	pushed        map[string]int // head -> times the remote actually changed
	failAfterPush func(call int) error
	crashAfter    func(call int) bool
	onPublish     func()
}

func (p *fPub) Publish(_ context.Context, t store.Task) error {
	p.mu.Lock()
	p.calls++
	n := p.calls
	if p.pushed == nil {
		p.pushed = map[string]int{}
	}
	if _, ok := p.pushed[t.HeadSHA]; !ok {
		p.pushed[t.HeadSHA] = 1 // the remote changes once per distinct head
	}
	fail, crash, hook := p.failAfterPush, p.crashAfter, p.onPublish
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if crash != nil && crash(n) {
		panic("simulated orchd kill after push")
	}
	if fail != nil {
		return fail(n)
	}
	return nil
}

func (p *fPub) stats() (calls int, distinct int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, len(p.pushed)
}

// ---- rig -------------------------------------------------------------------

type rig struct {
	t      *testing.T
	path   string
	repo   string
	base   string
	agent  *fAgent
	test   steps.TestDriver
	pub    *fPub
	cfg    steps.Config
	home   string
	st     *store.Store
	m      *machine.Machine
	id     int64
	gateFn steps.GateFunc
}

func gitRepo(t *testing.T) (repo, base string) {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		full := append([]string{"-C", d, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	return d, run("rev-parse", "HEAD")
}

func okReport(o gate.Options) report.Report {
	return report.Report{SchemaVersion: 1, Category: rules.CategoryOK, Repo: o.Repo, Base: o.Base, Head: o.Head}
}

// protectedReport is a check_weakened verdict whose approval hash depends on
// the head, like the real gate (new commit => new hash).
func protectedReport(o gate.Options) report.Report {
	return report.Report{SchemaVersion: 1, Category: rules.CategoryWeakened, ExitCode: 3, Repo: o.Repo, Base: o.Base, Head: o.Head,
		Violations: []rules.Violation{{Rule: "protected_modified", Category: rules.CategoryWeakened, Detail: "protected file changed", IDs: []string{"tests/x"}}},
		Approvals:  []report.ApprovalState{{Hash: "hash-" + o.Head[:8], Valid: false}}}
}

func newRig(t *testing.T, mod func(*rig)) *rig {
	t.Helper()
	r := &rig{t: t, path: filepath.Join(t.TempDir(), "db", "orch.db"), agent: &fAgent{head: head1},
		test: &fTest{}, pub: &fPub{}, home: t.TempDir()}
	r.repo, r.base = gitRepo(t)
	r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) { return okReport(o), nil }
	if mod != nil {
		mod(r)
	}
	r.cfg = steps.DefaultConfig()
	r.cfg.Agent, r.cfg.Test, r.cfg.Publisher = r.agent, r.test, r.pub
	r.cfg.ReportDir = filepath.Join(t.TempDir(), "reports")
	r.cfg.GateHome = r.home
	r.cfg.Gate = func(ctx context.Context, o gate.Options) (report.Report, error) { return r.gateFn(ctx, o) }
	r.open()
	var err error
	r.id, err = r.st.CreateTask(context.Background(), store.Task{Repo: r.repo, Base: r.base, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// open (re)opens the database file with a new store handle and a new Machine,
// which is what a restarted orchd does (the caller decides whether to Recover).
func (r *rig) open() {
	r.t.Helper()
	st, err := store.Open(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { st.Close() })
	p, err := steps.NewProvider(st, r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	r.st, r.m = st, machine.New(st, p.Steps())
}

// restart abandons the current store/machine without any cleanup (a kill) and
// opens a fresh handle on the same file.
func (r *rig) restart() {
	r.t.Helper()
	r.st.Close() // the OS closes the fds of a killed process; no state is written
	r.open()
	if err := r.m.Recover(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) task() store.Task {
	r.t.Helper()
	tk, err := r.st.GetTask(context.Background(), r.id)
	if err != nil {
		r.t.Fatal(err)
	}
	return tk
}

func (r *rig) events() []store.Event {
	r.t.Helper()
	ev, err := r.st.Events(context.Background(), r.id)
	if err != nil {
		r.t.Fatal(err)
	}
	return ev
}

func (r *rig) drain() store.Task {
	r.t.Helper()
	for i := 0; i < 100; i++ {
		err := r.m.Tick(context.Background())
		if errors.Is(err, machine.ErrNoWork) {
			return r.task()
		}
		if err != nil {
			r.t.Fatal(err)
		}
	}
	r.t.Fatal("machine did not settle")
	return store.Task{}
}

// tickCatch runs one Tick and turns a panic (the simulated kill) into an error.
func tickCatch(m *machine.Machine) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("killed: %v", p)
		}
	}()
	return m.Tick(context.Background())
}

// tickUntil ticks until the task is in state s (before its step runs).
func (r *rig) tickUntil(s store.State) {
	r.t.Helper()
	for i := 0; i < 50 && r.task().State != s; i++ {
		if err := r.m.Tick(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
	if r.task().State != s {
		r.t.Fatalf("never reached %s, at %s", s, r.task().State)
	}
}

func count(evs []store.Event, from, to store.State) int {
	n := 0
	for _, e := range evs {
		if e.From == from && e.To == to {
			n++
		}
	}
	return n
}

func (r *rig) never(states ...store.State) {
	r.t.Helper()
	for _, e := range r.events() {
		for _, s := range states {
			if e.To == s {
				r.t.Fatalf("task reached %s: %+v", s, r.events())
			}
		}
	}
}

var notPassed = []store.State{machine.StatePassed, machine.StatePublishing, machine.StateDone}

func (r *rig) pending() []store.Approval {
	r.t.Helper()
	p, err := approvals.New(r.st).Pending(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// ---- row 1: check VM / agent VM unreachable -------------------------------

// Row 1 ("검사 VM·에이전트 VM 접속 불가"): the VM dies mid-run. The step reports
// an infrastructure error, the task goes error -> retry, is bounded by the
// attempt limit, and never reaches passed.
func TestFault_VMStoppedMidStep(t *testing.T) {
	t.Run("agent vm down for good: bounded retries then failed", func(t *testing.T) {
		r := newRig(t, func(r *rig) {
			r.agent.runErr = func(int) error { return errors.New("ssh: connect to host agent-vm: connection refused") }
		})
		tk := r.drain()
		if tk.State != machine.StateFailed {
			t.Fatalf("state %s", tk.State)
		}
		if _, runs, _ := r.agent.counts(); runs != machine.DefaultMaxAttempts {
			t.Fatalf("agent ran %d times, want %d", runs, machine.DefaultMaxAttempts)
		}
		if n := count(r.events(), machine.StateAgentRunning, machine.StateError); n != machine.DefaultMaxAttempts-1 {
			t.Fatalf("error transitions %d", n)
		}
		if !strings.Contains(tk.ErrorText, "connection refused") {
			t.Fatalf("error text %q", tk.ErrorText)
		}
		r.never(append(notPassed, machine.StateCollecting)...)
	})
	t.Run("agent vm comes back: retry succeeds", func(t *testing.T) {
		r := newRig(t, func(r *rig) {
			r.agent.runErr = func(n int) error {
				if n == 1 {
					return errors.New("ssh: connection reset")
				}
				return nil
			}
		})
		if tk := r.drain(); tk.State != machine.StateDone {
			t.Fatalf("state %s (%s)", tk.State, tk.ErrorText)
		}
		if _, runs, _ := r.agent.counts(); runs != 2 {
			t.Fatalf("runs %d", runs)
		}
	})
	t.Run("gate cannot reach the check vm: error, never passed", func(t *testing.T) {
		r := newRig(t, func(r *rig) {
			r.gateFn = func(context.Context, gate.Options) (report.Report, error) {
				return report.Report{}, errors.New("ssh: test-vm unreachable")
			}
		})
		tk := r.drain()
		if tk.State != machine.StateFailed || tk.Verdict != "" {
			t.Fatalf("state %s verdict %q", tk.State, tk.Verdict)
		}
		r.never(notPassed...)
	})
}

// ---- row 2: snapshot restore fails ----------------------------------------

type brokenCloud struct{ creates atomic.Int32 }

func (*brokenCloud) DeleteServer(context.Context, string) error { return nil }
func (c *brokenCloud) CreateVolumeFromSnapshot(context.Context, string, string) error {
	c.creates.Add(1)
	return errors.New("cinder: snapshot restore failed")
}
func (*brokenCloud) CreateServer(context.Context, string, string) error { return nil }
func (*brokenCloud) WaitSSH(context.Context, string) error              { return nil }

// Row 2 ("스냅샷 복원 실패"): the task errors, then fails after the limit, the
// gate never runs, and the (real) TestVM is marked unusable so no stale
// executor is handed out.
func TestFault_RestoreFailureMarksVMUnusable(t *testing.T) {
	cloud := &brokenCloud{}
	tv := &vm.TestVM{Cloud: cloud, RestoreTimeout: time.Second}
	var gateCalls atomic.Int32
	r := newRig(t, func(r *rig) {
		r.test = tv
		r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) {
			gateCalls.Add(1)
			return okReport(o), nil
		}
	})
	tk := r.drain()
	if tk.State != machine.StateFailed || !strings.Contains(tk.ErrorText, "restore") {
		t.Fatalf("state %s err %q", tk.State, tk.ErrorText)
	}
	if n := count(r.events(), machine.StateVerifying, machine.StateError); n != machine.DefaultMaxAttempts-1 {
		t.Fatalf("error transitions %d", n)
	}
	if int(cloud.creates.Load()) != machine.DefaultMaxAttempts {
		t.Fatalf("restore attempts %d", cloud.creates.Load())
	}
	if gateCalls.Load() != 0 {
		t.Fatal("gate ran on a VM whose restore failed")
	}
	if tv.Usable() == nil {
		t.Fatal("test VM is not marked unusable")
	}
	if ex, err := tv.Executor(); err == nil || ex != nil {
		t.Fatalf("executor handed out by an unusable VM: %v %v", ex, err)
	}
	r.never(notPassed...)
}

// ---- row 3: orchd restarts ------------------------------------------------

// Row 3 ("orchd 재시작"): orchd is killed while a step has claimed the task. A
// new process (new store handle, new Machine, Recover) re-runs the SAME step
// once, applies its result once, and nothing downstream happens twice.
func TestFault_OrchdKilledMidStep(t *testing.T) {
	r := newRig(t, func(r *rig) {
		r.agent.onCollect = func(n int) {
			if n == 1 {
				panic("simulated orchd kill during collect")
			}
		}
	})
	r.tickUntil(machine.StateCollecting)
	if err := tickCatch(r.m); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("expected the kill, got %v", err)
	}
	if got := r.task().State; got != machine.StateCollecting {
		t.Fatalf("state after kill %s", got)
	}
	if err := r.m.Tick(context.Background()); !errors.Is(err, machine.ErrNoWork) {
		t.Fatalf("claimed task was run by the same DB again: %v", err)
	}

	r.restart()
	if tk := r.drain(); tk.State != machine.StateDone {
		t.Fatalf("final %s (%s)", tk.State, tk.ErrorText)
	}
	p, run, c := r.agent.counts()
	if p != 1 || run != 1 || c != 2 {
		t.Fatalf("prepare/run/collect = %d/%d/%d, want 1/1/2", p, run, c)
	}
	evs := r.events()
	if n := count(evs, machine.StateCollecting, machine.StateVerifying); n != 1 {
		t.Fatalf("collect result applied %d times", n)
	}
	if n := count(evs, machine.StateCollecting, machine.StateCollecting); n != 1 {
		t.Fatalf("recovery events %d, want 1", n)
	}
	if calls, distinct := r.pub.stats(); calls != 1 || distinct != 1 {
		t.Fatalf("publications %d/%d", calls, distinct)
	}
}

// The same kill during verify, after the approval entry was written: the
// re-run must not duplicate the approval entry.
func TestFault_OrchdKilledAfterApprovalQueued(t *testing.T) {
	var first atomic.Bool
	var r *rig
	r = newRig(t, func(r *rig) {
		r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) {
			rep := protectedReport(o)
			if first.CompareAndSwap(false, true) {
				// The previous run got as far as queueing the entry, then died.
				if err := approvals.New(r.st).Enqueue(context.Background(), r.id, store.KindProtectedModified,
					o.Base, o.Head, rep.Approvals[0].Hash, "protected file changed"); err != nil {
					panic(err)
				}
				panic("simulated orchd kill after the approval was queued")
			}
			return rep, nil
		}
	})
	r.tickUntil(machine.StateVerifying)
	if err := tickCatch(r.m); err == nil {
		t.Fatal("expected the kill")
	}
	r.restart()
	if tk := r.drain(); tk.State != machine.StateNeedsApproval {
		t.Fatalf("state %s", tk.State)
	}
	if p := r.pending(); len(p) != 1 {
		t.Fatalf("approval entries after re-run: %+v", p)
	}
	r.never(notPassed...)
}

// A kill after the publisher pushed but before the state moved to done: the
// re-run publishes the same head again; the remote changed exactly once and the
// task reaches done once.
func TestFault_OrchdKilledAfterPublish(t *testing.T) {
	r := newRig(t, func(r *rig) { r.pub.crashAfter = func(n int) bool { return n == 1 } })
	r.tickUntil(machine.StatePublishing)
	if err := tickCatch(r.m); err == nil {
		t.Fatal("expected the kill")
	}
	r.restart()
	if tk := r.drain(); tk.State != machine.StateDone {
		t.Fatalf("state %s", tk.State)
	}
	if calls, distinct := r.pub.stats(); calls != 2 || distinct != 1 {
		t.Fatalf("publish calls %d, distinct pushes %d (want 2/1)", calls, distinct)
	}
	if n := count(r.events(), machine.StatePublishing, machine.StateDone); n != 1 {
		t.Fatalf("done applied %d times", n)
	}
}

// ---- row 4 (extra): publisher fails after push ----------------------------

// Plan Task 13 step 3 ("publisher failure after push"): the publisher errors
// after the remote changed. The step errors and retries; publishing the same
// head again is idempotent, so the task finishes once and nothing is pushed twice.
func TestFault_PublisherFailsAfterPush(t *testing.T) {
	r := newRig(t, func(r *rig) {
		r.pub.failAfterPush = func(n int) error {
			if n == 1 {
				return errors.New("network cut after push")
			}
			return nil
		}
	})
	if tk := r.drain(); tk.State != machine.StateDone {
		t.Fatalf("state %s (%s)", tk.State, tk.ErrorText)
	}
	calls, distinct := r.pub.stats()
	if calls != 2 || distinct != 1 {
		t.Fatalf("publish calls %d, distinct pushes %d (want 2/1)", calls, distinct)
	}
	if n := count(r.events(), machine.StatePublishing, machine.StateDone); n != 1 {
		t.Fatalf("done applied %d times", n)
	}
}

// ---- new commit while waiting for approval --------------------------------

// Plan Task 13 step 3 ("승인 중 새 커밋"): a task waits for approval of head1.
// A human's grant for that hash exists. When the task is judged again for a new
// head the old entry is superseded, the old grant does not match the new hash,
// and the task needs approval AGAIN with the new hash.
func TestFault_NewCommitDuringApprovalWait(t *testing.T) {
	r := newRig(t, func(r *rig) {
		r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) { return protectedReport(o), nil }
	})
	ctx := context.Background()
	if tk := r.drain(); tk.State != machine.StateNeedsApproval {
		t.Fatalf("state %s", tk.State)
	}
	old := r.pending()
	if len(old) != 1 || old[0].Hash != "hash-01234567" {
		t.Fatalf("entries %+v", old)
	}
	id, _, err := gate.RepoID(r.repo)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := approvals.Grants{Home: r.home}.Path()
	if err := (approval.Store{Path: path}).Grant(id, old[0].Base, old[0].Hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := (approvals.Grants{Home: r.home}).Granted(r.repo, old[0]); err != nil || !ok {
		t.Fatalf("grant for the first head not found: %v %v", ok, err)
	}

	// The agent's work changes: the task is judged again with the new head.
	if err := r.st.SetHeadSHA(ctx, r.id, head2); err != nil {
		t.Fatal(err)
	}
	var verify machine.Step
	p, err := steps.NewProvider(r.st, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Steps() {
		if s.Name() == machine.StateVerifying {
			verify = s
		}
	}
	out, err := verify.Run(ctx, r.task())
	if err != nil || out.Next != machine.StateNeedsApproval {
		t.Fatalf("re-judge: %+v %v", out, err)
	}
	pend := r.pending()
	if len(pend) != 1 || pend[0].Hash != "hash-89abcdef" || pend[0].Head != head2 {
		t.Fatalf("pending after new head: %+v", pend)
	}
	o, err := r.st.GetApproval(ctx, old[0].ID)
	if err != nil || o.Status != store.ApprovalSuperseded {
		t.Fatalf("old entry %+v %v", o, err)
	}
	if ok, _ := (approvals.Grants{Home: r.home}).Granted(r.repo, o); ok {
		t.Fatal("a superseded entry counted as granted")
	}
	if ok, _ := (approvals.Grants{Home: r.home}).Granted(r.repo, pend[0]); ok {
		t.Fatal("the old grant covers the new hash")
	}
	if err := r.m.Approve(ctx, old[0].ID); !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("approving the superseded entry: %v", err)
	}
	if got := r.task().State; got != machine.StateNeedsApproval {
		t.Fatalf("task left needs_approval: %s", got)
	}
	r.never(notPassed...)
}

// ---- approval granted, then cancelled -------------------------------------

// Plan Task 13 step 3 ("승인 후 취소"): approved, then cancelled before the
// task moved on: nothing is published and the task stays cancelled.
func TestFault_ApprovedThenCancelledNeverPublishes(t *testing.T) {
	var weak atomic.Bool
	weak.Store(true)
	r := newRig(t, func(r *rig) {
		r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) {
			if weak.Load() {
				return protectedReport(o), nil
			}
			return okReport(o), nil // would pass once approved
		}
	})
	ctx := context.Background()
	if tk := r.drain(); tk.State != machine.StateNeedsApproval {
		t.Fatalf("state %s", tk.State)
	}
	weak.Store(false)
	if err := r.m.Approve(ctx, r.pending()[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := r.task().State; got != machine.StateVerifying {
		t.Fatalf("after approve %s", got)
	}
	if err := r.m.Cancel(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	if tk := r.drain(); tk.State != machine.StateCancelled {
		t.Fatalf("state %s", tk.State)
	}
	if calls, _ := r.pub.stats(); calls != 0 {
		t.Fatalf("published %d times after cancel", calls)
	}
	r.never(notPassed...)
	if err := r.m.Cancel(ctx, r.id); !errors.Is(err, machine.ErrTerminal) {
		t.Fatalf("second cancel: %v", err)
	}
}

// Cancelled while the publish step is running: the in-flight call cannot be
// undone (documented), but its result is dropped; the task stays cancelled.
func TestFault_CancelledDuringPublishStaysCancelled(t *testing.T) {
	var r *rig
	r = newRig(t, func(r *rig) {
		r.pub.onPublish = func() {
			if err := r.m.Cancel(context.Background(), r.id); err != nil {
				panic(err)
			}
		}
	})
	if tk := r.drain(); tk.State != machine.StateCancelled {
		t.Fatalf("state %s", tk.State)
	}
	if n := count(r.events(), machine.StatePublishing, machine.StateDone); n != 0 {
		t.Fatal("done was recorded for a cancelled task")
	}
}

// ---- double tick on one DB ------------------------------------------------

// Plan Task 13 step 3 ("Tick 이중 실행"): the supported setup is ONE orchd
// process per database; the claim flag (tasks.running) is what stops two
// workers in that process from running one task twice. Two Machines on two
// store handles of the same file behave the same way inside one process.
func TestFault_DoubleTickRunsEachStepOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r := newRig(t, func(r *rig) {
		r.agent.onRun = func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	})
	second, err := store.Open(r.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	p2, err := steps.NewProvider(second, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	m2 := machine.New(second, p2.Steps())

	r.tickUntil(machine.StateAgentRunning)
	done := make(chan error, 1)
	go func() { done <- r.m.Tick(context.Background()) }()
	<-entered
	// The first worker holds the claim: the second sees no work at all.
	if err := m2.Tick(context.Background()); !errors.Is(err, machine.ErrNoWork) {
		t.Fatalf("second machine got %v while the task was claimed", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Now hammer both machines concurrently to the end.
	var wg sync.WaitGroup
	for _, m := range []*machine.Machine{r.m, m2} {
		wg.Add(1)
		go func(m *machine.Machine) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := m.Tick(context.Background()); err != nil && !errors.Is(err, machine.ErrNoWork) {
					t.Error(err)
					return
				}
			}
		}(m)
	}
	wg.Wait()
	if tk := r.task(); tk.State != machine.StateDone {
		t.Fatalf("state %s (%s)", tk.State, tk.ErrorText)
	}
	p, run, c := r.agent.counts()
	if p != 1 || run != 1 || c != 1 {
		t.Fatalf("prepare/run/collect = %d/%d/%d, want 1/1/1", p, run, c)
	}
	if calls, _ := r.pub.stats(); calls != 1 {
		t.Fatalf("published %d times", calls)
	}
}

// ---- report file missing or corrupted at verify time ----------------------

// Plan Task 13 step 3 ("보고서 손상/없음"): whatever goes wrong with the report
// (the check VM returned garbage, nothing, a report for another commit, or the
// report file cannot be written) is an infrastructure error, never a verdict
// and never passed.
func TestFault_CorruptedOrMissingReportIsErrorNotPassed(t *testing.T) {
	cases := map[string]func(r *rig){
		"gate fails to parse the report from the vm": func(r *rig) {
			r.gateFn = func(context.Context, gate.Options) (report.Report, error) {
				return report.Report{}, errors.New("decode report: invalid character '}'")
			}
		},
		"empty report (no category)": func(r *rig) {
			r.gateFn = func(context.Context, gate.Options) (report.Report, error) { return report.Report{}, nil }
		},
		"report names another head": func(r *rig) {
			r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) {
				rep := okReport(o)
				rep.Head = head2
				return rep, nil
			}
		},
		"report claims initial mode": func(r *rig) {
			r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) {
				rep := okReport(o)
				rep.Initial = true
				return rep, nil
			}
		},
		"unknown category": func(r *rig) {
			r.gateFn = func(_ context.Context, o gate.Options) (report.Report, error) {
				rep := okReport(o)
				rep.Category = "mostly_ok"
				return rep, nil
			}
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, mod)
			tk := r.drain()
			if tk.State != machine.StateFailed || tk.Verdict == "ok" || tk.ErrorText == "" {
				t.Fatalf("state %s verdict %q err %q", tk.State, tk.Verdict, tk.ErrorText)
			}
			r.never(notPassed...)
			if n := count(r.events(), machine.StateVerifying, machine.StateError); n != machine.DefaultMaxAttempts-1 {
				t.Fatalf("error transitions %d", n)
			}
		})
	}
	t.Run("report file cannot be written", func(t *testing.T) {
		r := newRig(t, nil)
		// A regular file where the report directory should be.
		if err := os.WriteFile(r.cfg.ReportDir, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		tk := r.drain()
		if tk.State != machine.StateFailed || tk.Verdict != "" {
			t.Fatalf("state %s verdict %q", tk.State, tk.Verdict)
		}
		r.never(notPassed...)
	})
}

// ---- database file deleted mid-run ----------------------------------------

// Plan Task 13 step 3 ("DB 파일 삭제"): the database file disappears while
// orchd runs. Ticking must not panic; it must never invent a verdict, and it
// must not silently finish the task on an unlinked file.
func TestFault_DBFileDeletedMidRun(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.tickUntil(machine.StateAgentRunning)
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(r.path + suf)
	}
	var tickErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic with the database deleted: %v", p)
			}
		}()
		tickErr = r.m.Tick(ctx)
	}()
	if !errors.Is(tickErr, store.ErrDBGone) {
		t.Fatalf("tick after the delete returned %v, want store.ErrDBGone", tickErr)
	}
	if p, run, c := r.agent.counts(); p != 1 || run != 0 || c != 0 {
		t.Fatalf("work continued on the unlinked file: %d/%d/%d", p, run, c)
	}
	// A restarted orchd opens a fresh, empty database: the old task is gone,
	// and nothing was published.
	r.restart()
	if _, err := r.st.GetTask(ctx, r.id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("task found in a fresh database: %v", err)
	}
	if calls, _ := r.pub.stats(); calls != 0 {
		t.Fatalf("published %d times", calls)
	}
}
