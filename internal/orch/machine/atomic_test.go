package machine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"aigate/internal/orch/store"

	_ "modernc.org/sqlite"
)

// Approve and Reject are one transaction: when the task transition fails
// (here: a trigger aborts the event insert), the approval entry is still
// pending, so the task is never left waiting with nothing to approve.
func TestApproveRejectAreAtomic(t *testing.T) {
	for _, tc := range []struct {
		name, to string
		act      func(m *Machine, id int64) error
	}{
		{"approve", "verifying", func(m *Machine, id int64) error { return m.Approve(context.Background(), id) }},
		{"reject", "failed", func(m *Machine, id int64) error { return m.Reject(context.Background(), id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := happySteps()
			steps[3] = fakeStep{StateVerifying, next(StateNeedsApproval)}
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "o.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			m := New(st, steps)
			ctx := context.Background()
			id := create(t, st)
			drain(t, m)
			aid, err := st.AddApproval(ctx, store.Approval{TaskID: id, Kind: store.KindProtectedModified, Hash: "h1"})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "o.db")+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER inject BEFORE INSERT ON task_events WHEN NEW.to_state='` + tc.to + `'
				BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := tc.act(m, aid); err == nil {
				t.Fatal("expected the injected failure")
			}
			a, _ := st.GetApproval(ctx, aid)
			if a.Status != store.ApprovalPending || state(t, st, id) != StateNeedsApproval {
				t.Fatalf("entry %s, task %s: not atomic", a.Status, state(t, st, id))
			}
			if _, err := raw.Exec(`DROP TRIGGER inject`); err != nil {
				t.Fatal(err)
			}
			if err := tc.act(m, aid); err != nil {
				t.Fatal(err) // still approvable after the failure
			}
			if got := string(state(t, st, id)); got != tc.to {
				t.Fatalf("task %s, want %s", got, tc.to)
			}
		})
	}
}
