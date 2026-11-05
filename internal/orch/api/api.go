// Package api is the local control surface of orchd: a small JSON API served
// over a unix socket. There is no authentication beyond the socket file mode
// (0600, owned by the orchd user) and the 0700 directory around it; anyone who
// can open the socket can submit and cancel tasks. Do not expose it elsewhere.
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
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, Error: t.ErrorText, HeadSHA: t.HeadSHA, Verdict: t.Verdict}
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
}

func (e *httpError) Error() string { return e.msg }

func bad(format string, a ...any) *httpError {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

// handler serves the API. roots are resolved (symlink-free) absolute paths.
type handler struct {
	st    *store.Store
	m     *machine.Machine
	roots []string
	mux   *http.ServeMux
}

func newHandler(st *store.Store, m *machine.Machine, allowed []string) (*handler, error) {
	h := &handler{st: st, m: m}
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
	mux.HandleFunc("GET /approvals", h.approvals)
	h.mux = mux
	return h, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		he = &httpError{http.StatusInternalServerError, "내부 오류"}
	}
	writeJSON(w, he.code, map[string]string{"error": he.msg})
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
			writeErr(w, &httpError{http.StatusRequestEntityTooLarge, "요청이 너무 큽니다"})
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
			writeErr(w, &httpError{http.StatusRequestEntityTooLarge, "요청이 너무 큽니다"})
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
		return t, &httpError{http.StatusForbidden, "허용된 저장소 루트가 없습니다(orchd --allow-repo 설정 필요)"}
	}
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		if !h.underRoot(repo) {
			return t, &httpError{http.StatusForbidden, "허용된 저장소 루트 밖의 경로입니다"}
		}
		return t, bad("저장소 경로가 없습니다")
	}
	if !h.underRoot(real) {
		return t, &httpError{http.StatusForbidden, "허용된 저장소 루트 밖의 경로입니다"}
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
		writeErr(w, &httpError{404, "작업을 찾을 수 없습니다"})
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
		writeErr(w, &httpError{404, "작업을 찾을 수 없습니다"})
		return
	case errors.Is(err, machine.ErrTerminal):
		writeErr(w, &httpError{409, "이미 끝난 작업입니다"})
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

func (h *handler) approvals(w http.ResponseWriter, r *http.Request) {
	as, err := h.st.PendingApprovals(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]ApprovalView, 0, len(as))
	for _, a := range as {
		out = append(out, ApprovalView{ID: a.ID, TaskID: a.TaskID, Kind: a.Kind, Hash: a.Hash,
			Base: a.Base, Head: a.Head, Reason: a.Reason, Status: a.Status, CreatedAt: a.CreatedAt})
	}
	writeJSON(w, 200, out)
}
