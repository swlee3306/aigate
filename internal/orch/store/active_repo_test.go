package store

import (
	"context"
	"testing"
)

func TestCountActiveByRepo(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mk := func(repo string, to State) int64 {
		id, err := s.CreateTask(ctx, Task{Repo: repo, Base: "b", Prompt: "p", Class: "docs", Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if to != StateQueued {
			if err := s.Transition(ctx, id, StateQueued, to, "t"); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	count := func(repo string) int {
		n, err := s.CountActiveByRepo(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, st := range []State{StateQueued, StatePreparing, StateAgentRunning, StateCollecting, StateVerifying, StateNeedsApproval, StatePassed, StatePublishing, StateError} {
		if count("/r/app") != 0 {
			t.Fatal("count not zero")
		}
		id := mk("/r/app", st)
		if count("/r/app") != 1 {
			t.Fatalf("state %s is not counted as active", st)
		}
		if err := s.Transition(ctx, id, st, StateCancelled, "t"); err != nil {
			t.Fatal(err)
		}
	}
	mk("/r/app", StateDone)
	mk("/r/app", StateFailed)
	mk("/r/other", StateQueued)
	mk("/r/app2", StateQueued) // a sibling whose name starts the same
	mk("/r/ap_p", StateQueued) // LIKE wildcard look-alike
	mk("/r/a%p", StateQueued)  // LIKE wildcard look-alike
	if count("/r/app") != 0 {
		t.Fatalf("final, foreign and look-alike tasks counted: %d", count("/r/app"))
	}
	mk("/r/app/sub", StateQueued)
	if count("/r/app") != 1 {
		t.Fatal("a task below the project path is not counted")
	}
	if count("/r/a_p") != 0 || count("/r/a%") != 0 {
		t.Fatal("LIKE wildcards in the path were interpreted")
	}
}
