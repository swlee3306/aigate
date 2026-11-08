package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	if v != 6 {
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

// A version-3 database keeps its tasks through the v4 migration; old tasks have
// no policy hash, and the hash can then be recorded.
func TestMigrateV3ToV4KeepsTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, schemaV3,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (3)`,
		`INSERT INTO tasks(repo, base, prompt, class, agent, state, created_at, updated_at)
		 VALUES ('/r','abc','old','docs','claude','queued',1,2)`,
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
	got, err := s.GetTask(ctx, 1)
	if err != nil || got.Prompt != "old" || got.PolicyHash != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.SetPolicyHash(ctx, 1, "abc123"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetTask(ctx, 1); got.PolicyHash != "abc123" {
		t.Fatalf("%+v", got)
	}
}

// Regression (fault injection): deleting the database file while the store is
// open must not leave a worker running tasks on the unlinked inode.
func TestDeletedOrReplacedDBFileIsReported(t *testing.T) {
	for _, replace := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "o.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateTask(context.Background(), Task{Repo: "r", Base: "b", Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suf)
		}
		if replace {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.NextRunnable(context.Background()); !errors.Is(err, ErrDBGone) {
			t.Fatalf("replace=%v NextRunnable: %v", replace, err)
		}
		if err := s.Transition(context.Background(), id, StateQueued, StatePreparing, "x"); !errors.Is(err, ErrDBGone) {
			t.Fatalf("replace=%v Transition: %v", replace, err)
		}
		s.Close()
	}
}

func TestAddApprovalResolvedIsADeadEnd(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id, _ := s.CreateTask(ctx, Task{Repo: "/r", Base: "b", Prompt: "p"})
	aid, err := s.AddApproval(ctx, Approval{TaskID: id, Kind: KindProtectedModified, Hash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveApproval(ctx, aid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddApproval(ctx, Approval{TaskID: id, Kind: KindProtectedModified, Hash: "h"}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("err %v", err)
	}
}

// A version-4 database keeps its tasks through v5; the backfill marks only old
// failed tasks whose last failure note is "<step> failed after N attempts".
func TestMigrateV4ToV5BackfillsInfraFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{schemaV1, schemaV2, schemaV3, schemaV4,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (4)`}
	// id, state, error_text, last event (from, to, note)
	type row struct{ state, errText, from, note string }
	rows := []row{
		{"failed", "restore quota 413", "verifying", "verifying failed after 3 attempts: restore quota 413"}, // 1 infra
		{"failed", "", "verifying", "behavior_failure"},                                                      // 2 verdict
		{"failed", "approval rejected: x", "needs_approval", "approval rejected"},                            // 3
		{"failed", "step repeatedly interrupted by a crash", "collecting", "collecting interrupted 3 times"}, // 4
		{"failed", "", "collecting", "collecting failed after 3 attempts: x"},                                // 5 empty error_text
		{"done", "old error", "publishing", "publishing failed after 3 attempts: x"},                         // 6 not failed
		{"failed", "x", "preparing", "verifying failed after 3 attempts: x"},                                 // 7 step mismatch
		{"failed", "push boom", "publishing", "publishing failed after 3 attempts: push boom"},               // 8 infra (publishing, refused later)
	}
	for i, r := range rows {
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO tasks(repo, base, prompt, state, error_text, created_at, updated_at) VALUES ('/r','b','p%d','%s','%s',1,2)`, i+1, r.state, r.errText),
			fmt.Sprintf(`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (%d,'%s','%s','%s',3)`, i+1, r.from, r.state, r.note))
	}
	for _, q := range stmts {
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
	want := map[int64]State{1: StateVerifying, 8: StatePublishing}
	for i := range rows {
		id := int64(i + 1)
		got, err := s.GetTask(ctx, id)
		if err != nil || got.Prompt != fmt.Sprintf("p%d", id) {
			t.Fatalf("task %d: %+v %v", id, got, err)
		}
		step, infra := want[id]
		if got.InfraFailed != (infra != false) || (infra && got.RetryState != step) || (!infra && got.RetryState != "") {
			t.Errorf("task %d: infra=%v retry_state=%q", id, got.InfraFailed, got.RetryState)
		}
		if got.Retries != 0 {
			t.Errorf("task %d retries %d", id, got.Retries)
		}
	}
	var v int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 6 {
		t.Fatalf("version %d", v)
	}
}

func TestEnsureMinNextTaskID(t *testing.T) {
	ctx := context.Background()
	t.Run("empty db without sequence row", func(t *testing.T) {
		s, _ := openTemp(t)
		for _, n := range []int64{-5, 0, 1} { // no-ops
			if err := s.EnsureMinNextTaskID(ctx, n); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.EnsureMinNextTaskID(ctx, 21); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureMinNextTaskID(ctx, 21); err != nil { // idempotent
			t.Fatal(err)
		}
		if id := newTask(t, s); id != 21 {
			t.Fatalf("first id %d, want 21", id)
		}
		if id := newTask(t, s); id != 22 {
			t.Fatalf("second id %d, want 22", id)
		}
	})
	t.Run("never lowers", func(t *testing.T) {
		s, _ := openTemp(t)
		for i := 0; i < 5; i++ {
			newTask(t, s)
		}
		if err := s.EnsureMinNextTaskID(ctx, 3); err != nil {
			t.Fatal(err)
		}
		if id := newTask(t, s); id != 6 {
			t.Fatalf("id %d, want 6", id)
		}
	})
	t.Run("raises a db with tasks", func(t *testing.T) {
		s, _ := openTemp(t)
		newTask(t, s)
		newTask(t, s)
		if err := s.EnsureMinNextTaskID(ctx, 50); err != nil {
			t.Fatal(err)
		}
		if id := newTask(t, s); id != 50 {
			t.Fatalf("id %d, want 50", id)
		}
	})
	t.Run("deleted ids", func(t *testing.T) {
		s, path := openTemp(t)
		for i := 0; i < 3; i++ {
			newTask(t, s)
		}
		s.Close()
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`DELETE FROM task_events`, `DELETE FROM tasks`} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
		s2, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		if err := s2.EnsureMinNextTaskID(ctx, 2); err != nil { // below the old sequence: no change
			t.Fatal(err)
		}
		if id := newTask(t, s2); id != 4 {
			t.Fatalf("id %d, want 4 (sequence survives deletes)", id)
		}
		if err := s2.EnsureMinNextTaskID(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if seq, err := s2.TaskSeq(ctx); err != nil || seq != 9 {
			t.Fatalf("seq %d err %v, want 9", seq, err)
		}
	})
}
