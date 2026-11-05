package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/orch/approvals"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/store"
)

// passTask drives the env's task to passed.
func passTask(t *testing.T, e *env) store.Task {
	t.Helper()
	for i := 0; i < 20 && e.task().State != machine.StatePassed; i++ {
		if err := e.m.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	a := e.task()
	if a.State != machine.StatePassed {
		t.Fatalf("state %s", a.State)
	}
	return a
}

// Task A recorded L0, the file becomes L3 and task B starts (and reloads it):
// A must NOT publish unattended under a policy it did not start with.
func TestPublishPolicyChangedParksForHuman(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, live, path := polEnv(t, "L0", pub, nil)
	ctx := context.Background()
	a := passTask(t, e)
	hA := a.PolicyHash
	writePolicy(t, filepath.Dir(path), policyBody("L3"))
	idB, _ := e.st.CreateTask(ctx, store.Task{Repo: e.repo, Base: e.base, Prompt: "b", Class: "docs", Agent: "claude"})
	b, _ := e.st.GetTask(ctx, idB)
	if _, err := e.p.prepare(ctx, b); err != nil {
		t.Fatal(err)
	}
	newHash := live.Current().Hash()
	out, err := e.p.publish(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if out.Next != machine.StateNeedsApproval || pub.calls != 0 {
		t.Fatalf("outcome %s %q, publisher calls %d", out.Next, out.Note, pub.calls)
	}
	want := "policy changed since the task started: " + hA[:12] + " -> " + newHash[:12]
	pend, _ := e.st.PendingApprovals(ctx)
	if len(pend) != 1 || pend[0].Kind != store.KindPublishOutsideAllowlist || pend[0].TaskID != a.ID ||
		!strings.Contains(pend[0].Reason, want) ||
		pend[0].Hash != approvals.PublishHash(a.ID, a.HeadSHA, PolicyChangeKind(newHash)) {
		t.Fatalf("pending %+v want reason %q", pend, want)
	}
	// A human approves exactly this policy change: it publishes under the
	// then-current policy and the transition note names the hash used.
	pub.granted = map[string]bool{PolicyChangeKind(newHash): true, "push_branch": true}
	out, err = e.p.publish(ctx, a)
	if err != nil || out.Next != machine.StateDone || pub.calls != 1 {
		t.Fatalf("after approval: %v %s %q calls %d", err, out.Next, out.Note, pub.calls)
	}
	if !strings.Contains(out.Note, newHash[:12]) {
		t.Fatalf("note %q does not name the policy used %s", out.Note, newHash[:12])
	}
	// The approval of one policy hash does not cover the next change.
	writePolicy(t, filepath.Dir(path), policyBody("L2"))
	pub.calls = 0
	out, err = e.p.publish(ctx, a)
	if err != nil || out.Next != machine.StateNeedsApproval || pub.calls != 0 {
		t.Fatalf("second change: %v %s calls %d", err, out.Next, pub.calls)
	}
}

// A policy file that stops loading stops in-flight tasks at publish (an
// infrastructure error), with no fallback to the last good policy.
func TestPublishUnloadablePolicyIsInfraError(t *testing.T) {
	pub := &planPub{kinds: []string{"push_branch"}}
	e, live, path := polEnv(t, "L3", pub, nil)
	a := passTask(t, e)
	before := live.Current().Hash()
	if err := os.WriteFile(path, []byte("version: 1\nclasses: [not a map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := e.p.publish(context.Background(), a)
	if err == nil || pub.calls != 0 || out.Next != "" {
		t.Fatalf("err=%v out=%+v calls=%d", err, out, pub.calls)
	}
	if live.Current().Hash() != before {
		t.Fatal("current policy changed by a failed reload")
	}
	// through the machine it is an error that retries and ends failed, never done
	e.drain()
	if st := e.task().State; st != machine.StateFailed || pub.calls != 0 {
		t.Fatalf("state %s calls %d", st, pub.calls)
	}
}

// push_branch is allowed by Decide and the allowlist, notify is not allowlisted:
// the task parks BEFORE push_branch runs (no partial publication), and the L2
// notification fires on this path.
func TestPublishParksBeforeAnyActionWhenAllowlistRefuses(t *testing.T) {
	n := &fakeNotifier{}
	pub := &planPub{kinds: []string{"push_branch", "notify"}, deny: map[string]bool{"notify": true}}
	e, _, _ := polEnv(t, "L2", pub, func(c *Config) { c.Notifier = n })
	tk := e.drain()
	if tk.State != machine.StateNeedsApproval || pub.calls != 0 {
		t.Fatalf("state %s calls %d", tk.State, pub.calls)
	}
	pend, _ := e.st.PendingApprovals(context.Background())
	if len(pend) != 1 || pend[0].Hash != approvals.PublishHash(tk.ID, tk.HeadSHA, "notify") ||
		!strings.Contains(pend[0].Reason, "notify") {
		t.Fatalf("%+v", pend)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("notify-on-park did not fire: %v", n.msgs)
	}
	// once a human granted notify, everything runs
	pub.granted = map[string]bool{"notify": true}
	if out, err := e.p.publish(context.Background(), tk); err != nil || out.Next != machine.StateDone || pub.calls != 1 {
		t.Fatalf("%v %+v calls %d", err, out, pub.calls)
	}
}

// The reviewer's probe with the real publisher: policy docs L1, allowlist
// [push_branch], the publisher plans push_branch then notify.
func TestNoPartialPublicationRealPublisher(t *testing.T) {
	var e *env
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	dir := t.TempDir()
	path := writePolicy(t, dir, "version: 1\nclasses:\n  docs: {level: L1}\nallowlist: [push_branch]\n")
	live, err := policy.NewLive(path, policy.OrchLedgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, func(c *Config, en *env) {
		en.agent.head = agentCommit(t, en.repo, map[string]string{"x.txt": "x\n"})
		p, err := publish.New(publish.Config{Remote: remote, Notifier: &fakeNotifier{},
			AllowSource: func() policy.Allowlist { return live.Current().AllowList() },
			Grants:      approvals.Grants{Home: c.GateHome}})
		if err != nil {
			t.Fatal(err)
		}
		c.Publisher = p
		c.Policy = live
	})
	tk := e.drain()
	ref, _ := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/orch/1").Output()
	if tk.State != machine.StateNeedsApproval || len(ref) != 0 {
		t.Fatalf("state %s, remote ref %q", tk.State, ref)
	}
}
