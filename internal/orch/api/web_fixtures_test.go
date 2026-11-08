package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
)

// fixturesDir holds the JSON the browser-side tests (internal/orch/web/test)
// consume. They are produced by marshalling the REAL response types of this
// package, so a field rename or type change fails here (drift) instead of
// silently desynchronising the console. Set ORCH_UPDATE_FIXTURES=1 to rewrite.
var fixturesDir = filepath.Join("..", "web", "test", "fixtures")

func checkFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := filepath.Join(fixturesDir, name)
	if os.Getenv("ORCH_UPDATE_FIXTURES") != "" {
		if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (set ORCH_UPDATE_FIXTURES=1 to create it)", name, err)
	}
	if !bytes.Equal(old, b) {
		t.Errorf("fixture %s drifted from the API types; set ORCH_UPDATE_FIXTURES=1 to rewrite it and review the JS tests", name)
	}
}

func fixtureTask(id int64, state store.State, prompt string, t0 time.Time) store.Task {
	return store.Task{ID: id, Repo: "/srv/repos/proj", Base: "main", Prompt: prompt, Class: "docs", Agent: "claude",
		State: state, Attempts: 1, CreatedAt: t0, UpdatedAt: t0.Add(3 * time.Minute)}
}

// TestWebFixturesGolden pins the JSON shapes of the task list, the timeline
// (events) endpoint and the diff endpoint against the JS fixtures.
func TestWebFixturesGolden(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	// GET /api/v1/tasks
	failed := fixtureTask(37, store.StateFailed, "리팩터링", t0.Add(-time.Hour))
	failed.ErrorText = "weakened: tests-weakened"
	failed.Verdict = "weakened"
	failed.HeadSHA = "0123456789abcdef0123456789abcdef01234567"
	pending := fixtureTask(38, store.StateNeedsApproval, "calc 테스트에 케이스를 추가하세요", t0)
	pending.HeadSHA = "89abcdef0123456789abcdef0123456789abcdef"
	pending.Verdict = "weakened"
	queued := fixtureTask(39, store.StateQueued, "README 오타 수정", t0.Add(time.Hour))
	checkFixture(t, "tasks.json", []TaskView{viewOf(queued), viewOf(pending), viewOf(failed)})

	// GET /api/v1/tasks/{id}/events: steps from Timeline, verify from the note.
	evs := []store.Event{
		{To: store.StateQueued, Note: "created", At: t0},
		{From: store.StateQueued, To: store.StatePreparing, Note: "worker picked up", At: t0.Add(200 * time.Millisecond)},
		{From: store.StatePreparing, To: store.StateAgentRunning, Note: "agent started", At: t0.Add(2200 * time.Millisecond)},
		{From: store.StateAgentRunning, To: store.StateCollecting, Note: "agent exited 0", At: t0.Add(27200 * time.Millisecond)},
		{From: store.StateCollecting, To: store.StateVerifying, Note: "head 89abcdef", At: t0.Add(28500 * time.Millisecond)},
		{From: store.StateVerifying, To: store.StateNeedsApproval, Note: noteFresh, At: t0.Add(182500 * time.Millisecond)},
	}
	var verify *VerifyBreakdown
	for i := len(evs) - 1; i >= 0; i-- {
		if b, ok := ParseVerifyNote(evs[i].Note); ok {
			verify = &b
			break
		}
	}
	if verify == nil {
		t.Fatal("setup: no verify note parsed")
	}
	checkFixture(t, "events.json", map[string]any{"task": viewOf(pending), "steps": Timeline(evs), "verify": verify})

	// GET /api/v1/tasks/{id}/diff from a real repository: one changed protected
	// file whose lines carry markup, ANSI and bidi characters, one new guarded
	// file, one unprotected change.
	e := newEnv(t, noSteps{}, 1)
	id, _, _ := diffTask(t, e, func() {
		putFile(t, e.repo, "tests/spec.txt", "line1\n<script>alert(1)</script>\nesc \x1b[31mred\x1b[0m bidi ‮evil\n")
		putFile(t, e.repo, "pytest.ini", "[pytest]\n")
		putFile(t, e.repo, "src/a.txt", "a\nb\nc\n")
	})
	code, v, raw := getDiff(t, e, id)
	if code != 200 {
		t.Fatalf("diff %d: %.200s (task %s)", code, raw, strconv.FormatInt(id, 10))
	}
	checkFixture(t, "diff.json", v)
	// The truncated variant is the same view cut by the server's own capUTF8
	// (which drops the trailing "A <path>" lines first).
	cut, trunc := capUTF8(v.HashedText, len(v.HashedText)-20)
	if !trunc {
		t.Fatal("setup: nothing cut")
	}
	full := v.HashedText
	v.HashedText, v.Truncated = cut, true
	checkFixture(t, "diff-truncated.json", v)

	// GET /api/v1/approvals/{id} in signed mode (the approval screen). The
	// repo id and commits of a temp repository change every run, so the
	// stored values are fixed; the message, digest and meaning come from the
	// real messageFor/meaningOf path (fillSigning minus the git lookups) and
	// the hashed text from the real diff above.
	det := fixtureApproval(t, full, false, v.ProtectedFiles, v.Hash)
	checkFixture(t, "approval.json", det)
	checkFixture(t, "approval-truncated.json", fixtureApproval(t, cut, true, v.ProtectedFiles, v.Hash))
	pub := fixturePublishApproval(t)
	checkFixture(t, "approval-publish.json", pub)
	checkFixture(t, "approvals.json", []ApprovalView{det.ApprovalView, pub.ApprovalView})
	checkFixture(t, "publish_vectors.json", publishVectors())
}

// fixtureRepoID is the repo id of a stored task repo path. The daemon stores
// the symlink-resolved top level (validate: EvalSymlinks + .git at the top),
// so gate.RepoID (sha256 of git's toplevel) equals sha256(path)[:12]; the
// console compares the message's repo id with this (and so does
// `orch approve` when the path is not local, repoIDOf).
func fixtureRepoID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:12]
}

// fixtureApproval builds the signed-mode approval detail the server would
// send for a protected_modified entry with fixed stored values.
func fixtureApproval(t *testing.T, text string, truncated bool, prot []string, hash string) ApprovalDetail {
	t.Helper()
	a := store.Approval{ID: 9, TaskID: 38, Kind: store.KindProtectedModified, Hash: hash,
		Base: "1111111111111111111111111111111111111111", Head: "89abcdef0123456789abcdef0123456789abcdef",
		Reason: "보호 파일 변경 <b>tests/spec.txt</b>", Status: store.ApprovalPending,
		CreatedAt: time.Date(2026, 11, 9, 10, 3, 2, 0, time.UTC)}
	det := fixtureDetail(t, a, grantable{diff: &gate.Protected{}}, len(prot))
	det.HashedText, det.Truncated, det.ProtectedFiles = text, truncated, prot
	return det
}

const fixturePolicyHash = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

// fixturePublishApproval: a publish_outside_allowlist entry for push_branch
// (hash from the real approvals.PublishHash, action and meaning as fillSigning).
func fixturePublishApproval(t *testing.T) ApprovalDetail {
	t.Helper()
	head := "89abcdef0123456789abcdef0123456789abcdef"
	a := store.Approval{ID: 11, TaskID: 38, Kind: store.KindPublishOutsideAllowlist,
		Hash: approvals.PublishHash(38, head, publish.KindPushBranch),
		Base: "1111111111111111111111111111111111111111", Head: head, Reason: "push", Status: store.ApprovalPending,
		CreatedAt: time.Date(2026, 11, 9, 10, 3, 3, 0, time.UTC)}
	det := fixtureDetail(t, a, grantable{action: publish.KindPushBranch}, 0)
	det.Action = publish.KindPushBranch
	det.PolicyHash = fixturePolicyHash
	det.ProtectedFiles = []string{}
	return det
}

func fixtureDetail(t *testing.T, a store.Approval, g grantable, files int) ApprovalDetail {
	t.Helper()
	issued := time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)
	task := fixtureTask(a.TaskID, store.StateNeedsApproval, "calc 테스트에 케이스를 추가하세요", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	task.Base, task.HeadSHA = a.Base, a.Head
	m := signing.Message{Kind: a.Kind, Repo: fixtureRepoID(task.Repo), Task: task.ID, Base: a.Base, Hash: a.Hash, Issued: issued}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	det := ApprovalDetail{ApprovalView: approvalView(a), Repo: task.Repo, TaskState: string(task.State),
		RequiresSignature: true, Issued: issued.Format(time.RFC3339), Message: m.Text()}
	d := m.Digest()
	det.Digest = hex.EncodeToString(d[:])
	det.Meaning = meaningOf(a, task, g, files)
	return det
}

// publishVectors are approvals.PublishHash outputs the console's JS mirror
// (webauthn.js publishHash/deriveAction) must reproduce byte for byte.
func publishVectors() []map[string]any {
	out := []map[string]any{}
	for _, c := range []struct {
		task int64
		head string
	}{{38, "89abcdef0123456789abcdef0123456789abcdef"}, {1, "0000000000000000000000000000000000000000000000000000000000000000"}, {123456789012, "ffffffffffffffffffffffffffffffffffffffff"}} {
		for _, action := range []string{publish.KindPushBranch, publish.KindDraftPR, publish.KindNotify, steps.PolicyChangeKind(fixturePolicyHash)} {
			out = append(out, map[string]any{"task": c.task, "head": c.head, "action": action, "hash": approvals.PublishHash(c.task, c.head, action)})
		}
	}
	return out
}
