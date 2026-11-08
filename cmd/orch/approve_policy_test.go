package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aigate/internal/orch/approvals"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/store"
)

const policyV1 = "version: 1\nclasses:\n  docs: {level: L3}\nallowlist: [push_branch]\n"
const policyV3 = "version: 1\nclasses:\n  docs: {level: L3}\nallowlist: [push_branch, draft_pr]\n"
const policyV2 = "version: 1\nclasses:\n  docs: {level: L3}\nallowlist: [push_branch, notify]\n"

// policyChangeEnv starts a task under policyV1 and rewrites the policy while
// the agent works, so publishing parks on "policy changed".
func policyChangeEnv(t *testing.T) (*e2e, string) {
	t.Helper()
	bare, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, bare, "init", "-q", "--bare")
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yml")
	if err := os.WriteFile(path, []byte(policyV1), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := policy.NewLive(path, policy.OrchLedgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	lazy := &lazyPublisher{}
	var once sync.Once // only the first task sees the policy change
	e := newE2EPolicy(t, map[string]string{"src/new.txt": "n\n"}, lazy, live, func() {
		once.Do(func() { _ = os.WriteFile(path, []byte(policyV2), 0o600) })
	})
	pub, err := publish.New(publish.Config{Remote: bare, Grants: approvals.Grants{Home: e.home},
		AllowSource: func() policy.Allowlist { return live.Current().AllowList() }})
	if err != nil {
		t.Fatal(err)
	}
	lazy.set(pub)
	e.policyPath = path
	return e, bare
}

// A policy change since the task started parks the task; the human can approve
// exactly that change from the terminal and the task then publishes.
func TestApprovePolicyChangeFlow(t *testing.T) {
	e, bare := policyChangeEnv(t)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	pend, _ := e.st.PendingApprovals(context.Background())
	if len(pend) != 1 || !strings.Contains(pend[0].Reason, "policy changed") {
		t.Fatalf("pending %+v", pend)
	}
	fakeTTY(t)
	code, out, errS := e.orch(strings.NewReader("yes\n"), "approve", "1")
	if code != 0 || !strings.Contains(out, "policy_changed:") {
		t.Fatalf("approve %d %q %q", code, out, errS)
	}
	e.waitState(id, store.StateDone)
	if got := git(t, bare, "for-each-ref", "--format=%(refname) %(objectname)"); got != "refs/heads/orch/1 "+e.head {
		t.Fatalf("remote refs %q", got)
	}
}

// A grant is bound to the policy hash that parked the task: once another
// policy is in force, the old entry is refused and nothing is recorded.
func TestApprovePolicyChangeStaleHashRefused(t *testing.T) {
	e, _ := policyChangeEnv(t)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	// Another task starting loads yet another policy; it is the one in force now.
	if err := os.WriteFile(e.policyPath, []byte(policyV3), 0o600); err != nil {
		t.Fatal(err)
	}
	id2 := e.submit()
	e.waitState(id2, store.StateDone)
	fakeTTY(t)
	code, _, errS := e.orch(strings.NewReader("yes\n"), "approve", "1")
	if code == 0 || !strings.Contains(errS, "맞지 않습니다") {
		t.Fatalf("approve %d %q", code, errS)
	}
	if b, err := os.ReadFile(approvalsFile(e)); err == nil && strings.Contains(string(b), `"hash"`) {
		t.Fatalf("a grant was recorded for a refused entry: %s", b)
	}
}
