package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCreateTaskLimitedCountsOnlyQueued(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	task := Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"}
	a, err := s.CreateTaskLimited(ctx, task, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(ctx, task, 0, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(ctx, task, 0, 2); !errors.Is(err, ErrQueueFullTotal) {
		t.Fatalf("third: %v, want ErrQueueFullTotal", err)
	}
	if n, err := s.CountQueued(ctx); err != nil || n != 2 {
		t.Fatalf("queued %d %v", n, err)
	}
	// A task that left queued no longer counts.
	if err := s.Transition(ctx, a, StateQueued, StatePreparing, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(ctx, task, 0, 2); err != nil {
		t.Fatalf("after one left queued: %v", err)
	}
	// total <= 0 is no limit.
	for i := 0; i < 5; i++ {
		if _, err := s.CreateTaskLimited(ctx, task, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.CountQueued(ctx); n != 7 {
		t.Fatalf("queued %d, want 7", n)
	}
}

// Count and insert are one transaction: concurrent callers never overshoot.
func TestCreateTaskLimitedConcurrent(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	task := Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"}
	const limit = 3
	for round := 0; round < 10; round++ {
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for i := 0; i < limit+10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.CreateTaskLimited(ctx, task, 0, limit)
				if err != nil && !errors.Is(err, ErrQueueFullTotal) {
					t.Errorf("unexpected error: %v", err)
				}
				if err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		n, err := s.CountQueued(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != limit || ok != limit {
			t.Fatalf("round %d: queued %d, created %d, want %d", round, n, ok, limit)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET state='cancelled' WHERE state='queued'`); err != nil {
			t.Fatal(err)
		}
	}
}

func actorCtx(a string) context.Context { return WithActor(context.Background(), a) }

// The per-actor limit counts only the actor's own queued tasks (earliest
// created event); the other actor, the total check and a freed slot behave
// independently.
func TestCreateTaskLimitedPerActor(t *testing.T) {
	s, _ := openTemp(t)
	task := Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"}
	a, b := actorCtx("session:alice"), actorCtx("token:mac")
	var first int64
	for i := 0; i < 2; i++ {
		id, err := s.CreateTaskLimited(a, task, 2, 100)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
	}
	if _, err := s.CreateTaskLimited(a, task, 2, 100); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("alice third: %v, want ErrQueueFull", err)
	}
	if _, err := s.CreateTaskLimited(b, task, 2, 100); err != nil {
		t.Fatalf("bob: %v", err)
	}
	// perActor <= 0 skips the check.
	if _, err := s.CreateTaskLimited(a, task, 0, 100); err != nil {
		t.Fatalf("perActor 0: %v", err)
	}
	// A task that left queued frees one slot for its actor only.
	if err := s.Transition(a, first, StateQueued, StatePreparing, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(a, task, 2, 100); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("alice still has 2 queued (one via perActor 0): %v", err)
	}
	// system / invalid actors are never limited per actor.
	for _, x := range []context.Context{context.Background(), actorCtx("bad actor!")} {
		for i := 0; i < 4; i++ {
			if _, err := s.CreateTaskLimited(x, task, 1, 100); err != nil {
				t.Fatalf("system/invalid submit %d: %v", i, err)
			}
		}
	}
}

// Total is checked after the per-actor limit; both are refused with their own
// sentinel.
func TestCreateTaskLimitedTotalAndOrder(t *testing.T) {
	s, _ := openTemp(t)
	task := Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"}
	for _, a := range []string{"session:a", "session:b", "session:c"} {
		if _, err := s.CreateTaskLimited(actorCtx(a), task, 5, 3); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateTaskLimited(actorCtx("session:d"), task, 5, 3); !errors.Is(err, ErrQueueFullTotal) {
		t.Fatalf("total: %v", err)
	}
	// When both would refuse, the per-actor sentinel wins.
	if _, err := s.CreateTaskLimited(actorCtx("session:a"), task, 1, 3); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("both: %v", err)
	}
}

// Tasks whose creating event has no actor (made before schema v7) and tasks
// made by "system" count toward the total only.
func TestCreateTaskLimitedLegacyAndSystemCountTotalOnly(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	task := Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"}
	for i := 0; i < 3; i++ {
		id, err := s.CreateTaskLimited(ctx, task, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 { // legacy: NULL actor
			if _, err := s.db.Exec(`UPDATE task_events SET actor = NULL WHERE task_id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.CreateTaskLimited(actorCtx("session:a"), task, 1, 100); err != nil {
		t.Fatalf("legacy/system tasks counted for an actor: %v", err)
	}
	if _, err := s.CreateTaskLimited(actorCtx("session:b"), task, 1, 4); !errors.Is(err, ErrQueueFullTotal) {
		t.Fatalf("legacy/system tasks must count toward the total: %v", err)
	}
}
