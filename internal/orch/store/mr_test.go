package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A version-7 database keeps its tasks, events and approvals when opened by
// this build; the new merge request columns are NULL (empty in Task).
func TestMigrateV7ToV8KeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (7)`,
		`INSERT INTO tasks(repo, base, prompt, state, created_at, updated_at) VALUES ('/r','b','keepme','done',1,2)`,
		`INSERT INTO task_events(task_id, from_state, to_state, note, at, actor) VALUES (1,'','queued','created',1,'socket:uid:1')`,
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
	if err != nil || got.Prompt != "keepme" || got.State != StateDone || got.LastActor != "socket:uid:1" {
		t.Fatalf("task: %+v %v", got, err)
	}
	if got.Branch != "" || got.MRIID != 0 || got.MRURL != "" || got.MRState != "" || got.MRCode != "" {
		t.Fatalf("new columns not empty: %+v", got)
	}
	var nulls int
	if err := s.db.QueryRow(`SELECT count(*) FROM tasks WHERE branch IS NULL AND mr_iid IS NULL AND mr_url IS NULL AND mr_state IS NULL AND mr_code IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("NULL columns %d %v", nulls, err)
	}
	if a, err := s.GetApproval(ctx, 1); err != nil || a.Hash != "h" {
		t.Fatalf("approval: %+v %v", a, err)
	}
	if evs, err := s.Events(ctx, 1); err != nil || len(evs) != 1 {
		t.Fatalf("events: %+v %v", evs, err)
	}
	var v int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if v != 8 {
		t.Fatalf("version %d", v)
	}
	// The column set works after the migration.
	if err := s.SetMR(WithActor(ctx, "socket:uid:7"), 1, "orch/1", 5, "https://g/x/-/merge_requests/5", MRCreated, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, 1)
	if got.MRState != MRCreated || got.MRIID != 5 || got.Branch != "orch/1" || got.LastActor != "socket:uid:7" {
		t.Fatalf("after SetMR: %+v", got)
	}
}

func TestSetMRRules(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id, _ := s.CreateTask(ctx, Task{Repo: "/r", Base: "b", Prompt: "p"})
	if err := s.SetMR(ctx, id, "orch/1", 0, "", MRSkipped, ""); err != ErrStateMismatch {
		t.Fatalf("not done: %v", err) // only a done task has an MR outcome
	}
	s.db.Exec(`UPDATE tasks SET state='done' WHERE id=?`, id)
	if err := s.SetMR(ctx, id, "", 0, "", MRFailed, "Bad Code"); err == nil {
		t.Fatal("invalid code accepted")
	}
	if err := s.SetMRFrom(ctx, id, []string{""}, "orch/1", 0, "", MRFailed, "upstream_error"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMRFrom(ctx, id, []string{""}, "orch/1", 3, "http://g/m/3", MRCreated, ""); err != ErrStateMismatch {
		t.Fatalf("second claim from empty: %v", err)
	}
	if err := s.SetMR(ctx, id, "orch/1", 3, "http://g/m/3", MRCreated, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMR(ctx, id, "orch/1", 4, "http://g/m/4", MRCreated, ""); err != ErrStateMismatch {
		t.Fatalf("created must never be overwritten: %v", err)
	}
	evs, _ := s.Events(ctx, id)
	if n := len(evs); n != 3 || evs[1].Note != "mr_failed: upstream_error" || evs[2].Note != "mr_created" || evs[2].Actor != ActorSystem {
		t.Fatalf("events %+v", evs)
	}
}
