package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
)

// Merge request states of a published task (tasks.mr_state, schema v8).
const (
	MRCreated = "created"
	MRFailed  = "failed"
	MRSkipped = "skipped"
)

var mrCodeRe = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

// SetMR records the outcome of the merge request step of a done task: the
// branch, the merge request number and address (created only), the state and
// the fixed failure code (failed only), in one transaction. It is allowed
// from "not attempted", failed and skipped, never from created (the MR exists
// and is never overwritten): ErrStateMismatch then.
//
// A created or failed outcome also records the task event "mr_<state>" (from
// and to the task's current state), with the actor of ctx (ActorFrom).
// skipped records no event: it is the normal outcome whenever the feature is
// off, and a task that merely finished must not grow an extra history row.
func (s *Store) SetMR(ctx context.Context, id int64, branch string, iid int, url, state, code string) error {
	return s.SetMRFrom(ctx, id, []string{"", MRFailed, MRSkipped}, branch, iid, url, state, code)
}

// SetMRFrom is SetMR with the allowed previous states named: "" is "not
// attempted yet". The hook uses {""} so a task gets its merge request step
// once; the retry endpoint uses {failed, skipped}. A task that is not done, or
// whose mr_state is not in prev, is ErrStateMismatch (nothing written).
func (s *Store) SetMRFrom(ctx context.Context, id int64, prev []string, branch string, iid int, url, state, code string) error {
	switch state {
	case MRCreated:
		if iid <= 0 || url == "" || code != "" {
			return errors.New("store: a created merge request needs an iid and a url and no code")
		}
	case MRFailed:
		if !mrCodeRe.MatchString(code) || iid != 0 || url != "" {
			return errors.New("store: a failed merge request needs a fixed code and no iid or url")
		}
	case MRSkipped:
		if iid != 0 || url != "" || code != "" {
			return errors.New("store: a skipped merge request carries no data")
		}
	default:
		return errors.New("store: unknown merge request state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur State
	var curMR sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state, mr_state FROM tasks WHERE id = ?`, id).Scan(&cur, &curMR)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ok := cur == StateDone && curMR.String != MRCreated
	if ok {
		ok = false
		for _, p := range prev {
			if p == curMR.String {
				ok = true
			}
		}
	}
	if !ok {
		return ErrStateMismatch
	}
	n := now()
	var b, u, c any
	if branch != "" {
		b = branch
	}
	if url != "" {
		u = url
	}
	if code != "" {
		c = code
	}
	var i any
	if iid > 0 {
		i = iid
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET branch=?, mr_iid=?, mr_url=?, mr_state=?, mr_code=?, updated_at=? WHERE id=?`,
		b, i, u, state, c, n, id); err != nil {
		return err
	}
	if state != MRSkipped {
		note := "mr_" + state
		if code != "" {
			note += ": " + code
		}
		if _, err := tx.ExecContext(ctx, insertEvent, id, cur, cur, note, n, ActorFrom(ctx)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
