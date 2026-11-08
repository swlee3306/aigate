// Package api is the local control surface of orchd: a small JSON API served
// over a unix socket. There is no authentication beyond the socket file mode
// (0600, owned by the orchd user) and the 0700 directory around it; anyone who
// can open the socket can submit and cancel tasks. Do not expose it elsewhere.
//
// Approvals: socket access alone can never GRANT one. The authority is a grant
// in the approvals file (the one the gate reads): in legacy mode written by a
// human from a real terminal with `orch approve`; in signed-only mode (signers
// registry present) a signed entry that the daemon AND the gate verify against
// the root-owned registry. POST /api/v1/approvals/{id}/grant (grant.go) records
// such an entry only when the submitted signature verifies over the message the
// server rebuilt for the entry, so a caller without a signer key changes
// nothing. POST /approvals/{id}/reverify only asks the daemon to re-judge: the
// daemon itself checks that the approvals file holds a grant for exactly that
// entry's (repo, base, hash) and, if it does not, changes nothing.
// destructive_blocked and superseded entries can never be granted or reverified.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aigate/internal/gitx"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
)

const (
	// MaxBodyBytes caps a request body.
	MaxBodyBytes = 1 << 20
	// MaxPromptBytes caps the prompt of one task.
	MaxPromptBytes = 64 << 10
)

var (
	classRe = regexp.MustCompile(`^[a-z0-9_가-힣-]{1,32}$`)
	baseRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^@{}+-]{0,199}$`)
)

// Agents accepted by submit.
var agents = map[string]bool{"claude": true, "codex": true}

// TaskView is the JSON form of a task.
type TaskView struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Base       string    `json:"base"`
	Prompt     string    `json:"prompt"`
	Class      string    `json:"class"`
	Agent      string    `json:"agent"`
	State      string    `json:"state"`
	Attempts   int       `json:"attempts"`
	RetryState string    `json:"retry_state,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Error      string    `json:"error,omitempty"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
	// Retryable: `orch retry` is allowed now (failed on infrastructure, within the retry limit).
	Retryable bool `json:"retryable,omitempty"`
	// Retries is how many times an operator retried this task.
	Retries int `json:"retries,omitempty"`
}

// ApprovalView is the JSON form of a pending-approvals entry.
type ApprovalView struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	Kind      string    `json:"kind"`
	Hash      string    `json:"hash"`
	Base      string    `json:"base"`
	Head      string    `json:"head"`
	Reason    string    `json:"reason"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func viewOf(t store.Task) TaskView {
	return TaskView{ID: t.ID, Repo: t.Repo, Base: t.Base, Prompt: t.Prompt, Class: t.Class,
		Agent: t.Agent, State: string(t.State), Attempts: t.Attempts, RetryState: string(t.RetryState),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, Error: t.ErrorText, HeadSHA: t.HeadSHA, Verdict: t.Verdict,
		Retryable: machine.Retryable(t) == nil, Retries: t.Retries}
}

type submitRequest struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Prompt string `json:"prompt"`
	Class  string `json:"class"`
	Agent  string `json:"agent"`
}

type httpError struct {
	code int
	msg  string
	// v1Code, when set, is the envelope's machine-readable code instead of the
	// one derived from the status (codeOf).
	v1Code string
}

// apiErr is an error with a fixed v1 envelope code. msg must be a fixed text:
// never the Error() of a signing, registry or approval error (CF1).
func apiErr(status int, v1Code, msg string) *httpError {
	return &httpError{code: status, msg: msg, v1Code: v1Code}
}

func (e *httpError) Error() string { return e.msg }

func bad(format string, a ...any) *httpError {
	return &httpError{code: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

// handler serves the API. roots are resolved (symlink-free) absolute paths.
type handler struct {
	st    *store.Store
	m     *machine.Machine
	roots []string
	mux   *http.ServeMux
	// grants reads the approvals file the gate reads (reverify only).
	grants approvals.Grants
	// policyHash reports the policy in force (nil: none).
	policyHash func() string
	// reportDir confines the report endpoint (empty: nothing is served).
	reportDir string
	// policy and ops supply the /policy and /ops views (nil: empty object).
	policy func() PolicyView
	ops    func() OpsView
}

func newHandler(st *store.Store, m *machine.Machine, allowed []string, gateHome string) (*handler, error) {
	h := &handler{st: st, m: m, grants: approvals.Grants{Home: gateHome}}
	for _, r := range allowed {
		if !filepath.IsAbs(r) || filepath.Clean(r) != r {
			return nil, fmt.Errorf("allowed repo root %q must be an absolute clean path", r)
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		h.roots = append(h.roots, r)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", h.submit)
	mux.HandleFunc("GET /tasks", h.list)
	mux.HandleFunc("GET /tasks/{id}", h.get)
	mux.HandleFunc("POST /tasks/{id}/cancel", h.cancel)
	mux.HandleFunc("POST /tasks/{id}/retry", h.retry)
	mux.HandleFunc("GET /approvals", h.approvals)
	mux.HandleFunc("GET /approvals/{id}", h.approvalGet)
	mux.HandleFunc("POST /approvals/{id}/reverify", h.reverify)
	h.mountV1(mux)
	h.mux = mux
	return h, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isV1Path(r.URL.Path) {
		v1Wrap(h.mux).ServeHTTP(w, r)
		return
	}
	h.mux.ServeHTTP(&jsonErrors{ResponseWriter: w}, r)
}

// jsonErrors turns the mux's plain-text 404/405 into the JSON error shape.
type jsonErrors struct {
	http.ResponseWriter
	swallow bool
}

func (j *jsonErrors) WriteHeader(code int) {
	ct := j.Header().Get("Content-Type")
	if (code == 404 || code == 405) && strings.HasPrefix(ct, "text/plain") {
		j.swallow = true
		j.Header().Set("Content-Type", "application/json")
		j.Header().Del("Content-Length")
		j.ResponseWriter.WriteHeader(code)
		msg := "찾을 수 없는 경로입니다"
		if code == 405 {
			msg = "허용되지 않는 메서드입니다"
		}
		b, _ := json.Marshal(map[string]string{"error": msg})
		j.ResponseWriter.Write(append(b, '\n'))
		return
	}
	j.ResponseWriter.WriteHeader(code)
}

func (j *jsonErrors) Write(b []byte) (int, error) {
	if j.swallow {
		return len(b), nil
	}
	return j.ResponseWriter.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var he *httpError
	if !errors.As(err, &he) {
		log.Printf("orchd: internal error: %v", err)
		he = &httpError{code: http.StatusInternalServerError, msg: "내부 오류"}
	}
	body := map[string]string{"error": he.msg}
	if he.v1Code != "" {
		body["code"] = he.v1Code
	}
	writeJSON(w, he.code, body)
}

func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req submitRequest
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			writeErr(w, &httpError{code: http.StatusRequestEntityTooLarge, msg: "요청이 너무 큽니다"})
		case errors.Is(err, io.EOF):
			writeErr(w, bad("요청 본문이 비어 있습니다"))
		default:
			writeErr(w, bad("잘못된 JSON: %v", err))
		}
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, &httpError{code: http.StatusRequestEntityTooLarge, msg: "요청이 너무 큽니다"})
			return
		}
		writeErr(w, bad("JSON 뒤에 불필요한 데이터가 있습니다"))
		return
	}
	t, err := h.validate(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := h.st.CreateTask(r.Context(), t)
	if err != nil {
		writeErr(w, err)
		return
	}
	got, err := h.st.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(got))
}

// validate checks a submit request and returns the task to store. Repo is
// stored symlink-resolved and base as the commit SHA it resolved to, so the
// task is pinned to what was checked.
func (h *handler) validate(req submitRequest) (store.Task, error) {
	var t store.Task
	if !agents[req.Agent] {
		return t, bad("agent는 claude 또는 codex여야 합니다")
	}
	if !classRe.MatchString(req.Class) {
		return t, bad("class는 [a-z0-9_가-힣-] 1~32자여야 합니다")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return t, bad("prompt가 비어 있습니다")
	}
	if len(req.Prompt) > MaxPromptBytes {
		return t, bad("prompt가 너무 깁니다(최대 %d바이트)", MaxPromptBytes)
	}
	if !utf8.ValidString(req.Prompt) || strings.ContainsRune(req.Prompt, 0) {
		return t, bad("prompt에 유효하지 않은 문자가 있습니다")
	}
	repo := req.Repo
	if !filepath.IsAbs(repo) {
		return t, bad("repo는 절대 경로여야 합니다")
	}
	if filepath.Clean(repo) != repo {
		return t, bad("repo 경로는 정리된 형태여야 합니다(.., 중복 /, 끝의 / 금지)")
	}
	if len(h.roots) == 0 {
		return t, &httpError{code: http.StatusForbidden, msg: "허용된 저장소 루트가 없습니다(orchd --allow-repo 설정 필요)"}
	}
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		if !h.underRoot(repo) {
			return t, &httpError{code: http.StatusForbidden, msg: "허용된 저장소 루트 밖의 경로입니다"}
		}
		return t, bad("저장소 경로가 없습니다")
	}
	if !h.underRoot(real) {
		return t, &httpError{code: http.StatusForbidden, msg: "허용된 저장소 루트 밖의 경로입니다"}
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return t, bad("저장소 경로가 디렉터리가 아닙니다")
	}
	if _, err := os.Lstat(filepath.Join(real, ".git")); err != nil {
		return t, bad("git 저장소가 아닙니다(최상위 디렉터리여야 합니다)")
	}
	if !baseRe.MatchString(req.Base) {
		return t, bad("base 형식이 올바르지 않습니다")
	}
	sha, err := gitx.Resolve(real, req.Base)
	if err != nil {
		return t, bad("base를 저장소의 커밋으로 해석할 수 없습니다: %s", req.Base)
	}
	return store.Task{Repo: real, Base: sha, Prompt: req.Prompt, Class: req.Class, Agent: req.Agent}, nil
}

func (h *handler) underRoot(p string) bool {
	for _, r := range h.roots {
		rel, err := filepath.Rel(r, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	ts, err := h.st.ListTasks(r.Context(), 200)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]TaskView, 0, len(ts))
	for _, t := range ts {
		out = append(out, viewOf(t))
	}
	writeJSON(w, 200, out)
}

func taskID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, bad("작업 ID가 올바르지 않습니다")
	}
	return id, nil
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := h.st.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &httpError{code: 404, msg: "작업을 찾을 수 없습니다"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, viewOf(t))
}

func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	switch err := h.m.Cancel(r.Context(), id); {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, &httpError{code: 404, msg: "작업을 찾을 수 없습니다"})
		return
	case errors.Is(err, machine.ErrTerminal):
		writeErr(w, &httpError{code: 409, msg: "이미 끝난 작업입니다"})
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	t, err := h.st.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, viewOf(t))
}

// retry resumes an infrastructure-failed task from the step that failed.
func (h *handler) retry(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := h.m.Retry(r.Context(), id)
	var nr *machine.NotRetryableError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, &httpError{code: 404, msg: "작업을 찾을 수 없습니다"})
	case errors.As(err, &nr):
		writeErr(w, &httpError{code: 409, msg: nr.Reason})
	case errors.Is(err, store.ErrStateMismatch):
		writeErr(w, &httpError{code: 409, msg: "작업 상태가 방금 바뀌어 다시 시도하지 않았습니다. 변경 없음"})
	case err != nil:
		writeErr(w, err)
	default:
		writeJSON(w, 200, viewOf(t))
	}
}

func (h *handler) approvals(w http.ResponseWriter, r *http.Request) {
	as, err := h.st.PendingApprovals(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]ApprovalView, 0, len(as))
	for _, a := range as {
		out = append(out, approvalView(a))
	}
	writeJSON(w, 200, out)
}

// ApprovalDetail is an approval entry with what `orch approve` needs to show it.
type ApprovalDetail struct {
	ApprovalView
	Repo      string `json:"repo"`
	TaskState string `json:"task_state"`
	// PolicyHash is the policy in force now. The CLI never trusts it by
	// itself: it only uses it to rebuild the entry's hash and compare.
	PolicyHash string `json:"policy_hash,omitempty"`

	// Signing view (grant.go fillSigning). Message is the canonical text to
	// sign, built by the server from the entry and the task with Issued; it,
	// Digest (hex SHA-256 of Message) and HashedText are empty when the entry
	// cannot be granted now. HashedText/ProtectedFiles/Truncated are the diff
	// view's protected part (protected_modified only).
	Message        string   `json:"message"`
	Digest         string   `json:"digest"`
	HashedText     string   `json:"hashed_text"`
	ProtectedFiles []string `json:"protected_files"`
	Truncated      bool     `json:"truncated"`
	Meaning        string   `json:"meaning"`
	// Action is the publish action a publish_outside_allowlist entry allows
	// (derived from its hash; empty when none matches or for other kinds).
	Action            string `json:"action,omitempty"`
	RequiresSignature bool   `json:"requires_signature"`
	Issued            string `json:"issued"`
}

func (h *handler) approvalGet(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, bad("승인 항목 ID가 올바르지 않습니다"))
		return
	}
	a, err := h.st.GetApproval(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &httpError{code: 404, msg: "승인 항목을 찾을 수 없습니다"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := h.st.GetTask(r.Context(), a.TaskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	det := ApprovalDetail{ApprovalView: approvalView(a), Repo: t.Repo, TaskState: string(t.State)}
	if h.policyHash != nil {
		det.PolicyHash = h.policyHash()
	}
	fillSigning(&det, a, t, time.Now(), h.policyHash)
	writeJSON(w, 200, det)
}

func approvalView(a store.Approval) ApprovalView {
	return ApprovalView{ID: a.ID, TaskID: a.TaskID, Kind: a.Kind, Hash: a.Hash,
		Base: a.Base, Head: a.Head, Reason: a.Reason, Status: a.Status, CreatedAt: a.CreatedAt}
}

// reverify marks the entry approved ONLY IF the grant file holds a grant for
// it (the daemon reads the file; the caller is not trusted) and then lets the
// task be re-judged. Without a grant nothing changes.
func (h *handler) reverify(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, bad("승인 항목 ID가 올바르지 않습니다"))
		return
	}
	a, err := h.st.GetApproval(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &httpError{code: 404, msg: "승인 항목을 찾을 수 없습니다"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	switch {
	case a.Kind == store.KindDestructiveBlocked:
		writeErr(w, &httpError{code: 403, msg: "destructive_blocked 항목은 승인할 수 없습니다"})
		return
	case a.Status != store.ApprovalPending:
		writeErr(w, &httpError{code: 409, msg: "대기 중인 항목이 아닙니다(상태: " + a.Status + ")"})
		return
	}
	t, err := h.st.GetTask(r.Context(), a.TaskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The entry must still describe the task as it is now.
	if t.State != store.StateNeedsApproval || t.Base != a.Base || t.HeadSHA != a.Head {
		writeErr(w, &httpError{code: 409, msg: "작업의 현재 상태·base·head와 이 항목이 맞지 않습니다. 변경 없음"})
		return
	}
	v, err := h.judgeGranted(r.Context(), a, t)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}
