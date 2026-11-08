package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"aigate/internal/orch/store"
)

const (
	noteFresh = `verdict ok | restore 55s (delete_server 11s, quota_ok 3.3s, create_volume 6.1s, create_server 17s, wait_ssh 17s) | gate 153s (base 79s, head 71s)`
	noteCache = `verdict ok | restore 50s (delete_server 9.4s, quota_ok 3.2s, create_volume 5.8s, create_server 9.8s, wait_ssh 22s) | gate 84s (base 0.0s, head 80s) | base cache 1/1`
)

func TestTimelineDurations(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	evs := []store.Event{
		{From: "", To: store.StateQueued, Note: "created", At: t0},
		{From: store.StateQueued, To: store.StateVerifying, Note: "go", At: t0.Add(1500 * time.Millisecond)},
		{From: store.StateVerifying, To: store.StateError, Note: "boom", At: t0.Add(11500 * time.Millisecond)},
	}
	got := Timeline(evs)
	if len(got) != 3 {
		t.Fatalf("len %d", len(got))
	}
	want := []float64{1.5, 10, 0}
	for i, s := range got {
		if s.Seconds != want[i] || s.State != string(evs[i].To) || s.Note != evs[i].Note || !s.At.Equal(evs[i].At) {
			t.Fatalf("step %d = %+v", i, s)
		}
	}
	if len(Timeline(nil)) != 0 {
		t.Fatal("nil events should give no steps")
	}
}

func TestParseVerifyNote(t *testing.T) {
	b, ok := ParseVerifyNote(noteFresh)
	want := VerifyBreakdown{RestoreSec: 55, GateSec: 153, BaseSec: 79, HeadSec: 71}
	if !ok || b != want {
		t.Fatalf("fresh %+v %v want %+v", b, ok, want)
	}
	b, ok = ParseVerifyNote(noteCache)
	want = VerifyBreakdown{RestoreSec: 50, GateSec: 84, BaseSec: 0, HeadSec: 80, BaseCacheHits: 1, BaseCacheRuns: 1}
	if !ok || b != want {
		t.Fatalf("cache %+v %v want %+v", b, ok, want)
	}
	// no restore part (nil record): gate only
	b, ok = ParseVerifyNote("verdict ok | gate 9.5s (base 4.5s, head 5.0s)")
	if !ok || b.RestoreSec != 0 || b.GateSec != 9.5 || b.BaseSec != 4.5 || b.HeadSec != 5 {
		t.Fatalf("gate only %+v %v", b, ok)
	}
}

func TestParseVerifyNoteMissing(t *testing.T) {
	for _, n := range []string{"", "created", "verdict ok", "gate soon"} {
		if b, ok := ParseVerifyNote(n); ok || b != (VerifyBreakdown{}) {
			t.Fatalf("%q -> %+v %v", n, b, ok)
		}
	}
}

func TestEventsEndpoint(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	ctx := context.Background()
	id, err := e.st.CreateTask(ctx, store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "버그수정", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Transition(ctx, id, store.StateQueued, store.StateVerifying, "start"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Transition(ctx, id, store.StateVerifying, store.StateError, noteCache); err != nil {
		t.Fatal(err)
	}
	code, b := e.do(t, "GET", fmt.Sprintf("/api/v1/tasks/%d/events", id), "")
	if code != 200 {
		t.Fatalf("events %d %s", code, b)
	}
	var r struct {
		Task   TaskView
		Steps  []StepTime
		Verify *VerifyBreakdown
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Task.ID != id || len(r.Steps) != 3 || r.Steps[0].State != string(store.StateQueued) || r.Steps[2].Seconds != 0 {
		t.Fatalf("bad response %s", b)
	}
	if r.Verify == nil || r.Verify.GateSec != 84 || r.Verify.BaseCacheRuns != 1 {
		t.Fatalf("verify %+v", r.Verify)
	}

	code, b = e.do(t, "GET", "/api/v1/tasks/9999/events", "")
	wantEnvelope(t, code, b, 404, "not_found")
	code, b = e.do(t, "GET", "/api/v1/tasks/abc/events", "")
	wantEnvelope(t, code, b, 400, "bad_request")
}
