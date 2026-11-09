package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/refine"
	"aigate/internal/orch/store"
)

const (
	// RefinePerActorHour is D8's per-actor limit of AI plan requests.
	RefinePerActorHour = 5
	// ProjectProposePerActorHour and ProjectApplyPerActorHour bound the
	// registry endpoints per actor (propose builds a signing message; apply
	// reaches the broker, which spends a one-time request id per signed try).
	ProjectProposePerActorHour = 60
	ProjectApplyPerActorHour   = 30
	// PreviewPerActorHour bounds previews per actor (they cost git reads and a
	// stored selection, not AI spend).
	PreviewPerActorHour = 30
	// maxRefineBody bounds a preview/refine request body (the text is at most
	// 2000 characters).
	maxRefineBody = 64 << 10
	// maxPlanBytes bounds the plan orchd passes on (the broker's limits keep
	// it far below this; a bigger reply is a broken plan).
	maxPlanBytes = 256 << 10
)

// maxPlanTokens bounds the usage numbers a plan may report.
const maxPlanTokens = 10_000_000

var (
	itemIDRe     = regexp.MustCompile(`^r[0-9]{1,2}$`)
	questionIDRe = regexp.MustCompile(`^q[0-9]{1,2}$`)
)

// The AI plan endpoints' errors: fixed codes and sentences. A broker's or an
// upstream's own text is never used.
var (
	errRefineRepoNotAllowed = apiErr(http.StatusForbidden, "repo_not_allowed", "이 저장소는 AI 전송이 허용되어 있지 않습니다")
	errBrokerUnavailable    = apiErr(http.StatusServiceUnavailable, "broker_unavailable", "AI 계획 서비스에 연결할 수 없습니다")
	errRefineRateLimited    = apiErr(http.StatusTooManyRequests, "refine_rate_limited", "AI 계획 요청 횟수 한도를 초과했습니다. 잠시 후 다시 시도하세요")
	errSelectionChanged     = apiErr(http.StatusConflict, "selection_changed", "미리보기 이후 저장소나 요청 글이 바뀌었습니다. 미리보기를 다시 하세요")
	errSelectionExpired     = apiErr(http.StatusGone, "selection_expired", "미리보기가 만료되었거나 없습니다. 미리보기를 다시 하세요")
	errRefineBudget         = apiErr(http.StatusTooManyRequests, "budget_exceeded", "AI 사용 예산 또는 호출 한도를 초과했습니다")
	errRefineUpstream       = apiErr(http.StatusBadGateway, "upstream_error", "AI 서비스 호출에 실패했습니다")
	errRefineBadPlan        = apiErr(http.StatusBadGateway, "bad_plan", "AI 응답이 계획서 형식을 지키지 않았습니다")
	errRefineBadBody        = apiErr(http.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	errRefineBadText        = apiErr(http.StatusBadRequest, "bad_request", "요구사항 글이 비었거나 너무 길거나(2000자) 허용되지 않는 문자가 있습니다")
	errRefineSecretText     = apiErr(http.StatusBadRequest, "bad_request", "요구사항 글에 비밀 값처럼 보이는 내용이 있어 보낼 수 없습니다")
	errRefineBadRepo        = apiErr(http.StatusBadRequest, "bad_request", "저장소 경로가 올바르지 않거나 읽을 수 없습니다")
	errRefineNoCommit       = apiErr(http.StatusBadRequest, "bad_request", "커밋이 없는 저장소입니다")
	errRefineSelRepo        = apiErr(http.StatusBadRequest, "bad_request", "미리보기가 이 저장소의 것이 아닙니다")
	errRefineTooBig         = apiErr(http.StatusBadRequest, "bad_request", "보낼 내용이 너무 큽니다")
	errRefineSecretFile     = apiErr(http.StatusBadRequest, "bad_request", "비밀 값처럼 보이는 내용이 있어 보낼 수 없습니다")
)

// brokerError turns a broker failure into orchd's fixed error. Only the
// code is looked at; unknown codes become upstream_error.
func brokerError(e *broker.OpError) *httpError {
	switch e.Code {
	case "repo_not_allowed":
		return errRefineRepoNotAllowed
	case "budget_exceeded":
		return errRefineBudget
	case "rate_limited":
		return errRefineRateLimited
	case "bad_plan":
		return errRefineBadPlan
	case "looks_like_secret":
		return errRefineSecretFile
	case "too_large", "bad_file", "bad_request":
		return errRefineTooBig
	case broker.CodeUnavailable, "forbidden", "busy", "unknown_op", "not_implemented":
		return errBrokerUnavailable
	}
	// upstream_error, timeout, internal_error and anything unregistered.
	return errRefineUpstream
}

// actorLimiter is D8's per-actor counter: n takes per hour per actor, an
// in-memory sliding window on the injected clock.
type actorLimiter struct {
	mu  sync.Mutex
	n   int
	win time.Duration
	m   map[string][]time.Time
}

func newActorLimiter(n int, win time.Duration) *actorLimiter {
	return &actorLimiter{n: n, win: win, m: map[string][]time.Time{}}
}

// refund removes the use recorded at time at (the call never cost anything).
func (l *actorLimiter) refund(actor string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.m[actor]
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(at) {
			l.m[actor] = append(ts[:i:i], ts[i+1:]...)
			return
		}
	}
}

// take records one use for actor and reports whether it was within the limit.
func (l *actorLimiter) take(actor string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, ts := range l.m { // drop idle actors
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.win {
			delete(l.m, k)
		}
	}
	ts := l.m[actor]
	keep := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < l.win {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.n {
		l.m[actor] = keep
		return false
	}
	l.m[actor] = append(keep, now)
	return true
}

type refineRequest struct {
	Repo        string `json:"repo"`
	Text        string `json:"text"`
	SelectionID string `json:"selection_id"`
}

type previewRequest struct {
	Repo string `json:"repo"`
	Text string `json:"text"`
}

// decodeRefineBody reads a small strict JSON body (one object, known fields,
// valid UTF-8, nothing after it). Error text is fixed.
func decodeRefineBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRefineBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return errRefineBadBody
	}
	if !utf8.Valid(raw) {
		return errRefineBadBody
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errRefineBadBody
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errRefineBadBody
	}
	return nil
}

// refineText normalises CRLF and checks the requirement (length, hostile
// characters, secret shapes).
func refineText(text string) (string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !refine.CheckRequirement(text) {
		return "", errRefineBadText
	}
	if refine.LooksLikeSecret(text) {
		return "", errRefineSecretText
	}
	return text, nil
}

// refineRepo resolves repo the way submit does (absolute and clean, real
// path under an allowed root, a work tree of its own whose git directories
// are under the roots). Fixed errors only.
func (h *handler) refineRepo(ctx context.Context, repo string) (string, error) {
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return "", errRefineBadRepo
	}
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		if !h.underRoot(repo) {
			return "", errRefineRepoNotAllowed
		}
		return "", errRefineBadRepo
	}
	if !h.underRoot(real) {
		return "", errRefineRepoNotAllowed
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", errRefineBadRepo
	}
	if _, err := os.Lstat(filepath.Join(real, ".git")); err != nil {
		return "", errRefineBadRepo
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !h.ownRepo(cctx, real) {
		return "", errRefineBadRepo
	}
	return real, nil
}

// brokerAllows asks the broker's status whether repo may be sent to the AI.
func (h *handler) brokerAllows(ctx context.Context, repo string) (bool, error) {
	if h.broker == nil || h.broker.Socket == "" {
		return false, errBrokerUnavailable
	}
	res, oerr := h.broker.Do(ctx, "status", map[string]any{})
	if oerr != nil {
		return false, brokerError(oerr)
	}
	var st struct {
		ContextRepos []string `json:"context_repos"`
	}
	if err := json.Unmarshal(res, &st); err != nil {
		return false, errBrokerUnavailable
	}
	for _, r := range st.ContextRepos {
		if r == repo {
			return true, nil
		}
	}
	return false, nil
}

func collectError(err error) error {
	switch {
	case errors.Is(err, refine.ErrNoCommit):
		return errRefineNoCommit
	default:
		return errRefineBadRepo
	}
}

type previewResponse struct {
	Allowed     bool               `json:"allowed"`
	SelectionID string             `json:"selection_id"`
	Files       []refine.FileEntry `json:"files"`
	Excluded    []refine.Excluded  `json:"excluded"`
	TotalBytes  int                `json:"total_bytes"`
}

// refinePreview is POST /api/v1/refine/preview: which files would go to the
// AI for this requirement, read from HEAD. The broker's allow list is asked
// first; for a repository outside it nothing is read and no file appears.
func (h *handler) refinePreview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := decodeRefineBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	text, err := refineText(req.Text)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !h.previewLimit.take(store.ActorFrom(r.Context()), h.clock()) {
		writeErr(w, errRefineRateLimited)
		return
	}
	repo, err := h.refineRepo(r.Context(), req.Repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	allowed, err := h.brokerAllows(r.Context(), repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusOK, previewResponse{Files: []refine.FileEntry{}, Excluded: []refine.Excluded{}})
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sel, err := refine.Collect(cctx, repo, text, h.refineOpts)
	if err != nil {
		writeErr(w, collectError(err))
		return
	}
	// What the broker will count is known here: trim the selection so a preview
	// that says "allowed" is never refused as too large.
	sel = refine.Fit(sel, repo, text)
	id, err := h.refineStore.Put(store.ActorFrom(r.Context()), repo, text, sel)
	if errors.Is(err, refine.ErrStoreFull) {
		writeErr(w, errRefineRateLimited)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, previewResponse{Allowed: true, SelectionID: id,
		Files: sel.Files, Excluded: sel.Excluded, TotalBytes: sel.TotalBytes})
}

// Plan shapes (D4), decoded strictly from the broker's result and re-encoded:
// what the console receives is exactly these fields, within these bounds.
type planItem struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

type planQuestion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type planUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type planResponse struct {
	Summary    string         `json:"summary"`
	Items      []planItem     `json:"items"`
	Questions  []planQuestion `json:"questions"`
	OutOfScope []string       `json:"out_of_scope"`
	Usage      planUsage      `json:"usage"`
}

// parsePlan decodes and re-bounds the broker's plan. Item files must be paths
// that were sent. Any deviation is bad_plan.
func parsePlan(res json.RawMessage, sent map[string]bool) (planResponse, error) {
	var p planResponse
	if len(res) > maxPlanBytes {
		return p, errRefineBadPlan
	}
	dec := json.NewDecoder(bytes.NewReader(res))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, errRefineBadPlan
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return p, errRefineBadPlan
	}
	rn := utf8.RuneCountInString
	if refine.BadText(p.Summary, true) || p.Usage.InputTokens > maxPlanTokens || p.Usage.OutputTokens > maxPlanTokens {
		return p, errRefineBadPlan
	}
	if p.Summary == "" || rn(p.Summary) > 600 || len(p.Items) < 1 || len(p.Items) > 30 ||
		len(p.Questions) > 10 || len(p.OutOfScope) > 15 || p.Usage.InputTokens < 0 || p.Usage.OutputTokens < 0 {
		return p, errRefineBadPlan
	}
	for _, it := range p.Items {
		if !itemIDRe.MatchString(it.ID) || refine.BadText(it.Title, false) || refine.BadText(it.Description, true) ||
			it.Title == "" || rn(it.Title) > 120 || rn(it.Description) > 800 || len(it.Files) > 10 {
			return p, errRefineBadPlan
		}
		for _, f := range it.Files {
			if !sent[f] {
				return p, errRefineBadPlan
			}
		}
	}
	for _, q := range p.Questions {
		if !questionIDRe.MatchString(q.ID) || refine.BadText(q.Text, false) || q.Text == "" || rn(q.Text) > 300 {
			return p, errRefineBadPlan
		}
	}
	for _, s := range p.OutOfScope {
		if refine.BadText(s, false) || rn(s) > 200 {
			return p, errRefineBadPlan
		}
	}
	// Empty lists are arrays, never null.
	for i := range p.Items {
		if p.Items[i].Files == nil {
			p.Items[i].Files = []string{}
		}
	}
	if p.Questions == nil {
		p.Questions = []planQuestion{}
	}
	if p.OutOfScope == nil {
		p.OutOfScope = []string{}
	}
	return p, nil
}

// refinePlan is POST /api/v1/refine: the previewed selection, re-read and
// compared by content hash, goes to the broker with the raw requirement. The
// broker's validated plan comes back unchanged (bounds re-checked).
func (h *handler) refinePlan(w http.ResponseWriter, r *http.Request) {
	var req refineRequest
	if err := decodeRefineBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	text, err := refineText(req.Text)
	if err != nil {
		writeErr(w, err)
		return
	}
	if h.broker == nil || h.broker.Socket == "" {
		writeErr(w, errBrokerUnavailable)
		return
	}
	ctx := r.Context()
	repo, err := h.refineRepo(ctx, req.Repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	actor := store.ActorFrom(ctx)
	held, ok := h.refineStore.Get(req.SelectionID, actor)
	if !ok {
		writeErr(w, errSelectionExpired)
		return
	}
	if held.Repo != repo {
		writeErr(w, errRefineSelRepo)
		return
	}
	if held.TextHash != refine.TextHash(text) {
		writeErr(w, errSelectionChanged)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sel, err := refine.Reread(cctx, repo, held.Paths, h.refineOpts)
	cancel()
	if err != nil {
		writeErr(w, errSelectionChanged)
		return
	}
	if subtle.ConstantTimeCompare([]byte(sel.Hash), []byte(held.Hash)) != 1 {
		writeErr(w, errSelectionChanged)
		return
	}
	files := make([]map[string]string, len(sel.Contents))
	sent := make(map[string]bool, len(sel.Contents))
	for i, f := range sel.Contents {
		files[i] = map[string]string{"path": f.Path, "content": f.Content}
		sent[f.Path] = true
	}
	args := map[string]any{"repo": repo, "requirement": text, "files": files, "selection": req.SelectionID}
	// Too big for the wire: refused before it uses a slot.
	if enc, err := broker.Encode("refine", args); err != nil || len(enc) > broker.MaxRequestBytes {
		writeErr(w, errRefineTooBig)
		return
	}
	slotAt := h.clock()
	if !h.refineLimit.take(actor, slotAt) {
		writeErr(w, errRefineRateLimited)
		return
	}
	// The call goes on even if the browser goes away: cancelling the request
	// must not close the socket early, give the slot back and leave the broker
	// (which may already be spending) unrecorded. The client's own time limit
	// bounds it.
	res, oerr := h.broker.Do(context.WithoutCancel(ctx), "refine", args)
	var plan planResponse
	var perr error
	if oerr != nil {
		perr = brokerError(oerr)
		// Only a failure that provably cost nothing gives the slot back: the
		// request never completely left (no broker, dial failure, refused
		// locally) or the broker itself refused before doing anything (busy,
		// not allowed, too large). Anything after a complete write keeps it.
		if oerr.Unsent || oerr.Code == "busy" || oerr.Code == "forbidden" || oerr.Code == broker.CodeTooLarge {
			h.refineLimit.refund(actor, slotAt)
		}
	} else {
		plan, perr = parsePlan(res, sent)
	}
	h.recordRefine(actor, repo, len(sel.Files), sel.TotalBytes, plan.Usage, perr == nil)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// recordRefine appends the refine ledger event: who, which repository, how
// many files and bytes, the token usage and the outcome. Never the
// requirement or any file content.
func (h *handler) recordRefine(actor, repo string, files, bytes int, u planUsage, ok bool) {
	home, err := gate.Home(h.grants.Home)
	if err != nil {
		log.Printf("orchd: refine not recorded in the ledger: home unknown")
		return
	}
	err = ledger.Append(policy.OrchLedgerPath(home), "refine", policy.OrchRepo, map[string]any{
		"actor": actor, "repo": repo, "files": files, "bytes": bytes,
		"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "ok": ok,
	}, h.clock().UTC())
	if err != nil {
		log.Printf("orchd: refine not recorded in the ledger")
	}
}
