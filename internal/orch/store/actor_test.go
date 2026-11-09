package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A version-6 database keeps its tasks, events and approvals when opened by
// this build; the events written before v7 have no actor (NULL).
func TestMigrateV6ToV7KeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (6)`,
		`INSERT INTO tasks(repo, base, prompt, state, created_at, updated_at) VALUES ('/r','b','keepme','needs_approval',1,2)`,
		`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (1,'','queued','created',1)`,
		`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (1,'queued','needs_approval','old',2)`,
		`INSERT INTO approvals_pending(task_id, kind, hash, base, head, reason, created_at, status) VALUES (1,'protected_modified','h','b','hd','why',1,'pending')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	got, err := s.GetTask(ctx, 1)
	if err != nil || got.Prompt != "keepme" || got.LastActor != "" {
		t.Fatalf("task: %+v %v", got, err)
	}
	if a, err := s.GetApproval(ctx, 1); err != nil || a.Hash != "h" || a.Status != ApprovalPending {
		t.Fatalf("approval: %+v %v", a, err)
	}
	evs, err := s.Events(ctx, 1)
	if err != nil || len(evs) != 2 || evs[1].Note != "old" {
		t.Fatalf("events: %+v %v", evs, err)
	}
	for _, e := range evs {
		if e.Actor != "" {
			t.Fatalf("old event got actor %q", e.Actor)
		}
	}
	var nulls int
	if err := s.db.QueryRow(`SELECT count(*) FROM task_events WHERE actor IS NULL`).Scan(&nulls); err != nil || nulls != 2 {
		t.Fatalf("NULL actors %d %v", nulls, err)
	}
	var v int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 7 {
		t.Fatalf("version %d", v)
	}
	// A new event on the migrated database records its actor; the old ones stay NULL.
	if err := s.Transition(WithActor(ctx, "socket:uid:1000"), 1, StateNeedsApproval, StateCancelled, "cancelled"); err != nil {
		t.Fatal(err)
	}
	evs, _ = s.Events(ctx, 1)
	if len(evs) != 3 || evs[0].Actor != "" || evs[2].Actor != "socket:uid:1000" {
		t.Fatalf("events after: %+v", evs)
	}
}

// lastActor is the actor of a task's latest event.
func lastActor(t *testing.T, s *Store, id int64) string {
	t.Helper()
	evs, err := s.Events(context.Background(), id)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	return evs[len(evs)-1].Actor
}

// Submit, transition, retry and cancel each record the actor from the context.
func TestEventsRecordActorFromContext(t *testing.T) {
	s, _ := openTemp(t)
	bg := context.Background()
	sub := WithActor(bg, "session:mac-passkey")
	id, err := s.CreateTask(sub, Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id); a != "session:mac-passkey" {
		t.Fatalf("submit actor %q", a)
	}
	if tk, _ := s.GetTask(bg, id); tk.LastActor != "session:mac-passkey" {
		t.Fatalf("task LastActor %q", tk.LastActor)
	}

	// transition (the worker has no actor: system)
	if err := s.Transition(bg, id, StateQueued, StatePreparing, "started"); err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id); a != ActorSystem {
		t.Fatalf("worker transition actor %q", a)
	}
	if err := s.TransitionWith(WithActor(bg, "token:mac-ssh"), id, StatePreparing, StateFailed, "boom",
		TransitionOpts{InfraFailed: true, RetryState: StatePreparing, ErrorText: "boom"}); err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id); a != "token:mac-ssh" {
		t.Fatalf("transition actor %q", a)
	}

	// retry
	if err := s.RetryFailed(WithActor(bg, "socket:uid:1000"), id, StatePreparing, 3, "retry"); err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id); a != "socket:uid:1000" {
		t.Fatalf("retry actor %q", a)
	}

	// cancel
	if err := s.Transition(WithActor(bg, "session:alice"), id, StatePreparing, StateCancelled, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id); a != "session:alice" {
		t.Fatalf("cancel actor %q", a)
	}
	if tk, _ := s.GetTask(bg, id); tk.LastActor != "session:alice" {
		t.Fatalf("task LastActor %q", tk.LastActor)
	}

	// approval resolution moves the task with the resolver's actor
	id2 := newTask(t, s)
	if err := s.Transition(bg, id2, StateQueued, StateNeedsApproval, "park"); err != nil {
		t.Fatal(err)
	}
	aid, err := s.AddApproval(bg, Approval{TaskID: id2, Kind: KindProtectedModified, Hash: "h", Base: "abc", Head: "hd", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, moved, err := s.ResolveAndAdvance(WithActor(bg, "session:bob"), aid, ApprovalApproved,
		StateVerifying, StateFailed, "approved", TransitionOpts{}); err != nil || !moved {
		t.Fatalf("resolve %v %v", moved, err)
	}
	if a := lastActor(t, s, id2); a != "session:bob" {
		t.Fatalf("approve actor %q", a)
	}

	// crash recovery is the system, whatever the caller's context says
	if _, ok, err := s.NextRunnable(bg); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if _, err := s.ReleaseClaims(WithActor(bg, "session:mallory")); err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id2); a != ActorSystem {
		t.Fatalf("recovery actor %q", a)
	}
}

func TestActorDefaultsToSystem(t *testing.T) {
	if a := ActorFrom(context.Background()); a != "system" {
		t.Fatalf("ActorFrom(empty ctx) = %q", a)
	}
	s, _ := openTemp(t)
	id := newTask(t, s)
	if a := lastActor(t, s, id); a != "system" {
		t.Fatalf("event actor %q", a)
	}
}

func TestActorFormat(t *testing.T) {
	ok := []string{"system", "socket:uid:1000", "session:mac-passkey", "token:mac-ssh",
		"session:a.b_c@d-e", "token:" + strings.Repeat("x", 74)} // 80 bytes
	for _, a := range ok {
		if got := ActorFrom(WithActor(context.Background(), a)); got != a {
			t.Errorf("valid %q stored as %q", a, got)
		}
	}
	bad := []string{"", "alice", ":alice", "session:", "session:al ice", "session:al/ice",
		"session:é", "session:a\nb", "session:a\x00b", "token:" + strings.Repeat("x", 75), // 81 bytes
		"session:<script>"}
	for _, a := range bad {
		if got := ActorFrom(WithActor(context.Background(), a)); got != "invalid" {
			t.Errorf("invalid %q stored as %q", a, got)
		}
	}
	// An invalid actor never makes the write fail; the event says "invalid".
	s, _ := openTemp(t)
	id, err := s.CreateTask(WithActor(context.Background(), "session:"+strings.Repeat("y", 500)),
		Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if a := lastActor(t, s, id); a != "invalid" {
		t.Fatalf("event actor %q", a)
	}
}
