// Package approvals is the orchestrator's pending-approvals queue and the
// check of the grant file that a human's TTY approval leaves behind.
//
// The AUTHORITY for an approval is the grant in <gate home>/.aigate/approvals.json
// (the file the gate reads), written by `orch approve` / `aigate approve` in a
// human's terminal. The queue only lists what waits; marking an entry approved
// requires Grants.Granted to find a matching grant in that file.
package approvals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/orch/store"
)

// MaxReason caps the reason text of one entry.
const MaxReason = 1000

// Queue is the pending-approvals list over the store.
type Queue struct{ st *store.Store }

// New returns a queue over st.
func New(st *store.Store) *Queue { return &Queue{st: st} }

// Enqueue records that a task waits for a human.
//   - The same task, kind and hash again is idempotent: only the reason of the
//     pending entry is updated.
//   - A new hash (or head) for the task marks its previous pending entries
//     superseded; a superseded entry can never be approved.
//
// kind is one of the store.Kind* constants; hash must not be empty.
func (q *Queue) Enqueue(ctx context.Context, taskID int64, kind, base, head, hash, reason string) error {
	if taskID <= 0 {
		return errors.New("approvals: task id required")
	}
	if len(reason) > MaxReason {
		reason = reason[:MaxReason]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
		reason += "…"
	}
	_, err := q.st.AddApproval(ctx, store.Approval{TaskID: taskID, Kind: kind,
		Base: base, Head: head, Hash: hash, Reason: reason})
	return err
}

// Pending lists the entries still waiting (oldest first).
func (q *Queue) Pending(ctx context.Context) ([]store.Approval, error) {
	return q.st.PendingApprovals(ctx)
}

// PublishHash is the hash a publish_outside_allowlist entry (and its grant)
// binds: this task, this head commit, this action kind. A new head or another
// action is a different hash, so an old grant never covers it.
func PublishHash(taskID int64, head, action string) string {
	sum := sha256.Sum256([]byte("aigate-orch-publish-v1\x00" + strconv.FormatInt(taskID, 10) + "\x00" + head + "\x00" + action))
	return hex.EncodeToString(sum[:])
}

// Grants reads the grant file the gate reads.
type Grants struct{ Home string } // gate home; empty means the gate's default

// Path is the approvals file.
func (g Grants) Path() (string, error) {
	home, err := gate.Home(g.Home)
	if err != nil {
		return "", err
	}
	return gate.ApprovalsPath(home), nil
}

// Granted reports whether the grant file holds a grant for exactly this entry
// of this task: (repo id of the task's repository, entry base, entry hash).
// destructive_blocked entries and entries that are not pending never count.
func (g Grants) Granted(repo string, a store.Approval) (bool, error) {
	if a.Kind == store.KindDestructiveBlocked || a.Status != store.ApprovalPending || a.Hash == "" || a.Base == "" {
		return false, nil
	}
	id, _, err := gate.RepoID(repo)
	if err != nil {
		return false, err
	}
	path, err := g.Path()
	if err != nil {
		return false, err
	}
	return approval.Store{Path: path}.Valid(id, a.Base, a.Hash), nil
}

// CheckPrivate refuses a file (database, approvals, grant) that is readable
// or writable by anyone but its owner, or sits in a directory that is not
// private (0700).
func CheckPrivate(file string) error {
	fi, err := os.Stat(file)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; it must be 0600", file, fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(file))
	if err != nil {
		return err
	}
	if di.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; it must be 0700", filepath.Dir(file), di.Mode().Perm())
	}
	return nil
}

// CheckOutside refuses path when it is, or lies under, one of the exposed
// roots: directories that the agent or the test VM can see (the repositories
// that are bundled or checked out for them). Approvals, the database and the
// token must live elsewhere. Symlinks are resolved on both sides.
func CheckOutside(path string, exposed []string) error {
	p := resolve(path)
	for _, r := range exposed {
		r = resolve(r)
		rel, err := filepath.Rel(r, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s is inside %s, which the agent or test VM can see", path, r)
		}
	}
	return nil
}

func resolve(p string) string {
	p = filepath.Clean(p)
	// Resolve the longest existing prefix so a not-yet-created file is judged
	// by where it will land.
	rest := ""
	for q := p; ; q = filepath.Dir(q) {
		if real, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(real, rest)
		}
		if filepath.Dir(q) == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
	}
}
