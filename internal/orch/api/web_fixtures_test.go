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
	"aigate/internal/orch/broker"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/refine"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/registrymsg"
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

	// Task actions (A5): a running task (cancellable, with an actor), a failed
	// task that may be retried (infra failure, one earlier retry) and a queued
	// task. The 201/200 bodies of cancel/retry are these same TaskViews.
	running := fixtureTask(41, store.StateAgentRunning, "로그인 화면 문구 수정", t0.Add(2*time.Hour))
	running.LastActor = "session:mac-passkey"
	retryable := fixtureTask(42, store.StateFailed, "빌드 스크립트 정리", t0.Add(3*time.Hour))
	retryable.InfraFailed, retryable.RetryState, retryable.Retries = true, store.StateVerifying, 1
	retryable.ErrorText = "verify: restore failed"
	retryable.LastActor = "token:mac-ssh"
	queuedTask := fixtureTask(43, store.StateQueued, "대기 중 작업", t0.Add(4*time.Hour))
	queuedTask.LastActor = "system"
	checkFixture(t, "tasks-actions.json", []TaskView{viewOf(running), viewOf(retryable), viewOf(queuedTask)})
	// B6: the AI plan path. plan.json is the real /refine response, the preview
	// fixtures the real /refine/preview responses (allowed and not allowed), and
	// the task-mr-* fixtures the /events body of a done task in each merge
	// request state (TaskView.mr_*; the retry endpoint answers with the same
	// TaskView).
	checkFixture(t, "plan.json", planResponse{
		Summary: "README의 오타를 고치고 설치 안내를 보강한다",
		Items: []planItem{
			{ID: "r1", Title: "README 오타 수정", Description: "설치 절에 있는 오타 세 곳을 고친다.", Files: []string{"README.md"}},
			{ID: "r2", Title: "설치 안내 보강", Description: "필요한 도구와 버전을 표로 적는다.\n표는 README 맨 위에 둔다.", Files: []string{"README.md", "docs/install.md"}},
			{ID: "r3", Title: "CI 설정 정리", Description: "사용하지 않는 단계를 제거한다.", Files: []string{".ci/pipeline.yml"}},
		},
		Questions:  []planQuestion{{ID: "q1", Text: "영문 문서도 함께 고칠까요?"}, {ID: "q2", Text: "표 형식은 어떤 것을 쓸까요?"}},
		OutOfScope: []string{"릴리스 노트 작성", "코드 동작 변경"},
		Usage:      planUsage{InputTokens: 4200, OutputTokens: 650},
	})
	checkFixture(t, "preview.json", previewResponse{Allowed: true, SelectionID: "sel0123456789abcdef0123456789ab",
		Files:      []refine.FileEntry{{Path: "README.md", Bytes: 2140, Why: refine.WhyReadme}, {Path: "docs/install.md", Bytes: 5300, Why: refine.WhyNameMatch}, {Path: "go.mod", Bytes: 120, Why: refine.WhySmallRepo}},
		Excluded:   []refine.Excluded{{Path: ".env.example", Reason: refine.ReasonSecretName}, {Path: "docs/key.txt", Reason: refine.ReasonLooksLikeSecret}, {Path: "docs/big.md", Reason: refine.ReasonTooLarge}},
		TotalBytes: 7560})
	checkFixture(t, "preview-not-allowed.json", previewResponse{Files: []refine.FileEntry{}, Excluded: []refine.Excluded{}})
	mrTask := func(id int64, state, code, url string, iid int64) store.Task {
		tk := fixtureTask(id, store.StateDone, "README 오타 수정", t0)
		tk.HeadSHA = "89abcdef0123456789abcdef0123456789abcdef"
		tk.Verdict = "pass"
		tk.Branch = "orch/" + strconv.FormatInt(id, 10)
		tk.MRState, tk.MRCode, tk.MRURL, tk.MRIID = state, code, url, iid
		return tk
	}
	mrEvents := func(tk store.Task) map[string]any {
		return map[string]any{"task": viewOf(tk), "steps": Timeline([]store.Event{{To: store.StateQueued, Note: "created", At: t0, Actor: "session:alice"}}), "verify": nil}
	}
	checkFixture(t, "task-mr-created.json", mrEvents(mrTask(51, "created", "", "http://100.64.0.7:18082/lab/toy/-/merge_requests/7", 7)))
	checkFixture(t, "task-mr-failed.json", mrEvents(mrTask(52, "failed", "upstream_error", "", 0)))
	checkFixture(t, "task-mr-skipped.json", mrEvents(mrTask(53, "skipped", "", "", 0)))
	checkFixture(t, "task-mr-none.json", mrEvents(mrTask(54, "", "", "", 0)))
	// POST /api/v1/refine and /refine/preview: the machine code and HTTP status
	// of each fixed error the console maps to its own text.
	refineErrs := map[string]int{}
	for _, e := range []*httpError{errRefineRepoNotAllowed, errBrokerUnavailable, errRefineRateLimited, errSelectionChanged,
		errSelectionExpired, errRefineBudget, errRefineUpstream, errRefineBadPlan} {
		refineErrs[e.v1Code] = e.code
	}
	checkFixture(t, "refine-errors.json", refineErrs)
	// GET /api/v1/policy: classes (keys feed the submit form), allowlist, floor.
	checkFixture(t, "policy.json", PolicyView{Hash: "0123abcd", Classes: map[string]string{"docs": "auto", "refactor": "manual", "기능": "manual"},
		Allowlist: []string{"origin"}, Floor: []string{".git/**"}, BaseCache: true})
	checkFixture(t, "policy-empty.json", PolicyView{Hash: "0123abcd", Classes: map[string]string{}, Allowlist: []string{}, Floor: []string{}})

	// GET /api/v1/repos (sorted by path; detached HEAD has branch "", an
	// unborn branch has head "").
	checkFixture(t, "repos.json", []RepoView{
		{Path: "/srv/repos/calc", Head: "0123456789abcdef0123456789abcdef01234567", Branch: "main", Branches: []string{"main", "기능/새것", "fix-1"}},
		{Path: "/srv/repos/docs", Head: "89abcdef0123456789abcdef0123456789abcdef", Branch: "", Branches: []string{"main"}},
		{Path: "/srv/repos/new", Head: "", Branch: "main", Branches: []string{}},
	})

	// GET /api/v1/ops with the cloud block (quota and test VM status), a
	// failed-cloud variant and the no-cloud-supplier variant.
	restoreAt := t0.Add(-2 * time.Hour)
	rv := RestoreView{Phases: []RestorePhase{{Name: "delete_server", Seconds: 4.2, OK: true},
		{Name: "create_volume", Seconds: 21.5, OK: true}, {Name: "create_server", Seconds: 18, OK: true},
		{Name: "wait_ssh", Seconds: 9.3, OK: true}}, Seconds: 53, At: restoreAt, OK: true}
	opsBase := OpsView{Build: "rev-abc123", Started: t0.Add(-48 * time.Hour), PolicyHash: "0123abcd", LastRestore: &rv, BaseCacheEntries: 3}
	opsOK := opsBase
	opsOK.Cloud = &CloudView{OK: true,
		Quota:  &QuotaView{Gigabytes: CloudLimit{Limit: 300, InUse: 180, Reserved: 30}, Volumes: CloudLimit{Limit: -1, InUse: 7, Unlimited: true}},
		TestVM: &VMState{Name: "test-vm", Status: "ACTIVE"}, RestoreVolumes: 2,
		History: []RestoreView{rv, {Phases: []RestorePhase{{Name: "delete_server", Seconds: 3.9, OK: true}, {Name: "create_volume", Seconds: 60, OK: false}},
			Seconds: 64, At: restoreAt.Add(-3 * time.Hour), OK: false}},
		CheckedAt: t0}
	opsDown := opsBase
	opsDown.Cloud = &CloudView{Code: CloudUnavailable, History: []RestoreView{}, CheckedAt: t0}
	// ok:true but the VM could not be read ("unknown"), no quota limit lines
	// near the threshold, and a quota past the 80% warning line.
	opsUnknown := opsBase
	opsUnknown.Cloud = &CloudView{OK: true,
		Quota:  &QuotaView{Gigabytes: CloudLimit{Limit: 300, InUse: 150, Reserved: 0}, Volumes: CloudLimit{Limit: 20, InUse: 4}},
		TestVM: &VMState{Name: "test-vm", Status: "unknown"}, RestoreVolumes: 0, History: []RestoreView{}, CheckedAt: t0}
	opsWarn := opsBase
	opsWarn.Cloud = &CloudView{OK: true,
		Quota:  &QuotaView{Gigabytes: CloudLimit{Limit: 300, InUse: 230, Reserved: 20}, Volumes: CloudLimit{Limit: 20, InUse: 17}},
		TestVM: &VMState{Name: "test-vm", Status: "ACTIVE"}, RestoreVolumes: 9, History: []RestoreView{rv}, CheckedAt: t0}
	opsTimeout := opsBase
	opsTimeout.Cloud = &CloudView{Code: CloudTimeout, History: []RestoreView{}, CheckedAt: t0}
	checkFixture(t, "ops-vm-unknown.json", opsUnknown)
	checkFixture(t, "ops-quota-warn.json", opsWarn)
	checkFixture(t, "ops-cloud-timeout.json", opsTimeout)
	checkFixture(t, "ops.json", opsOK)
	checkFixture(t, "ops-cloud-down.json", opsDown)
	checkFixture(t, "ops-no-cloud.json", opsBase)

	// GET /api/v1/tasks/{id}/events: steps from Timeline, verify from the note.
	evs := []store.Event{
		{To: store.StateQueued, Note: "created", At: t0, Actor: "session:alice"},
		{From: store.StateQueued, To: store.StatePreparing, Note: "worker picked up", At: t0.Add(200 * time.Millisecond), Actor: store.ActorSystem},
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

	// The same endpoint for the running task: the last step is still open.
	rt0 := t0.Add(2 * time.Hour)
	runEvs := []store.Event{
		{To: store.StateQueued, Note: "created", At: rt0, Actor: "token:mac-ssh"},
		{From: store.StateQueued, To: store.StatePreparing, Note: "worker picked up", At: rt0.Add(200 * time.Millisecond), Actor: store.ActorSystem},
		{From: store.StatePreparing, To: store.StateAgentRunning, Note: "agent started", At: rt0.Add(2200 * time.Millisecond)},
	}
	checkFixture(t, "events-running.json", map[string]any{"task": viewOf(running), "steps": Timeline(runEvs), "verify": nil})

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

// TestWebFixturesProjects pins the project registry JSON the console reads
// (registry list, candidates, propose, apply success) and the golden canonical
// change vectors the browser's computeChange must reproduce byte for byte. The
// propose fixture's key set is compared with a REAL propose response, so a
// renamed or added field fails here.
func TestWebFixturesProjects(t *testing.T) {
	gitlab := &projectGitLab{Project: "team/docs"}
	checkFixture(t, "projects-registry.json", map[string]any{
		"projects": []ProjectView{
			{ID: "docs", Path: "/home/orch/repos/docs", Kind: "docs", AIContext: false, GitLab: gitlab},
			{ID: "calc", Path: "/home/orch/repos/calc", Kind: "code", AIContext: true},
		},
		"providers":         []ProviderView{},
		"orphans":           []OrphanView{},
		"auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5},
	})
	checkFixture(t, "projects-candidates.json", map[string]any{
		"candidates": []CandidateView{
			{Path: "/home/orch/repos/notes", Head: "0123456789abcdef0123456789abcdef01234567"},
			{Path: "/home/orch/repos/website", Head: "89abcdef0123456789abcdef0123456789abcdef"},
		},
		"kinds": []string{"code", "docs"},
	})

	issued := time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)
	request := "00112233445566778899aabbccddeeff"
	propose := func(action, subject string, fields map[string]any) map[string]any {
		c := registryChange{Action: action, Subject: subject, Fields: fields}
		change, err := c.hash()
		if err != nil {
			t.Fatal(err)
		}
		msg := signing.RegistryMessage{Action: action, Subject: subject, Change: change, Request: request, Issued: issued}
		return map[string]any{"action": action, "subject": subject, "fields": fields, "message": msg.Text(),
			"change": change, "request": request, "issued": issued.Format(registryTimeFormat), "preview": previewSentences(c)}
	}
	reg := propose("register_project", "notes", map[string]any{"path": "/home/orch/repos/notes", "kind": "docs", "ai_context": false})
	regAI := propose("register_project", "notes", map[string]any{"path": "/home/orch/repos/notes", "kind": "docs", "ai_context": true})
	flag := propose("set_flag", "docs", map[string]any{"flag": "ai_context", "value": true})
	flagOff := propose("set_flag", "calc", map[string]any{"flag": "ai_context", "value": false})
	checkFixture(t, "projects-propose-register.json", reg)
	checkFixture(t, "projects-propose-register-ai.json", regAI)
	checkFixture(t, "projects-propose-setflag.json", flag)
	checkFixture(t, "projects-propose-setflag-off.json", flagOff)
	checkFixture(t, "projects-apply-ok.json", ProjectView{ID: "notes", Path: "/home/orch/repos/notes", Kind: "docs"})

	e := newProjEnv(t)
	code, b := e.sock(t, "POST", "/api/v1/projects/propose", e.proposeBody())
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var realKeys map[string]json.RawMessage
	if err := json.Unmarshal(b, &realKeys); err != nil {
		t.Fatal(err)
	}
	if len(realKeys) != len(reg) {
		t.Fatalf("propose has %d keys, fixture %d", len(realKeys), len(reg))
	}
	for k := range reg {
		if _, ok := realKeys[k]; !ok {
			t.Errorf("fixture key %q is not in the real propose response", k)
		}
	}

	// Canonical change vectors.
	type vec struct {
		Name      string         `json:"name"`
		Action    string         `json:"action"`
		Subject   string         `json:"subject"`
		Fields    map[string]any `json:"fields"`
		Canonical string         `json:"canonical,omitempty"`
		Change    string         `json:"change,omitempty"`
	}
	good := []vec{
		{Name: "register plain", Action: "register_project", Subject: "notes", Fields: map[string]any{"path": "/home/orch/repos/notes", "kind": "docs", "ai_context": false}},
		{Name: "register ai on", Action: "register_project", Subject: "notes", Fields: map[string]any{"path": "/home/orch/repos/notes", "kind": "docs", "ai_context": true}},
		{Name: "special characters", Action: "register_project", Subject: "a-1", Fields: map[string]any{"path": "/srv/a\"b\\c<d>&e/한글😀 f", "kind": "a_b-c", "ai_context": false}},
		{Name: "set flag on", Action: "set_flag", Subject: "docs", Fields: map[string]any{"flag": "ai_context", "value": true}},
		{Name: "set flag off", Action: "set_flag", Subject: "docs", Fields: map[string]any{"flag": "ai_context", "value": false}},
	}
	for i := range good {
		c, err := registrymsg.CanonicalChange(good[i].Action, good[i].Subject, good[i].Fields)
		if err != nil {
			t.Fatalf("%s: %v", good[i].Name, err)
		}
		h, _ := registrymsg.ChangeHash(good[i].Action, good[i].Subject, good[i].Fields)
		good[i].Canonical, good[i].Change = string(c), h
	}
	refused := []vec{
		{Name: "control char", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a\x01b", "kind": "docs", "ai_context": false}},
		{Name: "newline", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a\nb", "kind": "docs", "ai_context": false}},
		{Name: "U+2028", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a b", "kind": "docs", "ai_context": false}},
		{Name: "U+2029", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a b", "kind": "docs", "ai_context": false}},
		{Name: "DEL", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a\x7fb", "kind": "docs", "ai_context": false}},
		{Name: "empty path", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "", "kind": "docs", "ai_context": false}},
		{Name: "extra field", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": false, "x": "y"}},
		{Name: "missing field", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs"}},
		{Name: "ai_context string", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": "false"}},
		{Name: "ai_context number", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": float64(1)}},
		{Name: "bad subject upper", Action: "register_project", Subject: "A", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": false}},
		{Name: "bad subject digit first", Action: "register_project", Subject: "1a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": false}},
		{Name: "bad kind", Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "Docs", "ai_context": false}},
		{Name: "unknown action", Action: "delete_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": false}},
		{Name: "bad flag", Action: "set_flag", Subject: "a", Fields: map[string]any{"flag": "Auto Merge", "value": true}},
		{Name: "flag value string", Action: "set_flag", Subject: "a", Fields: map[string]any{"flag": "ai_context", "value": "true"}},
	}
	for _, r := range refused {
		if _, err := registrymsg.CanonicalChange(r.Action, r.Subject, r.Fields); err == nil {
			t.Fatalf("Go accepts %q; it must be in the refused vectors only if registrymsg refuses it", r.Name)
		}
	}
	m := signing.RegistryMessage{Action: "register_project", Subject: "notes", Change: good[0].Change, Request: request, Issued: issued}
	d := m.Digest()
	checkFixture(t, "registry-vectors.json", map[string]any{
		"change":  good,
		"refused": refused,
		"message": map[string]any{"action": m.Action, "subject": m.Subject, "change": m.Change, "request": m.Request,
			"issued": issued.Format(registryTimeFormat), "text": m.Text(), "digest": hex.EncodeToString(d[:])},
	})
}

// TestWebFixturesTokens pins the token JSON the console reads (list, begin,
// message, apply success) and the golden vectors the browser's computeValueHash
// and canonical change must reproduce byte for byte. Row shapes come from the
// real TokenView; the key sets of begin and message are compared with REAL
// responses, so a renamed or added field fails here.
func TestWebFixturesTokens(t *testing.T) {
	days := func(n int) *int { return &n }
	checkFixture(t, "tokens-list.json", map[string]any{
		"tokens": []TokenView{
			{Ref: "openai", Kind: "openai", Last4: "ZQxw", Expires: "2027-06-30", SetAt: "2026-11-09T10:04:05Z", Source: "store", DaysLeft: days(233), Usable: true},
			{Ref: "gitlab-main", Kind: "gitlab", Last4: "a1B2", Expires: "2026-11-13", SetAt: "2026-10-01T09:00:00Z", Source: "store", DaysLeft: days(3), Usable: true},
			{Ref: "github-old", Kind: "github", Last4: "x9Y8", Expires: "2026-11-08", SetAt: "2026-05-01T09:00:00Z", Source: "store", DaysLeft: days(-2), Usable: true},
			{Ref: "gitlab-docs", Kind: "gitlab", Last4: "q7w6", Expires: "", SetAt: "2026-09-01T09:00:00Z", Source: "store", DaysLeft: nil, Usable: true},
			{Ref: "github-lost", Kind: "github", Last4: "m5n4", Expires: "2027-01-31", SetAt: "2026-08-01T09:00:00Z", Source: "store", DaysLeft: days(82), Usable: false},
		},
		"warn_days": 14,
	})
	checkFixture(t, "tokens-list-config.json", map[string]any{
		"tokens": []TokenView{
			{Ref: "openai", Kind: "openai", Last4: "", Expires: "", SetAt: "", Source: "config", DaysLeft: nil, Usable: true},
		},
		"warn_days": 14,
	})

	issued := time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)
	request := "00112233445566778899aabbccddeeff"
	begin := map[string]string{"request": request, "issued": issued.Format(registryTimeFormat)}
	checkFixture(t, "tokens-begin.json", begin)

	valueHash := registrymsg.ValueHash(request, canaryValue)
	message := func(c tokenChange) map[string]any {
		change, err := c.hash()
		if err != nil {
			t.Fatal(err)
		}
		msg := signing.RegistryMessage{Action: c.Action, Subject: c.Subject, Change: change, Request: request, Issued: issued}
		return map[string]any{"message": msg.Text(), "change": change, "request": request, "issued": issued.Format(registryTimeFormat), "preview": tokenPreview(c)}
	}
	setFields := map[string]any{"kind": "gitlab", "expires": "2027-06-30", "value_hash": valueHash}
	setNoExp := map[string]any{"kind": "gitlab", "expires": "", "value_hash": valueHash}
	setOpenAI := map[string]any{"kind": "openai", "expires": "", "value_hash": valueHash}
	checkFixture(t, "tokens-message-set.json", message(tokenChange{Action: "set_token", Subject: "gitlab-main", Fields: setFields}))
	checkFixture(t, "tokens-message-set-noexp.json", message(tokenChange{Action: "set_token", Subject: "gitlab-main", Fields: setNoExp}))
	checkFixture(t, "tokens-message-set-openai.json", message(tokenChange{Action: "set_token", Subject: "openai", Fields: setOpenAI}))
	checkFixture(t, "tokens-message-delete.json", message(tokenChange{Action: "delete_token", Subject: "gitlab-main", Fields: map[string]any{}}))
	checkFixture(t, "tokens-message-delete-openai.json", message(tokenChange{Action: "delete_token", Subject: "openai", Fields: map[string]any{}}))
	checkFixture(t, "tokens-apply-set.json", TokenView{Ref: "gitlab-main", Kind: "gitlab", Last4: "ZQxw", Expires: "2027-06-30", SetAt: "2026-11-09T10:04:06Z", Source: "store", DaysLeft: days(233), Usable: true})
	checkFixture(t, "tokens-apply-delete.json", map[string]any{"ref": "gitlab-main", "deleted": true})

	// The key sets of begin and message are those of the REAL responses.
	e := newTokEnv(t)
	code, b := e.call(t, "begin", jsonBody(map[string]any{"action": "set_token", "subject": "gitlab-main", "fields": map[string]any{"kind": "gitlab", "expires": tokExpires}}))
	if code != 200 {
		t.Fatalf("begin %d %s", code, b)
	}
	sameKeys := func(what string, real []byte, fixture map[string]any) {
		var rk map[string]json.RawMessage
		if err := json.Unmarshal(real, &rk); err != nil {
			t.Fatal(err)
		}
		if len(rk) != len(fixture) {
			t.Errorf("%s: real response has %d keys, fixture %d", what, len(rk), len(fixture))
		}
		for k := range fixture {
			if _, ok := rk[k]; !ok {
				t.Errorf("%s: fixture key %q is not in the real response", what, k)
			}
		}
	}
	sameKeys("begin", b, map[string]any{"request": 0, "issued": 0})
	f := e.setFlow(t)
	code, b = e.call(t, "message", jsonBody(map[string]any{"action": f.Action, "subject": f.Subject, "fields": f.Fields, "request": f.Request, "issued": f.Issued}))
	if code != 200 {
		t.Fatalf("message %d %s", code, b)
	}
	sameKeys("message", b, message(tokenChange{Action: "set_token", Subject: "gitlab-main", Fields: setFields}))

	// Golden vectors: the salted value hash and the canonical change.
	type hashVec struct {
		Name    string `json:"name"`
		Request string `json:"request"`
		Value   string `json:"value"`
		Hash    string `json:"hash"`
	}
	long := ""
	for i := 0; i < maxTokenValue; i++ {
		long += string(rune(0x21 + i%94))
	}
	var hashes []hashVec
	for _, c := range []hashVec{
		{Name: "canary", Request: request, Value: canaryValue},
		{Name: "minimum length", Request: request, Value: "0123456789abcdef0123456789abcdef"},
		{Name: "maximum length, every printable character", Request: request, Value: long},
		{Name: "json-special characters", Request: request, Value: "\"quote\\backslash/slash<tag>&amp;'x'-0123456789abcdef"},
		{Name: "another request, same value", Request: "ffeeddccbbaa99887766554433221100", Value: canaryValue},
	} {
		c.Hash = registrymsg.ValueHash(c.Request, c.Value)
		if len(c.Hash) != 64 {
			t.Fatalf("%s: no hash", c.Name)
		}
		hashes = append(hashes, c)
	}
	badRequests := []hashVec{
		{Name: "request too short", Request: "0011", Value: canaryValue},
		{Name: "request upper case", Request: "00112233445566778899AABBCCDDEEFF", Value: canaryValue},
		{Name: "request with colon", Request: "0011223344556677889900aabbcc:dd", Value: canaryValue},
		{Name: "request empty", Request: "", Value: canaryValue},
	}
	for _, c := range badRequests {
		if registrymsg.ValueHash(c.Request, c.Value) != "" {
			t.Fatalf("%s: Go hashes it", c.Name)
		}
	}

	type vec struct {
		Name      string         `json:"name"`
		Action    string         `json:"action"`
		Subject   string         `json:"subject"`
		Fields    map[string]any `json:"fields"`
		Canonical string         `json:"canonical,omitempty"`
		Change    string         `json:"change,omitempty"`
	}
	good := []vec{
		{Name: "set with expiry", Action: "set_token", Subject: "gitlab-main", Fields: setFields},
		{Name: "set without expiry", Action: "set_token", Subject: "gitlab-main", Fields: setNoExp},
		{Name: "set openai", Action: "set_token", Subject: "openai", Fields: setOpenAI},
		{Name: "set github, latest year", Action: "set_token", Subject: "github", Fields: map[string]any{"kind": "github", "expires": "9999-12-31", "value_hash": valueHash}},
		{Name: "delete", Action: "delete_token", Subject: "gitlab-main", Fields: map[string]any{}},
	}
	for i := range good {
		c, err := registrymsg.CanonicalChange(good[i].Action, good[i].Subject, good[i].Fields)
		if err != nil {
			t.Fatalf("%s: %v", good[i].Name, err)
		}
		h, _ := registrymsg.ChangeHash(good[i].Action, good[i].Subject, good[i].Fields)
		good[i].Canonical, good[i].Change = string(c), h
	}
	hv := func(k string, v any) map[string]any {
		m := map[string]any{"kind": "gitlab", "expires": "2027-06-30", "value_hash": valueHash}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	refused := []vec{
		{Name: "missing value_hash", Action: "set_token", Subject: "a", Fields: hv("value_hash", nil)},
		{Name: "missing expires", Action: "set_token", Subject: "a", Fields: hv("expires", nil)},
		{Name: "missing kind", Action: "set_token", Subject: "a", Fields: hv("kind", nil)},
		{Name: "upper case hash", Action: "set_token", Subject: "a", Fields: hv("value_hash", "ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789")},
		{Name: "short hash", Action: "set_token", Subject: "a", Fields: hv("value_hash", "abcd")},
		{Name: "empty hash", Action: "set_token", Subject: "a", Fields: hv("value_hash", "")},
		{Name: "bad expires format", Action: "set_token", Subject: "a", Fields: hv("expires", "2027-6-30")},
		{Name: "impossible date", Action: "set_token", Subject: "a", Fields: hv("expires", "2027-02-30")},
		{Name: "year 1999", Action: "set_token", Subject: "a", Fields: hv("expires", "1999-12-31")},
		{Name: "kind outside the set", Action: "set_token", Subject: "a", Fields: hv("kind", "slack")},
		{Name: "kind upper case", Action: "set_token", Subject: "a", Fields: hv("kind", "GitLab")},
		{Name: "extra field", Action: "set_token", Subject: "a", Fields: map[string]any{"kind": "gitlab", "expires": "", "value_hash": valueHash, "value": "x"}},
		{Name: "bad ref upper", Action: "set_token", Subject: "A", Fields: hv("kind", "gitlab")},
		{Name: "bad ref underscore", Action: "set_token", Subject: "a_b", Fields: hv("kind", "gitlab")},
		{Name: "delete with a field", Action: "delete_token", Subject: "a", Fields: map[string]any{"kind": "gitlab"}},
		{Name: "unknown action", Action: "purge_token", Subject: "a", Fields: map[string]any{}},
	}
	for _, r := range refused {
		if _, err := registrymsg.CanonicalChange(r.Action, r.Subject, r.Fields); err == nil {
			t.Fatalf("Go accepts %q; it must be in the refused vectors only if registrymsg refuses it", r.Name)
		}
	}
	m := signing.RegistryMessage{Action: "set_token", Subject: "gitlab-main", Change: good[0].Change, Request: request, Issued: issued}
	d := m.Digest()
	checkFixture(t, "token-vectors.json", map[string]any{
		"value_hash":         hashes,
		"value_hash_refused": badRequests,
		"change":             good,
		"refused":            refused,
		"message": map[string]any{"action": m.Action, "subject": m.Subject, "change": m.Change, "request": m.Request,
			"issued": issued.Format(registryTimeFormat), "text": m.Text(), "digest": hex.EncodeToString(d[:])},
	})
}

// TestWebFixturesW3c pins the JSON of the provider, group and project-creation
// endpoints the console reads. Each fixture is built from the real Go types and
// its key set is compared with a REAL response of the handler, so a renamed or
// added field fails here.
func TestWebFixturesW3c(t *testing.T) {
	yes, no := true, false
	pid := int64(99)
	gl := ProviderView{ID: "gl", Kind: "gitlab", BaseURL: "http://gitlab.example:18082", SSHURL: "ssh://git@gitlab.example", TokenRef: "gl-admin"}
	glUsed, glFree := gl, gl
	glUsed.InUse, glFree.InUse = &yes, &no
	checkFixture(t, "providers.json", map[string]any{"providers": []ProviderView{glUsed, {ID: "gl2", Kind: "gitlab", BaseURL: "https://git.example", SSHURL: "ssh://git@git.example:2222", TokenRef: "gl2-admin", InUse: &no}}})
	checkFixture(t, "provider-groups.json", map[string]any{"groups": []GroupView{{Path: "team", Name: "team"}, {Path: "team/sub", Name: "sub"}}, "truncated": false})
	checkFixture(t, "providers-registry-w3c.json", map[string]any{
		"projects": []ProjectView{{ID: "app-one", Path: "/home/orch/repos/app-one", Kind: "registered", Provider: &gl.ID,
			GitLab: &projectGitLab{Project: "team/app-one", ProjectID: &pid}, Cloned: true}},
		"providers":         []ProviderView{glUsed},
		"orphans":           []OrphanView{{Provider: "gl", Project: "team/lost-app", ProjectID: &pid, Step: "deploy_key", At: "2026-11-09T12:00:00Z", Origin: "created"}, {Provider: "gl", Project: "team/lost-two", Step: "project", At: "2026-11-09T13:00:00Z", Origin: "linked"}},
		"auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5},
	})
	checkFixture(t, "provider-set-ok.json", map[string]any{"provider": glFree.withoutInUse()})
	checkFixture(t, "provider-delete-ok.json", map[string]any{"id": "gl", "deleted": true})
	checkFixture(t, "group-create-ok.json", map[string]any{"provider": "gl", "group": GroupView{Path: "team/newgroup", Name: "newgroup"}})
	proj := ProjectView{ID: "app-one", Path: "/home/orch/repos/app-one", Kind: "registered", Provider: &gl.ID, GitLab: &projectGitLab{Project: "team/app-one", ProjectID: &pid}, Cloned: true}
	checkFixture(t, "project-create-ok.json", map[string]any{"project": proj, "clone": CloneResult{Status: "cloned", Code: "ok"}})
	notCloned := proj
	notCloned.Cloned = false
	checkFixture(t, "project-create-clone-failed.json", map[string]any{"project": notCloned, "clone": CloneResult{Status: "failed", Code: "host_not_pinned"}})
	checkFixture(t, "project-clone-ok.json", map[string]any{"project": proj, "clone": CloneResult{Status: "cloned", Code: "ok"}})
	partial := partialError(&broker.PartialDetail{Provider: "gl", Project: "team/app-one", ProjectID: &pid, Step: "deploy_key", Recorded: true})
	checkFixture(t, "project-create-partial.json", map[string]any{"error": map[string]any{"code": partial.v1Code, "message": partial.msg, "detail": partial.detail}})

	// Real responses: same key sets.
	e := newW3cEnv(t)
	keys := func(b []byte) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return ledgerKeys(anyMap(m))
	}
	_, b := e.sockJSON(t, "GET", "/api/v1/providers", "")
	if keys(b) != "providers" {
		t.Errorf("providers keys %s", keys(b))
	}
	p := e.proposeAction(t, "create_project")
	code, b := e.apply(t, p)
	if code != 200 || keys(b) != "clone,project" {
		t.Fatalf("create apply %d %s", code, b)
	}
	var real struct {
		Project map[string]json.RawMessage `json:"project"`
		Clone   map[string]json.RawMessage `json:"clone"`
	}
	json.Unmarshal(b, &real)
	if ledgerKeys(anyMap(real.Project)) != "ai_context,auto_merge,cloned,gitlab,id,kind,path,provider" || ledgerKeys(anyMap(real.Clone)) != "code,status" {
		t.Errorf("create apply member keys: %s", b)
	}
	// create_partial envelope keys (the first create left its folder: a second create of the name is refused before the broker, so clear it)
	os.RemoveAll(filepath.Join(e.root, "app-one"))
	e.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "create_project" {
			return `{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"team/app-one","project_id":99,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false}}`
		}
		return ""
	})
	_, b = e.apply(t, e.proposeAction(t, "create_project"))
	var env struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	json.Unmarshal(b, &env)
	if ledgerKeys(anyMap(env.Error)) != "code,detail,message" {
		t.Errorf("partial envelope keys %s", b)
	}
	e.wb.setReply(nil)
	os.RemoveAll(filepath.Join(e.root, "app-one"))
	pr := e.proposeAction(t, "create_project")
	b, _ = json.Marshal(pr)
	if keys(b) != "action,change,deploy_key,fields,issued,message,preview,request,subject" {
		t.Errorf("create propose keys %s", keys(b))
	}
	checkFixture(t, "project-create-propose.json", map[string]any{"action": "create_project", "subject": "app-one",
		"fields": map[string]any{"provider": "gl", "group_path": "team", "template": "go", "description": "첫 프로젝트", "ai_context": false, "publish": "write",
			"deploy_key_sha256": "9f2b0f1d6a3c0e47b5a8d2c1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1"},
		"message": "(signing message text)", "change": "(change hash)", "request": "00112233445566778899aabbccddeeff", "issued": "2026-11-09T10:04:05Z",
		"preview":    []string{"(fixed Korean sentences)"},
		"deploy_key": map[string]string{"public_key": "ssh-ed25519 AAAA… orchd@deploy", "fingerprint": "SHA256:…"}})
}

func (p ProviderView) withoutInUse() ProviderView { p.InUse = nil; return p }

func anyMap(m map[string]json.RawMessage) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
