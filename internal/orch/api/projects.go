package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
	"aigate/internal/registrymsg"
)

// Project registry endpoints (W3a). The registry lives in the broker; orchd
// reads it, proposes changes (read-only, nothing stored) and forwards a signed
// change to the broker. orchd never judges a signature: the broker verifies it
// against the signer list and checks the change again. What orchd does check
// is that the request is self-consistent: it recomputes the change hash from
// the fields itself and refuses a request whose change differs.
//
// The older GET /api/v1/projects (tasks grouped by repository) is unchanged;
// the registry list is GET /api/v1/projects/registry.

// Patterns of a project id and a kind; the browser carries copies (checked by
// TestConstantsAgreeWithWeb) and registrymsg is the authority.
const (
	subjectPattern = `^[a-z][a-z0-9-]{0,39}$`
	kindPattern    = `^[a-z][a-z0-9_-]{0,31}$`
)

const (
	// maxProjectsBody bounds a propose/apply body.
	maxProjectsBody = 64 << 10
	// Caps of the signature parts orchd forwards.
	maxSigSigner = 128
	maxSigAlg    = 32
	maxSigBytes  = 16 << 10
	maxSigPart   = 8 << 10
	maxPathBytes = 4096
	// registryTimeFormat is the issued layout of the signed message.
	registryTimeFormat = "2006-01-02T15:04:05Z"
)

// Fixed refusals. No broker text, path or hostile input is ever echoed.
var (
	errProjBadBody        = apiErr(http.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다(알 수 없는 필드, 중복 키, 허용되지 않는 문자, 너무 긴 값 포함)")
	errProjBadPath        = apiErr(http.StatusBadRequest, "path_not_allowed", "이 경로는 프로젝트로 등록할 수 없습니다. 허용된 저장소 루트 바로 아래의 git 저장소만 등록할 수 있습니다")
	errProjUnknownKind    = apiErr(http.StatusBadRequest, "bad_request", "정책에 없는 종류(kind)입니다. 정책의 클래스 이름 중에서 고르세요")
	errProjNotSigned      = apiErr(http.StatusBadRequest, "not_signed", "서명이 없는 변경 요청입니다")
	errProjChangeMismatch = apiErr(http.StatusBadRequest, "change_mismatch", "서명한 변경 내용과 요청 내용이 다릅니다. 변경 없음")
	errProjNotSupported   = apiErr(http.StatusBadRequest, "not_supported", "이 버전에서는 지원하지 않는 변경입니다(AI 전송 허용 여부만 바꿀 수 있습니다)")
	errProjBadSignature   = apiErr(http.StatusForbidden, "bad_signature", "서명이 올바르지 않습니다. 변경 없음")
	errProjUnknownSigner  = apiErr(http.StatusForbidden, "unknown_signer", "등록되지 않은 서명자의 서명입니다. 변경 없음")
	errProjStale          = apiErr(http.StatusForbidden, "stale_signature", "서명이 만료되었거나 시각이 맞지 않습니다. 다시 변경을 만들어 서명하세요. 변경 없음")
	errProjSignersGone    = apiErr(http.StatusServiceUnavailable, "signers_unavailable", "서명자 목록을 사용할 수 없어 변경을 처리하지 못했습니다. 관리자가 확인해야 합니다")
	errProjReused         = apiErr(http.StatusConflict, "request_reused", "이미 사용된 요청입니다. 새로 변경을 만들어 다시 서명하세요")
	errProjExists         = apiErr(http.StatusConflict, "project_exists", "같은 id 또는 같은 경로의 프로젝트가 이미 등록되어 있습니다. 변경 없음")
	errProjUnknown        = apiErr(http.StatusNotFound, "project_unknown", "등록되지 않은 프로젝트입니다. 변경 없음")
	errProjRegistryGone   = apiErr(http.StatusServiceUnavailable, "registry_unavailable", "프로젝트 등록부를 사용할 수 없습니다. 관리자가 확인해야 합니다")
	errProjBrokerBad      = apiErr(http.StatusBadGateway, "broker_error", "프로젝트 등록부 서비스가 예상하지 못한 응답을 했습니다")
	errProjUnknownResult  = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 프로젝트 목록을 다시 읽어 확인한 뒤 필요하면 다시 변경하세요")
	errProjRateLimited    = apiErr(http.StatusTooManyRequests, "project_rate_limited", "프로젝트 변경 요청 횟수 한도를 초과했습니다. 잠시 후 다시 시도하세요")
	errProjListFailed     = apiErr(http.StatusServiceUnavailable, "repos_unavailable", "저장소 목록을 지금 읽을 수 없습니다. 잠시 후 다시 시도하세요")
)

// projectsBrokerError maps a broker failure to orchd's fixed refusal (D5).
// Only the code is looked at. write says the call was a registry write: then
// a failure that may have happened is outcome_unknown, never a plain retry.
func projectsBrokerError(e *broker.OpError, write bool) *httpError {
	if write && e.UnknownOutcome() {
		return errProjUnknownResult
	}
	switch e.Code {
	case "not_signed":
		return errProjNotSigned
	case "bad_signature":
		return errProjBadSignature
	case "unknown_signer":
		return errProjUnknownSigner
	case "stale_signature":
		return errProjStale
	case "signers_unavailable":
		return errProjSignersGone
	case "request_reused":
		return errProjReused
	case "change_mismatch":
		return errProjChangeMismatch
	case "project_exists":
		return errProjExists
	case "project_unknown":
		return errProjUnknown
	case "path_not_allowed":
		return errProjBadPath
	case "registry_unavailable":
		if write {
			return errProjUnknownResult // may already be applied (failed dir fsync after rename)
		}
		return errProjRegistryGone
	case "not_supported":
		return errProjNotSupported
	case "bad_request", broker.CodeTooLarge:
		return errProjBadBody
	case broker.CodeUnavailable, "forbidden", "busy", "unknown_op", "not_implemented":
		return errBrokerUnavailable
	}
	// timeout on a read, internal_error, anything unregistered. A WRITE that
	// got an answer orchd cannot interpret (bad_reply, internal_error, an
	// unregistered code) may still have been applied: unknown outcome.
	if write {
		return errProjUnknownResult
	}
	return errProjBrokerBad
}

// hostileText reports a string a person could be misled by on a signing
// screen: control characters, bidirectional controls, zero-width and other
// format characters, line/paragraph separators, private use and non-characters.
func hostileText(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) ||
			unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Co, r) || r == utf8.RuneError ||
			r == 0x2028 || r == 0x2029 || (r >= 0xfdd0 && r <= 0xfdef) || r&0xfffe == 0xfffe {
			return true
		}
	}
	return false
}

// safeProjectPath admits the paths a signing screen may show: printable ASCII
// and Hangul syllables only (no NBSP or other spaces besides ASCII space, no
// combining marks, variation selectors, zero-width or bidirectional
// characters), at most maxPathBytes.
func safeProjectPath(s string) bool {
	if s == "" || len(s) > maxPathBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !(r >= 0x20 && r < 0x7f) && !(r >= 0xAC00 && r <= 0xD7A3) {
			return false
		}
	}
	return true
}

// objectKeys splits a JSON object into its raw values by key. Only the
// allowed keys, spelled exactly (encoding/json would match "Path" to "path"),
// each at most once, and nothing after the object.
func objectKeys(raw []byte, allowed ...string) (map[string]json.RawMessage, bool) {
	out, ok := scanObject(raw, allowed...)
	if !ok {
		return nil, false
	}
	return out, true
}

// scanObject is objectKeys that, on failure, still returns the values it had
// already copied, so a caller that handles secrets can wipe them.
func scanObject(raw []byte, allowed ...string) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	out := map[string]json.RawMessage{}
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return out, false
	}
	for dec.More() {
		kt, err := dec.Token()
		key, isStr := kt.(string)
		if err != nil || !isStr {
			return out, false
		}
		known := false
		for _, a := range allowed {
			known = known || a == key
		}
		if _, dup := out[key]; dup || !known {
			return out, false
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return out, false
		}
		out[key] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return out, false
	}
	_, err := dec.Token()
	return out, err == io.EOF
}

// readProjectsBody reads the capped body.
func readProjectsBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectsBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errBodyTooLarge
		}
		return nil, errProjBadBody
	}
	if !utf8.Valid(raw) {
		return nil, errProjBadBody
	}
	return raw, nil
}

// registryChange is a validated change request: the action, the project id and
// the fields as registrymsg's schema defines them.
type registryChange struct {
	Action  string
	Subject string
	Fields  map[string]any
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// parseChange validates action, subject and the raw fields: registrymsg's
// strict field parser and schema, plus the text rules a signing screen needs
// (no hostile characters, length caps). Nothing here touches the disk.
func parseChange(rawAction, rawSubject, rawFields json.RawMessage) (registryChange, error) {
	action, ok1 := rawString(rawAction)
	subject, ok2 := rawString(rawSubject)
	if !ok1 || !ok2 || hostileText(action) || hostileText(subject) {
		return registryChange{}, errProjBadBody
	}
	if action != registrymsg.ActionRegisterProject && action != registrymsg.ActionSetFlag {
		return registryChange{}, errProjBadBody
	}
	fields, err := registrymsg.ParseFields(rawFields)
	if err != nil {
		return registryChange{}, errProjBadBody
	}
	for _, v := range fields {
		if s, isStr := v.(string); isStr && (hostileText(s) || len(s) > maxPathBytes) {
			return registryChange{}, errProjBadBody
		}
	}
	if path, has := fields["path"].(string); has && !safeProjectPath(path) {
		return registryChange{}, errProjBadBody
	}
	if action == registrymsg.ActionSetFlag {
		if f, _ := fields["flag"].(string); f != "ai_context" {
			if _, err := registrymsg.CanonicalChange(action, subject, fields); err != nil {
				return registryChange{}, errProjBadBody
			}
			return registryChange{}, errProjNotSupported
		}
	}
	if _, err := registrymsg.CanonicalChange(action, subject, fields); err != nil {
		return registryChange{}, errProjBadBody
	}
	return registryChange{Action: action, Subject: subject, Fields: fields}, nil
}

func (c registryChange) hash() (string, error) {
	return registrymsg.ChangeHash(c.Action, c.Subject, c.Fields)
}

// classNames are the policy's class names, sorted (all of them).
func (h *handler) classNames() []string {
	out := []string{}
	if h.policy != nil {
		for name := range h.policy().Classes {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// kindNames are the class names a registry entry can carry as its kind.
func (h *handler) kindNames() []string {
	out := []string{}
	for _, n := range h.classNames() {
		if registrymsg.ValidKind(n) {
			out = append(out, n)
		}
	}
	return out
}

// checkRegisterTarget is the orchd side of the path and kind rules for
// register_project (the broker checks again): the kind is one of the policy's
// classes, and the path is a candidate: a clean absolute path exactly one
// level below an allowed root, a directory that is not a symlink, a git work
// tree of its own whose git directories stay under the roots (W2a /repos
// defences), with a commit.
func (h *handler) checkRegisterTarget(ctx context.Context, fields map[string]any) error {
	kind, _ := fields["kind"].(string)
	known := false
	for _, k := range h.kindNames() {
		known = known || k == kind
	}
	if !known {
		return errProjUnknownKind
	}
	path, _ := fields["path"].(string)
	if !h.candidatePath(ctx, path) {
		return errProjBadPath
	}
	return nil
}

func (h *handler) candidatePath(ctx context.Context, path string) bool {
	if path == "" || len(path) > maxPathBytes || !filepath.IsAbs(path) || filepath.Clean(path) != path || !safeProjectPath(path) {
		return false
	}
	if !h.directUnderRoot(path) {
		return false
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	// .git is a directory or a regular file (worktree/submodule pointer), never
	// a symlink: the broker no longer stats registered paths, so this is the
	// only place that refuses it.
	gi, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil || !(gi.IsDir() || gi.Mode().IsRegular()) {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, reposPerRepoTimeout)
	defer cancel()
	if !h.ownRepo(cctx, path) {
		return false
	}
	st, err := gitx.ReadRepoState(cctx, path, 1, safeRefName)
	return err == nil && st.Head != ""
}

// directUnderRoot reports whether p is exactly one level below an allowed
// root (the shape the broker's registry accepts).
func (h *handler) directUnderRoot(p string) bool {
	parent := filepath.Dir(p)
	for _, r := range h.roots {
		if parent == r && p != r {
			return true
		}
	}
	return false
}

// ---- views ----

type projectGitLab struct {
	Project   string `json:"project"`
	ProjectID *int64 `json:"project_id"`
}

type projectAutoMerge struct {
	Enabled bool `json:"enabled"`
}

// ProjectView is one registry entry as the console receives it.
type ProjectView struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	AIContext bool   `json:"ai_context"`
	// Provider is the provider id of a project created through W3c (null for a
	// project registered by path).
	Provider  *string          `json:"provider"`
	GitLab    *projectGitLab   `json:"gitlab"`
	AutoMerge projectAutoMerge `json:"auto_merge"`
	// Cloned is what orchd sees on the file system right now (a directory,
	// not a symlink, with a .git entry): the registry does not know it.
	Cloned bool `json:"cloned"`
}

// projectView re-bounds a broker entry; false when it is not a plausible one.
func projectView(p broker.Project) (ProjectView, bool) {
	if !registrymsg.ValidSubject(p.ID) || !registrymsg.ValidKind(p.Kind) || p.Path == "" || !safePath(p.Path) || hostileText(p.Path) {
		return ProjectView{}, false
	}
	v := ProjectView{ID: p.ID, Path: p.Path, Kind: p.Kind, AIContext: p.AIContext, AutoMerge: projectAutoMerge{Enabled: p.AutoMerge.Enabled}}
	if p.Provider != nil {
		if !registrymsg.ValidProviderID(*p.Provider) {
			return ProjectView{}, false
		}
		pr := *p.Provider
		v.Provider = &pr
	}
	if p.GitLab != nil {
		if p.GitLab.Project == "" || len(p.GitLab.Project) > 200 || hostileText(p.GitLab.Project) {
			return ProjectView{}, false
		}
		v.GitLab = &projectGitLab{Project: p.GitLab.Project, ProjectID: p.GitLab.ProjectID}
	}
	return v, true
}

// registryData is the whole registry read as the console sees it.
type registryData struct {
	Projects  []ProjectView
	Providers []ProviderView
	Orphans   []OrphanView
	Global    broker.AutoMergeGlobal
}

// registryList reads the registry through the broker. It is all or nothing:
// if one entry does not look like a project (bad id, kind or path text), a
// provider or an orphan record is implausible, the whole list is refused as
// broker_error rather than shown partially, because a partial list would also
// make candidates offer an already registered repository. A broker only
// reports entries it validated itself, so this signals a broken or newer
// registry that an operator should look at.
func (h *handler) registryList(ctx context.Context) (registryData, error) {
	if h.broker == nil || h.broker.Socket == "" {
		return registryData{}, errBrokerUnavailable
	}
	rv, oerr := h.broker.Projects(ctx)
	if oerr != nil {
		return registryData{}, projectsBrokerError(oerr, false)
	}
	return h.registryDataOf(rv)
}

func (h *handler) registryDataOf(rv *broker.RegistryView) (registryData, error) {
	out := registryData{Projects: make([]ProjectView, 0, len(rv.Projects)), Global: rv.AutoMergeGlobal}
	used := map[string]bool{}
	for _, p := range rv.Projects {
		v, ok := projectView(p)
		if !ok {
			return registryData{}, errProjBrokerBad
		}
		v.Cloned = h.isCloned(v.Path)
		if v.Provider != nil {
			used[*v.Provider] = true
		}
		out.Projects = append(out.Projects, v)
	}
	if len(rv.Providers) > maxProviderRows || len(rv.Orphans) > maxOrphanRows {
		return registryData{}, errProjBrokerBad
	}
	out.Providers = make([]ProviderView, 0, len(rv.Providers))
	for _, p := range rv.Providers {
		v, ok := providerView(p.ID, p.Kind, p.BaseURL, p.SSHURL, p.TokenRef)
		if !ok {
			return registryData{}, errProjBrokerBad
		}
		inUse := used[v.ID]
		v.InUse = &inUse
		out.Providers = append(out.Providers, v)
	}
	out.Orphans = make([]OrphanView, 0, len(rv.Orphans))
	for _, o := range rv.Orphans {
		v, ok := orphanView(o)
		if !ok {
			return registryData{}, errProjBrokerBad
		}
		out.Orphans = append(out.Orphans, v)
	}
	return out, nil
}

// isCloned reports whether path is a directory (not a symlink) with a .git
// entry. It looks at the file system only: no git runs.
func (h *handler) isCloned(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}

// projectsRegistry is GET /api/v1/projects/registry.
func (h *handler) projectsRegistry(w http.ResponseWriter, r *http.Request) {
	reg, err := h.registryList(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects":          reg.Projects,
		"providers":         reg.Providers,
		"orphans":           reg.Orphans,
		"auto_merge_global": map[string]any{"enabled": reg.Global.Enabled, "max_per_day": reg.Global.MaxPerDay},
	})
}

// CandidateView is one repository that can be registered.
type CandidateView struct {
	Path string `json:"path"`
	Head string `json:"head"`
}

// projectsCandidates is GET /api/v1/projects/candidates: the git work trees
// exactly one level below an allowed root that are not registered yet, found
// with the same walk and per-repository checks as GET /repos (no symlink
// followed outside the roots, git directories under the roots, a commit), and
// the policy class names a project's kind can be chosen from.
func (h *handler) projectsCandidates(w http.ResponseWriter, r *http.Request) {
	timeout := h.reposTimeout
	if timeout <= 0 {
		timeout = defaultReposTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	reg, err := h.registryList(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	registered := map[string]bool{}
	for _, p := range reg.Projects {
		registered[p.Path] = true
	}
	var paths []string
	for _, p := range h.findRepos(ctx) {
		if h.directUnderRoot(p) && safeProjectPath(p) && !registered[p] {
			paths = append(paths, p)
		}
	}
	if ctx.Err() != nil {
		writeErr(w, errProjListFailed)
		return
	}
	limit := h.reposMax
	if limit <= 0 {
		limit = DefaultReposMax
	}
	if len(paths) > limit {
		paths = paths[:limit]
	}
	views := make([]*CandidateView, len(paths))
	next := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < reposWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				rctx, rcancel := context.WithTimeout(ctx, reposPerRepoTimeout)
				if h.ownRepo(rctx, paths[j]) {
					if st, err := gitx.ReadRepoState(rctx, paths[j], 1, safeRefName); err == nil && st.Head != "" {
						views[j] = &CandidateView{Path: paths[j], Head: st.Head}
					}
				}
				rcancel()
			}
		}()
	}
	for j := range paths {
		next <- j
	}
	close(next)
	wg.Wait()
	if ctx.Err() != nil {
		writeErr(w, errProjListFailed)
		return
	}
	out := make([]CandidateView, 0, len(views))
	for _, v := range views {
		if v != nil {
			out = append(out, *v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": out, "kinds": h.kindNames()})
}

// ---- propose ----

// Fixed Korean sentences of the signing screen. The only variable parts are
// the values that passed registrymsg's schema and the hostile-text rules.
func previewSentences(c registryChange) []string {
	const aiOn = "[중요] 이 저장소의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다"
	const aiOff = "이 저장소의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)"
	const stamp = "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"
	switch c.Action {
	case registrymsg.ActionRegisterProject:
		path, _ := c.Fields["path"].(string)
		kind, _ := c.Fields["kind"].(string)
		ai, _ := c.Fields["ai_context"].(bool)
		out := []string{
			"새 프로젝트 '" + c.Subject + "'를 등록합니다",
			"저장소 경로: " + path,
			"종류(정책 클래스): " + kind,
		}
		if ai {
			out = append(out, aiOn)
		} else {
			out = append(out, aiOff)
		}
		return append(out, "GitLab 연결과 자동 병합은 바뀌지 않습니다(연결 없음, 자동 병합 꺼짐)", stamp)
	default:
		v, _ := c.Fields["value"].(bool)
		out := []string{"프로젝트 '" + c.Subject + "'의 AI 전송 허용 설정을 바꿉니다"}
		if v {
			out = append(out, aiOn)
		} else {
			out = append(out, "AI 전송 허용을 끕니다. 이후 이 저장소의 파일 내용은 AI 서비스로 전송되지 않습니다")
		}
		return append(out, "경로와 종류, GitLab 연결, 자동 병합은 바뀌지 않습니다", stamp)
	}
}

// projectsPropose is POST /api/v1/projects/propose. It is read-only: nothing
// is stored and the broker is not called. The server builds the canonical
// change, the one-time request id, the issue time and the signing message.
func (h *handler) projectsPropose(w http.ResponseWriter, r *http.Request) {
	raw, err := readProjectsBody(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	top, ok := objectKeys(raw, "action", "subject", "fields")
	if !ok || len(top) != 3 {
		writeErr(w, errProjBadBody)
		return
	}
	c, err := parseChange(top["action"], top["subject"], top["fields"])
	if err != nil {
		writeErr(w, err)
		return
	}
	// Proposing sends nothing anywhere, so the slot is never given back.
	if !h.proposeLimit.take(store.ActorFrom(r.Context()), h.clock()) {
		writeErr(w, errProjRateLimited)
		return
	}
	if c.Action == registrymsg.ActionRegisterProject {
		if err := h.checkRegisterTarget(r.Context(), c.Fields); err != nil {
			writeErr(w, err)
			return
		}
	}
	change, err := c.hash()
	if err != nil {
		writeErr(w, errProjBadBody)
		return
	}
	msg := signing.RegistryMessage{Action: c.Action, Subject: c.Subject, Change: change,
		Request: registrymsg.NewRequestID(), Issued: h.clock().UTC().Truncate(time.Second)}
	text := msg.Text()
	if text == "" {
		writeErr(w, errProjBadBody)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":  c.Action,
		"subject": c.Subject,
		"fields":  c.Fields,
		"message": text,
		"change":  change,
		"request": msg.Request,
		"issued":  msg.Issued.Format(registryTimeFormat),
		"preview": previewSentences(c),
	})
}

// ---- apply ----

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// parseSignature validates the shape of the signature and returns it as the
// broker expects it. Whether it is VALID is the broker's call.
func parseSignature(raw json.RawMessage) (*signing.Signature, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errProjNotSigned
	}
	top, ok := objectKeys(raw, "signer", "alg", "sig", "webauthn")
	if !ok {
		return nil, errProjBadBody
	}
	var sig signing.Signature
	for key, dst := range map[string]*string{"signer": &sig.Signer, "alg": &sig.Alg, "sig": &sig.Sig} {
		v, has := top[key]
		if !has {
			return nil, errProjBadBody
		}
		s, ok := rawString(v)
		if !ok {
			return nil, errProjBadBody
		}
		*dst = s
	}
	if sig.Signer == "" || sig.Alg == "" || sig.Sig == "" || len(sig.Signer) > maxSigSigner || len(sig.Alg) > maxSigAlg ||
		len(sig.Sig) > maxSigBytes || hostileText(sig.Signer) || hostileText(sig.Alg) {
		return nil, errProjBadBody
	}
	if wa, has := top["webauthn"]; has && !bytes.Equal(bytes.TrimSpace(wa), []byte("null")) {
		parts, ok := objectKeys(wa, "authenticator_data", "client_data_json")
		if !ok || len(parts) != 2 {
			return nil, errProjBadBody
		}
		ad, ok1 := rawString(parts["authenticator_data"])
		cd, ok2 := rawString(parts["client_data_json"])
		if !ok1 || !ok2 || ad == "" || cd == "" || len(ad) > maxSigPart || len(cd) > maxSigPart {
			return nil, errProjBadBody
		}
		sig.WebAuthn = &signing.WebAuthnData{AuthenticatorData: ad, ClientDataJSON: cd}
	}
	return &sig, nil
}

// projectsApply is POST /api/v1/projects/apply. The server recomputes the
// change from the fields, checks that the request is self-consistent and
// forwards the change, the request id, the issue time and the signature to the
// broker unchanged. The broker verifies the signature and the change again.
func (h *handler) projectsApply(w http.ResponseWriter, r *http.Request) {
	raw, err := readProjectsBody(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	top, ok := objectKeys(raw, "action", "subject", "fields", "change", "request", "issued", "signature")
	if !ok {
		writeErr(w, errProjBadBody)
		return
	}
	if sg, has := top["signature"]; !has || bytes.Equal(bytes.TrimSpace(sg), []byte("null")) {
		writeErr(w, errProjNotSigned)
		return
	}
	for _, k := range []string{"action", "subject", "fields", "change", "request", "issued"} {
		if _, has := top[k]; !has {
			writeErr(w, errProjBadBody)
			return
		}
	}
	c, err := parseChange(top["action"], top["subject"], top["fields"])
	if err != nil {
		writeErr(w, err)
		return
	}
	claimed, ok1 := rawString(top["change"])
	request, ok2 := rawString(top["request"])
	issuedText, ok3 := rawString(top["issued"])
	if !ok1 || !ok2 || !ok3 || !isLowerHex(claimed, 64) || !isLowerHex(request, 32) {
		writeErr(w, errProjBadBody)
		return
	}
	issued, perr := time.Parse(registryTimeFormat, issuedText)
	if perr != nil || issued.Location() != time.UTC || issued.Format(registryTimeFormat) != issuedText {
		writeErr(w, errProjBadBody)
		return
	}
	change, herr := c.hash()
	if herr != nil {
		writeErr(w, errProjBadBody)
		return
	}
	if (signing.RegistryMessage{Action: c.Action, Subject: c.Subject, Change: change, Request: request, Issued: issued}).Validate() != nil {
		writeErr(w, errProjBadBody)
		return
	}
	// The change the signer saw must be the change of the fields we hold.
	if claimed != change {
		writeErr(w, errProjChangeMismatch)
		return
	}
	sig, err := parseSignature(top["signature"])
	if err != nil {
		writeErr(w, err)
		return
	}
	if h.broker == nil || h.broker.Socket == "" {
		writeErr(w, errBrokerUnavailable)
		return
	}
	actor := store.ActorFrom(r.Context())
	slotAt := h.clock()
	if !h.applyLimit.take(actor, slotAt) {
		writeErr(w, errProjRateLimited)
		return
	}
	if c.Action == registrymsg.ActionRegisterProject {
		if err := h.checkRegisterTarget(r.Context(), c.Fields); err != nil {
			writeErr(w, err)
			return
		}
	}
	args := map[string]any{"id": c.Subject, "change": change, "request": request, "issued": issuedText, "signature": sig}
	for k, v := range c.Fields {
		args[k] = v
	}
	// The write goes on even if the browser goes away: closing the socket
	// early would leave a change that may have happened unrecorded. The
	// client's own time limit bounds it. One attempt, never retried.
	p, oerr := h.broker.Write(context.WithoutCancel(r.Context()), c.Action, args)
	if oerr != nil {
		// Only a failure that provably cost nothing gives the slot back (same
		// rule as refine): the request never completely left, or the broker
		// refused before looking at it. A refusal after the broker read the
		// signature keeps it.
		if oerr.Unsent || oerr.Code == "busy" || oerr.Code == "forbidden" {
			h.applyLimit.refund(actor, slotAt)
		}
		herr := projectsBrokerError(oerr, true)
		h.recordProjectChange(actor, c.Action, c.Subject, false, herr.v1Code)
		writeErr(w, herr)
		return
	}
	v, ok := projectView(*p)
	if ok {
		v.Cloned = h.isCloned(v.Path)
	}
	if !ok || v.ID != c.Subject {
		// The broker said ok, so the change may well have happened.
		h.recordProjectChange(actor, c.Action, c.Subject, false, errProjUnknownResult.v1Code)
		writeErr(w, errProjUnknownResult)
		return
	}
	h.recordProjectChange(actor, c.Action, c.Subject, true, "ok")
	writeJSON(w, http.StatusOK, v)
}

// recordProjectChange appends the project_change ledger event: who, which
// action, which project id, the outcome and its code. Never a path, a field
// value or any part of the signature.
func (h *handler) recordProjectChange(actor, action, subject string, ok bool, code string) {
	home, err := gate.Home(h.grants.Home)
	if err != nil {
		log.Printf("orchd: project change not recorded in the ledger: home unknown")
		return
	}
	err = ledger.Append(policy.OrchLedgerPath(home), "project_change", policy.OrchRepo, map[string]any{
		"actor": actor, "action": action, "subject": subject, "ok": ok, "code": code,
	}, h.clock().UTC())
	if err != nil {
		log.Printf("orchd: project change not recorded in the ledger")
	}
}
