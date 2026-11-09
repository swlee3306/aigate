package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// Each step carries the actor of the event that ENTERED it: submit, a worker
// transition, a cancel and a retry; a NULL column (before schema v7) is empty.
func TestTimelineStepCarriesActor(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	evs := []store.Event{
		{To: store.StateQueued, Note: "created", At: t0, Actor: "session:alice"},
		{From: store.StateQueued, To: store.StatePreparing, At: t0.Add(time.Second), Actor: store.ActorSystem},
		{From: store.StatePreparing, To: store.StateError, At: t0.Add(2 * time.Second), Actor: ""},
		{From: store.StateError, To: store.StateQueued, Note: "retry", At: t0.Add(3 * time.Second), Actor: "token:mac-ssh"},
		{From: store.StateQueued, To: store.StateCancelled, Note: "cancelled", At: t0.Add(4 * time.Second), Actor: "socket:uid:501"},
		{From: store.StateCancelled, To: store.StateQueued, At: t0.Add(5 * time.Second), Actor: store.ActorInvalid},
	}
	got := Timeline(evs)
	want := []string{"session:alice", "system", "", "token:mac-ssh", "socket:uid:501", "invalid"}
	for i, s := range got {
		if s.Actor != want[i] {
			t.Fatalf("step %d (%s) actor %q want %q", i, s.State, s.Actor, want[i])
		}
	}
	// an empty actor is omitted from the JSON, a set one is present
	b0, _ := json.Marshal(got[2])
	b1, _ := json.Marshal(got[0])
	if strings.Contains(string(b0), "actor") || !strings.Contains(string(b1), `"actor":"session:alice"`) {
		t.Fatalf("json: %s / %s", b0, b1)
	}
}

// The actor on the timeline is only what the server recorded from the
// authenticated principal: forged headers on submit, cancel AND the read
// itself change nothing, and a body "actor" is refused.
func TestTimelineActorNeverEchoesClientInput(t *testing.T) {
	w, _ := adversaryEnv(t)
	tok, csrf := w.newSession(t, "alice")
	forged := hdr{"X-Actor": "socket:uid:0", "X-Orch-Actor": "system", "X-Forwarded-User": "root",
		"Remote-User": "root", "Tailscale-User-Login": "admin@evil.example", "X-Principal": "token:root"}
	h := with(sessionHdr(tok, csrf), forged)
	code, _, b := w.web(t, "POST", "/api/v1/tasks", w.submitBody("p"), h)
	var v TaskView
	if code != 201 || json.Unmarshal(b, &v) != nil {
		t.Fatalf("submit: %d %s", code, b)
	}
	if code, _, b = w.web(t, "POST", "/api/v1/tasks", strings.TrimSuffix(w.submitBody("q"), "}")+`,"actor":"system"}`, h); code != 400 {
		t.Fatalf("body actor: %d %s", code, b)
	}
	if code, _, b = w.web(t, "POST", "/api/v1/tasks/"+itoa(v.ID)+"/cancel", "", h); code != 200 {
		t.Fatalf("cancel: %d %s", code, b)
	}
	code, _, b = w.web(t, "GET", "/api/v1/tasks/"+itoa(v.ID)+"/events", "", h)
	var resp struct {
		Steps []StepTime `json:"steps"`
	}
	if code != 200 || json.Unmarshal(b, &resp) != nil || len(resp.Steps) != 2 {
		t.Fatalf("events: %d %s", code, b)
	}
	for _, s := range resp.Steps {
		if s.Actor != "session:alice" {
			t.Fatalf("step %s actor %q, want session:alice: %s", s.State, s.Actor, b)
		}
	}
	for _, bad := range []string{"root", "admin", "evil", "socket:uid:0", "system"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("response echoes client input %q: %s", bad, b)
		}
	}
	// a stored actor outside the allowed characters comes back as "invalid"
	ctx := store.WithActor(context.Background(), "alice‮<img>")
	id, err := w.env.st.CreateTask(ctx, store.Task{Repo: w.repo, Base: "HEAD", Prompt: "p", Class: "c", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	code, _, b = w.web(t, "GET", "/api/v1/tasks/"+itoa(id)+"/events", "", h)
	if code != 200 || !strings.Contains(string(b), `"actor":"invalid"`) || strings.Contains(string(b), "img") {
		t.Fatalf("invalid actor: %d %s", code, b)
	}
}
