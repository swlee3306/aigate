package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func newTask(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.CreateTask(context.Background(), Task{Repo: "r", Base: "abc", Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTransitionRequiresExpectedState(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	err := s.Transition(ctx, id, StateVerifying, StatePassed, "wrong from")
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("want ErrStateMismatch, got %v", err)
	}
	got, _ := s.GetTask(ctx, id)
	if got.State != StateQueued {
		t.Fatalf("state changed to %s", got.State)
	}
	if err := s.Transition(ctx, id, StateQueued, StatePreparing, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, 9999, StateQueued, StatePreparing, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTransitionIsAtomicWithEvent(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	// Make the event insert fail; the state update must roll back with it.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON task_events
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, id, StateQueued, StatePreparing, "x"); err == nil {
		t.Fatal("expected failure")
	}
	got, _ := s.GetTask(ctx, id)
	if got.State != StateQueued {
		t.Fatalf("state leaked: %s", got.State)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_event`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, id, StateQueued, StatePreparing, "x"); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Events(ctx, id)
	if len(evs) != 2 || evs[1].From != StateQueued || evs[1].To != StatePreparing || evs[1].Note != "x" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestConcurrentTransitionsOneWins(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	// A second handle on the same file makes this a real multi-connection race.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	const n = 24
	var wins, losses atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := s
			if i%2 == 1 {
				st = s2
			}
			<-start
			err := st.Transition(ctx, id, StateQueued, StatePreparing, "race")
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrStateMismatch):
				losses.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || losses.Load() != n-1 {
		t.Fatalf("wins=%d losses=%d", wins.Load(), losses.Load())
	}
	evs, _ := s.Events(ctx, id)
	if len(evs) != 2 { // created + the one winner
		t.Fatalf("events: %+v", evs)
	}
}

func TestReopenKeepsState(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	if err := s.Transition(ctx, id, StateQueued, StatePreparing, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddApproval(ctx, Approval{TaskID: id, Kind: KindProtectedModified, Hash: "h"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetTask(ctx, id)
	if err != nil || got.State != StatePreparing || got.Prompt != "p" {
		t.Fatalf("got %+v err %v", got, err)
	}
	ap, _ := s2.PendingApprovals(ctx)
	if len(ap) != 1 {
		t.Fatalf("approvals not persisted: %+v", ap)
	}
}

func TestIdempotentApprovalPerTaskHash(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	a := Approval{TaskID: id, Kind: KindProtectedModified, Hash: "h1", Reason: "first"}
	id1, err := s.AddApproval(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	a.Reason = "second"
	id2, err := s.AddApproval(ctx, a)
	if err != nil || id2 != id1 {
		t.Fatalf("id1=%d id2=%d err=%v", id1, id2, err)
	}
	// Concurrent duplicates also collapse.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.AddApproval(ctx, a); err != nil || got != id1 {
				t.Errorf("got %d err %v", got, err)
			}
		}()
	}
	wg.Wait()
	// A repeat refreshes the reason of the pending entry and nothing else.
	if ap, _ := s.PendingApprovals(ctx); len(ap) != 1 || ap[0].Reason != "second" {
		t.Fatalf("pending after repeats: %+v", ap)
	}
	// A different hash is a new entry and supersedes the old one.
	if idN, _ := s.AddApproval(ctx, Approval{TaskID: id, Kind: KindProtectedModified, Hash: "h2"}); idN == id1 {
		t.Fatal("different hash must create a new entry")
	}
	ap, _ := s.PendingApprovals(ctx)
	if len(ap) != 1 || ap[0].Hash != "h2" {
		t.Fatalf("pending: %+v", ap)
	}
	if old, _ := s.GetApproval(ctx, id1); old.Status != ApprovalSuperseded {
		t.Fatalf("old entry: %+v", old)
	}
}

func TestDestructiveBlockedNeverApprovable(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	aid, err := s.AddApproval(ctx, Approval{TaskID: id, Kind: KindDestructiveBlocked, Hash: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveApproval(ctx, aid); !errors.Is(err, ErrNotApprovable) {
		t.Fatalf("want ErrNotApprovable, got %v", err)
	}
	// The schema also refuses it, independent of the Go check.
	if _, err := s.db.Exec(`UPDATE approvals_pending SET status='approved' WHERE id=?`, aid); err == nil {
		t.Fatal("schema allowed approving destructive_blocked")
	}
	if _, err := s.AddApproval(ctx, Approval{TaskID: id, Kind: "bogus", Hash: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestApproveAndForeignKey(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if _, err := s.AddApproval(ctx, Approval{TaskID: 12345, Kind: KindProtectedModified, Hash: "h"}); err == nil {
		t.Fatal("foreign key not enforced")
	}
	id := newTask(t, s)
	aid, _ := s.AddApproval(ctx, Approval{TaskID: id, Kind: KindPublishOutsideAllowlist, Hash: "h"})
	if err := s.ApproveApproval(ctx, aid); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveApproval(ctx, aid); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second approve: %v", err)
	}
	if ap, _ := s.PendingApprovals(ctx); len(ap) != 0 {
		t.Fatalf("still pending: %+v", ap)
	}
}

func TestNextRunnableClaimsOnce(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		newTask(t, s)
	}
	var claimed sync.Map
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, ok, err := s.NextRunnable(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				if _, dup := claimed.LoadOrStore(tk.ID, true); dup {
					t.Errorf("task %d claimed twice", tk.ID)
				}
			}
		}()
	}
	wg.Wait()
	n := 0
	claimed.Range(func(_, _ any) bool { n++; return true })
	if n != 5 {
		t.Fatalf("claimed %d tasks, want 5", n)
	}
	if _, ok, _ := s.NextRunnable(ctx); ok {
		t.Fatal("nothing should be left")
	}
}

func TestNextRunnableSkipsNonRunnable(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	a, b, c := newTask(t, s), newTask(t, s), newTask(t, s)
	s.Transition(ctx, a, StateQueued, StateNeedsApproval, "")
	s.Transition(ctx, b, StateQueued, StateDone, "")
	tk, ok, err := s.NextRunnable(ctx)
	if err != nil || !ok || tk.ID != c {
		t.Fatalf("got %+v ok=%v err=%v", tk, ok, err)
	}
}

func TestFilePermissionsAndPragmas(t *testing.T) {
	s, path := openTemp(t)
	newTask(t, s)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // -wal/-shm may not exist yet on some builds
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", p, fi.Mode().Perm())
		}
	}
	var mode string
	s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode=%s", mode)
	}
	var fk int
	s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Errorf("foreign_keys=%d", fk)
	}
	var v int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("schema_version=%d", v)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	s, path := openTemp(t)
	s.db.Exec(`UPDATE schema_version SET version=999`)
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected error for newer schema")
	}
}

func TestErrorTextCapped(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := newTask(t, s)
	big := make([]byte, 100000)
	for i := range big {
		big[i] = 'x'
	}
	err := s.TransitionWith(ctx, id, StateQueued, StateError, "boom", TransitionOpts{ErrorText: string(big), RetryState: StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, id)
	if len(got.ErrorText) > MaxErrorText+16 || got.Attempts != 1 || got.RetryState != StateQueued {
		t.Fatalf("len=%d attempts=%d retry=%s", len(got.ErrorText), got.Attempts, got.RetryState)
	}
}

func TestListTasksNewestFirst(t *testing.T) {
	s, _ := openTemp(t)
	a, b := newTask(t, s), newTask(t, s)
	got, err := s.ListTasks(context.Background(), 0)
	if err != nil || len(got) != 2 || got[0].ID != b || got[1].ID != a {
		t.Fatalf("got %v %v", got, err)
	}
	one, _ := s.ListTasks(context.Background(), 1)
	if len(one) != 1 {
		t.Fatalf("limit ignored: %d", len(one))
	}
}

// A database made by schema version 1 keeps its tasks when opened by this build.
func TestMigrateV1ToV2KeepsTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (1)`,
		`INSERT INTO tasks(repo, base, prompt, class, agent, state, created_at, updated_at)
		 VALUES ('/r','abc','old prompt','docs','claude','verifying',1,2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetTask(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "old prompt" || got.State != StateVerifying || got.HeadSHA != "" || got.Verdict != "" || got.ReportPath != "" {
		t.Fatalf("task after migration: %+v", got)
	}
	var v int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 3 {
		t.Fatalf("version %d", v)
	}
	if err := s.SetHeadSHA(context.Background(), 1, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVerdict(context.Background(), 1, "ok", "/p.json"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(context.Background(), 1)
	if got.HeadSHA != "deadbeef" || got.Verdict != "ok" || got.ReportPath != "/p.json" {
		t.Fatalf("%+v", got)
	}
	if err := s.SetHeadSHA(context.Background(), 99, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// A version-2 database keeps its approval rows (ids included) through the v3
// rebuild and then accepts the superseded status.
func TestMigrateV2ToV3KeepsApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (2)`,
		`INSERT INTO tasks(repo, base, prompt, class, agent, state, created_at, updated_at)
		 VALUES ('/r','abc','p','docs','claude','needs_approval',1,2)`,
		`INSERT INTO approvals_pending(id, task_id, kind, hash, base, head, reason, created_at, status)
		 VALUES (7, 1, 'protected_modified', 'old', 'b', 'h', 'why', 5, 'pending')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	got, err := s.GetApproval(ctx, 7)
	if err != nil || got.Hash != "old" || got.Status != ApprovalPending || got.Reason != "why" {
		t.Fatalf("%+v %v", got, err)
	}
	id, err := s.AddApproval(ctx, Approval{TaskID: 1, Kind: KindProtectedModified, Hash: "new", Head: "h2"})
	if err != nil || id <= 7 {
		t.Fatalf("id %d err %v", id, err)
	}
	if old, _ := s.GetApproval(ctx, 7); old.Status != ApprovalSuperseded {
		t.Fatalf("old %+v", old)
	}
	if err := s.ApproveApproval(ctx, 7); !errors.Is(err, ErrNotPending) {
		t.Fatalf("superseded must not be approvable: %v", err)
	}
}
