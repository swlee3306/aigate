package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/clone"
	"aigate/internal/registrymsg"
)

// Project creation (W3c): propose and apply of the signed create_project
// change, the clone of a registered project, and the policy class a new
// project needs. The broker creates the GitLab project; orchd clones it from
// the REGISTRY entry (the provider's ssh address plus the GitLab project
// path), never from anything a request or a broker reply says.

// registeredClass is the policy class every new project's kind is: it must
// exist in the policy (the policy file itself is edited by an administrator,
// not by this endpoint).
const registeredClass = "registered"

const (
	// createClonePart is how long the whole create_project apply (broker call
	// plus the automatic clone) may take: it stays under the browser's 90 s.
	createTotalBudget = 88 * time.Second
	// minAutoCloneBudget keeps a late clone from being given no time at all.
	minAutoCloneBudget = 10 * time.Second
)

var (
	errPolicyClassMissing = apiErr(http.StatusBadRequest, "policy_class_missing", "정책에 'registered' 클래스가 없어 새 프로젝트를 만들 수 없습니다. 관리자가 정책(policy.yml)에 registered 클래스를 추가해야 합니다")
	errCloneUnavailable   = apiErr(http.StatusServiceUnavailable, "clone_unavailable", "복제 설정(배포 키와 known_hosts)이 없어 복제할 수 없습니다. 관리자가 확인해야 합니다")
	errNotClonable        = apiErr(http.StatusBadRequest, "not_clonable", "이 프로젝트는 등록부의 제공자 연결로 복제 주소를 만들 수 없습니다")
	errHostNotPinned      = apiErr(http.StatusBadRequest, "host_not_pinned", "이 GitLab 서버의 호스트 키가 고정되어 있지 않아(known_hosts) 복제하지 않았습니다. 관리자가 확인해야 합니다")
	errAlreadyCloned      = apiErr(http.StatusConflict, "already_cloned", "이 프로젝트 폴더가 이미 있어 복제하지 않았습니다")
	errCloneInProgress    = apiErr(http.StatusConflict, "clone_in_progress", "이 프로젝트를 이미 복제하는 중입니다")
	errCloneTimeout       = apiErr(http.StatusGatewayTimeout, "clone_timeout", "복제 시간이 초과되었습니다. 만들다 만 폴더는 지웠습니다. 다시 복제할 수 있습니다")
	errCloneFailed        = apiErr(http.StatusBadGateway, "clone_failed", "복제에 실패했습니다. 만들다 만 폴더는 지웠습니다. 배포 키 권한과 GitLab 연결을 확인한 뒤 다시 복제하세요")
)

// hasPolicyClass reports whether the policy in force has a class named name.
func (h *handler) hasPolicyClass(name string) bool {
	for _, n := range h.classNames() {
		if n == name {
			return true
		}
	}
	return false
}

// brokerDeployKeyRE is the broker's rule for the public key line it accepts
// (a copy; the broker decides). A key that passes the .pub reader but not this
// would only fail after somebody signed, so it is refused here first.
var brokerDeployKeyRE = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/]{16,1000}={0,2}(?: [!-~]{1,100})?$`)

// currentDeployKey reads the deploy key's public line from "<--git-ssh-key>.pub"
// (the private key is never opened) and checks it against the broker's rule.
func (h *handler) currentDeployKey() (line, fingerprint string, ok bool) {
	line, fp, ok := readDeployKey(h.deployKeyPath)
	if !ok || !brokerDeployKeyRE.MatchString(line) {
		return "", "", false
	}
	return line, fp, true
}

func keyHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

func createPreview(c registryChange, fingerprint string) []string {
	const aiOn = "[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다"
	const aiOff = "이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)"
	str := func(k string) string { s, _ := c.Fields[k].(string); return s }
	desc := str("description")
	if desc == "" {
		desc = "(없음)"
	}
	pub := "쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다"
	if str("publish") == "read" {
		pub = "읽기: 배포 키는 읽기 전용으로 등록합니다(이 서버는 브랜치를 올리지 못합니다)"
	}
	ai := aiOff
	if v, _ := c.Fields["ai_context"].(bool); v {
		ai = aiOn
	}
	return []string{
		"GitLab 제공자 '" + str("provider") + "'의 그룹 '" + str("group_path") + "'에 새 프로젝트 '" + c.Subject + "'를 만듭니다",
		"템플릿: " + str("template") + " (코드에 고정된 파일들이 첫 커밋으로 들어갑니다)",
		"설명: " + desc,
		ai,
		"게시 권한 — " + pub,
		"배포 키 지문(SHA-256): " + fingerprint + " (이 서버의 배포 키 공개 키 파일 기준)",
		"등록 후 정책 종류(kind)는 registered 입니다",
		"GitLab 프로젝트는 비공개로 만들어지고, 만든 뒤 이 도구는 삭제하지 않습니다",
		"만든 뒤 이 서버에 자동으로 한 번 복제합니다",
		w3cStamp,
	}
}

// createPropose is POST /api/v1/projects/create/propose. Read-only: nothing
// is stored and the broker is not called. A new project's kind is the policy
// class `registered`, which must exist: without it the proposal is refused
// before anything is signed. The deploy key hash is computed here from the
// .pub file, never taken from the request.
func (h *handler) createPropose(w http.ResponseWriter, r *http.Request) {
	var fp string
	h.proposeCore(w, r, []string{registrymsg.ActionCreateProject}, func(c *registryChange) (map[string]any, error) {
		if !h.hasPolicyClass(registeredClass) {
			return nil, errPolicyClassMissing
		}
		line, f, ok := h.currentDeployKey()
		if !ok {
			return nil, errDeployKeyGone
		}
		fp = f
		c.Fields["deploy_key_sha256"] = keyHash(line)
		if _, err := registrymsg.CanonicalChange(c.Action, c.Subject, c.Fields); err != nil {
			return nil, errProjBadBody
		}
		return map[string]any{"deploy_key": map[string]string{"public_key": line, "fingerprint": f}}, nil
	}, func(c registryChange) []string { return createPreview(c, fp) })
}

// createApply is POST /api/v1/projects/create/apply.
func (h *handler) createApply(w http.ResponseWriter, r *http.Request) {
	h.applyCore(w, r, []string{registrymsg.ActionCreateProject}, func(c registryChange, args map[string]any) error {
		// Local checks that cost nothing and must hold at the moment of the call
		// too: the policy class may have gone, the .pub may have been replaced
		// since the proposal.
		if !h.hasPolicyClass(registeredClass) {
			return errPolicyClassMissing
		}
		line, _, ok := h.currentDeployKey()
		if !ok {
			return errDeployKeyGone
		}
		if want, _ := c.Fields["deploy_key_sha256"].(string); keyHash(line) != want {
			return errKeyMismatch
		}
		args["deploy_key"] = line
		return nil
	}, h.finishCreate)
}

// finishCreate turns the broker's ok result into the response: it checks that
// the project is the one asked for, then clones it once from the REGISTRY.
func (h *handler) finishCreate(r *http.Request, c registryChange, res json.RawMessage, wall time.Time) (any, error) {
	var reply struct {
		Project broker.Project `json:"project"`
	}
	if json.Unmarshal(res, &reply) != nil {
		return nil, errCreateUnknown
	}
	v, ok := projectView(reply.Project)
	prov, _ := c.Fields["provider"].(string)
	group, _ := c.Fields["group_path"].(string)
	if !ok || v.ID != c.Subject || v.Kind != registeredClass || v.Provider == nil || *v.Provider != prov ||
		v.GitLab == nil || v.GitLab.Project != group+"/"+c.Subject {
		return nil, errCreateUnknown
	}
	budget := createTotalBudget - time.Since(wall)
	if budget < minAutoCloneBudget {
		budget = minAutoCloneBudget
	}
	// The project exists now whatever the clone does; the clone has its own
	// ending (a person may clone again later, with no signature).
	ctx := context.WithoutCancel(r.Context())
	cv, cr, herr := h.cloneProject(ctx, c.Subject, budget)
	if herr == nil {
		v = cv
	} else {
		v.Cloned = h.isCloned(v.Path)
	}
	return map[string]any{"project": v, "clone": cr}, nil
}

// CloneResult is how a clone ended. Status is cloned, already_cloned or
// failed; Code is ok, or the fixed code of the failure.
type CloneResult struct {
	Status string `json:"status"`
	Code   string `json:"code"`
}

// cloneProject clones the registered project id, from the registry entry only:
// the provider's ssh address and the GitLab project path give the remote, the
// entry's path (exactly one level below an allowed root, with the project id
// as its folder name) the destination. It reads the registry itself (a fresh
// read) and returns the project view, a CloneResult for the response and, on
// failure, the fixed error.
func (h *handler) cloneProject(ctx context.Context, id string, timeout time.Duration) (ProjectView, CloneResult, *httpError) {
	fail := func(he *httpError) (ProjectView, CloneResult, *httpError) {
		if he == errAlreadyCloned {
			return ProjectView{}, CloneResult{Status: "already_cloned", Code: he.v1Code}, he
		}
		return ProjectView{}, CloneResult{Status: "failed", Code: he.v1Code}, he
	}
	if h.sshKey == "" || h.knownHosts == "" {
		return fail(errCloneUnavailable)
	}
	if h.broker == nil || h.broker.Socket == "" {
		return fail(errBrokerUnavailable)
	}
	rv, oerr := h.broker.Projects(ctx)
	if oerr != nil {
		return fail(projectsBrokerError(oerr, false))
	}
	var entry *broker.Project
	for i := range rv.Projects {
		if rv.Projects[i].ID == id {
			entry = &rv.Projects[i]
		}
	}
	if entry == nil {
		return fail(errProjUnknown)
	}
	view, ok := projectView(*entry)
	if !ok {
		return fail(errProjBrokerBad)
	}
	if entry.Provider == nil || entry.GitLab == nil {
		return fail(errNotClonable)
	}
	var sshURL string
	for _, p := range rv.Providers {
		if p.ID == *entry.Provider {
			sshURL = p.SSHURL
		}
	}
	remote, ok := clone.RemoteFor(sshURL, entry.GitLab.Project)
	if !ok || !projectNameIs(entry.GitLab.Project, id) || filepath.Base(entry.Path) != id {
		return fail(errNotClonable)
	}
	if !h.directUnderRoot(entry.Path) || !safeProjectPath(entry.Path) {
		return fail(errProjBadPath)
	}
	err := h.cloneFn(ctx, clone.Options{Remote: remote, Dest: entry.Path, Roots: h.roots, SSHKey: h.sshKey, KnownHosts: h.knownHosts, Timeout: timeout})
	if err != nil {
		return fail(cloneError(err))
	}
	view.Cloned = h.isCloned(view.Path)
	return view, CloneResult{Status: "cloned", Code: "ok"}, nil
}

func projectNameIs(project, id string) bool {
	for i := len(project) - 1; i >= 0; i-- {
		if project[i] == '/' {
			return project[i+1:] == id
		}
	}
	return false
}

// cloneError maps the clone helper's sentinels to fixed refusals (git's own
// output is never available here).
func cloneError(err error) *httpError {
	switch {
	case errors.Is(err, clone.ErrHostNotPinned):
		return errHostNotPinned
	case errors.Is(err, clone.ErrExists):
		return errAlreadyCloned
	case errors.Is(err, clone.ErrInProgress):
		return errCloneInProgress
	case errors.Is(err, clone.ErrTimeout):
		return errCloneTimeout
	case errors.Is(err, clone.ErrBadPath):
		return errProjBadPath
	case errors.Is(err, clone.ErrBadRemote):
		return errNotClonable
	case errors.Is(err, clone.ErrBadConfig):
		return errCloneUnavailable
	}
	return errCloneFailed
}

// projectClone is POST /api/v1/projects/{id}/clone: clone a registered project
// that is not on this server yet. No signature: it follows an entry the
// signed registry already holds and changes nothing in it. The body, if any,
// is ignored: the remote and the folder come from the registry entry alone.
func (h *handler) projectClone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !registrymsg.ValidSubject(id) {
		writeErr(w, errProjBadBody)
		return
	}
	// Drain a bounded body so the connection is clean; its content is not used.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	_, _ = readAllDiscard(r)
	view, cr, herr := h.cloneProject(r.Context(), id, clone.DefaultTimeout)
	if herr != nil {
		writeErr(w, herr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": view, "clone": cr})
}

func readAllDiscard(r *http.Request) (int64, error) {
	var n int64
	buf := make([]byte, 1024)
	for {
		m, err := r.Body.Read(buf)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
}
