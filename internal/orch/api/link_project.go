package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/keys"
	"aigate/internal/orch/store"
	"aigate/internal/registrymsg"
)

// W3e: GitHub projects, existing-repository links, and the per-repository
// deploy keys they need. Same shape as the W3c endpoints: orchd builds the
// signing message (nothing stored, nothing sent to a provider), checks that an
// apply request is self-consistent and forwards the signed change to the broker
// unchanged; the broker verifies the signature. Everything the broker or a
// provider said stays out of the answers: fixed sentences only.

// LinkAttachPerActorHour caps link_project and attach_remote TOGETHER per
// actor (one bucket). Create keeps its own 5 an hour.
const LinkAttachPerActorHour = 10

var (
	errRepoNotFound            = apiErr(http.StatusNotFound, "repo_not_found", "제공자에서 그 저장소를 찾을 수 없습니다. 이 도구는 이미 있는 저장소만 연결하며, 토큰이 볼 수 있는 저장소여야 합니다. 변경 없음")
	errAlreadyLinked           = apiErr(http.StatusConflict, "already_linked", "이미 제공자에 연결된 프로젝트이거나, 같은 원격 저장소가 다른 프로젝트에 연결되어 있습니다. 변경 없음")
	errGHRefused               = apiErr(http.StatusBadGateway, "github_refused", "GitHub이 요청을 받아들이지 않았습니다. 토큰 권한(저장소 만들기, 콘텐츠, 브랜치 보호, 배포 키)과 GitHub 설정을 확인하세요")
	errProviderRateLimited     = apiErr(http.StatusTooManyRequests, "provider_rate_limited", "제공자가 요청 횟수 한도를 알려 왔습니다. 잠시 후 다시 시도하세요")
	errNotSupportedForProvider = apiErr(http.StatusBadRequest, "not_supported_for_provider", "이 종류의 제공자에서는 지원하지 않는 작업입니다(GitHub에서는 그룹을 만들 수 없습니다. 기존 계정이나 조직을 소유자로 고르세요). 변경 없음")
	errVisibilityRequired      = apiErr(http.StatusBadRequest, "visibility_required", "공개 여부(public 또는 private)를 반드시 골라야 합니다. 변경 없음")
	errVisibilityNotAllowed    = apiErr(http.StatusBadRequest, "visibility_not_allowed", "이 제공자에서는 공개 여부를 정할 수 없습니다. GitLab 프로젝트는 항상 비공개로 만들어집니다. 변경 없음")
	errKeyExpired              = apiErr(http.StatusConflict, "key_expired", "제안할 때 만든 저장소별 배포 키가 만료되었거나(15분) 서버가 다시 시작되어 없습니다. 새로 변경을 만들어 서명하세요. 변경 없음")
	errKeyStagingFailed        = apiErr(http.StatusServiceUnavailable, "key_staging_failed", "저장소별 배포 키를 준비하지 못했습니다(키 폴더가 없거나 안전하지 않거나, 같은 이름의 키 폴더가 이미 있거나, 임시 키가 너무 많습니다). 관리자가 확인해야 합니다. 변경 없음")
	errStaticRemoteWins        = apiErr(http.StatusConflict, "static_remote_wins", "이 프로젝트는 서버 설정(--git-remote-for)에 정해진 원격이 있어 그것이 우선합니다. 등록부에 원격을 연결해도 효과가 없으므로 연결하지 않았습니다. 변경 없음")
	errKeyUnavailable          = apiErr(http.StatusServiceUnavailable, "key_unavailable", "이 프로젝트의 저장소별 배포 키 파일을 쓸 수 없습니다(없거나, 권한이 맞지 않거나, 옮기지 못했습니다). 관리자가 확인해야 합니다")
	errLinkPartial             = apiErr(http.StatusBadGateway, "link_partial", "이미 있던 저장소에 이 서버의 배포 키가 붙었을 수 있습니다. 저장소는 지우지 마세요. 제공자 화면에서 이 서버의 배포 키(제목이 orchd로 시작)만 직접 제거한 뒤 다시 시도하세요")
	errLinkUnknown             = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "원격 저장소가 연결되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 프로젝트 목록(알림 포함)과 제공자 화면의 배포 키를 확인한 뒤 필요하면 다시 시도하세요")

	// GitHub wording of the sentences that name GitLab.
	errGHUnreachable    = apiErr(http.StatusBadGateway, "provider_unreachable", "GitHub에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요")
	errGHBadURL         = apiErr(http.StatusBadRequest, "bad_url", "GitHub API 주소가 허용되지 않습니다. 주소 모양과 접속 대상을 확인하세요. 변경 없음")
	errGHOwnerUnknown   = apiErr(http.StatusNotFound, "group_unknown", "GitHub에 그런 소유자(내 계정 또는 내가 속한 조직)가 없습니다. 변경 없음")
	errGHNameTaken      = apiErr(http.StatusConflict, "project_name_taken", "같은 이름의 프로젝트가 이미 등록되어 있거나 GitHub에 같은 이름의 저장소가 있습니다. 기존 저장소는 가져오지 않습니다(연결 기능을 쓰세요). 변경 없음")
	errGHCreatePartial  = apiErr(http.StatusBadGateway, "create_partial", "GitHub에 일부만 만들어졌습니다. 일부만 만들어진 저장소는 등록되지 않아 삭제 화면으로 지울 수 없고 이 도구는 삭제하지 않으니 GitHub에서 직접 정리한 뒤 다시 만드세요")
	errGHCreateUnknown  = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "저장소가 만들어졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. GitHub와 프로젝트 목록(알림 포함)을 확인한 뒤 필요하면 다시 만드세요")
	errGHProvUnknownRes = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 제공자 목록을 다시 읽어 확인한 뒤 필요하면 다시 변경하세요")

	// Provider-neutral wording for link and attach (the kind is not known where
	// the broker's answer is mapped).
	errNeutralUnreachable = apiErr(http.StatusBadGateway, "provider_unreachable", "제공자에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요")
)

// providerWording picks the sentence for the provider kind named by kind:
// "github" for GitHub, "neutral" for link/attach, anything else keeps he.
func providerWording(kind string, he *httpError) *httpError {
	switch kind {
	case "github":
		switch he {
		case errGLUnreachable:
			return errGHUnreachable
		case errBadURL:
			return errGHBadURL
		case errGroupUnknown:
			return errGHOwnerUnknown
		case errNameTaken:
			return errGHNameTaken
		case errCreatePartial:
			return errGHCreatePartial
		case errCreateUnknown:
			return errGHCreateUnknown
		case errProvUnknownRes:
			return errGHProvUnknownRes
		}
	case "neutral":
		if he == errGLUnreachable {
			return errNeutralUnreachable
		}
	}
	return he
}

// fieldKind is what the change itself says about its provider kind: set_provider
// carries it, a create with `visibility` is a GitHub create, link and attach
// use neutral wording.
func fieldKind(c registryChange) string {
	switch c.Action {
	case registrymsg.ActionSetProvider:
		if k, _ := c.Fields["kind"].(string); k == "github" {
			return "github"
		}
	case registrymsg.ActionCreateProject:
		if _, gh := c.Fields["visibility"]; gh {
			return "github"
		}
	case registrymsg.ActionLinkProject, registrymsg.ActionAttachRemote:
		return "neutral"
	}
	return ""
}

// ---- the per-repository key of one flow ----

// keyOutcome tells settle how an apply ended.
type keyOutcome int

const (
	// keyKept: nothing reached the broker (or it provably did nothing): the
	// same signed change may still be applied, the staged key stays.
	keyKept keyOutcome = iota
	// keyRefused: a known refusal; nothing changed anywhere: the key is deleted.
	keyRefused
	// keyPartial: create_partial / link_partial: the provider side may hold the
	// key, the registry was not written (registering is the last step).
	keyPartial
	// keyUnknown: the outcome is unknown.
	keyUnknown
)

// keyPlan is the staged key one apply holds (staged false: a shared-key flow,
// nothing to settle).
type keyPlan struct {
	h      *handler
	staged bool
	sha    string
	id     string // project id: the key ends up in keys/<id>/
	prov   string
}

// promote moves the staged key to keys/<id>/ (only after the broker answered
// that the project is registered).
func (p *keyPlan) promote() error {
	if p == nil || !p.staged {
		return nil
	}
	if err := p.h.keys.Promote(p.sha, p.id); err != nil {
		log.Printf("orchd: the deploy key of a registered project could not be moved into place (%v)", err)
		return err
	}
	p.staged = false
	return nil
}

// settle disposes of the staged key after an apply that did not end in a fully
// trusted success: kept for refusals that cost nothing, deleted after a known
// refusal, and for a partial or unknown outcome decided by the registry itself
// (one read): registered -> moved into place (the project needs it), absent ->
// deleted (a private key is never needed without a registered project), the
// read failing -> deleted after a partial (the registry is written last, so it
// was not registered) and left to expire after an unknown outcome.
func (p *keyPlan) settle(c registryChange, o keyOutcome) {
	if p == nil || !p.staged {
		return
	}
	switch o {
	case keyKept:
		p.h.keys.Release(p.sha)
	case keyRefused:
		p.h.keys.Discard(p.sha)
		p.staged = false
	case keyPartial:
		// the registry is written last, so a partial was not registered unless the
		// registry says so
		if registered, known := p.h.projectRegistered(p.id, p.prov); known && registered {
			_ = p.promote()
		} else {
			p.h.keys.Discard(p.sha)
			p.staged = false
		}
	case keyUnknown:
		// The request was forwarded and its outcome is unknown: the key is NEVER
		// deleted here. Registered -> moved into place. Otherwise (not registered
		// yet, or the registry cannot be read) it is retained past its expiry and
		// reconciled later (reconcileKeys: promote if registered, discard once it
		// is older than the staging TTL and the registry shows no project).
		if registered, known := p.h.projectRegistered(p.id, p.prov); known && registered {
			_ = p.promote()
		} else if !p.h.keys.Retain(p.sha, p.id, p.prov) {
			p.h.keys.Release(p.sha)
		}
	}
}

// reconcileKeys settles the retained keys (outcome unknown) against the
// registry: registered -> promoted; not registered and older than the staging
// TTL -> discarded; registry unreadable -> kept. It runs lazily before a new key
// is staged. (A restart forgets them: see the operator recovery procedure.)
func (h *handler) reconcileKeys() {
	if h.keys == nil {
		return
	}
	retained := h.keys.RetainedKeys()
	if len(retained) == 0 {
		return
	}
	// ONE registry read per pass (not one per key), 20 s at most.
	rv, known := h.readProjectsOnce()
	if !known {
		return // registry unreadable: try again next time
	}
	for _, r := range retained {
		registered := registeredIn(rv, r.ID, r.Provider)
		switch {
		case registered:
			if err := h.keys.Promote(r.SHA, r.ID); err != nil {
				log.Printf("orchd: a retained deploy key could not be moved into place (%v)", err)
			}
		case r.Age >= keys.StagingTTL:
			h.keys.Discard(r.SHA)
		}
	}
}

// readProjectsOnce reads the broker's registry view with a 20 s cap; known is
// false when it could not be read.
func (h *handler) readProjectsOnce() (*broker.RegistryView, bool) {
	if h.broker == nil || h.broker.Socket == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rv, oerr := h.broker.Projects(ctx)
	if oerr != nil || rv == nil {
		return nil, false
	}
	return rv, true
}

// registeredIn: project id is provider-linked to prov with a remote in this view.
// NOTE (narrow race, accepted): the registry does not carry the key's hash, so
// "registered" means "this id is linked to this provider", not "with THIS key".
// A retained key whose id was registered by a DIFFERENT, later apply would be
// promoted under that id; that needs a second apply for the same id to succeed
// while the first was of unknown outcome, which the broker's per-id/remote locks
// and the request-ID burn make a narrow window, and the staged key is still the
// one whose deploy-key line was sent for that id.
func registeredIn(rv *broker.RegistryView, id, prov string) bool {
	for _, p := range rv.Projects {
		if p.ID == id {
			return p.Provider != nil && *p.Provider == prov && p.Remote != nil
		}
	}
	return false
}

// stageKey is the propose step for a GitHub flow: it makes a NEW key pair in
// staging (nothing in the registry, nothing sent to the provider), puts the
// public key's hash into the change and returns the key as the response shows it.
func (h *handler) stageKey(c *registryChange) (map[string]any, error) {
	if h.keys == nil {
		return nil, errKeyStagingFailed
	}
	h.reconcileKeys()
	k, err := h.keys.Stage()
	if err != nil {
		log.Printf("orchd: staging a deploy key failed: %v", err)
		return nil, errKeyStagingFailed
	}
	// the same rule the shared key's line must pass: nothing is signed that the
	// broker would refuse
	if !brokerDeployKeyRE.MatchString(k.Public) {
		h.keys.Discard(k.SHA256)
		return nil, errKeyStagingFailed
	}
	c.Fields["deploy_key_sha256"] = k.SHA256
	if _, err := registrymsg.CanonicalChange(c.Action, c.Subject, c.Fields); err != nil {
		h.keys.Discard(k.SHA256)
		return nil, errProjBadBody
	}
	return map[string]any{"deploy_key": map[string]string{"public_key": k.Public, "fingerprint": k.Fingerprint}}, nil
}

// sharedKeyExtra is the propose step for a GitLab flow: the hash of the shared
// deploy key's public line, exactly as W3c did it.
func (h *handler) sharedKeyExtra(c *registryChange) (string, map[string]any, error) {
	line, fp, ok := h.currentDeployKey()
	if !ok {
		return "", nil, errDeployKeyGone
	}
	c.Fields["deploy_key_sha256"] = keyHash(line)
	if _, err := registrymsg.CanonicalChange(c.Action, c.Subject, c.Fields); err != nil {
		return "", nil, errProjBadBody
	}
	return fp, map[string]any{"deploy_key": map[string]string{"public_key": line, "fingerprint": fp}}, nil
}

// applyKey is the apply step: it decides which public key the broker is given
// and checks it is the signed one. GitHub: the staged key found by the signed
// hash (never by anything that names a path), re-read and re-hashed; GitLab: the
// shared key's current .pub, re-hashed. Nothing is sent here.
func (h *handler) applyKey(c registryChange, args map[string]any, github bool, plan *keyPlan) error {
	want, _ := c.Fields["deploy_key_sha256"].(string)
	if github {
		if h.keys == nil {
			return errKeyStagingFailed
		}
		if h.keys.Exists(c.Subject) {
			// a key directory of this project exists: the project was registered by
			// an earlier apply (a second apply of the same change lands here)
			return errProjExists
		}
		k, err := h.keys.Staged(want)
		switch {
		case err == nil:
		case errors.Is(err, keys.ErrMismatch):
			return errKeyMismatch
		case errors.Is(err, keys.ErrHeld):
			return errProjReused // the same signed change is being applied right now
		default: // unknown (swept, restarted) or expired
			return errKeyExpired
		}
		plan.staged, plan.sha, plan.id = true, want, c.Subject
		args["deploy_key"] = k.Public
		return nil
	}
	line, _, ok := h.currentDeployKey()
	if !ok {
		return errDeployKeyGone
	}
	if keyHash(line) != want {
		return errKeyMismatch
	}
	args["deploy_key"] = line
	return nil
}

// staticPathTaken reports whether the folder a NEW project of this id would get
// under an allowed repository root is named by a static --git-remote-for entry:
// the static remote would win over the registry for it, so creating or linking
// a project there is refused before anything is signed.
func (h *handler) staticPathTaken(id string) bool {
	for _, r := range h.roots {
		if h.staticRepos[filepath.Clean(filepath.Join(r, id))] {
			return true
		}
	}
	return false
}

// ---- registry facts for link and attach ----

func (h *handler) readRegistry(ctx context.Context) (*broker.RegistryView, error) {
	if h.broker == nil || h.broker.Socket == "" {
		return nil, errBrokerUnavailable
	}
	rv, oerr := h.broker.Projects(ctx)
	if oerr != nil {
		return nil, projectsBrokerError(oerr, false)
	}
	return rv, nil
}

func sameRemote(kind, a, b string) bool {
	if kind == "github" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// remoteChecks is everything link and attach can decide from the request, the
// policy, orchd's own configuration and one registry read, before anything is
// signed or sent. It mirrors the broker's shape and registry rules (the broker
// still decides) and adds what only orchd knows: a static remote wins, and the
// folder of a new project must be free. It returns the provider's kind.
func (h *handler) remoteChecks(ctx context.Context, c registryChange) (string, error) {
	link := c.Action == registrymsg.ActionLinkProject
	if link {
		if h.folderTaken(c.Subject) {
			return "", errPathExists
		}
		if h.staticPathTaken(c.Subject) {
			return "", errStaticRemoteWins
		}
		if !h.hasPolicyClass(registeredClass) {
			return "", errPolicyClassMissing
		}
	}
	rv, err := h.readRegistry(ctx)
	if err != nil {
		return "", err
	}
	provID, _ := c.Fields["provider"].(string)
	var prov *broker.ProviderRow
	for i := range rv.Providers {
		if rv.Providers[i].ID == provID {
			prov = &rv.Providers[i]
		}
	}
	if prov == nil {
		return "", errProvUnknown
	}
	remotePath, _ := c.Fields["remote_path"].(string)
	if !registrymsg.ValidRemotePathForKind(prov.Kind, remotePath) {
		return "", errProjBadBody
	}
	// The project id (folder, keys/<id>, registry id) is the user's choice and independent of the
	// repository name (W3f F1b): the remote comes from the registry path, never from the id.
	var found *broker.Project
	for i := range rv.Projects {
		if rv.Projects[i].ID == c.Subject {
			found = &rv.Projects[i]
		}
	}
	if link {
		if found != nil {
			if found.Provider != nil {
				return "", errAlreadyLinked
			}
			return "", errProjExists
		}
	} else {
		switch {
		case found == nil:
			return "", errProjUnknown
		case found.Provider != nil || found.Remote != nil:
			return "", errAlreadyLinked
		case h.staticRepos[filepath.Clean(found.Path)]:
			return "", errStaticRemoteWins
		}
	}
	for _, p := range rv.Projects {
		if p.ID != c.Subject && p.Provider != nil && *p.Provider == provID && p.Remote != nil && sameRemote(prov.Kind, p.Remote.Path, remotePath) {
			return "", errAlreadyLinked
		}
	}
	return prov.Kind, nil
}

// attachPushWarn is shown, exactly once, on an attach with write permission: the
// local history of an existing project may be pushed to a remote that may be public.
const attachPushWarn = "[중요] 게시 권한이 쓰기입니다: 이 프로젝트의 로컬 저장소 기록(커밋 이력)이 이 원격으로 올라갈 수 있습니다. 원격이 공개 저장소라면 누구나 그 기록을 읽을 수 있습니다"

func remotePreview(c registryChange, kind, fp string) []string {
	const aiOn = "[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다"
	const aiOff = "이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)"
	str := func(k string) string { s, _ := c.Fields[k].(string); return s }
	pub := "쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다"
	if str("publish") == "read" {
		pub = "읽기: 배포 키는 읽기 전용으로 등록합니다(이 서버는 브랜치를 올리지 못합니다)"
	}
	keyLine := "배포 키 지문(SHA-256): " + fp + " (이 서버의 공용 배포 키 공개 키 파일 기준)"
	if kind == "github" {
		keyLine = "저장소 전용 배포 키 지문(SHA-256): " + fp + " (이 저장소 하나를 위해 지금 새로 만든 키입니다)"
	}
	keyWarn := "[중요] 이미 있는 저장소의 설정에 이 서버의 배포 키가 등록됩니다. 이 도구는 저장소를 만들거나 지우거나 다른 설정을 바꾸지 않습니다. 연결한 저장소는 이 도구로 지울 수 없고, 등록한 키는 삭제 화면의 \"배포 키만 떼기\"로 뗄 수 있습니다"
	if c.Action == registrymsg.ActionAttachRemote {
		out := []string{
			"등록된 프로젝트 '" + c.Subject + "'에 제공자 '" + str("provider") + "'의 저장소 '" + str("remote_path") + "'를 연결합니다",
			"연결한 뒤 이 프로젝트의 게시(push)와 복제는 이 원격을 씁니다. 프로젝트의 파일과 AI 전송 설정은 바뀌지 않습니다",
			"게시 권한 — " + pub,
		}
		if str("publish") == "write" {
			out = append(out, attachPushWarn)
		}
		return append(out, keyLine, keyWarn, w3cStamp)
	}
	ai := aiOff
	if v, _ := c.Fields["ai_context"].(bool); v {
		ai = aiOn
	}
	return []string{
		"제공자 '" + str("provider") + "'에 이미 있는 저장소 '" + str("remote_path") + "'를 새 프로젝트 '" + c.Subject + "'로 가져옵니다",
		ai,
		"게시 권한 — " + pub,
		keyLine,
		keyWarn,
		"등록 후 정책 종류(kind)는 registered 입니다",
		"가져온 뒤 이 서버에 자동으로 한 번 복제합니다",
		w3cStamp,
	}
}

// remotePropose is the shared body of the link and attach propose endpoints.
func (h *handler) remotePropose(w http.ResponseWriter, r *http.Request, action string) {
	var kind, fp string
	h.proposeCore(w, r, []string{action}, func(c *registryChange) (map[string]any, error) {
		k, err := h.remoteChecks(r.Context(), *c)
		if err != nil {
			return nil, err
		}
		kind = k
		if kind == "github" {
			if h.keys == nil || h.keys.Exists(c.Subject) {
				return nil, errKeyStagingFailed
			}
			return nil, nil
		}
		f, extra, err := h.sharedKeyExtra(c)
		fp = f
		return extra, err
	}, func(c *registryChange) (map[string]any, error) {
		if kind != "github" {
			return nil, nil
		}
		extra, err := h.stageKey(c)
		if err == nil {
			fp = extra["deploy_key"].(map[string]string)["fingerprint"]
		}
		return extra, err
	}, func(c registryChange) []string { return remotePreview(c, kind, fp) })
}

// linkPropose is POST /api/v1/projects/link/propose.
func (h *handler) linkPropose(w http.ResponseWriter, r *http.Request) {
	h.remotePropose(w, r, registrymsg.ActionLinkProject)
}

// attachPropose is POST /api/v1/projects/attach/propose.
func (h *handler) attachPropose(w http.ResponseWriter, r *http.Request) {
	h.remotePropose(w, r, registrymsg.ActionAttachRemote)
}

// remoteApply is the shared body of the link and attach apply endpoints.
func (h *handler) remoteApply(w http.ResponseWriter, r *http.Request, action string) {
	plan := &keyPlan{h: h}
	h.applyCore(w, r, []string{action}, func(c registryChange, args map[string]any) error {
		kind, err := h.remoteChecks(r.Context(), c)
		if err != nil {
			return err
		}
		plan.prov, _ = c.Fields["provider"].(string)
		return h.applyKey(c, args, kind == "github", plan)
	}, func(r *http.Request, c registryChange, res json.RawMessage, wall time.Time) (any, error) {
		return h.finishRemote(r, c, res, wall, plan)
	}, func(c registryChange, o keyOutcome) { plan.settle(c, o) })
}

// linkApply is POST /api/v1/projects/link/apply.
func (h *handler) linkApply(w http.ResponseWriter, r *http.Request) {
	h.remoteApply(w, r, registrymsg.ActionLinkProject)
}

// attachApply is POST /api/v1/projects/attach/apply.
func (h *handler) attachApply(w http.ResponseWriter, r *http.Request) {
	h.remoteApply(w, r, registrymsg.ActionAttachRemote)
}

// finishRemote turns the broker's ok result into the response: the project
// must be the one asked for (id, class, provider, the remote asked for up to
// GitHub's case), then its key (GitHub) is moved into place, then a new project
// is cloned once from the REGISTRY.
func (h *handler) finishRemote(r *http.Request, c registryChange, res json.RawMessage, wall time.Time, plan *keyPlan) (any, error) {
	var reply struct {
		Project broker.Project `json:"project"`
	}
	if json.Unmarshal(res, &reply) != nil {
		return nil, errLinkUnknown
	}
	v, ok := projectView(reply.Project)
	prov, _ := c.Fields["provider"].(string)
	want, _ := c.Fields["remote_path"].(string)
	if !ok || v.ID != c.Subject || v.Provider == nil || *v.Provider != prov || v.GitLab == nil || !strings.EqualFold(v.GitLab.Project, want) {
		return nil, errLinkUnknown
	}
	if c.Action == registrymsg.ActionLinkProject && v.Kind != registeredClass {
		return nil, errLinkUnknown
	}
	promoteFailed := plan.promote() != nil
	if c.Action == registrymsg.ActionAttachRemote {
		v.Cloned = h.isCloned(v.Path)
		body := map[string]any{"project": v}
		if promoteFailed {
			body["key"] = map[string]string{"status": "failed", "code": errKeyUnavailable.v1Code}
		}
		return body, nil
	}
	if promoteFailed {
		v.Cloned = h.isCloned(v.Path)
		return map[string]any{"project": v, "clone": CloneResult{Status: "failed", Code: errKeyUnavailable.v1Code}}, nil
	}
	budget := createTotalBudget - time.Since(wall)
	if budget < minAutoCloneBudget {
		budget = minAutoCloneBudget
	}
	ctx := context.WithoutCancel(r.Context())
	cv, cr, herr, ran := h.cloneProject(ctx, c.Subject, budget)
	if ran {
		h.recordClone(store.ActorFrom(r.Context()), c.Subject, herr == nil, cr.Code)
	}
	if herr == nil {
		v = cv
	} else {
		v.Cloned = h.isCloned(v.Path)
	}
	return map[string]any{"project": v, "clone": cr}, nil
}

// projectRegistered asks the registry whether project id is provider-linked to
// prov now. known is false when the registry could not be read.
func (h *handler) projectRegistered(id, prov string) (registered, known bool) {
	rv, ok := h.readProjectsOnce()
	if !ok {
		return false, false
	}
	return registeredIn(rv, id, prov), true
}
