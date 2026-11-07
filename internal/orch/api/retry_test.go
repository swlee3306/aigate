package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aigate/internal/orch/store"
)

// failedTask makes a task that ended failed. infra selects an infrastructure
// failure in step; otherwise it is a verdict failure.
func failedTask(t *testing.T, e *env, step store.State, infra bool) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := e.st.CreateTask(ctx, store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "c", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	chain := []store.State{store.StateQueued, store.StatePreparing, store.StateAgentRunning, store.StateCollecting, store.StateVerifying, store.StatePassed, store.StatePublishing}
	for i := 0; i < len(chain)-1 && chain[i] != step; i++ {
		if err := e.st.Transition(ctx, id, chain[i], chain[i+1], "x"); err != nil {
			t.Fatal(err)
		}
	}
	o := store.TransitionOpts{ErrorText: "restore quota 413"}
	note := string(step) + " failed after 3 attempts: restore quota 413"
	if infra {
		o.InfraFailed, o.RetryState = true, step
	} else {
		o.ErrorText, note = "", "behavior_failure"
	}
	if err := e.st.TransitionWith(ctx, id, step, store.StateFailed, note, o); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRetryAPIRoundTrip(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id := failedTask(t, e, store.StateVerifying, true)
	path := "/tasks/" + itoa(id)

	code, b := e.do(t, "GET", path, "")
	var v TaskView
	if code != 200 || json.Unmarshal(b, &v) != nil || !v.Retryable || v.Retries != 0 || v.State != "failed" {
		t.Fatalf("before: %d %s", code, b)
	}
	code, b = e.do(t, "GET", "/tasks", "")
	var list []TaskView
	if code != 200 || json.Unmarshal(b, &list) != nil || len(list) != 1 || !list[0].Retryable {
		t.Fatalf("list: %d %s", code, b)
	}

	code, b = e.do(t, "POST", path+"/retry", "")
	if code != 200 {
		t.Fatalf("retry %d %s", code, b)
	}
	v = TaskView{}
	if json.Unmarshal(b, &v) != nil || v.State != "verifying" || v.Attempts != 0 || v.Error != "" || v.Retries != 1 || v.Retryable {
		t.Fatalf("after: %s", b)
	}
	if strings.Contains(string(b), `"retryable"`) {
		t.Fatalf("retryable should be omitted when false: %s", b)
	}
	evs, _ := e.st.Events(context.Background(), id)
	if last := evs[len(evs)-1]; last.From != store.StateFailed || last.To != store.StateVerifying ||
		!strings.Contains(last.Note, "retry requested by operator (was: ") {
		t.Fatalf("event %+v", last)
	}

	// second retry: the task is running again -> 409
	code, b = e.do(t, "POST", path+"/retry", "")
	if code != 409 || !strings.Contains(errText(b), "failed") {
		t.Fatalf("second retry %d %s", code, b)
	}
}

func TestRetryAPIRefusals(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	verdict := failedTask(t, e, store.StateVerifying, false)
	publishing := failedTask(t, e, store.StatePublishing, true)
	queued, _ := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p"})
	for name, c := range map[string]struct {
		id   int64
		want string
	}{
		"verdict": {verdict, "인프라"}, "publishing": {publishing, "게시"}, "queued": {queued, "failed"},
	} {
		code, b := e.do(t, "POST", "/tasks/"+itoa(c.id)+"/retry", "")
		if code != 409 || !strings.Contains(errText(b), c.want) {
			t.Errorf("%s: %d %s", name, code, b)
		}
		got, _ := e.st.GetTask(context.Background(), c.id)
		if got.State == store.StateVerifying && name != "queued" {
			t.Errorf("%s moved", name)
		}
	}
	if code, b := e.do(t, "POST", "/tasks/999/retry", ""); code != 404 || errText(b) == "" {
		t.Errorf("missing: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/tasks/abc/retry", ""); code != 400 || errText(b) == "" {
		t.Errorf("bad id: %d %s", code, b)
	}
	if code, b := e.do(t, "GET", "/tasks/"+itoa(verdict)+"/retry", ""); code != 405 || errText(b) == "" {
		t.Errorf("GET retry: %d %s", code, b)
	}
	var v TaskView
	_, b := e.do(t, "GET", "/tasks/"+itoa(verdict), "")
	json.Unmarshal(b, &v)
	if v.Retryable {
		t.Errorf("verdict failure shown retryable")
	}
}

func TestRetryAPIConcurrentDouble(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id := failedTask(t, e, store.StateCollecting, true)
	codes := make(chan int, 6)
	for i := 0; i < 6; i++ {
		go func() {
			c, _ := e.do(t, "POST", "/tasks/"+itoa(id)+"/retry", "")
			codes <- c
		}()
	}
	ok := 0
	for i := 0; i < 6; i++ {
		switch c := <-codes; c {
		case 200:
			ok++
		case 409:
		default:
			t.Errorf("code %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d succeeded", ok)
	}
}
