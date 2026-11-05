package approvals

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/orch/store"
)

func newQueue(t *testing.T) (*Queue, *store.Store, int64, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db", "o.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.CreateTask(context.Background(), store.Task{Repo: "/r", Base: "b", Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return New(st), st, id, dbPath
}

func TestDuplicateEnqueueIsIdempotent(t *testing.T) {
	q, st, id, _ := newQueue(t)
	ctx := context.Background()
	for i, reason := range []string{"first", "second", "third"} {
		if err := q.Enqueue(ctx, id, store.KindProtectedModified, "b", "h", "hash1", reason); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	got, _ := q.Pending(ctx)
	if len(got) != 1 || got[0].Reason != "third" || got[0].Hash != "hash1" {
		t.Fatalf("pending: %+v", got)
	}
	// An approved entry is history: a repeat does not reopen it and says so
	// (the task would otherwise park with nothing left to approve).
	if err := st.ApproveApproval(ctx, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, id, store.KindProtectedModified, "b", "h", "hash1", "again"); !errors.Is(err, store.ErrApprovalResolved) {
		t.Fatalf("err %v", err)
	}
	if got, _ = q.Pending(ctx); len(got) != 0 {
		t.Fatalf("approved entry re-opened: %+v", got)
	}
}

func TestNewHeadInvalidatesPreviousApproval(t *testing.T) {
	q, st, id, _ := newQueue(t)
	ctx := context.Background()
	if err := q.Enqueue(ctx, id, store.KindProtectedModified, "b", "head1", "hash1", "r1"); err != nil {
		t.Fatal(err)
	}
	old, _ := q.Pending(ctx)
	if err := q.Enqueue(ctx, id, store.KindProtectedModified, "b", "head2", "hash2", "r2"); err != nil {
		t.Fatal(err)
	}
	pend, _ := q.Pending(ctx)
	if len(pend) != 1 || pend[0].Hash != "hash2" || pend[0].Head != "head2" {
		t.Fatalf("pending: %+v", pend)
	}
	o, err := st.GetApproval(ctx, old[0].ID)
	if err != nil || o.Status != store.ApprovalSuperseded {
		t.Fatalf("old entry: %+v %v", o, err)
	}
	// A superseded entry can never be approved, and not by Approve either.
	if err := st.ApproveApproval(ctx, old[0].ID); !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("approve superseded: %v", err)
	}
	// A new head supersedes pending entries of other kinds as well.
	if err := q.Enqueue(ctx, id, store.KindPublishOutsideAllowlist, "b", "head3", "pub3", "p"); err != nil {
		t.Fatal(err)
	}
	pend, _ = q.Pending(ctx)
	if len(pend) != 1 || pend[0].Kind != store.KindPublishOutsideAllowlist {
		t.Fatalf("pending: %+v", pend)
	}
}

func TestEnqueueValidates(t *testing.T) {
	q, _, id, _ := newQueue(t)
	ctx := context.Background()
	if q.Enqueue(ctx, id, "bogus", "b", "h", "x", "r") == nil {
		t.Error("unknown kind accepted")
	}
	if q.Enqueue(ctx, id, store.KindProtectedModified, "b", "h", "", "r") == nil {
		t.Error("empty hash accepted")
	}
	if q.Enqueue(ctx, 0, store.KindProtectedModified, "b", "h", "x", "r") == nil {
		t.Error("task 0 accepted")
	}
}

func TestDestructiveBlockedNeverGranted(t *testing.T) {
	q, st, id, _ := newQueue(t)
	ctx := context.Background()
	if err := q.Enqueue(ctx, id, store.KindDestructiveBlocked, "b", "h", "d", "no"); err != nil {
		t.Fatal(err)
	}
	pend, _ := q.Pending(ctx)
	if err := st.ApproveApproval(ctx, pend[0].ID); !errors.Is(err, store.ErrNotApprovable) {
		t.Fatalf("approve: %v", err)
	}
	// Even with a matching grant in the file it never counts.
	repo, base := toyRepo(t)
	home := t.TempDir()
	id2, _, _ := gate.RepoID(repo)
	if err := (approval.Store{Path: gate.ApprovalsPath(home)}).Grant(id2, base, "d", time.Now()); err != nil {
		t.Fatal(err)
	}
	ok, err := Grants{Home: home}.Granted(repo, store.Approval{Kind: store.KindDestructiveBlocked, Status: store.ApprovalPending, Base: base, Hash: "d"})
	if err != nil || ok {
		t.Fatalf("granted=%v err=%v", ok, err)
	}
}

func TestGrantedNeedsExactTriple(t *testing.T) {
	repo, base := toyRepo(t)
	home := t.TempDir()
	rid, _, _ := gate.RepoID(repo)
	if err := (approval.Store{Path: gate.ApprovalsPath(home)}).Grant(rid, base, "good", time.Now()); err != nil {
		t.Fatal(err)
	}
	g := Grants{Home: home}
	a := store.Approval{Kind: store.KindProtectedModified, Status: store.ApprovalPending, Base: base, Hash: "good"}
	if ok, err := g.Granted(repo, a); err != nil || !ok {
		t.Fatalf("exact: %v %v", ok, err)
	}
	for name, mod := range map[string]func(*store.Approval){
		"wrong hash": func(a *store.Approval) { a.Hash = "bad" },
		"wrong base": func(a *store.Approval) { a.Base = "0000" },
		"superseded": func(a *store.Approval) { a.Status = store.ApprovalSuperseded },
	} {
		b := a
		mod(&b)
		if ok, _ := g.Granted(repo, b); ok {
			t.Errorf("%s: granted", name)
		}
	}
	if ok, _ := (Grants{Home: t.TempDir()}).Granted(repo, a); ok {
		t.Error("empty grant file: granted")
	}
}

// The database, the approvals file and their directories are private, and the
// location check rejects anything the agent or test VM can see.
func TestAgentVMCannotReadApprovals(t *testing.T) {
	_, _, _, dbPath := newQueue(t)
	home := filepath.Join(t.TempDir(), "gate-home")
	approvalsPath := gate.ApprovalsPath(home)
	if err := (approval.Store{Path: approvalsPath}).Grant("rid", "base", "hash", time.Now()); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	for _, f := range []string{dbPath, approvalsPath} {
		if m := mode(f); m != 0o600 {
			t.Errorf("%s mode %04o, want 0600", f, m)
		}
		if err := CheckPrivate(f); err != nil {
			t.Errorf("CheckPrivate(%s): %v", f, err)
		}
		if m := mode(filepath.Dir(f)); m != 0o700 {
			t.Errorf("%s mode %04o, want 0700", filepath.Dir(f), m)
		}
	}
	// A loose file or directory is refused.
	loose := filepath.Join(t.TempDir(), "loose.json")
	os.WriteFile(loose, []byte("{}"), 0o644)
	if CheckPrivate(loose) == nil {
		t.Error("0644 file accepted")
	}
	os.Chmod(loose, 0o600)
	if CheckPrivate(loose) == nil { // t.TempDir is 0700-ish only by luck; force a loose dir
		// the directory of a TempDir may already be 0700; make it loose explicitly
		os.Chmod(filepath.Dir(loose), 0o755)
		if CheckPrivate(loose) == nil {
			t.Error("file in a 0755 directory accepted")
		}
	}
	// Exposed roots (repositories handed to the agent / test VM).
	repoRoot := t.TempDir()
	if err := CheckOutside(filepath.Join(repoRoot, "proj", ".aigate", "approvals.json"), []string{repoRoot}); err == nil {
		t.Error("path inside an exposed root accepted")
	}
	if err := CheckOutside(repoRoot, []string{repoRoot}); err == nil {
		t.Error("the exposed root itself accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repoRoot, link); err == nil {
		if CheckOutside(filepath.Join(link, "x"), []string{repoRoot}) == nil {
			t.Error("path through a symlink into an exposed root accepted")
		}
	}
	if err := CheckOutside(home, []string{repoRoot}); err != nil {
		t.Errorf("gate home outside the roots refused: %v", err)
	}
	if err := CheckOutside(repoRoot+"-sibling/x", []string{repoRoot}); err != nil {
		t.Errorf("sibling directory refused: %v", err)
	}
}
