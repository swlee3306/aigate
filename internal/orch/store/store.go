// Package store keeps orchestrator state in one SQLite file (WAL). Every state
// change is a compare-and-set on the current state and is recorded in
// task_events in the same transaction.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaV1 string

//go:embed schema_v2.sql
var schemaV2 string

//go:embed schema_v3.sql
var schemaV3 string

//go:embed schema_v4.sql
var schemaV4 string

// MaxErrorText caps Task.ErrorText so a noisy failure cannot bloat the DB.
const MaxErrorText = 4096

var (
	ErrNotFound      = errors.New("store: not found")
	ErrStateMismatch = errors.New("store: task is not in the expected state")
	ErrNotApprovable = errors.New("store: this approval kind is record-only")
	ErrNotPending    = errors.New("store: approval is not pending")
)

// Task is one unit of work submitted to the orchestrator.
type Task struct {
	ID         int64
	Repo       string
	Base       string
	Prompt     string
	Class      string
	Agent      string
	State      State
	Attempts   int   // failed attempts of the current step
	RetryState State // step to resume when State == StateError
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ErrorText  string
	HeadSHA    string // the agent's head commit, set by the collecting step
	Verdict    string // gate category of the last verification
	ReportPath string // saved report JSON of the last verification
	// PolicyHash is the hash of the policy.yml the task started under.
	PolicyHash string
}

// Approval is a pending-approvals entry (design section 6).
type Approval struct {
	ID        int64
	TaskID    int64
	Kind      string
	Hash      string
	Base      string
	Head      string
	Reason    string
	CreatedAt time.Time
	Status    string
}

// Event is one recorded transition.
type Event struct {
	ID     int64
	TaskID int64
	From   State
	To     State
	Note   string
	At     time.Time
}

// TransitionOpts carries the optional extras of a transition.
type TransitionOpts struct {
	ErrorText  string // recorded when moving to StateError or StateFailed
	RetryState State  // step to resume, recorded when moving to StateError
}

// Store is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// migrations[i] upgrades schema version i to i+1.
var migrations = []func(tx *sql.Tx) error{
	func(tx *sql.Tx) error { _, err := tx.Exec(schemaV1); return err },
	func(tx *sql.Tx) error { _, err := tx.Exec(schemaV2); return err },
	func(tx *sql.Tx) error { _, err := tx.Exec(schemaV3); return err },
	func(tx *sql.Tx) error { _, err := tx.Exec(schemaV4); return err },
}

// Open creates or opens the database at path with mode 0600.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file ourselves so SQLite never makes it with a looser umask;
	// SQLite gives -wal/-shm the mode of the main file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			_ = os.Chmod(path+suffix, 0o600)
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var cur int
	err = tx.QueryRow(`SELECT version FROM schema_version`).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if cur > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this build (%d)", cur, len(migrations))
	}
	for v := cur; v < len(migrations); v++ {
		if err := migrations[v](tx); err != nil {
			return fmt.Errorf("store: migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, v+1); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func now() int64 { return time.Now().UTC().UnixNano() }

func ts(n int64) time.Time { return time.Unix(0, n).UTC() }

func capText(s string) string {
	if len(s) <= MaxErrorText {
		return s
	}
	s = s[:MaxErrorText]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…[cut]"
}

const taskCols = `id, repo, base, prompt, class, agent, state, attempts, retry_state, error_text, created_at, updated_at, head_sha, verdict, report_path, policy_hash`

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (Task, error) {
	var t Task
	var c, u int64
	err := r.Scan(&t.ID, &t.Repo, &t.Base, &t.Prompt, &t.Class, &t.Agent, &t.State,
		&t.Attempts, &t.RetryState, &t.ErrorText, &c, &u, &t.HeadSHA, &t.Verdict, &t.ReportPath, &t.PolicyHash)
	t.CreatedAt, t.UpdatedAt = ts(c), ts(u)
	return t, err
}

// CreateTask inserts t in StateQueued (State and timestamps are ignored).
func (s *Store) CreateTask(ctx context.Context, t Task) (int64, error) {
	if t.Repo == "" {
		return 0, errors.New("store: task needs a repo")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(repo, base, prompt, class, agent, state, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		t.Repo, t.Base, t.Prompt, t.Class, t.Agent, StateQueued, n, n)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (?,?,?,?,?)`,
		id, "", StateQueued, "created", n); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// GetTask returns one task or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

// ListTasks returns up to limit tasks, newest first (limit <= 0 means 100).
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskCols+` FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetPolicyHash records the hash of the policy the task runs under. It is
// idempotent and overwrites (a retried first step records the current hash).
func (s *Store) SetPolicyHash(ctx context.Context, id int64, hash string) error {
	return s.setCols(ctx, id, `policy_hash=?`, hash)
}

// SetHeadSHA records the agent's head commit. It is idempotent.
func (s *Store) SetHeadSHA(ctx context.Context, id int64, sha string) error {
	return s.setCols(ctx, id, `head_sha=?`, sha)
}

// SetVerdict records the gate category and the saved report path of the last
// verification. It is idempotent.
func (s *Store) SetVerdict(ctx context.Context, id int64, verdict, reportPath string) error {
	return s.setCols(ctx, id, `verdict=?, report_path=?`, verdict, reportPath)
}

func (s *Store) setCols(ctx context.Context, id int64, set string, args ...any) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET `+set+` WHERE id=?`, append(args, id)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Transition moves a task from `from` to `to` only if it is currently in
// `from`, and records the event in the same transaction. The rules about which
// pairs are legal belong to the caller (machine.Allowed).
func (s *Store) Transition(ctx context.Context, id int64, from, to State, note string) error {
	return s.TransitionWith(ctx, id, from, to, note, TransitionOpts{})
}

// TransitionWith is Transition with extras. Moving to StateError counts one
// failed attempt; leaving StateError keeps the count; any other move resets it.
// Every transition releases the worker claim.
func (s *Store) TransitionWith(ctx context.Context, id int64, from, to State, note string, o TransitionOpts) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n := now()
	var q string
	var args []any
	switch {
	case to == StateError:
		q = `UPDATE tasks SET state=?, attempts=attempts+1, retry_state=?, error_text=?, running=0, updated_at=?
		     WHERE id=? AND state=?`
		args = []any{to, o.RetryState, capText(o.ErrorText), n, id, from}
	case from == StateError:
		q = `UPDATE tasks SET state=?, retry_state='', running=0, updated_at=?, error_text=CASE WHEN ?<>'' THEN ? ELSE error_text END
		     WHERE id=? AND state=?`
		args = []any{to, n, o.ErrorText, capText(o.ErrorText), id, from}
	default:
		q = `UPDATE tasks SET state=?, attempts=0, retry_state='', running=0, updated_at=?, error_text=?
		     WHERE id=? AND state=?`
		args = []any{to, n, capText(o.ErrorText), id, from}
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		var cur State
		err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, id).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: task %d is %s, expected %s", ErrStateMismatch, id, cur, from)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (?,?,?,?,?)`,
		id, from, to, note, n); err != nil {
		return err
	}
	return tx.Commit()
}

// Events lists a task's transitions oldest first.
func (s *Store) Events(ctx context.Context, taskID int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, from_state, to_state, note, at FROM task_events WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.TaskID, &e.From, &e.To, &e.Note, &at); err != nil {
			return nil, err
		}
		e.At = ts(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// NextRunnable picks the oldest runnable task nobody holds and claims it
// atomically, so concurrent workers never get the same task. The claim is
// released by any transition, or by ReleaseClaims after a crash.
func (s *Store) NextRunnable(ctx context.Context) (Task, bool, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(runnable)), ",")
	args := make([]any, len(runnable))
	for i, st := range runnable {
		args[i] = st
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE running=0 AND state IN (`+ph+`) ORDER BY id LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET running=1 WHERE id=?`, t.ID); err != nil {
		return Task{}, false, err
	}
	return t, true, tx.Commit()
}

// ReleaseClaims is for process start: every task still claimed was in flight
// when the previous process died. It clears the claims, counts that as one
// failed attempt of the step, records an event (from == to), and returns the
// affected tasks as they are after the update.
func (s *Store) ReleaseClaims(ctx context.Context) ([]Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM tasks WHERE running=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type idState struct {
		id int64
		st State
	}
	var hit []idState
	for rows.Next() {
		var x idState
		if err := rows.Scan(&x.id, &x.st); err != nil {
			rows.Close()
			return nil, err
		}
		hit = append(hit, x)
	}
	rows.Close()
	n := now()
	var out []Task
	for _, x := range hit {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET running=0, attempts=attempts+1, updated_at=? WHERE id=?`, n, x.id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (?,?,?,?,?)`,
			x.id, x.st, x.st, "recovered: step was in flight when the process stopped", n); err != nil {
			return nil, err
		}
		t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, x.id))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, tx.Commit()
}

// AddApproval records a pending approval.
//   - Same (task_id, kind, hash): idempotent; the existing entry's id is
//     returned and, while it is pending, only its reason is refreshed. An
//     approved or rejected entry is left alone; a superseded one (the task
//     came back to this hash) becomes pending again.
//   - A new hash: the task's pending entries that this one replaces are marked
//     superseded (same kind with another hash, or any kind for another head).
//     A superseded entry can never be approved.
//
// Status and CreatedAt of the argument are ignored.
func (s *Store) AddApproval(ctx context.Context, a Approval) (int64, error) {
	switch a.Kind {
	case KindProtectedModified, KindPublishOutsideAllowlist, KindDestructiveBlocked:
	default:
		return 0, fmt.Errorf("store: unknown approval kind %q", a.Kind)
	}
	if a.Hash == "" {
		return 0, errors.New("store: approval needs a hash")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	var cur string
	err = tx.QueryRowContext(ctx,
		`SELECT id, status FROM approvals_pending WHERE task_id=? AND kind=? AND hash=?`,
		a.TaskID, a.Kind, a.Hash).Scan(&id, &cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO approvals_pending(task_id, kind, hash, base, head, reason, created_at, status)
			 VALUES (?,?,?,?,?,?,?,?)`,
			a.TaskID, a.Kind, a.Hash, a.Base, a.Head, a.Reason, now(), ApprovalPending)
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	case err != nil:
		return 0, err
	case cur == ApprovalPending:
		if _, err := tx.ExecContext(ctx, `UPDATE approvals_pending SET reason=? WHERE id=?`, a.Reason, id); err != nil {
			return 0, err
		}
	case cur == ApprovalSuperseded:
		if _, err := tx.ExecContext(ctx,
			`UPDATE approvals_pending SET status=?, reason=?, base=?, head=? WHERE id=?`,
			ApprovalPending, a.Reason, a.Base, a.Head, id); err != nil {
			return 0, err
		}
	default:
		return id, tx.Commit() // approved or rejected: history, not touched
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE approvals_pending SET status=? WHERE task_id=? AND status=? AND id<>?
		   AND ((kind=? AND hash<>?) OR head<>?)`,
		ApprovalSuperseded, a.TaskID, ApprovalPending, id, a.Kind, a.Hash, a.Head); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

const approvalCols = `id, task_id, kind, hash, base, head, reason, created_at, status`

func scanApproval(r scanner) (Approval, error) {
	var a Approval
	var c int64
	err := r.Scan(&a.ID, &a.TaskID, &a.Kind, &a.Hash, &a.Base, &a.Head, &a.Reason, &c, &a.Status)
	a.CreatedAt = ts(c)
	return a, err
}

// GetApproval returns one entry or ErrNotFound.
func (s *Store) GetApproval(ctx context.Context, id int64) (Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals_pending WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// PendingApprovals lists entries with status pending, oldest first.
func (s *Store) PendingApprovals(ctx context.Context) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalCols+` FROM approvals_pending WHERE status='pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingApprovableCount counts a task's pending entries a human can approve
// (destructive_blocked entries are excluded).
func (s *Store) PendingApprovableCount(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM approvals_pending WHERE task_id=? AND status='pending' AND kind<>?`,
		taskID, KindDestructiveBlocked).Scan(&n)
	return n, err
}

// ApproveApproval marks a pending entry approved. It refuses destructive_blocked.
func (s *Store) ApproveApproval(ctx context.Context, id int64) error {
	return s.resolve(ctx, id, ApprovalApproved)
}

// RejectApproval marks a pending entry rejected.
func (s *Store) RejectApproval(ctx context.Context, id int64) error {
	return s.resolve(ctx, id, ApprovalRejected)
}

func (s *Store) resolve(ctx context.Context, id int64, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, cur string
	err = tx.QueryRowContext(ctx, `SELECT kind, status FROM approvals_pending WHERE id=?`, id).Scan(&kind, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == ApprovalApproved && kind == KindDestructiveBlocked {
		return ErrNotApprovable
	}
	if cur != ApprovalPending {
		return ErrNotPending
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals_pending SET status=? WHERE id=?`, status, id); err != nil {
		return err
	}
	return tx.Commit()
}
