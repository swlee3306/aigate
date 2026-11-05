package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/store"
)

// parked builds a task in needs_approval with one pending entry of kind.
func parked(t *testing.T, e *env, kind, hash string) (taskID, apprID int64, home string) {
	t.Helper()
	home = t.TempDir()
	e.d.handler.grants = approvals.Grants{Home: home}
	ctx := context.Background()
	id, err := e.st.CreateTask(ctx, store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetHeadSHA(ctx, id, "0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Transition(ctx, id, store.StateQueued, store.StateNeedsApproval, "test"); err != nil {
		t.Fatal(err)
	}
	aid, err := e.st.AddApproval(ctx, store.Approval{TaskID: id, Kind: kind, Hash: hash, Base: e.sha,
		Head: "0123456789abcdef0123456789abcdef01234567", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return id, aid, home
}

func grant(t *testing.T, e *env, home, base, hash string) {
	t.Helper()
	rid, _, err := gate.RepoID(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := (approval.Store{Path: gate.ApprovalsPath(home)}).Grant(rid, base, hash, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func state(t *testing.T, e *env, id int64) store.State {
	t.Helper()
	tk, err := e.st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.State
}

func TestReverifyWithoutGrantChangesNothing(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	tid, aid, home := parked(t, e, store.KindProtectedModified, "hash1")
	path := "/approvals/" + itoa(aid) + "/reverify"
	code, b := e.do(t, "POST", path, "")
	if code != http.StatusConflict || !strings.Contains(errText(b), "grant") {
		t.Fatalf("no grant: %d %s", code, b)
	}
	if state(t, e, tid) != store.StateNeedsApproval {
		t.Fatal("task moved without a grant")
	}
	if a, _ := e.st.GetApproval(context.Background(), aid); a.Status != store.ApprovalPending {
		t.Fatalf("entry %+v", a)
	}
	// A grant with the wrong hash, or for the wrong base, does not count.
	grant(t, e, home, e.sha, "other-hash")
	grant(t, e, home, "0000000000000000000000000000000000000000", "hash1")
	if code, _ = e.do(t, "POST", path, ""); code != http.StatusConflict || state(t, e, tid) != store.StateNeedsApproval {
		t.Fatalf("wrong grant accepted: %d", code)
	}
	// The exact grant is accepted: the entry is approved, the task is re-judged.
	grant(t, e, home, e.sha, "hash1")
	code, b = e.do(t, "POST", path, "")
	if code != 200 {
		t.Fatalf("reverify %d %s", code, b)
	}
	var v TaskView
	json.Unmarshal(b, &v)
	if v.State != "verifying" || state(t, e, tid) != store.StateVerifying {
		t.Fatalf("state %+v", v)
	}
	if a, _ := e.st.GetApproval(context.Background(), aid); a.Status != store.ApprovalApproved {
		t.Fatalf("entry %+v", a)
	}
	// And not twice.
	if code, _ = e.do(t, "POST", path, ""); code != http.StatusConflict {
		t.Fatalf("second reverify %d", code)
	}
}

func TestReverifyRefusesDestructiveAndSuperseded(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	tid, aid, home := parked(t, e, store.KindDestructiveBlocked, "d")
	grant(t, e, home, e.sha, "d")
	if code, b := e.do(t, "POST", "/approvals/"+itoa(aid)+"/reverify", ""); code != http.StatusForbidden {
		t.Fatalf("destructive: %d %s", code, b)
	}
	if state(t, e, tid) != store.StateNeedsApproval {
		t.Fatal("moved")
	}
	// Superseded: a newer head replaced the entry; even a matching grant is useless.
	tid2, aid2, home2 := parked(t, e, store.KindProtectedModified, "old")
	grant(t, e, home2, e.sha, "old")
	if _, err := e.st.AddApproval(context.Background(), store.Approval{TaskID: tid2, Kind: store.KindProtectedModified,
		Hash: "new", Base: e.sha, Head: "ffffffffffffffffffffffffffffffffffffffff"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "POST", "/approvals/"+itoa(aid2)+"/reverify", ""); code != http.StatusConflict {
		t.Fatalf("superseded: %d", code)
	}
	if state(t, e, tid2) != store.StateNeedsApproval {
		t.Fatal("moved")
	}
	if code, _ := e.do(t, "POST", "/approvals/9999/reverify", ""); code != 404 {
		t.Fatalf("missing: %d", code)
	}
}

func TestApprovalGetShowsTask(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	_, aid, _ := parked(t, e, store.KindProtectedModified, "h")
	code, b := e.do(t, "GET", "/approvals/"+itoa(aid), "")
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil || d.Repo != e.repo || d.TaskState != "needs_approval" || d.Hash != "h" {
		t.Fatalf("%d %s", code, b)
	}
	if code, _ = e.do(t, "GET", "/approvals/abc", ""); code != 400 {
		t.Fatalf("bad id %d", code)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
