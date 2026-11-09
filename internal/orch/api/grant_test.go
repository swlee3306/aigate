package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/store"
)

// None of these tests may use t.Parallel: they change the process-wide signers
// registry path and options (signing.SetRegistryPath, signing.DefaultOptions).

// useTestRegistry points the process-wide registry at a trusted-looking temp
// layout owned by the current user (same approach as internal/approval's
// signed_test.go) and restores the globals on cleanup. signers == nil leaves
// the file absent (legacy mode). It returns the registry file path.
func useTestRegistry(t *testing.T, signers []signing.Signer) string {
	t.Helper()
	oldPath, oldOpts := signing.RegistryPath(), signing.DefaultOptions
	t.Cleanup(func() {
		signing.SetRegistryPath(oldPath)
		signing.DefaultOptions = oldOpts
	})
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "signers.json")
	if signers != nil {
		b, err := json.Marshal(map[string]any{"version": 1, "signers": signers})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	signing.SetRegistryPath(p)
	signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }}
	return p
}

type testSigner struct {
	id, key string
	signer  signing.Signer
}

func newTestSigner(t *testing.T, id string) testSigner {
	t.Helper()
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen이 없습니다")
	}
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{id: id, key: key, signer: signing.Signer{ID: id, Type: signing.TypeSSHEd25519, Label: "L",
		PublicKey: strings.TrimSpace(string(pub)), AddedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}}
}

// signText signs text exactly as the CLI will: ssh-keygen -Y sign in the
// approval namespace.
func (s testSigner) signText(t *testing.T, text string) string {
	t.Helper()
	if text == "" {
		t.Fatal("refusing to sign an empty message")
	}
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-f", s.key, "-n", signing.NamespaceApproval)
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v: %s", err, stderr.String())
	}
	return stdout.String()
}

func commitChange(t *testing.T, e *env, name, content string) string {
	t.Helper()
	putFile(t, e.repo, name, content)
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "c")
	return git(t, e.repo, "rev-parse", "HEAD")
}

// grantFixture is a signed-mode environment: one SSH signer in the registry, a
// repo with a protected file, a gate home for the approvals file and ledger.
type grantFixture struct {
	e     *env
	alice testSigner
	home  string
	rid   string
	reg   string // registry path
	base  string // commit with the protected file
}

func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	alice := newTestSigner(t, "alice")
	reg := useTestRegistry(t, []signing.Signer{alice.signer})
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	f := &grantFixture{e: e, alice: alice, home: t.TempDir(), reg: reg}
	e.d.handler.grants = approvals.Grants{Home: f.home}
	putFile(t, e.repo, ".aigate.yml", diffYAML)
	f.base = commitChange(t, e, "tests/spec.txt", "line1\nline2\n")
	rid, _, err := gate.RepoID(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	f.rid = rid
	return f
}

// waiting stores a task base..head in needs_approval with one pending entry of
// kind whose hash is the gate's real diff hash for base..head (or hash, when
// not empty).
func (f *grantFixture) waiting(t *testing.T, kind, base, head, hash string) (tid, aid int64) {
	t.Helper()
	ctx := context.Background()
	if hash == "" {
		p, err := gate.Prepare(gate.Options{Repo: f.e.repo, Base: base, Head: head})
		if err != nil {
			t.Fatal(err)
		}
		hash = p.Diff.Hash
	}
	tid, err := f.e.st.CreateTask(ctx, store.Task{Repo: f.e.repo, Base: base, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.st.SetHeadSHA(ctx, tid, head); err != nil {
		t.Fatal(err)
	}
	if err := f.e.st.Transition(ctx, tid, store.StateQueued, store.StateNeedsApproval, "test"); err != nil {
		t.Fatal(err)
	}
	aid, err = f.e.st.AddApproval(ctx, store.Approval{TaskID: tid, Kind: kind, Hash: hash, Base: base, Head: head, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return tid, aid
}

// protectedTask commits a change to the protected file and parks a task for it.
func (f *grantFixture) protectedTask(t *testing.T, content string) (tid, aid int64, head string) {
	t.Helper()
	base := git(t, f.e.repo, "rev-parse", "HEAD")
	head = commitChange(t, f.e, "tests/spec.txt", content)
	tid, aid = f.waiting(t, store.KindProtectedModified, base, head, "")
	return tid, aid, head
}

// message is what the server must rebuild for the entry, built independently.
func (f *grantFixture) message(t *testing.T, aid int64, issued time.Time) signing.Message {
	t.Helper()
	a, err := f.e.st.GetApproval(context.Background(), aid)
	if err != nil {
		t.Fatal(err)
	}
	m := signing.Message{Kind: a.Kind, Repo: f.rid, Task: a.TaskID, Base: a.Base, Hash: a.Hash, Issued: issued}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

func nowSec() time.Time { return time.Now().UTC().Truncate(time.Second) }

func (f *grantFixture) post(t *testing.T, aid int64, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return f.e.do(t, "POST", "/api/v1/approvals/"+strconv.FormatInt(aid, 10)+"/grant", string(b))
}

func (f *grantFixture) body(sig string, issued time.Time) map[string]any {
	return map[string]any{"signer": f.alice.id, "alg": signing.TypeSSHEd25519, "sig": sig, "issued": issued.Format(time.RFC3339)}
}

// unchanged asserts that a refused grant changed nothing: the entry is still
// pending, the task still waits and no approvals file was written.
func (f *grantFixture) unchanged(t *testing.T, tid, aid int64) {
	t.Helper()
	if s := state(t, f.e, tid); s != store.StateNeedsApproval {
		t.Fatalf("task moved to %s", s)
	}
	if a, _ := f.e.st.GetApproval(context.Background(), aid); a.Status != store.ApprovalPending {
		t.Fatalf("entry %+v", a)
	}
	if _, err := os.Stat(gate.ApprovalsPath(f.home)); !os.IsNotExist(err) {
		t.Fatalf("approvals file written: %v", err)
	}
}

func TestKindConstantsMatchStore(t *testing.T) {
	if signing.KindProtectedModified != store.KindProtectedModified {
		t.Errorf("protected: %q vs %q", signing.KindProtectedModified, store.KindProtectedModified)
	}
	if signing.KindPublishOutsideAllowlist != store.KindPublishOutsideAllowlist {
		t.Errorf("publish: %q vs %q", signing.KindPublishOutsideAllowlist, store.KindPublishOutsideAllowlist)
	}
}

func TestGrantHappyPathSSH(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	issued := nowSec()
	m := f.message(t, aid, issued)
	code, b := f.post(t, aid, f.body(f.alice.signText(t, m.Text()), issued))
	if code != http.StatusOK {
		t.Fatalf("grant %d %s", code, b)
	}
	var v TaskView
	if err := json.Unmarshal(b, &v); err != nil || v.ID != tid || v.State != string(store.StateVerifying) {
		t.Fatalf("view %s (%v)", b, err)
	}
	if s := state(t, f.e, tid); s != store.StateVerifying {
		t.Fatalf("task %s", s)
	}
	if a, _ := f.e.st.GetApproval(context.Background(), aid); a.Status != store.ApprovalApproved {
		t.Fatalf("entry %+v", a)
	}
	// The grant the gate reads verifies on its own.
	a, _ := f.e.st.GetApproval(context.Background(), aid)
	if ok, err := (approvals.Grants{Home: f.home}).Granted(f.e.repo, store.Approval{Kind: a.Kind,
		Status: store.ApprovalPending, Base: a.Base, Hash: a.Hash}); err != nil || !ok {
		t.Fatalf("grant file: %v %v", ok, err)
	}
}

func TestGrantWrongHashConflict409(t *testing.T) {
	f := newGrantFixture(t)
	base := git(t, f.e.repo, "rev-parse", "HEAD")
	head := commitChange(t, f.e, "tests/spec.txt", "line1\nCHANGED\n")
	// The entry waits for a hash that is not the current diff's.
	tid, aid := f.waiting(t, store.KindProtectedModified, base, head, strings.Repeat("d", 64))
	issued := nowSec()
	m := f.message(t, aid, issued) // a valid signature over the entry's message
	code, b := f.post(t, aid, f.body(f.alice.signText(t, m.Text()), issued))
	wantEnvelope(t, code, b, http.StatusConflict, "hash_changed")
	f.unchanged(t, tid, aid)
	// The hash check comes before the signature: garbage is refused the same way.
	code, b = f.post(t, aid, f.body("garbage", issued))
	wantEnvelope(t, code, b, http.StatusConflict, "hash_changed")
	// A head that no longer matches the task is refused too.
	_, aid2, _ := f.protectedTask(t, "line1\nAGAIN\n")
	a2, _ := f.e.st.GetApproval(context.Background(), aid2)
	if err := f.e.st.SetHeadSHA(context.Background(), a2.TaskID, base); err != nil {
		t.Fatal(err)
	}
	code, b = f.post(t, aid2, f.body("garbage", issued))
	wantEnvelope(t, code, b, http.StatusConflict, "hash_changed")
}

func TestGrantStaleIssued403(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	for _, issued := range []time.Time{nowSec().Add(-10 * time.Minute), nowSec().Add(10 * time.Minute)} {
		m := f.message(t, aid, issued)
		code, b := f.post(t, aid, f.body(f.alice.signText(t, m.Text()), issued))
		wantEnvelope(t, code, b, http.StatusForbidden, "stale_signature")
	}
	f.unchanged(t, tid, aid)
	// issued that is not whole-second UTC RFC 3339 cannot form a message.
	for _, s := range []string{"", "yesterday", "2026-11-09T12:00:00+09:00", nowSec().Format(time.RFC3339Nano)[:19] + ".5Z"} {
		code, b := f.post(t, aid, map[string]any{"signer": "alice", "alg": signing.TypeSSHEd25519, "sig": "x", "issued": s})
		wantEnvelope(t, code, b, http.StatusBadRequest, "invalid_message")
	}
	f.unchanged(t, tid, aid)
}

func TestGrantUnknownSigner403(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	mallory := newTestSigner(t, "mallory") // a real key, not in the registry
	issued := nowSec()
	m := f.message(t, aid, issued)
	code, b := f.post(t, aid, map[string]any{"signer": "mallory", "alg": signing.TypeSSHEd25519,
		"sig": mallory.signText(t, m.Text()), "issued": issued.Format(time.RFC3339)})
	wantEnvelope(t, code, b, http.StatusForbidden, "unknown_signer")
	// Mallory's signature under alice's id is a bad signature.
	code, b = f.post(t, aid, f.body(mallory.signText(t, m.Text()), issued))
	wantEnvelope(t, code, b, http.StatusForbidden, "bad_signature")
	f.unchanged(t, tid, aid)
}

func TestGrantNotPending409(t *testing.T) {
	f := newGrantFixture(t)
	_, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	issued := nowSec()
	sig := f.alice.signText(t, f.message(t, aid, issued).Text())
	if code, b := f.post(t, aid, f.body(sig, issued)); code != 200 {
		t.Fatalf("first grant %d %s", code, b)
	}
	code, b := f.post(t, aid, f.body(sig, issued))
	wantEnvelope(t, code, b, http.StatusConflict, "not_pending")

	// A superseded entry: the task got a newer head.
	tid2, aid2, _ := f.protectedTask(t, "line1\nTWO\n")
	sig2 := f.alice.signText(t, f.message(t, aid2, issued).Text())
	if _, err := f.e.st.AddApproval(context.Background(), store.Approval{TaskID: tid2, Kind: store.KindProtectedModified,
		Hash: strings.Repeat("e", 64), Base: f.base, Head: strings.Repeat("f", 40)}); err != nil {
		t.Fatal(err)
	}
	code, b = f.post(t, aid2, f.body(sig2, issued))
	wantEnvelope(t, code, b, http.StatusConflict, "not_pending")
	if s := state(t, f.e, tid2); s != store.StateNeedsApproval {
		t.Fatalf("task moved to %s", s)
	}
	code, b = f.post(t, 9999, f.body(sig2, issued))
	wantEnvelope(t, code, b, http.StatusNotFound, "not_found")
}

func TestGrantDestructiveBlockedRefused(t *testing.T) {
	f := newGrantFixture(t)
	base := git(t, f.e.repo, "rev-parse", "HEAD")
	head := commitChange(t, f.e, "tests/spec.txt", "line1\nCHANGED\n")
	tid, aid := f.waiting(t, store.KindDestructiveBlocked, base, head, "")
	a, _ := f.e.st.GetApproval(context.Background(), aid)
	// A valid signature for the same task, base and hash (signed as the only
	// kinds a message can carry) still never grants a destructive entry.
	issued := nowSec()
	m := signing.Message{Kind: signing.KindProtectedModified, Repo: f.rid, Task: tid, Base: a.Base, Hash: a.Hash, Issued: issued}
	code, b := f.post(t, aid, f.body(f.alice.signText(t, m.Text()), issued))
	wantEnvelope(t, code, b, http.StatusForbidden, "forbidden")
	f.unchanged(t, tid, aid)
	// The detail offers nothing to sign.
	code, b = f.e.do(t, "GET", "/api/v1/approvals/"+strconv.FormatInt(aid, 10), "")
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil || d.Message != "" || d.Digest != "" || d.Meaning == "" {
		t.Fatalf("detail %d %s", code, b)
	}
	if !strings.Contains(string(b), `"protected_files":[]`) {
		t.Fatalf("protected_files must be an array: %s", b)
	}
}

// Review point 1: a signature for entry A is useless for entry B.
func TestGrantSignatureForOtherApprovalRefused(t *testing.T) {
	f := newGrantFixture(t)
	tidA, aidA, _ := f.protectedTask(t, "line1\nA\n")
	tidB, aidB, _ := f.protectedTask(t, "line1\nB\n")
	issued := nowSec()
	sigA := f.alice.signText(t, f.message(t, aidA, issued).Text())
	code, b := f.post(t, aidB, f.body(sigA, issued))
	wantEnvelope(t, code, b, http.StatusForbidden, "bad_signature")
	f.unchanged(t, tidB, aidB)
	f.unchanged(t, tidA, aidA)
	// Same base, head and hash but another task: still refused (the task id
	// is part of the message).
	aB, _ := f.e.st.GetApproval(context.Background(), aidB)
	tidC, aidC := f.waiting(t, store.KindProtectedModified, aB.Base, aB.Head, "")
	sigB := f.alice.signText(t, f.message(t, aidB, issued).Text())
	code, b = f.post(t, aidC, f.body(sigB, issued))
	wantEnvelope(t, code, b, http.StatusForbidden, "bad_signature")
	f.unchanged(t, tidC, aidC)
	// And A's own signature still works for A.
	if code, b := f.post(t, aidA, f.body(sigA, issued)); code != 200 {
		t.Fatalf("A %d %s", code, b)
	}
}

func TestGrantMessageRebuiltServerSide(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	issued := nowSec()
	server := f.message(t, aid, issued)
	// The client claims other values for every message field. They must be
	// ignored: only issued (and the signature) come from the body.
	client := server
	client.Hash = strings.Repeat("9", 64)
	client.Base = strings.Repeat("8", 40)
	client.Repo = "abcdefabcdef"
	client.Task = tid + 100
	client.Kind = signing.KindPublishOutsideAllowlist
	lie := func(sig string) map[string]any {
		b := f.body(sig, issued)
		b["kind"], b["repo"], b["task"], b["base"], b["hash"] = client.Kind, client.Repo, client.Task, client.Base, client.Hash
		b["message"], b["digest"] = client.Text(), "00"
		return b
	}
	// A signature over the client's own message is refused, whatever it claims.
	code, b := f.post(t, aid, lie(f.alice.signText(t, client.Text())))
	wantEnvelope(t, code, b, http.StatusForbidden, "bad_signature")
	f.unchanged(t, tid, aid)
	// A signature over the server's message with the same issued succeeds
	// despite the lying fields.
	if code, b := f.post(t, aid, lie(f.alice.signText(t, server.Text()))); code != 200 {
		t.Fatalf("grant %d %s", code, b)
	}
	if s := state(t, f.e, tid); s != store.StateVerifying {
		t.Fatalf("task %s", s)
	}
}

func TestGrantLedgerRecordsSigner(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	issued := nowSec()
	m := f.message(t, aid, issued)
	if code, b := f.post(t, aid, f.body(f.alice.signText(t, m.Text()), issued)); code != 200 {
		t.Fatalf("grant %d %s", code, b)
	}
	evs, err := ledger.Events(gate.LedgerPath(f.home, f.rid))
	if err != nil || len(evs) != 1 {
		t.Fatalf("ledger %v %+v", err, evs)
	}
	ev := evs[0]
	p := ev.Payload
	if ev.Type != "approval_granted" || ev.Repo != f.rid || p["signer"] != "alice" || p["alg"] != signing.TypeSSHEd25519 ||
		p["via"] != "socket:uid:"+strconv.Itoa(os.Getuid()) || p["actor"] != p["via"] || p["hash"] != m.Hash || p["base"] != m.Base || p["kind"] != m.Kind {
		t.Fatalf("event %+v", ev)
	}
	if n, ok := p["task"].(json.Number); !ok || n.String() != strconv.FormatInt(tid, 10) {
		t.Fatalf("task %#v", p["task"])
	}
	if _, err := ledger.Verify(gate.LedgerPath(f.home, f.rid)); err != nil {
		t.Fatal(err)
	}
}

func TestDetailMessageMatchesGrantVerification(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, head := f.protectedTask(t, "line1\nCHANGED\n")
	code, b := f.e.do(t, "GET", "/api/v1/approvals/"+strconv.FormatInt(aid, 10), "")
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil {
		t.Fatalf("detail %d %s", code, b)
	}
	if !d.RequiresSignature || d.Meaning == "" || d.Truncated {
		t.Fatalf("detail %+v", d)
	}
	issued, err := time.Parse(time.RFC3339, d.Issued)
	if err != nil || issued.Location() != time.UTC || issued.Nanosecond() != 0 || time.Since(issued).Abs() > time.Minute {
		t.Fatalf("issued %q %v", d.Issued, err)
	}
	if want := f.message(t, aid, issued).Text(); d.Message != want {
		t.Fatalf("message\n got %q\nwant %q", d.Message, want)
	}
	sum := sha256.Sum256([]byte(d.Message))
	if d.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s", d.Digest)
	}
	p, err := gate.Prepare(gate.Options{Repo: f.e.repo, Base: d.Base, Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if d.HashedText != p.Diff.HashedText() || len(d.ProtectedFiles) != 1 || d.ProtectedFiles[0] != "tests/spec.txt" {
		t.Fatalf("hashed text/protected files %+v", d)
	}
	// Sign exactly what the detail shows, with its issued.
	code, b = f.post(t, aid, map[string]any{"signer": "alice", "alg": signing.TypeSSHEd25519,
		"sig": f.alice.signText(t, d.Message), "issued": d.Issued})
	if code != 200 {
		t.Fatalf("grant %d %s", code, b)
	}
	if s := state(t, f.e, tid); s != store.StateVerifying {
		t.Fatalf("task %s", s)
	}
	// Once granted the detail offers nothing to sign any more.
	_, b = f.e.do(t, "GET", "/api/v1/approvals/"+strconv.FormatInt(aid, 10), "")
	d = ApprovalDetail{}
	if json.Unmarshal(b, &d) != nil || d.Message != "" || d.Digest != "" {
		t.Fatalf("after grant %s", b)
	}
}

// Legacy mode: no registry, so no signature is required and grant is refused.
func TestGrantNotSignedMode400(t *testing.T) {
	alice := newTestSigner(t, "alice")
	useTestRegistry(t, nil)
	e := newEnv(t, noSteps{}, 1)
	tid, aid, _ := parked(t, e, store.KindProtectedModified, strings.Repeat("a", 64))
	code, b := e.do(t, "GET", "/api/v1/approvals/"+strconv.FormatInt(aid, 10), "")
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil || d.RequiresSignature {
		t.Fatalf("detail %d %s", code, b)
	}
	body, _ := json.Marshal(map[string]any{"signer": alice.id, "alg": signing.TypeSSHEd25519, "sig": "x", "issued": nowSec().Format(time.RFC3339)})
	code, b = e.do(t, "POST", "/api/v1/approvals/"+strconv.FormatInt(aid, 10)+"/grant", string(body))
	wantEnvelope(t, code, b, http.StatusBadRequest, "not_signed_mode")
	if state(t, e, tid) != store.StateNeedsApproval {
		t.Fatal("moved")
	}
}

// A registry that exists but cannot be trusted fails closed: detail says a
// signature is required, grant answers 503 without detail.
func TestGrantSignersUnavailable503(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	issued := nowSec()
	sig := f.alice.signText(t, f.message(t, aid, issued).Text())
	if err := os.Chmod(f.reg, 0o666); err != nil {
		t.Fatal(err)
	}
	code, b := f.e.do(t, "GET", "/api/v1/approvals/"+strconv.FormatInt(aid, 10), "")
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil || !d.RequiresSignature {
		t.Fatalf("detail %d %s", code, b)
	}
	code, b = f.post(t, aid, f.body(sig, issued))
	wantEnvelope(t, code, b, http.StatusServiceUnavailable, "signers_unavailable")
	if strings.Contains(string(b), f.reg) || strings.Contains(string(b), "0666") {
		t.Fatalf("detail leaked: %s", b)
	}
	f.unchanged(t, tid, aid)
}

// CF1: no refusal echoes attacker-chosen input.
func TestGrantErrorsDoNotEchoInput(t *testing.T) {
	f := newGrantFixture(t)
	tid, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	const marker = "EVIL<img src=x onerror=alert(1)>MARKER"
	issued := nowSec().Format(time.RFC3339)
	bodies := []string{
		`{"signer":"` + "EVIL-MARKER-" + `alice","alg":"ssh-ed25519","sig":"x","issued":"` + issued + `"}`,
		`{"signer":"alice","alg":"ssh-ed25519","sig":"-----BEGIN SSH SIGNATURE-----\nEVIL-MARKER\n-----END SSH SIGNATURE-----\n","issued":"` + issued + `"}`,
		`{"signer":"alice","alg":"EVIL-MARKER","sig":"x","issued":"` + issued + `"}`,
		`{"signer":"alice","alg":"ssh-ed25519","sig":"x","issued":"EVIL-MARKER"}`,
		`{"signer":"alice","alg":"ssh-ed25519","sig":"x","issued":"` + issued + `","webauthn":{"authenticator_data":"EVIL-MARKER","client_data_json":"EVIL-MARKER"}}`,
		`{"signer": EVIL-MARKER}`,
		`EVIL-MARKER`,
	}
	for _, mk := range []string{"EVIL-MARKER", marker} {
		for _, body := range bodies {
			body = strings.ReplaceAll(body, "EVIL-MARKER", strings.ReplaceAll(mk, `"`, `\"`))
			code, b := f.e.do(t, "POST", "/api/v1/approvals/"+strconv.FormatInt(aid, 10)+"/grant", body)
			if code < 400 {
				t.Fatalf("accepted %d: %s", code, body)
			}
			var env v1Err
			if json.Unmarshal(b, &env) != nil || env.Error.Code == "" {
				t.Fatalf("not an envelope: %s", b)
			}
			if strings.Contains(string(b), "EVIL") || strings.Contains(string(b), "MARKER") || strings.Contains(string(b), "onerror") {
				t.Fatalf("response echoes input: %s", b)
			}
		}
	}
	f.unchanged(t, tid, aid)
}

// Custom codes survive the v1 envelope; the legacy flat shape is unchanged.
func TestGrantCodesSurviveEnvelope(t *testing.T) {
	f := newGrantFixture(t)
	_, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	code, b := f.post(t, aid, f.body("x", nowSec()))
	wantEnvelope(t, code, b, http.StatusForbidden, "bad_signature")
	var raw map[string]map[string]any
	if json.Unmarshal(b, &raw) != nil || len(raw["error"]) != 2 {
		t.Fatalf("envelope shape %s", b)
	}
	// grant is a v1-only route.
	if code, _ := f.e.do(t, "POST", "/approvals/"+strconv.FormatInt(aid, 10)+"/grant", "{}"); code != http.StatusNotFound {
		t.Fatalf("legacy grant route %d", code)
	}
}

// publishTask parks a task (base..a fresh head) with one publish entry whose
// hash is approvals.PublishHash for action (or hash, when not empty).
func (f *grantFixture) publishTask(t *testing.T, action, hash string) (tid, aid int64) {
	t.Helper()
	ctx := context.Background()
	base := git(t, f.e.repo, "rev-parse", "HEAD")
	head := commitChange(t, f.e, "src/p.txt", "p"+strconv.FormatInt(time.Now().UnixNano(), 10)+"\n")
	tid, err := f.e.st.CreateTask(ctx, store.Task{Repo: f.e.repo, Base: base, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.st.SetHeadSHA(ctx, tid, head); err != nil {
		t.Fatal(err)
	}
	if err := f.e.st.Transition(ctx, tid, store.StateQueued, store.StateNeedsApproval, "test"); err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		hash = approvals.PublishHash(tid, head, action)
	}
	aid, err = f.e.st.AddApproval(ctx, store.Approval{TaskID: tid, Kind: store.KindPublishOutsideAllowlist,
		Hash: hash, Base: base, Head: head, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return tid, aid
}

func (f *grantFixture) detail(t *testing.T, aid int64) ApprovalDetail {
	t.Helper()
	code, b := f.e.do(t, "GET", "/api/v1/approvals/"+strconv.FormatInt(aid, 10), "")
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil {
		t.Fatalf("detail %d %s", code, b)
	}
	return d
}

const testPolicyHash = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

func TestGrantPublishActions(t *testing.T) {
	f := newGrantFixture(t)
	f.e.d.handler.policyHash = func() string { return testPolicyHash }
	for _, tc := range []struct{ action, wantAction, inMeaning string }{
		{"push_branch", "push_branch", "push_branch"},
		{"draft_pr", "draft_pr", "draft_pr"},
		{"policy_changed:" + testPolicyHash, "policy_changed:" + testPolicyHash, "바뀐 정책"},
	} {
		tid, aid := f.publishTask(t, tc.action, "")
		d := f.detail(t, aid)
		if d.Action != tc.wantAction || !strings.Contains(d.Meaning, tc.inMeaning) || d.Message == "" {
			t.Fatalf("%s: detail action %q meaning %q message %q", tc.action, d.Action, d.Meaning, d.Message)
		}
		if d.Message != f.message(t, aid, mustIssued(t, d.Issued)).Text() {
			t.Fatalf("%s: message %q", tc.action, d.Message)
		}
		code, b := f.post(t, aid, map[string]any{"signer": "alice", "alg": signing.TypeSSHEd25519,
			"sig": f.alice.signText(t, d.Message), "issued": d.Issued})
		if code != 200 {
			t.Fatalf("%s: grant %d %s", tc.action, code, b)
		}
		if s := state(t, f.e, tid); s != store.StateVerifying {
			t.Fatalf("%s: task %s", tc.action, s)
		}
	}
}

func mustIssued(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts.UTC()
}

// A publish entry whose hash no known action of this task and head produces
// is refused before the signature, even with a valid one.
func TestGrantPublishUnknownHash409(t *testing.T) {
	f := newGrantFixture(t)
	f.e.d.handler.policyHash = func() string { return testPolicyHash }
	tid, aid := f.publishTask(t, "", strings.Repeat("d", 64))
	d := f.detail(t, aid)
	if d.Action != "" || d.Message != "" || d.Meaning == "" {
		t.Fatalf("detail %+v", d)
	}
	issued := nowSec()
	m := f.message(t, aid, issued) // valid signature over the entry's own message
	code, b := f.post(t, aid, f.body(f.alice.signText(t, m.Text()), issued))
	wantEnvelope(t, code, b, http.StatusConflict, "hash_changed")
	f.unchanged(t, tid, aid)
	// A policy-change entry for a policy that is no longer in force is stale too.
	tid2, aid2 := f.publishTask(t, "policy_changed:"+strings.Repeat("1", 64), "")
	m2 := f.message(t, aid2, issued)
	code, b = f.post(t, aid2, f.body(f.alice.signText(t, m2.Text()), issued))
	wantEnvelope(t, code, b, http.StatusConflict, "hash_changed")
	f.unchanged(t, tid2, aid2)
}

// Only the request that resolves the entry logs approval_granted: concurrent
// grants of one entry and a later repeat leave exactly one event.
func TestGrantLedgerOnlyForResolvingRequest(t *testing.T) {
	f := newGrantFixture(t)
	_, aid, _ := f.protectedTask(t, "line1\nCHANGED\n")
	issued := nowSec()
	body := f.body(f.alice.signText(t, f.message(t, aid, issued).Text()), issued)
	const n = 8
	codes := make(chan int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", "http://orchd/api/v1/approvals/"+strconv.FormatInt(aid, 10)+"/grant", strings.NewReader(string(b)))
			resp, err := f.e.client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		switch c {
		case 200:
			ok++
		case http.StatusConflict:
		default:
			t.Fatalf("status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d successful grants", ok)
	}
	code, b := f.post(t, aid, body)
	wantEnvelope(t, code, b, http.StatusConflict, "not_pending")
	evs, err := ledger.Events(gate.LedgerPath(f.home, f.rid))
	if err != nil || len(evs) != 1 || evs[0].Type != "approval_granted" {
		t.Fatalf("ledger %v: %d events %+v", err, len(evs), evs)
	}
}

// TestOldShortHashEntryDetailAndGrant: a pending row from a build before
// 2026-11-09 holds the 16-hex prefix of the diff hash. The detail still
// renders (no message to sign, since the current 64-hex hash differs) and a
// grant is refused as hash_changed; nothing is written.
func TestOldShortHashEntryDetailAndGrant(t *testing.T) {
	f := newGrantFixture(t)
	base := git(t, f.e.repo, "rev-parse", "HEAD")
	head := commitChange(t, f.e, "tests/spec.txt", "line1\nCHANGED\n")
	p, err := gate.Prepare(gate.Options{Repo: f.e.repo, Base: base, Head: head})
	if err != nil || len(p.Diff.Hash) != 64 {
		t.Fatalf("prepare: %v %q", err, p.Diff.Hash)
	}
	tid, aid := f.waiting(t, store.KindProtectedModified, base, head, p.Diff.Hash[:16])
	d := f.detail(t, aid)
	if d.Hash != p.Diff.Hash[:16] || d.Message != "" || d.Digest != "" || !d.RequiresSignature {
		t.Fatalf("detail of a 16-hex entry: %+v", d)
	}
	issued := nowSec()
	code, b := f.post(t, aid, f.body("garbage", issued))
	wantEnvelope(t, code, b, http.StatusConflict, "hash_changed")
	f.unchanged(t, tid, aid)
}
