package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/policy/autonomy"
	"aigate/internal/orch/store"
	"aigate/internal/report"
	"aigate/internal/rules"
)

// planPub is a Planner that records what ran.
type planPub struct {
	mu      sync.Mutex
	kinds   []string
	granted map[string]bool
	deny    map[string]bool // kinds the "publisher" would not run unattended
	err     error
	calls   int
}

func (p *planPub) Publish(context.Context, store.Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.err
}
func (p *planPub) Plan() []string                      { return p.kinds }
func (p *planPub) Granted(_ store.Task, k string) bool { return p.granted[k] }
func (p *planPub) Permit(k string) error {
	if p.deny[k] {
		return errors.New("not in the allowlist")
	}
	return nil
}

type fakeNotifier struct{ msgs []string }

func (n *fakeNotifier) Notify(_ context.Context, _ int64, m string) error {
	n.msgs = append(n.msgs, m)
	return nil
}

func policyBody(level string) string {
	return "version: 1\nclasses:\n  docs: {level: " + level + "}\nallowlist: [push_branch, draft_pr, notify]\n"
}

func writePolicy(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "policy.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// polEnv builds an env whose class "docs" runs under a policy of the given level.
func polEnv(t *testing.T, level string, pub *planPub, mod func(*Config)) (*env, *policy.Live, string) {
	t.Helper()
	dir := t.TempDir()
	path := writePolicy(t, dir, policyBody(level))
	live, err := policy.NewLive(path, policy.OrchLedgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config, _ *env) {
		c.Publisher = pub
		c.Policy = live
		if mod != nil {
			mod(c)
		}
	})
	return e, live, path
}

func TestPolicyRecordsHashAndL1Publishes(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch", "draft_pr"}}
	e, live, _ := polEnv(t, "L1", pub, nil)
	tk := e.drain()
	if tk.State != machine.StateDone || pub.calls != 1 {
		t.Fatalf("state %s calls %d err %q", tk.State, pub.calls, tk.ErrorText)
	}
	if tk.PolicyHash == "" || tk.PolicyHash != live.Current().Hash() {
		t.Fatalf("hash %q vs %q", tk.PolicyHash, live.Current().Hash())
	}
}

func TestPolicyL0ParksEveryPublish(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, _, _ := polEnv(t, "L0", pub, nil)
	tk := e.drain()
	if tk.State != machine.StateNeedsApproval || pub.calls != 0 {
		t.Fatalf("state %s calls %d", tk.State, pub.calls)
	}
	pend, _ := e.st.PendingApprovals(context.Background())
	if len(pend) != 1 || pend[0].Kind != store.KindPublishOutsideAllowlist {
		t.Fatalf("%+v", pend)
	}
	if !strings.Contains(e.noteFor(machine.StateNeedsApproval), "push_branch") {
		t.Fatalf("note %q", e.noteFor(machine.StateNeedsApproval))
	}
}

func TestPolicyL0RunsOnceHumanGranted(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}, granted: map[string]bool{"push_branch": true}}
	e, _, _ := polEnv(t, "L0", pub, nil)
	if tk := e.drain(); tk.State != machine.StateDone || pub.calls != 1 {
		t.Fatalf("state %s calls %d", tk.State, pub.calls)
	}
}

func TestPolicyPickedUpAtNextTaskStart(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, live, path := polEnv(t, "L1", pub, nil)
	first := e.drain()
	h1 := first.PolicyHash
	writePolicy(t, filepath.Dir(path), policyBody("L2"))
	if live.Current().Hash() != h1 {
		t.Fatal("policy re-read before a task started")
	}
	id2, err := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.base, Prompt: "again", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	e.id = id2
	second := e.drain()
	if second.PolicyHash == "" || second.PolicyHash == h1 || second.PolicyHash != live.Current().Hash() {
		t.Fatalf("hashes %q %q", h1, second.PolicyHash)
	}
	if first, _ = e.st.GetTask(context.Background(), 1); first.PolicyHash != h1 {
		t.Fatal("the first task's recorded hash changed")
	}
	evs, _ := ledger.Events(policy.OrchLedgerPath(filepath.Dir(path)))
	if len(evs) != 2 || evs[1].Payload["new_hash"] != second.PolicyHash {
		t.Fatalf("ledger %+v", evs)
	}
}

func TestPolicyThatNoLongerLoadsStopsTheTask(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, _, path := polEnv(t, "L3", pub, nil)
	writePolicy(t, filepath.Dir(path), "version: 1\nbogus: true\n")
	tk := e.drain()
	if tk.State == machine.StateDone || pub.calls != 0 {
		t.Fatalf("state %s calls %d", tk.State, pub.calls)
	}
	e.neverPassed()
}

func TestNonOKNeverPublishesAtL3(t *testing.T) {
	for _, cat := range []rules.Category{rules.CategoryBehavior, rules.CategoryIncomplete, rules.CategoryWeakened} {
		pub := &planPub{kinds: []string{"push_branch", "draft_pr", "notify"}}
		e, _, _ := polEnv(t, "L3", pub, func(c *Config) {
			c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
				r := report.Report{SchemaVersion: 1, Category: cat, Repo: o.Repo, Base: o.Base, Head: o.Head,
					Violations: []rules.Violation{{Rule: "regression", Category: cat}}}
				return r, nil
			}
		})
		e.drain()
		e.neverPassed()
		if pub.calls != 0 {
			t.Fatalf("%s: published", cat)
		}
		// Even if a task with a non-ok verdict reached the publishing step, it must not publish.
		tk := e.task()
		tk.Verdict = string(cat)
		tk.HeadSHA = fakeHead
		out, err := e.p.publish(context.Background(), tk)
		if err != nil || out.Next != machine.StateFailed || pub.calls != 0 {
			t.Fatalf("%s: %+v %v calls %d", cat, out, err, pub.calls)
		}
	}
}

func TestFloorDenialDemotesOneLevelAndLogs(t *testing.T) {
	pub := &planPub{kinds: []string{"delete_branch"}}
	e, live, _ := polEnv(t, "L3", pub, nil)
	tk := e.drain()
	if tk.State != machine.StateFailed || pub.calls != 0 {
		t.Fatalf("state %s calls %d", tk.State, pub.calls)
	}
	if l, _ := autonomy.Effective(live.Current(), "docs"); l != autonomy.L2 {
		t.Fatalf("effective %s", l)
	}
	fi, err := os.Stat(live.Current().StatePath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v %v", fi, err)
	}
	id, _, _ := gate.RepoID(e.repo)
	home := e.p.cfg.GateHome
	evs, err := ledger.Events(gate.LedgerPath(home, id))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		found = found || ev.Type == "autonomy_demote" && ev.Payload["class"] == "docs" && ev.Payload["from"] == "L3" && ev.Payload["to"] == "L2"
	}
	if !found {
		t.Fatalf("no autonomy_demote event: %+v", evs)
	}
}

func TestPublisherFloorErrorAlsoDemotes(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}, err: policy.Floor("merge")}
	e, live, _ := polEnv(t, "L2", pub, nil)
	if tk := e.drain(); tk.State != machine.StateFailed {
		t.Fatalf("state %s", tk.State)
	}
	if l, _ := autonomy.Effective(live.Current(), "docs"); l != autonomy.L1 {
		t.Fatalf("effective %s", l)
	}
}

// This calls verify directly on a task that already reached done. The state
// machine never does that (done is terminal), so it checks the demotion logic of
// verify only, not a path reachable in production. See "Known limits" in
// docs/09-RESULTS-orchestrator.md.
func TestVerifyDirectlyOnDoneTaskDemotes(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, live, _ := polEnv(t, "L2", pub, nil)
	tk := e.drain()
	if tk.State != machine.StateDone {
		t.Fatalf("state %s", tk.State)
	}
	// The same task is verified again and now fails: an incident after auto-publishing.
	e.p.cfg.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
		return report.Report{SchemaVersion: 1, Category: rules.CategoryBehavior, Repo: o.Repo, Base: o.Base, Head: o.Head}, nil
	}
	if _, err := e.p.verify(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	if l, _ := autonomy.Effective(live.Current(), "docs"); l != autonomy.L1 {
		t.Fatalf("effective %s", l)
	}
}

func TestNoDemotionWithoutIncident(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, live, _ := polEnv(t, "L3", pub, nil)
	e.drain()
	if _, err := os.Stat(live.Current().StatePath()); !os.IsNotExist(err) {
		t.Fatalf("state file exists after a clean run: %v", err)
	}
}

func TestNotifications(t *testing.T) {
	// L3: post-hoc report after an automatic publish.
	n := &fakeNotifier{}
	pub := &planPub{kinds: []string{"push_branch"}}
	e, _, _ := polEnv(t, "L3", pub, func(c *Config) { c.Notifier = n })
	if tk := e.drain(); tk.State != machine.StateDone || len(n.msgs) != 1 {
		t.Fatalf("L3: %s %v", tk.State, n.msgs)
	}
	// L1: nothing extra.
	n1 := &fakeNotifier{}
	e1, _, _ := polEnv(t, "L1", &planPub{kinds: []string{"push_branch"}}, func(c *Config) { c.Notifier = n1 })
	e1.drain()
	if len(n1.msgs) != 0 {
		t.Fatalf("L1 notified: %v", n1.msgs)
	}
	// L2: a task that needs approval (protected file) is only notified about.
	n2 := &fakeNotifier{}
	e2, _, _ := polEnv(t, "L2", &planPub{kinds: []string{"push_branch"}}, func(c *Config) {
		c.Notifier = n2
		c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
			return weakened(o, "protected_modified"), nil
		}
	})
	if tk := e2.drain(); tk.State != machine.StateNeedsApproval || len(n2.msgs) != 1 {
		t.Fatalf("L2: %s %v", tk.State, n2.msgs)
	}
	e2.neverPassed()
}

func TestPolicyNeedsPlanner(t *testing.T) {
	dir := t.TempDir()
	live, err := policy.NewLive(writePolicy(t, dir, policyBody("L1")), "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(filepath.Join(dir, "o.db"))
	defer st.Close()
	_, err = NewProvider(st, Config{Agent: &fakeAgent{}, Test: &fakeTest{}, ReportDir: dir, Policy: live, Publisher: &fakePub{}})
	if err == nil {
		t.Fatal("a Publisher without Plan was accepted with a policy")
	}
}
