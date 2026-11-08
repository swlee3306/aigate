package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
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
	checkFixture(t, "approvals.json", []ApprovalView{det.ApprovalView})
}

// fixtureApproval builds the signed-mode approval detail the server would
// send for a protected_modified entry with fixed stored values.
func fixtureApproval(t *testing.T, text string, truncated bool, prot []string, hash string) ApprovalDetail {
	t.Helper()
	issued := time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)
	a := store.Approval{ID: 9, TaskID: 38, Kind: store.KindProtectedModified, Hash: hash,
		Base: "1111111111111111111111111111111111111111", Head: "89abcdef0123456789abcdef0123456789abcdef",
		Reason: "보호 파일 변경 <b>tests/spec.txt</b>", Status: store.ApprovalPending,
		CreatedAt: time.Date(2026, 11, 9, 10, 3, 2, 0, time.UTC)}
	task := fixtureTask(38, store.StateNeedsApproval, "calc 테스트에 케이스를 추가하세요", time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	task.Base, task.HeadSHA = a.Base, a.Head
	m := signing.Message{Kind: a.Kind, Repo: "0123456789ab", Task: task.ID, Base: a.Base, Hash: a.Hash, Issued: issued}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	det := ApprovalDetail{ApprovalView: approvalView(a), Repo: task.Repo, TaskState: string(task.State),
		RequiresSignature: true, Issued: issued.Format(time.RFC3339), HashedText: text, Truncated: truncated,
		ProtectedFiles: prot, Message: m.Text()}
	d := m.Digest()
	det.Digest = hex.EncodeToString(d[:])
	det.Meaning = meaningOf(a, task, grantable{diff: &gate.Protected{}}, len(prot))
	return det
}
