package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/store"
)

func TestStagesStatic(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	code, b := e.do(t, "GET", "/api/v1/stages", "")
	if code != 200 {
		t.Fatalf("stages %d %s", code, b)
	}
	var got []Stage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	ids := []string{"design", "implement", "verify", "approve", "publish", "operate"}
	if len(got) != len(ids) {
		t.Fatalf("len %d: %s", len(got), b)
	}
	for i, s := range got {
		if s.ID != ids[i] || s.Name == "" || s.Next == "" || s.Components == nil {
			t.Fatalf("stage %d = %+v", i, s)
		}
		if s.Status != "✅" && s.Status != "🔄" && s.Status != "⬜" {
			t.Fatalf("stage %s status %q", s.ID, s.Status)
		}
	}
	if got[3].Next != "대기 중인 승인이 없습니다" {
		t.Fatalf("approve next %q", got[3].Next)
	}
}

func mkTask(t *testing.T, e *env, to ...store.State) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := e.st.CreateTask(ctx, store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "버그수정", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	from := store.StateQueued
	for _, s := range to {
		if err := e.st.Transition(ctx, id, from, s, "n"); err != nil {
			t.Fatal(err)
		}
		from = s
	}
	return id
}

func getOverview(t *testing.T, e *env) Overview {
	t.Helper()
	code, b := e.do(t, "GET", "/api/v1/overview", "")
	if code != 200 {
		t.Fatalf("overview %d %s", code, b)
	}
	var o Overview
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOverviewCounts(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	mkTask(t, e)
	mkTask(t, e, store.StatePreparing)
	mkTask(t, e, store.StatePreparing, store.StateAgentRunning)
	mkTask(t, e, store.StateVerifying, store.StateError)
	o := getOverview(t, e)
	if o.Total != 4 || o.ByState["queued"] != 1 || o.ByState["preparing"] != 1 ||
		o.ByState["agent_running"] != 1 || o.ByState["error"] != 1 || len(o.ByState) != 4 {
		t.Fatalf("overview %+v", o)
	}
	if o.PendingApprovals != 0 || o.Todo == nil || len(o.Todo) != 0 || len(o.StageAvg) != 0 {
		t.Fatalf("overview %+v", o)
	}
}

func TestOverviewTodoListsPendingApprovals(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	id := mkTask(t, e)
	aid, err := e.st.AddApproval(context.Background(), store.Approval{
		TaskID: id, Kind: store.KindProtectedModified, Hash: "h1", Base: "b", Head: "h", Reason: "보호 파일 수정"})
	if err != nil {
		t.Fatal(err)
	}
	o := getOverview(t, e)
	if o.PendingApprovals != 1 || len(o.Todo) != 1 {
		t.Fatalf("overview %+v", o)
	}
	td := o.Todo[0]
	if td.Kind != "approval" || td.Task != id || td.Approval != aid || !strings.Contains(td.Text, "승인 대기") || !strings.Contains(td.Text, "보호 파일 수정") {
		t.Fatalf("todo %+v", td)
	}
	code, b := e.do(t, "GET", "/api/v1/stages", "")
	var ss []Stage
	if code != 200 || json.Unmarshal(b, &ss) != nil || ss[3].Next != "승인 대기 1건" {
		t.Fatalf("stages %d %s", code, b)
	}
}

func TestOverviewStageAvgFromEvents(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	// no finished task: no keys
	mkTask(t, e, store.StatePreparing)
	if o := getOverview(t, e); o.StageAvg == nil || len(o.StageAvg) != 0 {
		t.Fatalf("avg %+v", o.StageAvg)
	}
	mkTask(t, e, store.StatePreparing, store.StateAgentRunning, store.StateCollecting,
		store.StateVerifying, store.StatePassed, store.StatePublishing, store.StateDone)
	o := getOverview(t, e)
	for _, k := range []string{"queued", "preparing", "agent_running", "collecting", "verifying", "publishing"} {
		if v, ok := o.StageAvg[k]; !ok || v < 0 {
			t.Fatalf("avg %+v missing %s", o.StageAvg, k)
		}
	}
	if len(o.StageAvg) != 6 {
		t.Fatalf("avg %+v", o.StageAvg)
	}
}

func TestStageAvgMath(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	ev := func(to store.State, sec int) store.Event {
		return store.Event{To: to, At: t0.Add(time.Duration(sec) * time.Second)}
	}
	a := []store.Event{ev(store.StateQueued, 0), ev(store.StatePreparing, 2), ev(store.StateAgentRunning, 12),
		ev(store.StateVerifying, 15), ev(store.StateDone, 20)}
	b := []store.Event{ev(store.StateQueued, 0), ev(store.StatePreparing, 4), ev(store.StateDone, 10)}
	got := stageAvg([][]store.Event{a, b})
	want := map[string]float64{"queued": 3, "preparing": 8, "agent_running": 3, "verifying": 5}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("avg[%s]=%v want %v", k, got[k], v)
		}
	}
	if g := stageAvg(nil); g == nil || len(g) != 0 {
		t.Fatalf("empty %+v", g)
	}
}
