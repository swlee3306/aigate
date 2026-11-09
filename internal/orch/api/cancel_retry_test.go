package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
)

type v1ErrBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"error"`
}

func decodeV1Err(t *testing.T, b []byte) v1ErrBody {
	t.Helper()
	var v v1ErrBody
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not a v1 error body: %s", b)
	}
	return v
}

func queuedTask(t *testing.T, e *env) int64 {
	t.Helper()
	id, err := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "c", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCancelOfFinalTaskIsAlreadyFinal409(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id := failedTask(t, e, store.StateVerifying, false)
	code, b := e.do(t, "POST", "/api/v1/tasks/"+itoa(id)+"/cancel", "")
	if code != 409 {
		t.Fatalf("%d %s", code, b)
	}
	if v := decodeV1Err(t, b); v.Error.Code != "already_final" || v.Error.Message != "이미 끝난 작업입니다" {
		t.Fatalf("%s", b)
	}
	code, b = e.do(t, "POST", "/api/v1/tasks/999999/cancel", "")
	if v := decodeV1Err(t, b); code != 404 || v.Error.Code != "not_found" {
		t.Fatalf("%d %s", code, b)
	}
}

// Cancel gives up after 5 lost compare-and-sets; that is a 409 state_changed,
// not the generic 500. The seam moves the task forward between Cancel's read
// and its compare-and-set, five times.
func TestCancelRaceLostIsStateChanged409NotTheGeneric500(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id := queuedTask(t, e)
	chain := []store.State{store.StateQueued, store.StatePreparing, store.StateAgentRunning, store.StateCollecting, store.StateVerifying, store.StateNeedsApproval}
	n := 0
	e.d.handler.m = machine.New(e.st, nil, machine.WithBeforeCancelCAS(func(ctx context.Context, id int64) {
		if err := e.st.Transition(ctx, id, chain[n], chain[n+1], "race"); err != nil {
			t.Errorf("seam: %v", err)
		}
		n++
	}))
	code, b := e.do(t, "POST", "/api/v1/tasks/"+itoa(id)+"/cancel", "")
	if code != 409 {
		t.Fatalf("%d %s", code, b)
	}
	if v := decodeV1Err(t, b); v.Error.Code != "state_changed" || v.Error.Message == "" || v.Error.Message == "내부 오류" {
		t.Fatalf("%s", b)
	}
	if n != 5 {
		t.Fatalf("seam ran %d times, want 5", n)
	}
	// legacy flat form still carries the code
	n = 0
	id2 := queuedTask(t, e)
	var flat map[string]string
	lc, _, lb := e.sockDo(t, "POST", "/tasks/"+itoa(id2)+"/cancel", "", nil)
	_ = json.Unmarshal(lb, &flat)
	if lc != 409 || flat["code"] != "state_changed" || flat["error"] == "" {
		t.Fatalf("legacy: %d %s", lc, lb)
	}
}

func TestRetryNotRetryableCarriesReason(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	cancelled := queuedTask(t, e)
	if err := e.st.Transition(context.Background(), cancelled, store.StateQueued, store.StateCancelled, "x"); err != nil {
		t.Fatal(err)
	}
	done := queuedTask(t, e)
	for _, p := range [][2]store.State{{"queued", "preparing"}, {"preparing", "agent_running"}, {"agent_running", "collecting"}, {"collecting", "verifying"}, {"verifying", "passed"}, {"passed", "publishing"}, {"publishing", "done"}} {
		if err := e.st.Transition(context.Background(), done, p[0], p[1], "x"); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]int64{
		"cancelled": cancelled,
		"done":      done,
		"verdict":   failedTask(t, e, store.StateVerifying, false),
		"publish":   failedTask(t, e, store.StatePublishing, true),
	}
	for name, id := range cases {
		task, err := e.st.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		want := machine.Retryable(task).Error()
		code, b := e.do(t, "POST", "/api/v1/tasks/"+itoa(id)+"/retry", "")
		v := decodeV1Err(t, b)
		if code != 409 || v.Error.Code != "not_retryable" || v.Error.Reason != want || want == "" || v.Error.Message != want {
			t.Fatalf("%s: %d %s (want reason %q)", name, code, b, want)
		}
	}
	code, b := e.do(t, "POST", "/api/v1/tasks/999999/retry", "")
	if v := decodeV1Err(t, b); code != 404 || v.Error.Code != "not_found" {
		t.Fatalf("%d %s", code, b)
	}
}

// A retry that loses its compare-and-set while the task is still retryable
// cannot be staged through the API (the loser sees the task already moved, so
// it is not_retryable); the mapping itself is checked directly, and the
// real-flow loser is checked to be one of the two 409 codes.
func TestRetryStateChanged409(t *testing.T) {
	he, ok := retryErr(store.ErrStateMismatch).(*httpError)
	if !ok || he.code != 409 || he.v1Code != "state_changed" || he.reason != "" {
		t.Fatalf("%#v", he)
	}
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id := failedTask(t, e, store.StateVerifying, true)
	if code, b := e.do(t, "POST", "/api/v1/tasks/"+itoa(id)+"/retry", ""); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	code, b := e.do(t, "POST", "/api/v1/tasks/"+itoa(id)+"/retry", "")
	if v := decodeV1Err(t, b); code != 409 || (v.Error.Code != "not_retryable" && v.Error.Code != "state_changed") {
		t.Fatalf("%d %s", code, b)
	}
}

func TestCancellableFlagMatchesStateTable(t *testing.T) {
	for s := range machine.Allowed {
		want := slices.Contains(machine.Allowed[s], machine.StateCancelled)
		if got := viewOf(store.Task{State: s}).Cancellable; got != want {
			t.Errorf("state %s: cancellable=%v want %v", s, got, want)
		}
	}
	if viewOf(store.Task{State: store.StateDone}).Cancellable || !viewOf(store.Task{State: store.StateQueued}).Cancellable {
		t.Error("spot check")
	}
	// over the API
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	q, f := queuedTask(t, e), failedTask(t, e, store.StateVerifying, false)
	for id, want := range map[int64]bool{q: true, f: false} {
		_, b := e.do(t, "GET", "/api/v1/tasks/"+itoa(id), "")
		var v TaskView
		if json.Unmarshal(b, &v) != nil || v.Cancellable != want {
			t.Fatalf("%d: %s", id, b)
		}
	}
}

// Cancel and retry on the same failed task. A failed task is terminal for
// cancel, so cancel can only succeed AFTER the retry moved the task back into a
// step (then it cancels the resumed task). The brief's "exactly one 200" is
// therefore not an invariant; the real ones are: retry always wins, cancel is
// 200 only when it ran after the retry (final state cancelled) and otherwise a
// clean 409 already_final (final state is the resumed step), never a 5xx.
func TestCancelAndRetryConcurrentOneWins(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	for i := 0; i < 50; i++ {
		id := failedTask(t, e, store.StateVerifying, true)
		codes := make([]int, 2)
		bodies := make([][]byte, 2)
		var wg sync.WaitGroup
		for j, op := range []string{"cancel", "retry"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[j], bodies[j] = e.do(t, "POST", "/api/v1/tasks/"+itoa(id)+"/"+op, "")
			}()
		}
		wg.Wait()
		cur, err := e.st.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if codes[1] != 200 {
			t.Fatalf("iter %d: retry %d %s", i, codes[1], bodies[1])
		}
		switch codes[0] {
		case 200:
			if cur.State != store.StateCancelled {
				t.Fatalf("iter %d: cancel 200 but state %s", i, cur.State)
			}
		case 409:
			if v := decodeV1Err(t, bodies[0]); v.Error.Code != "already_final" || cur.State != store.StateVerifying {
				t.Fatalf("iter %d: %s state %s", i, bodies[0], cur.State)
			}
		default:
			t.Fatalf("iter %d: cancel %d %s", i, codes[0], bodies[0])
		}
	}
}

func TestCancelRetryRequireWriteTier(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id := failedTask(t, e, store.StateVerifying, true)
	srv := e.httpServer(t)
	tok, _ := e.newSession(t, "alice")
	for _, op := range []string{"cancel", "retry"} {
		path := "/api/v1/tasks/" + itoa(id) + "/" + op
		if c, _, _ := httpDo(t, srv, "POST", path, "", nil); c != http.StatusUnauthorized {
			t.Errorf("%s no auth: %d", op, c)
		}
		if c, _, _ := httpDo(t, srv, "POST", path, "", hdr{"Cookie": cookie(tok)}); c != http.StatusForbidden {
			t.Errorf("%s no csrf: %d", op, c)
		}
	}
}
