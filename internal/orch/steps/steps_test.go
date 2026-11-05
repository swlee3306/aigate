package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/gate"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
	"aigate/internal/report"
	"aigate/internal/rules"
)

const fakeHead = "0123456789abcdef0123456789abcdef01234567"

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, repo, name, content string) {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, repo, msg string) string {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", msg)
	return git(t, repo, "rev-parse", "HEAD")
}

const toyYAML = "version: 1\nprotected:\n  - \"tests/**\"\n  - \".aigate.yml\"\nchecks:\n" +
	"  - id: unit\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"test -f ok.flag\"]\n    timeout: 20s\n"

// toyRepo makes a repo with a green base commit and returns its path and base sha.
func toyRepo(t *testing.T) (repo, base string) {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, d, "init", "-q", "-b", "main")
	writeFile(t, d, ".aigate.yml", toyYAML)
	writeFile(t, d, "ok.flag", "")
	writeFile(t, d, "tests/spec.txt", "line1\n")
	writeFile(t, d, "src/a.txt", "a\n")
	return d, commitAll(t, d, "base")
}

// agentCommit makes a commit on a side branch (like the agent's) and leaves main checked out.
func agentCommit(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	git(t, repo, "checkout", "-q", "-b", "agent")
	for n, c := range files {
		writeFile(t, repo, n, c)
	}
	sha := commitAll(t, repo, "agent work")
	git(t, repo, "checkout", "-q", "main")
	return sha
}

type fakeAgent struct {
	mu                         sync.Mutex
	prepareErr, runErr         error
	collectErr                 error
	run                        vm.RunResult
	head                       string
	prepares, runs, collects   int
	lastLimit                  time.Duration
	lastAgent, lastBase, lastT string
	collectBase                string
}

func (f *fakeAgent) Prepare(_ context.Context, task, repo, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	f.lastBase, f.lastT = base, task
	return f.prepareErr
}
func (f *fakeAgent) Run(_ context.Context, task, agent, prompt string, limit time.Duration) (vm.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	f.lastLimit, f.lastAgent = limit, agent
	return f.run, f.runErr
}
func (f *fakeAgent) Collect(_ context.Context, task, repo, base string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collects++
	f.collectBase = base
	if f.collectErr != nil {
		return "", 0, f.collectErr
	}
	return f.head, 1, nil
}

type fakeTest struct {
	restoreErr, execErr error
	exec                func() gate.Executor
	restores            int
}

func (f *fakeTest) Restore(context.Context) error { f.restores++; return f.restoreErr }
func (f *fakeTest) Executor() (gate.Executor, error) {
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.exec != nil {
		return f.exec(), nil
	}
	return &gate.LocalExecutor{}, nil
}

type fakePub struct {
	err   error
	calls int
}

func (f *fakePub) Publish(context.Context, store.Task) error { f.calls++; return f.err }

type env struct {
	t     *testing.T
	st    *store.Store
	m     *machine.Machine
	p     *Provider
	agent *fakeAgent
	test  *fakeTest
	repo  string
	base  string
	id    int64
}

// newEnv builds a task over repo. The default gate is a fake returning gateRep.
func newEnv(t *testing.T, mod func(*Config, *env)) *env {
	t.Helper()
	repo, base := toyRepo(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, repo: repo, base: base,
		agent: &fakeAgent{head: fakeHead}, test: &fakeTest{}}
	cfg := DefaultConfig()
	cfg.Agent, cfg.Test = e.agent, e.test
	cfg.ReportDir = filepath.Join(t.TempDir(), "reports")
	cfg.GateHome = t.TempDir()
	cfg.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
		return report.Report{SchemaVersion: 1, Category: rules.CategoryOK, Repo: o.Repo, Base: o.Base, Head: o.Head}, nil
	}
	if mod != nil {
		mod(&cfg, e)
	}
	e.p, err = NewProvider(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.m = machine.New(st, e.p.Steps())
	e.id, err = st.CreateTask(context.Background(), store.Task{Repo: repo, Base: base, Prompt: "do it", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// drain ticks until nothing is runnable.
func (e *env) drain() store.Task {
	e.t.Helper()
	for i := 0; i < 100; i++ {
		err := e.m.Tick(context.Background())
		if errors.Is(err, machine.ErrNoWork) {
			return e.task()
		}
		if err != nil {
			e.t.Fatal(err)
		}
	}
	e.t.Fatal("machine did not settle")
	return store.Task{}
}

func (e *env) task() store.Task {
	e.t.Helper()
	tk, err := e.st.GetTask(context.Background(), e.id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

func (e *env) events() []store.Event {
	ev, err := e.st.Events(context.Background(), e.id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *env) noteFor(to store.State) string {
	for _, ev := range e.events() {
		if ev.To == to {
			return ev.Note
		}
	}
	return ""
}

func (e *env) neverPassed() {
	e.t.Helper()
	for _, ev := range e.events() {
		if ev.To == machine.StatePassed || ev.To == machine.StatePublishing || ev.To == machine.StateDone {
			e.t.Fatalf("task reached %s: %+v", ev.To, e.events())
		}
	}
}

func TestHappyPath(t *testing.T) {
	pub := &fakePub{}
	e := newEnv(t, func(c *Config, _ *env) { c.Publisher = pub })
	tk := e.drain()
	if tk.State != machine.StateDone {
		t.Fatalf("state %s, error %q, events %+v", tk.State, tk.ErrorText, e.events())
	}
	var path []store.State
	for _, ev := range e.events() {
		path = append(path, ev.To)
	}
	want := []store.State{machine.StateQueued, machine.StatePreparing, machine.StateAgentRunning,
		machine.StateCollecting, machine.StateVerifying, machine.StatePassed, machine.StatePublishing, machine.StateDone}
	if fmt.Sprint(path) != fmt.Sprint(want) {
		t.Fatalf("path %v", path)
	}
	if tk.HeadSHA != fakeHead || tk.Verdict != "ok" {
		t.Fatalf("head %q verdict %q", tk.HeadSHA, tk.Verdict)
	}
	if e.agent.lastBase != e.base || e.agent.lastT != "1" || e.agent.lastAgent != "claude" || e.agent.lastLimit != DefaultAgentTimeout {
		t.Fatalf("agent args: %+v", e.agent)
	}
	fi, err := os.Stat(tk.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || filepath.Base(tk.ReportPath) != "1.json" {
		t.Fatalf("report %s mode %v", tk.ReportPath, fi.Mode())
	}
	if e.test.restores != 1 || pub.calls != 1 {
		t.Fatalf("restores %d publishes %d", e.test.restores, pub.calls)
	}
}

func TestLocalOnlyPublisherNote(t *testing.T) {
	e := newEnv(t, nil)
	if tk := e.drain(); tk.State != machine.StateDone {
		t.Fatalf("state %s", tk.State)
	}
	if got := e.noteFor(machine.StateDone); got != "local-only: nothing pushed" {
		t.Fatalf("note %q", got)
	}
}

func TestRestoreEachJobFalseSkipsRestore(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *env) { c.RestoreEachJob = false })
	if tk := e.drain(); tk.State != machine.StateDone {
		t.Fatalf("state %s", tk.State)
	}
	if e.test.restores != 0 {
		t.Fatalf("restores %d", e.test.restores)
	}
}

func TestAgentTimeoutFails(t *testing.T) {
	e := newEnv(t, func(c *Config, e *env) { e.agent.run = vm.RunResult{TimedOut: true, RC: 124} })
	tk := e.drain()
	if tk.State != machine.StateFailed || e.noteFor(machine.StateFailed) != "agent timeout" {
		t.Fatalf("state %s note %q", tk.State, e.noteFor(machine.StateFailed))
	}
	if e.agent.collects != 0 || tk.ErrorText != "" {
		t.Fatalf("collects %d error %q (a timeout is a verdict, not an error)", e.agent.collects, tk.ErrorText)
	}
	e.neverPassed()
}

func TestAgentNonZeroExitStillCollects(t *testing.T) {
	e := newEnv(t, func(c *Config, e *env) { e.agent.run = vm.RunResult{RC: 3} })
	if tk := e.drain(); tk.State != machine.StateDone || e.agent.collects != 1 {
		t.Fatalf("state %s collects %d", tk.State, e.agent.collects)
	}
}

func TestNoCommitAndOverLimitFail(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		note string
	}{
		"no commit":  {vm.ErrNoCommit, "agent made no commit"},
		"over limit": {fmt.Errorf("%w: 101 commits", vm.ErrOverLimit), "over limits"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(c *Config, e *env) { e.agent.collectErr = tc.err })
			tk := e.drain()
			if tk.State != machine.StateFailed || !strings.Contains(e.noteFor(machine.StateFailed), tc.note) {
				t.Fatalf("state %s note %q", tk.State, e.noteFor(machine.StateFailed))
			}
			if e.agent.collects != 1 || tk.ErrorText != "" || tk.HeadSHA != "" {
				t.Fatalf("collects %d err %q head %q: a deterministic result must not be retried", e.agent.collects, tk.ErrorText, tk.HeadSHA)
			}
			e.neverPassed()
		})
	}
}

// An infrastructure error at any step is retried up to the attempt limit,
// then the task fails; it never passes and never gets a verdict.
func TestInfraErrorAtEachStepNeverPasses(t *testing.T) {
	boom := errors.New("ssh: connection refused")
	cases := map[string]struct {
		mod   func(*Config, *env)
		calls func(*env) int
		state store.State
	}{
		"prepare":  {func(c *Config, e *env) { e.agent.prepareErr = boom }, func(e *env) int { return e.agent.prepares }, machine.StatePreparing},
		"run":      {func(c *Config, e *env) { e.agent.runErr = boom }, func(e *env) int { return e.agent.runs }, machine.StateAgentRunning},
		"collect":  {func(c *Config, e *env) { e.agent.collectErr = boom }, func(e *env) int { return e.agent.collects }, machine.StateCollecting},
		"restore":  {func(c *Config, e *env) { e.test.restoreErr = boom }, func(e *env) int { return e.test.restores }, machine.StateVerifying},
		"executor": {func(c *Config, e *env) { e.test.execErr = boom }, func(e *env) int { return e.test.restores }, machine.StateVerifying},
		"gate": {func(c *Config, e *env) {
			c.Gate = func(context.Context, gate.Options) (report.Report, error) { return report.Report{}, boom }
		}, func(e *env) int { return e.test.restores }, machine.StateVerifying},
		"publish": {func(c *Config, e *env) { c.Publisher = &fakePub{err: boom} }, func(e *env) int { return 3 }, machine.StatePublishing},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.mod)
			tk := e.drain()
			if tk.State != machine.StateFailed {
				t.Fatalf("state %s (want failed): %+v", tk.State, e.events())
			}
			if !strings.Contains(tk.ErrorText, "connection refused") {
				t.Fatalf("error text %q", tk.ErrorText)
			}
			if n := tc.calls(e); n != machine.DefaultMaxAttempts {
				t.Fatalf("step ran %d times, want %d", n, machine.DefaultMaxAttempts)
			}
			if tk.Verdict != "" && name != "publish" {
				t.Fatalf("an infrastructure error produced verdict %q", tk.Verdict)
			}
			retries := 0
			for _, ev := range e.events() {
				if ev.To == machine.StateError && ev.From == tc.state {
					retries++
				}
				if ev.To == machine.StatePassed && name != "publish" {
					t.Fatalf("reached passed: %+v", e.events())
				}
				if ev.To == machine.StateDone {
					t.Fatalf("reached done: %+v", e.events())
				}
			}
			if retries != machine.DefaultMaxAttempts-1 {
				t.Fatalf("%d retries via error, want %d", retries, machine.DefaultMaxAttempts-1)
			}
		})
	}
}

func TestRestoreFailureNeverRunsGate(t *testing.T) {
	gateCalls := 0
	e := newEnv(t, func(c *Config, e *env) {
		e.test.restoreErr = errors.New("openstack down")
		c.Gate = func(context.Context, gate.Options) (report.Report, error) { gateCalls++; return report.Report{}, nil }
	})
	if tk := e.drain(); tk.State != machine.StateFailed || gateCalls != 0 {
		t.Fatalf("state %s gate calls %d", tk.State, gateCalls)
	}
	e.neverPassed()
}

func weakened(o gate.Options, rule string) report.Report {
	return report.Report{SchemaVersion: 1, Category: rules.CategoryWeakened, ExitCode: 3, Repo: o.Repo, Base: o.Base, Head: o.Head,
		Violations: []rules.Violation{{Rule: rule, Category: rules.CategoryWeakened, Detail: "protected file changed", IDs: []string{"tests/spec.txt"}}},
		Approvals:  []report.ApprovalState{{Hash: "abc123hash", Valid: false}}}
}

func TestProtectedModifiedNeedsApprovalOnce(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *env) {
		c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
			return weakened(o, "protected_modified"), nil
		}
	})
	tk := e.drain()
	if tk.State != machine.StateNeedsApproval || tk.Verdict != "check_weakened" {
		t.Fatalf("state %s verdict %q", tk.State, tk.Verdict)
	}
	pend, _ := e.st.PendingApprovals(context.Background())
	if len(pend) != 1 || pend[0].Kind != store.KindProtectedModified || pend[0].Hash != "abc123hash" ||
		pend[0].Base != e.base || pend[0].Head != fakeHead || pend[0].Reason == "" || pend[0].TaskID != e.id {
		t.Fatalf("approvals %+v", pend)
	}
	// The step re-runs (a crash, or a re-verification): still exactly one entry.
	var verifying machine.Step
	for _, s := range e.p.Steps() {
		if s.Name() == machine.StateVerifying {
			verifying = s
		}
	}
	for i := 0; i < 2; i++ {
		if out, err := verifying.Run(context.Background(), tk); err != nil || out.Next != machine.StateNeedsApproval {
			t.Fatalf("rerun: %+v %v", out, err)
		}
	}
	// And through the machine, after the human's approval re-enters verifying.
	if err := e.m.Approve(context.Background(), pend[0].ID); err != nil {
		t.Fatal(err)
	}
	if tk = e.task(); tk.State != machine.StateVerifying {
		t.Fatalf("after approve: %s", tk.State)
	}
	// The gate still sees the same protected hash although it was approved:
	// parking again would be a dead end, so the task fails with a clear error
	// instead of waiting for an approval nobody can give.
	if tk = e.drain(); tk.State != machine.StateFailed || !strings.Contains(tk.ErrorText, "already approved") {
		t.Fatalf("re-judged: %s %q", tk.State, tk.ErrorText)
	}
	all, _ := e.st.PendingApprovals(context.Background())
	if len(all) != 0 {
		t.Fatalf("approved entry was re-opened: %+v", all)
	}
	e.neverPassed()
}

func TestOtherWeakeningsAndBehaviorFail(t *testing.T) {
	mk := func(cat rules.Category, vs ...rules.Violation) func(*Config, *env) {
		return func(c *Config, _ *env) {
			c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
				return report.Report{SchemaVersion: 1, Category: cat, Repo: o.Repo, Base: o.Base, Head: o.Head, Violations: vs,
					Approvals: []report.ApprovalState{{Hash: "h", Valid: false}}}, nil
			}
		}
	}
	cases := map[string]struct {
		mod  func(*Config, *env)
		note string
	}{
		"behavior": {mk(rules.CategoryBehavior, rules.Violation{Rule: "regression", Category: rules.CategoryBehavior}), "behavior_failure: regression"},
		"incomplete": {mk(rules.CategoryIncomplete, rules.Violation{Rule: "base_not_green", Category: rules.CategoryIncomplete}),
			"verification_incomplete: base_not_green"},
		"weakened other": {mk(rules.CategoryWeakened, rules.Violation{Rule: "test_skipped", Category: rules.CategoryWeakened}), "check_weakened: test_skipped"},
		"weakened mixed": {mk(rules.CategoryWeakened, rules.Violation{Rule: "protected_modified", Category: rules.CategoryWeakened},
			rules.Violation{Rule: "test_missing", Category: rules.CategoryWeakened}), "check_weakened: protected_modified, test_missing"},
		"unknown category": {mk("weird"), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.mod)
			tk := e.drain()
			if tk.State != machine.StateFailed {
				t.Fatalf("state %s", tk.State)
			}
			if tc.note != "" && e.noteFor(machine.StateFailed) != tc.note {
				t.Fatalf("note %q want %q", e.noteFor(machine.StateFailed), tc.note)
			}
			if pend, _ := e.st.PendingApprovals(context.Background()); len(pend) != 0 {
				t.Fatalf("approvals created: %+v", pend)
			}
			e.neverPassed()
		})
	}
}

// A report that is not about this task's commits is not a verdict.
func TestForeignReportIsError(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *env) {
		c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
			return report.Report{SchemaVersion: 1, Category: rules.CategoryOK, Repo: o.Repo, Base: o.Base, Head: "ffffffffffffffffffffffffffffffffffffffff"}, nil
		}
	})
	if tk := e.drain(); tk.State != machine.StateFailed || tk.Verdict != "" {
		t.Fatalf("state %s verdict %q", tk.State, tk.Verdict)
	}
	e.neverPassed()
}

// Crash in the middle of collecting: the claim is released by Recover and
// collect runs again (fetching twice is fine).
func TestCrashMidCollectRerunsCollect(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	for e.task().State != machine.StateCollecting {
		if err := e.m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// "Crash": the step ran (fetch happened) but the process died before the move.
	claimed, ok, err := e.st.NextRunnable(ctx)
	if err != nil || !ok || claimed.State != machine.StateCollecting {
		t.Fatalf("claim: %+v %v %v", claimed, ok, err)
	}
	if _, _, err := e.agent.Collect(ctx, "1", e.repo, e.base); err != nil {
		t.Fatal(err)
	}
	e2 := machine.New(e.st, e.p.Steps()) // a restarted process
	if err := e2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	e.m = e2
	tk := e.drain()
	if tk.State != machine.StateDone || e.agent.collects != 2 {
		t.Fatalf("state %s collects %d", tk.State, e.agent.collects)
	}
}

func TestNewProviderValidates(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "o.db"))
	defer st.Close()
	good := DefaultConfig()
	good.Agent, good.Test, good.ReportDir = &fakeAgent{}, &fakeTest{}, t.TempDir()
	if _, err := NewProvider(st, good); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*Config){
		"no agent": func(c *Config) { c.Agent = nil },
		"no test":  func(c *Config) { c.Test = nil },
		"no dir":   func(c *Config) { c.ReportDir = "" },
	} {
		c := good
		f(&c)
		if _, err := NewProvider(st, c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// Real temp repo, real gate, local executor: an ordinary change passes.
func TestRealGateOKPasses(t *testing.T) {
	e := newEnv(t, func(c *Config, e *env) { c.Gate = gate.Run })
	e.agent.head = agentCommit(t, e.repo, map[string]string{"src/new.txt": "n\n"})
	tk := e.drain()
	if tk.State != machine.StateDone || tk.Verdict != "ok" {
		t.Fatalf("state %s verdict %q error %q note %q", tk.State, tk.Verdict, tk.ErrorText, e.noteFor(machine.StateFailed))
	}
	if tk.HeadSHA != e.agent.head {
		t.Fatalf("head %s", tk.HeadSHA)
	}
}

// Real gate: editing an existing protected test file needs a human approval.
func TestRealGateModifiedTestNeedsApproval(t *testing.T) {
	e := newEnv(t, func(c *Config, e *env) { c.Gate = gate.Run })
	e.agent.head = agentCommit(t, e.repo, map[string]string{"tests/spec.txt": "line1\nline2\n"})
	tk := e.drain()
	if tk.State != machine.StateNeedsApproval || tk.Verdict != "check_weakened" {
		t.Fatalf("state %s verdict %q error %q note %q", tk.State, tk.Verdict, tk.ErrorText, e.noteFor(machine.StateFailed))
	}
	pend, _ := e.st.PendingApprovals(context.Background())
	if len(pend) != 1 || len(pend[0].Hash) < 16 || pend[0].Head != e.agent.head || pend[0].Base != e.base {
		t.Fatalf("approvals %+v", pend)
	}
	// Re-running verification does not duplicate the entry.
	if err := e.st.TransitionWith(context.Background(), e.id, machine.StateNeedsApproval, machine.StateVerifying, "again", store.TransitionOpts{}); err != nil {
		t.Fatal(err)
	}
	if tk = e.drain(); tk.State != machine.StateNeedsApproval {
		t.Fatalf("state %s", tk.State)
	}
	if pend, _ = e.st.PendingApprovals(context.Background()); len(pend) != 1 {
		t.Fatalf("approvals %+v", pend)
	}
}

// Real gate: a regression at head is a behavior failure, a failed task.
func TestRealGateBehaviorFailure(t *testing.T) {
	e := newEnv(t, func(c *Config, e *env) { c.Gate = gate.Run })
	git(t, e.repo, "checkout", "-q", "-b", "agent")
	if err := os.Remove(filepath.Join(e.repo, "ok.flag")); err != nil {
		t.Fatal(err)
	}
	e.agent.head = commitAll(t, e.repo, "break")
	git(t, e.repo, "checkout", "-q", "main")
	tk := e.drain()
	if tk.State != machine.StateFailed || tk.Verdict == "ok" || tk.Verdict == "" {
		t.Fatalf("state %s verdict %q", tk.State, tk.Verdict)
	}
	e.neverPassed()
}

// A publisher that hits the hard floor fails the task and leaves only a
// destructive_blocked record, which nothing can approve.
func TestPublishFloorFailsTaskWithBlockedRecord(t *testing.T) {
	pub := &fakePub{err: fmt.Errorf("publish: %w: merge", policy.ErrFloor)}
	e := newEnv(t, func(c *Config, _ *env) { c.Publisher = pub })
	tk := e.drain()
	if tk.State != machine.StateFailed {
		t.Fatalf("state %s", tk.State)
	}
	all, _ := e.st.PendingApprovals(context.Background())
	if len(all) != 1 || all[0].Kind != store.KindDestructiveBlocked {
		t.Fatalf("approvals %+v", all)
	}
	if err := e.st.ApproveApproval(context.Background(), all[0].ID); !errors.Is(err, store.ErrNotApprovable) {
		t.Fatalf("approve: %v", err)
	}
}

// A publisher that wants a human parks the task with a publish_outside_allowlist entry.
func TestPublishNeedsApprovalParksTask(t *testing.T) {
	pub := &fakePub{err: &publish.NeedsApprovalError{Action: publish.Action{Kind: "draft_pr", Task: 1}, Hash: "ph", Reason: "draft_pr is not in the allowlist"}}
	e := newEnv(t, func(c *Config, _ *env) { c.Publisher = pub })
	tk := e.drain()
	if tk.State != machine.StateNeedsApproval {
		t.Fatalf("state %s", tk.State)
	}
	all, _ := e.st.PendingApprovals(context.Background())
	if len(all) != 1 || all[0].Kind != store.KindPublishOutsideAllowlist || all[0].Hash != "ph" || all[0].Head != fakeHead {
		t.Fatalf("approvals %+v", all)
	}
}
