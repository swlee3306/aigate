package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"aigate/internal/orch/store"
)

func withPolicyClasses(classes map[string]string) func(*Config) {
	return func(c *Config) {
		c.Policy = func() PolicyView { return PolicyView{Hash: "h", Classes: classes} }
	}
}

func withQueueLimit(n int) func(*Config) { return func(c *Config) { c.QueueLimit = n } }

func (e *env) submitV1(t *testing.T, class string) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"repo": e.repo, "base": "HEAD", "prompt": "p", "class": class, "agent": "claude"})
	return e.do(t, "POST", "/api/v1/tasks", string(b))
}

func TestSubmitUnknownClassRefused(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, withPolicyClasses(map[string]string{"docs": "L2", "test": "L1"}))
	e.d.pauseWorkers()
	code, b := e.submitV1(t, "deploy")
	if code != 400 {
		t.Fatalf("%d %s", code, b)
	}
	v := decodeV1Err(t, b)
	if v.Error.Code != "unknown_class" || v.Error.Message == "" {
		t.Fatalf("%s", b)
	}
	if n := e.taskCount(t); n != 0 {
		t.Fatalf("%d tasks created", n)
	}
	// legacy flat form carries the code too
	lc, _, lb := e.sockDo(t, "POST", "/tasks", e.submitBody("p"), nil) // class "docs": known
	if lc != 201 {
		t.Fatalf("known class on legacy path: %d %s", lc, lb)
	}
	body, _ := json.Marshal(map[string]string{"repo": e.repo, "base": "HEAD", "prompt": "p", "class": "nope", "agent": "claude"})
	lc, _, lb = e.sockDo(t, "POST", "/tasks", string(body), nil)
	if lc != 400 || flatCode(lb) != "unknown_class" {
		t.Fatalf("legacy: %d %s", lc, lb)
	}

	// No policy (h.policy == nil): every regex-valid class is accepted as before.
	e2 := newEnv(t, noSteps{}, 1)
	e2.d.pauseWorkers()
	if code, b := e2.submitV1(t, "deploy"); code != 201 {
		t.Fatalf("no policy: %d %s", code, b)
	}
	// A policy with an empty class list (R3) also accepts every valid class.
	e3 := newEnv(t, noSteps{}, 1, withPolicyClasses(map[string]string{}))
	e3.d.pauseWorkers()
	if code, b := e3.submitV1(t, "deploy"); code != 201 {
		t.Fatalf("empty class list: %d %s", code, b)
	}
	// The regex still runs first: an invalid class is a plain bad_request.
	if code, b := e.submitV1(t, "BAD CLASS"); code != 400 || v1Code(b) != "bad_request" {
		t.Fatalf("regex: %d %s", code, b)
	}
}

func TestSubmitKnownClassAccepted(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, withPolicyClasses(map[string]string{"docs": "L2", "테스트": "L1"}))
	e.d.pauseWorkers()
	for _, c := range []string{"docs", "테스트"} {
		code, b := e.submitV1(t, c)
		if code != 201 {
			t.Fatalf("%s: %d %s", c, code, b)
		}
		var v TaskView
		if err := json.Unmarshal(b, &v); err != nil || v.Class != c {
			t.Fatalf("%s: %s", c, b)
		}
	}
}

func TestSubmitQueueFull429(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, withQueueLimit(3))
	e.d.pauseWorkers()
	for i := 0; i < 3; i++ {
		if code, b := e.submitV1(t, "docs"); code != 201 {
			t.Fatalf("submit %d: %d %s", i, code, b)
		}
	}
	code, b := e.submitV1(t, "docs")
	if code != 429 {
		t.Fatalf("%d %s", code, b)
	}
	v := decodeV1Err(t, b)
	if v.Error.Code != "queue_full" || v.Error.Message != msgPerActor {
		t.Fatalf("%s", b)
	}
	// The keyed path is limited too, and a refused keyed submit leaves no
	// idempotency record behind (the same key works once there is room).
	kc, _, kb := e.sockDo(t, "POST", "/api/v1/tasks", e.submitBody("k"), hdr{"Idempotency-Key": "full-1"})
	if kc != 429 || v1Code(kb) != "queue_full" {
		t.Fatalf("keyed: %d %s", kc, kb)
	}
	if n := e.taskCount(t); n != 3 {
		t.Fatalf("%d tasks, want 3 (nothing created by the refused submits)", n)
	}
	// legacy flat form
	lc, _, lb := e.sockDo(t, "POST", "/tasks", e.submitBody("p"), nil)
	if lc != 429 || flatCode(lb) != "queue_full" {
		t.Fatalf("legacy: %d %s", lc, lb)
	}
	ts, _ := e.st.ListTasks(context.Background(), 10)
	if err := e.st.Transition(context.Background(), ts[0].ID, store.StateQueued, store.StateCancelled, "x"); err != nil {
		t.Fatal(err)
	}
	kc, _, kb = e.sockDo(t, "POST", "/api/v1/tasks", e.submitBody("k"), hdr{"Idempotency-Key": "full-1"})
	if kc != 201 {
		t.Fatalf("keyed after room: %d %s", kc, kb)
	}

	// Default limits: 20 per principal, 100 in all.
	e2 := newEnv(t, noSteps{}, 1)
	e2.d.pauseWorkers()
	if e2.d.handler.queueLimit != DefaultQueueLimit || DefaultQueueLimit != 20 {
		t.Fatalf("default limit %d / %d", e2.d.handler.queueLimit, DefaultQueueLimit)
	}
	if e2.d.handler.queueLimitTotal != DefaultQueueLimitTotal || DefaultQueueLimitTotal != 100 {
		t.Fatalf("default total %d / %d", e2.d.handler.queueLimitTotal, DefaultQueueLimitTotal)
	}
}

func TestQueueLimitCountsOnlyQueued(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, withQueueLimit(2))
	e.d.pauseWorkers()
	ctx := context.Background()
	running := queuedTask(t, e)
	if err := e.st.Transition(ctx, running, store.StateQueued, store.StatePreparing, "x"); err != nil {
		t.Fatal(err)
	}
	finished := queuedTask(t, e)
	if err := e.st.Transition(ctx, finished, store.StateQueued, store.StateCancelled, "x"); err != nil {
		t.Fatal(err)
	}
	failedTask(t, e, store.StateVerifying, false)
	for i := 0; i < 2; i++ {
		if code, b := e.submitV1(t, "docs"); code != 201 {
			t.Fatalf("submit %d: %d %s (non-queued tasks were counted)", i, code, b)
		}
	}
	if code, b := e.submitV1(t, "docs"); code != 429 {
		t.Fatalf("third: %d %s", code, b)
	}
	if n, err := e.st.CountQueued(ctx); err != nil || n != 2 {
		t.Fatalf("queued %d %v", n, err)
	}
}

// Review focus 1: N+10 concurrent submits, keyed and unkeyed mixed, never
// leave more than N tasks queued; every refusal is the intended 429 and no
// other status appears. Several rounds, because a non-atomic count/insert has
// a narrow window.
func TestQueueLimitRaceNeverExceeds(t *testing.T) {
	const limit = 4
	e := newEnv(t, noSteps{}, 1, withQueueLimit(limit))
	e.d.pauseWorkers()
	ctx := context.Background()
	for round := 0; round < 15; round++ {
		const n = limit + 10
		codes := make([]int, n)
		bodies := make([][]byte, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := hdr(nil)
				if i%2 == 0 {
					h = hdr{"Idempotency-Key": fmt.Sprintf("race-%d-%d", round, i)}
				}
				codes[i], _, bodies[i] = e.sockDo(t, "POST", "/api/v1/tasks", e.submitBody(fmt.Sprint("p", i)), h)
			}()
		}
		wg.Wait()
		created := 0
		for i, c := range codes {
			switch c {
			case 201:
				created++
			case 429:
				if v1Code(bodies[i]) != "queue_full" || errMessage(bodies[i]) != msgPerActor {
					t.Fatalf("round %d: 429 with %s", round, bodies[i])
				}
			default:
				t.Fatalf("round %d: unexpected %d %s", round, c, bodies[i])
			}
		}
		q, err := e.st.CountQueued(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if q > limit || created != limit || q != limit {
			t.Fatalf("round %d: %d queued, %d created, limit %d", round, q, created, limit)
		}
		// Empty the queue for the next round.
		ts, err := e.st.ListTasks(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range ts {
			if tk.State == store.StateQueued {
				if err := e.st.Transition(ctx, tk.ID, store.StateQueued, store.StateCancelled, "x"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// CF-W2a-1: the keyed path still writes task_submitted exactly once per
// created task and never on replay, and not while holding idemMu.
func TestKeyedSubmitLedgerAfterIdemRecord(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	var mu sync.Mutex
	var seen []int64
	e.d.handler.onSubmitted = func(ctx context.Context, tk store.Task) {
		// idemMu must be free here: TryLock succeeds.
		if !e.d.handler.idemMu.TryLock() {
			t.Error("idemMu held during the ledger hook")
		} else {
			e.d.handler.idemMu.Unlock()
		}
		// A keyed submit's idempotency record is already written.
		if p, ok := PrincipalFrom(ctx); ok && tk.Prompt == "ledger" {
			id, _, found, err := e.st.GetIdempotent(ctx, scopedIdemKey(p, "led-1"))
			if err != nil || !found || id != tk.ID {
				t.Errorf("idempotency record not written before the ledger hook: %d %v %v", id, found, err)
			}
		}
		mu.Lock()
		seen = append(seen, tk.ID)
		mu.Unlock()
	}
	body := e.submitBody("ledger")
	c1, _, b1 := e.sockDo(t, "POST", "/api/v1/tasks", body, hdr{"Idempotency-Key": "led-1"})
	c2, h2, _ := e.sockDo(t, "POST", "/api/v1/tasks", body, hdr{"Idempotency-Key": "led-1"})
	if c1 != 201 || c2 != 200 || h2.Get("Idempotent-Replay") != "true" {
		t.Fatalf("%d %s / %d", c1, b1, c2)
	}
	if c, b := e.submitV1(t, "docs"); c != 201 {
		t.Fatalf("unkeyed: %d %s", c, b)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("ledger hook ran %d times, want 2 (once per created task)", len(seen))
	}
}
