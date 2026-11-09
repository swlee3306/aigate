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
	a, err := s.CreateTaskLimited(ctx, task, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(ctx, task, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(ctx, task, 2); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third: %v, want ErrQueueFull", err)
	}
	if n, err := s.CountQueued(ctx); err != nil || n != 2 {
		t.Fatalf("queued %d %v", n, err)
	}
	// A task that left queued no longer counts.
	if err := s.Transition(ctx, a, StateQueued, StatePreparing, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskLimited(ctx, task, 2); err != nil {
		t.Fatalf("after one left queued: %v", err)
	}
	// maxQueued <= 0 is no limit.
	for i := 0; i < 5; i++ {
		if _, err := s.CreateTaskLimited(ctx, task, 0); err != nil {
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
				_, err := s.CreateTaskLimited(ctx, task, limit)
				if err != nil && !errors.Is(err, ErrQueueFull) {
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
