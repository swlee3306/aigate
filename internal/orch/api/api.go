// Package api is the control surface of orchd: a JSON API served over a unix
// socket and, when Config.HTTPListen is set, a loopback HTTP listener behind
// `tailscale serve` with the security middleware of secure.go. Every request except
// GET /api/v1/healthz, the SSH login POSTs /api/v1/auth/challenge and
// /api/v1/auth/token (login_ssh.go) and the passkey registration POSTs
// /api/v1/auth/webauthn/register/{begin,finish} (webauthn_register.go, gated
// by a one-time code from the socket-only /api/v1/admin/enroll) is
// authenticated before any handler runs
// (auth.go): a
// socket peer must have an allowlisted uid (Config.SocketUIDs, always plus the
// daemon's own uid) on top of the socket file mode (0600) and its 0700
// directory; an HTTP caller needs a bearer token or a session cookie. Writes
// need the write tier (socket, token, or session + CSRF header).
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
	"bytes"
	"context"
	"crypto/subtle"
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
	"sync"
	"time"
	"unicode/utf8"

	"aigate/internal/gitx"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/clone"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/refine"
	"aigate/internal/orch/store"
)

const (
	// MaxBodyBytes caps a request body.
	MaxBodyBytes = 1 << 20
	// MaxPromptBytes caps the prompt of one task.
	MaxPromptBytes = 64 << 10
	// DefaultQueueLimit is how many tasks one principal may have waiting in
	// StateQueued at once when Config.QueueLimit is 0.
	DefaultQueueLimit = 20
	// DefaultQueueLimitTotal is how many tasks may wait in StateQueued in
	// all, whoever made them, when Config.QueueLimitTotal is 0.
	DefaultQueueLimitTotal = 100
)

// errQueueFull is the submit refusal when the queue is at its limit.
// The two queue_full refusals differ only in their fixed message, which
// never names an actor or a count.
var (
	errQueueFull      = apiErr(http.StatusTooManyRequests, "queue_full", "내 대기 작업이 가득 찼습니다")
	errQueueFullTotal = apiErr(http.StatusTooManyRequests, "queue_full", "서버 대기열이 가득 찼습니다")
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
	// Cancellable: `orch cancel` is allowed now. Computed from machine.Allowed,
	// the table machine.Cancel itself checks, so the two cannot disagree.
	Cancellable bool `json:"cancellable,omitempty"`
	// Retries is how many times an operator retried this task.
	Retries int `json:"retries,omitempty"`
	// Actor is who made the task's latest change ("kind:id" or "system";
	// empty when that event predates schema v7).
	Actor string `json:"actor,omitempty"`
	// Merge request of a published task (schema v8): the pushed branch, the
	// Draft MR's address and number, its state ("created", "failed",
	// "skipped"; absent before the step ran) and, for a failed one, the fixed
	// failure code. Never the broker's or GitLab's own text.
	Branch  string `json:"branch,omitempty"`
	MRURL   string `json:"mr_url,omitempty"`
	MRIID   int64  `json:"mr_iid,omitempty"`
	MRState string `json:"mr_state,omitempty"`
	MRCode  string `json:"mr_code,omitempty"`
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
		Retryable: machine.Retryable(t) == nil, Cancellable: machine.CanTransition(t.State, machine.StateCancelled), Retries: t.Retries, Actor: t.LastActor,
		Branch: t.Branch, MRURL: t.MRURL, MRIID: t.MRIID, MRState: t.MRState, MRCode: t.MRCode}
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
	// reason, when set, is a server-built fixed sentence that rides next to
	// the message (v1: error.reason; legacy flat body: "reason").
	reason string
	// detail, when set, is a server-built object of public values that rides
	// with the error (v1: error.detail). Only create_partial uses it.
	detail any
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
	// retryMR creates the merge request of a done task (steps.Provider.RetryMR;
	// nil when the provider has none: the endpoint answers mr_not_applicable).
	retryMR func(ctx context.Context, id int64) (store.Task, error)
	// reportDir confines the report endpoint (empty: nothing is served).
	reportDir string
	// policy and ops supply the /policy and /ops views (nil: empty object).
	policy func() PolicyView
	ops    func() OpsView
	cloud  func(ctx context.Context) CloudView
	// socketUIDs is the socket peer allowlist; auth resolves HTTP credentials
	// (nil: every HTTP request is unauthenticated).
	socketUIDs map[uint32]bool
	auth       Authenticator
	// root routes the unauthenticated endpoints and sends everything else
	// through authenticate to mux.
	root *http.ServeMux
	// idemMu serialises keyed submits (one orchd per database, see lockDB),
	// so a key never yields two tasks.
	idemMu sync.Mutex
	// now is the login clock (challenge issue and expiry); nil means
	// time.Now. loginChallengeCap bounds live login challenges.
	now               func() time.Time
	loginChallengeCap int
	// Passkey enrollment (webauthn_register.go): the relying party (empty:
	// passkeys off), the pending-enrollment file and its writer lock, and the
	// cap on live enrollment codes / registration challenges.
	rpID, origin       string
	pendingFile        string
	pendingMu          sync.Mutex
	enrollChallengeCap int
	// Passkey login (webauthn_login.go): the cap on live login challenges and
	// the per-process key behind the fake credential ids offered for
	// non-signers.
	webauthnLoginCap int
	credKey          []byte
	// sessionTTL is the idle expiry a new browser session gets
	// (Config.SessionTTL, default SessionIdleTTL).
	sessionTTL time.Duration
	// onSubmitted records a task the API just created (the steps provider's
	// task_submitted ledger event, with the actor from ctx); nil: nothing.
	onSubmitted func(ctx context.Context, t store.Task)
	// queueLimit caps the tasks one principal has waiting in StateQueued and
	// queueLimitTotal all waiting tasks; a submit beyond either is 429
	// queue_full (Config.QueueLimit / QueueLimitTotal, defaults
	// DefaultQueueLimit / DefaultQueueLimitTotal).
	queueLimit, queueLimitTotal int
	// reposTimeout and reposMax bound GET /repos (0: defaultReposTimeout,
	// DefaultReposMax); tests shorten them.
	reposTimeout time.Duration
	reposMax     int
	// AI plan path (refine.go): the broker client (nil or empty Socket:
	// every refine call is broker_unavailable), the previewed selections,
	// the per-actor limiter (D8) and the collection limits (zero: the
	// defaults; tests shrink them).
	broker       *broker.Client
	refineStore  *refine.Store
	refineLimit  *actorLimiter
	previewLimit *actorLimiter
	// Registry endpoints (projects.go): separate per-actor buckets.
	proposeLimit *actorLimiter
	applyLimit   *actorLimiter
	refineOpts   refine.Options
	// Token endpoints (tokens.go).
	tokenPrepLimit, tokenApplyLimit *actorLimiter
	tokenSeams                      tokenSeams
	deployKeyPath                   string
	// Provider, group and project-creation endpoints (providers.go,
	// create_project.go): separate per-actor buckets (D9), the deploy key's
	// PRIVATE path and the pinning file (named in clone's ssh command, never
	// opened here), the registry-change hook (drops the publish resolver's
	// cache) and the clone runner (clone.Clone; tests replace it).
	w3cProposeLimit, providerLimit, groupLimit, createLimit, groupsReadLimit *actorLimiter
	sshKey, knownHosts                                                       string
	// gitPath and sshPath are resolved once at startup from fixed directories
	// ("" = missing: clone answers git_missing / ssh_missing).
	gitPath, sshPath string
	cloneLimit       *actorLimiter
	onRegistryChange func()
	cloneFn          func(ctx context.Context, o clone.Options) error
}

func newHandler(st *store.Store, m *machine.Machine, allowed []string, gateHome string) (*handler, error) {
	h := &handler{st: st, m: m, grants: approvals.Grants{Home: gateHome}, socketUIDs: socketUIDSet(nil)}
	h.loginChallengeCap = DefaultLoginChallengeCap
	h.enrollChallengeCap = DefaultEnrollChallengeCap
	h.pendingFile = DefaultPendingFile
	h.webauthnLoginCap = DefaultWebAuthnLoginChallengeCap
	h.credKey = newCredKey()
	h.sessionTTL = SessionIdleTTL
	h.queueLimit, h.queueLimitTotal = DefaultQueueLimit, DefaultQueueLimitTotal
	h.refineStore = refine.NewStore(h.clock)
	h.refineLimit = newActorLimiter(RefinePerActorHour, time.Hour)
	h.previewLimit = newActorLimiter(PreviewPerActorHour, time.Hour)
	h.proposeLimit = newActorLimiter(ProjectProposePerActorHour, time.Hour)
	h.applyLimit = newActorLimiter(ProjectApplyPerActorHour, time.Hour)
	h.tokenPrepLimit = newActorLimiter(TokenPrepPerActorHour, time.Hour)
	h.tokenApplyLimit = newActorLimiter(TokenApplyPerActorHour, time.Hour)
	h.w3cProposeLimit = newActorLimiter(W3cProposePerActorHour, time.Hour)
	h.providerLimit = newActorLimiter(ProviderWritePerActorHour, time.Hour)
	h.groupLimit = newActorLimiter(CreateGroupPerActorHour, time.Hour)
	h.createLimit = newActorLimiter(CreateProjectPerActorHour, time.Hour)
	h.groupsReadLimit = newActorLimiter(ListGroupsPerActorHour, time.Hour)
	h.cloneFn = clone.Clone
	h.cloneLimit = newActorLimiter(CloneProjectPerActorHour, time.Hour)
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
	mux.HandleFunc("POST /tasks", h.requireWrite(h.submit))
	mux.HandleFunc("GET /tasks", h.list)
	mux.HandleFunc("GET /tasks/{id}", h.get)
	mux.HandleFunc("POST /tasks/{id}/cancel", h.requireWrite(h.cancel))
	mux.HandleFunc("POST /tasks/{id}/retry", h.requireWrite(h.retry))
	mux.HandleFunc("GET /approvals", h.approvals)
	mux.HandleFunc("GET /approvals/{id}", h.approvalGet)
	mux.HandleFunc("POST /approvals/{id}/reverify", h.requireWrite(h.reverify))
	h.mountV1(mux)
	h.mux = mux
	h.root = http.NewServeMux()
	// The only unauthenticated routes: healthz, the SSH challenge login
	// (login_ssh.go), the passkey registration ceremony, which needs a
	// one-time enrollment code issued over the socket (webauthn_register.go),
	// and the passkey login with its public signer list (webauthn_login.go).
	// Everything else, other methods on these paths included, goes through
	// authenticate.
	h.root.HandleFunc("GET "+v1Prefix+"/healthz", healthz)
	h.root.HandleFunc("POST "+v1Prefix+"/auth/challenge", h.authChallenge)
	h.root.HandleFunc("POST "+v1Prefix+"/auth/token", h.authToken)
	h.root.HandleFunc("POST "+v1Prefix+"/auth/webauthn/register/begin", h.registerBegin)
	h.root.HandleFunc("POST "+v1Prefix+"/auth/webauthn/register/finish", h.registerFinish)
	h.root.HandleFunc("GET "+v1Prefix+"/auth/signers", h.authSigners)
	h.root.HandleFunc("POST "+v1Prefix+"/auth/webauthn/login/begin", h.webauthnLoginBegin)
	h.root.HandleFunc("POST "+v1Prefix+"/auth/webauthn/login/finish", h.webauthnLoginFinish)
	h.root.Handle("/", h.authenticate(mux))
	return h, nil
}

// ServeHTTP authenticates every request except GET /api/v1/healthz, the two
// SSH login POSTs (/api/v1/auth/challenge, /api/v1/auth/token), the two
// passkey registration POSTs (/api/v1/auth/webauthn/register/*), GET
// /api/v1/auth/signers and the two passkey login POSTs
// (/api/v1/auth/webauthn/login/*) before it reaches a handler (see authenticate); mutating routes add requireWrite.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every API response is private and current: never cached (Task 21).
	w.Header().Set("Cache-Control", "no-store")
	if isV1Path(r.URL.Path) {
		v1Wrap(h.root).ServeHTTP(w, r)
		return
	}
	h.root.ServeHTTP(&jsonErrors{ResponseWriter: w}, r)
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
	if he.code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	body := map[string]any{"error": he.msg}
	if he.v1Code != "" {
		body["code"] = he.v1Code
	}
	if he.reason != "" {
		body["reason"] = he.reason
	}
	if he.detail != nil {
		body["detail"] = he.detail
	}
	writeJSON(w, he.code, body)
}

// submit creates a task. With an Idempotency-Key (scoped to the principal)
// the same key and byte-identical body within IdempotencyTTL replay the task
// first created (200, Idempotent-Replay: true, the task's current view); the
// same key with another body is 409 idempotency_conflict.
func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, errBodyTooLarge)
			return
		}
		writeErr(w, bad("요청 본문을 읽을 수 없습니다"))
		return
	}
	// encoding/json silently turns invalid UTF-8 into U+FFFD, so the raw
	// body is checked first: such bytes are refused, never rewritten.
	if !utf8.Valid(raw) {
		writeErr(w, bad("요청 본문이 올바른 UTF-8이 아닙니다"))
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var req submitRequest
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			writeErr(w, bad("요청 본문이 비어 있습니다"))
		} else {
			writeErr(w, bad("잘못된 JSON: %v", err))
		}
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeErr(w, bad("JSON 뒤에 불필요한 데이터가 있습니다"))
		return
	}
	t, err := h.validate(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	if key == "" {
		h.createAndRespond(w, r, t)
		return
	}
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		writeErr(w, errUnauthenticated)
		return
	}
	scoped, bh := scopedIdemKey(p, key), bodyHash(raw)
	id, created, done := h.createKeyed(w, r, t, scoped, bh)
	// The ledger event is written after the idempotency record and outside
	// idemMu (CF-W2a-1): a slow ledger never stalls other keyed submits.
	if created {
		h.submitted(r, id, t)
	}
	if !done {
		h.respondTask(w, r, id, http.StatusCreated, false)
	}
}

// createKeyed is the part of a keyed submit that runs under idemMu: the
// replay check, the limited insert and the idempotency record. created
// reports a new task (even when a raced key made it answer with a replay);
// done means the response is already written.
func (h *handler) createKeyed(w http.ResponseWriter, r *http.Request, t store.Task, scoped, bh string) (id int64, created, done bool) {
	h.idemMu.Lock()
	defer h.idemMu.Unlock()
	if done, err := h.replay(w, r, scoped, bh); done || err != nil {
		if err != nil {
			writeErr(w, err)
		}
		return 0, false, true
	}
	id, err := h.createTask(r.Context(), t)
	if err != nil {
		writeErr(w, err)
		return 0, false, true
	}
	// Under idemMu only another process (which lockDB rules out) or an expiry
	// race can make Put fail with ErrKeyExists. Retry once when the key turns
	// out to have expired meanwhile (CF6); a live key from elsewhere wins.
	for attempt := 0; ; attempt++ {
		err = h.st.PutIdempotent(r.Context(), scoped, bh, id, IdempotencyTTL)
		if !errors.Is(err, store.ErrKeyExists) || attempt == 1 {
			break
		}
		if done, rerr := h.replay(w, r, scoped, bh); done || rerr != nil {
			if rerr != nil {
				writeErr(w, rerr)
			}
			log.Printf("orchd: idempotency key raced; task %d was created but not recorded", id)
			return id, true, true
		}
	}
	if err != nil {
		log.Printf("orchd: idempotency key for task %d not recorded: %v", id, err)
	}
	return id, true, false
}

// replay answers from a live idempotency record: done=true when it wrote the
// response (replay or conflict), false when the key is unknown.
func (h *handler) replay(w http.ResponseWriter, r *http.Request, scoped, bh string) (bool, error) {
	id, stored, ok, err := h.st.GetIdempotent(r.Context(), scoped)
	if err != nil || !ok {
		return false, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(bh)) != 1 {
		return false, errIdemConflict
	}
	h.respondTask(w, r, id, http.StatusOK, true)
	return true, nil
}

func (h *handler) createAndRespond(w http.ResponseWriter, r *http.Request, t store.Task) {
	id, err := h.createTask(r.Context(), t)
	if err != nil {
		writeErr(w, err)
		return
	}
	h.submitted(r, id, t)
	h.respondTask(w, r, id, http.StatusCreated, false)
}

// createTask inserts t unless the submitting principal (the actor of ctx)
// already has h.queueLimit queued tasks or the server holds h.queueLimitTotal
// (counts and insert are one store transaction, for keyed and unkeyed
// submits alike).
func (h *handler) createTask(ctx context.Context, t store.Task) (int64, error) {
	id, err := h.st.CreateTaskLimited(ctx, t, h.queueLimit, h.queueLimitTotal)
	switch {
	case errors.Is(err, store.ErrQueueFull):
		return 0, errQueueFull
	case errors.Is(err, store.ErrQueueFullTotal):
		return 0, errQueueFullTotal
	}
	return id, err
}

// submitted reports a newly created task (never a replay) to onSubmitted.
func (h *handler) submitted(r *http.Request, id int64, t store.Task) {
	if h.onSubmitted != nil {
		t.ID = id
		h.onSubmitted(r.Context(), t)
	}
}

func (h *handler) respondTask(w http.ResponseWriter, r *http.Request, id int64, status int, replay bool) {
	got, err := h.st.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, status, viewOf(got))
}

// validate checks a submit request and returns the task to store. Repo is
// stored symlink-resolved and base as the commit SHA it resolved to, so the
// task is pinned to what was checked.
func (h *handler) validate(req submitRequest) (store.Task, error) {
	var t store.Task
	if !agents[req.Agent] {
		return t, bad("agent는 claude 또는 codex여야 합니다")
	}
	// NOTE: this class check runs BEFORE the idempotent replay lookup, so
	// replaying an existing key whose class has since left the policy gets
	// 400 unknown_class, not the recorded task (documented, docs/16 4).
	if !classRe.MatchString(req.Class) {
		return t, bad("class는 [a-z0-9_가-힣-] 1~32자여야 합니다")
	}
	// R3: only a policy with a class list restricts the class. The class
	// already matched classRe, so echoing it is safe.
	if h.policy != nil {
		if classes := h.policy().Classes; len(classes) > 0 {
			if _, ok := classes[req.Class]; !ok {
				return t, apiErr(http.StatusBadRequest, "unknown_class", "정책에 없는 작업 종류입니다: "+req.Class)
			}
		}
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
	// R8: git must find THIS directory as the top level, without walking up
	// into an enclosing repository (an empty or broken .git would otherwise
	// make git read the outer one, and the task would bundle it). The git
	// directory itself may live elsewhere (linked worktree, gitfile).
	if top, err := gitx.TopLevel(real); err != nil || !sameReal(top, real) {
		return t, bad("git 저장소가 아닙니다(최상위 디렉터리여야 합니다)")
	}
	if !baseRe.MatchString(req.Base) {
		return t, bad("base 형식이 올바르지 않습니다")
	}
	sha, err := gitx.ResolveAt(real, req.Base)
	if err != nil {
		return t, bad("base를 저장소의 커밋으로 해석할 수 없습니다: %s", req.Base)
	}
	return store.Task{Repo: real, Base: sha, Prompt: req.Prompt, Class: req.Class, Agent: req.Agent}, nil
}

// sameReal reports whether p resolves (symlinks) to exactly real.
func sameReal(p, real string) bool {
	r, err := filepath.EvalSymlinks(p)
	return err == nil && r == real
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
	if err := h.m.Cancel(r.Context(), id); err != nil {
		writeErr(w, cancelErr(err))
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
	if err != nil {
		writeErr(w, retryErr(err))
		return
	}
	writeJSON(w, 200, viewOf(t))
}

// cancelErr maps a machine.Cancel error to its API error (fixed texts only).
func cancelErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apiErr(http.StatusNotFound, "not_found", "작업을 찾을 수 없습니다")
	case errors.Is(err, machine.ErrTerminal):
		return apiErr(http.StatusConflict, "already_final", "이미 끝난 작업입니다")
	case errors.Is(err, store.ErrStateMismatch):
		// Cancel lost its compare-and-set five times in a row.
		return apiErr(http.StatusConflict, "state_changed", "작업 상태가 계속 바뀌어 취소하지 못했습니다. 변경 없음")
	}
	return err
}

// retryErr maps a machine.Retry error to its API error. not_retryable carries
// the machine's Korean reason (a fixed sentence built from the task state).
func retryErr(err error) error {
	var nr *machine.NotRetryableError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apiErr(http.StatusNotFound, "not_found", "작업을 찾을 수 없습니다")
	case errors.As(err, &nr):
		e := apiErr(http.StatusConflict, "not_retryable", nr.Reason)
		e.reason = nr.Reason
		return e
	case errors.Is(err, store.ErrStateMismatch):
		return apiErr(http.StatusConflict, "state_changed", "작업 상태가 방금 바뀌어 다시 시도하지 않았습니다. 변경 없음")
	}
	return err
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
