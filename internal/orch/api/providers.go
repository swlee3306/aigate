package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
	"aigate/internal/registrymsg"
)

// Provider, group and project-creation endpoints (W3c). Like the W3a/W3b
// endpoints they are thin: orchd builds the signing message (propose, nothing
// stored, the broker not called), checks that an apply request is
// self-consistent (it recomputes the change hash from the fields) and forwards
// the signed change to the broker unchanged. The broker verifies the signature
// and checks the change again. orchd never judges a signature, never holds a
// GitLab token, and shows nothing the broker or GitLab said except fixed
// sentences chosen by code.

const (
	// Per-actor limits (D9). Propose builds a signing message and, for a new
	// project, reads the deploy key's .pub; the others reach the broker.
	CreateProjectPerActorHour = 5
	CreateGroupPerActorHour   = 5
	// ProviderWritePerActorHour is one bucket for set_provider and delete_provider.
	ProviderWritePerActorHour = 10
	ListGroupsPerActorHour    = 60
	// CloneProjectPerActorHour caps POST /projects/{id}/clone per actor: a
	// clone runs git and ssh for up to two minutes, so it is not free. The
	// slot is given back when the request stopped before git ran. (The
	// automatic clone after a create is covered by the create limit.)
	CloneProjectPerActorHour = 10
	W3cProposePerActorHour   = 60

	maxProviderRows = 50
	maxOrphanRows   = 100
	maxGroupRows    = 500
	maxNameBytes    = 200
)

// Fixed refusals of the W3c endpoints (the broker's own sentences are never
// shown). Statuses follow the W3a/W3b convention: 400 request problems, 403
// signature problems, 404 unknown, 409 conflicts, 429 limits, 502 upstream
// refusals, 503 unavailable, 504 unknown outcome.
var (
	errProvUnknown    = apiErr(http.StatusNotFound, "provider_unknown", "등록되지 않은 제공자입니다. 변경 없음")
	errProvExists     = apiErr(http.StatusConflict, "provider_exists", "같은 id의 제공자가 이미 있습니다. 변경 없음")
	errProvInUse      = apiErr(http.StatusConflict, "provider_in_use", "이 제공자를 쓰는 프로젝트가 있어 삭제하거나 주소를 바꿀 수 없습니다. 변경 없음")
	errBadURL         = apiErr(http.StatusBadRequest, "bad_url", "GitLab 주소가 허용되지 않습니다. 주소 모양과 접속 대상을 확인하세요. 변경 없음")
	errTokenRefGone   = apiErr(http.StatusBadRequest, "token_ref_unknown", "제공자가 쓸 토큰이 없거나, 제공자와 종류가 맞지 않거나, 쓸 수 없습니다. 토큰 화면에서 확인하세요. 변경 없음")
	errGLUnreachable  = apiErr(http.StatusBadGateway, "provider_unreachable", "GitLab에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요")
	errGLRefused      = apiErr(http.StatusBadGateway, "gitlab_refused", "GitLab이 요청을 받아들이지 않았습니다. 토큰 권한과 GitLab 설정을 확인하세요")
	errGroupUnknown   = apiErr(http.StatusNotFound, "group_unknown", "GitLab에 그런 그룹이 없습니다. 변경 없음")
	errGroupExists    = apiErr(http.StatusConflict, "group_exists", "같은 경로의 그룹이 이미 있습니다. 변경 없음")
	errNameTaken      = apiErr(http.StatusConflict, "project_name_taken", "같은 이름의 프로젝트가 이미 등록되어 있거나 GitLab에 있습니다. 기존 프로젝트는 가져오지 않습니다. 변경 없음")
	errCreatePartial  = apiErr(http.StatusBadGateway, "create_partial", "GitLab에 일부만 만들어졌습니다. 일부만 만들어진 프로젝트는 등록되지 않아 삭제 화면으로 지울 수 없고 이 도구는 삭제하지 않으니 GitLab에서 직접 정리한 뒤 다시 만드세요")
	errKeyMismatch    = apiErr(http.StatusConflict, "deploy_key_mismatch", "서명한 배포 키와 지금의 배포 키가 다릅니다. 새로 변경을 만들어 서명하세요. 변경 없음")
	errTemplateGone   = apiErr(http.StatusBadRequest, "template_unknown", "알 수 없는 템플릿입니다. 변경 없음")
	errBrokerLimited  = apiErr(http.StatusTooManyRequests, "broker_rate_limited", "서버 전체의 시간당 생성 한도를 초과했습니다. 잠시 후 다시 시도하세요")
	errProvUnknownRes = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 제공자 목록을 다시 읽어 확인한 뒤 필요하면 다시 변경하세요")
	errCreateUnknown  = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "프로젝트가 만들어졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. GitLab과 프로젝트 목록(알림 포함)을 확인한 뒤 필요하면 다시 만드세요")
)

// ---- views ----

// ProviderView is a provider as the console receives it. InUse is set in
// lists (whether a project references the provider) and absent from the
// result of a write.
type ProviderView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	BaseURL  string `json:"base_url"`
	SSHURL   string `json:"ssh_url"`
	TokenRef string `json:"token_ref"`
	InUse    *bool  `json:"in_use,omitempty"`
}

// providerView re-bounds a provider the broker reported; false when it is not
// a plausible one (the shapes are registrymsg's, the authority).
func providerView(id, kind, baseURL, sshURL, tokenRef string) (ProviderView, bool) {
	if !registrymsg.ValidProviderID(id) || !registrymsg.ValidProviderKind(kind) || !registrymsg.ValidBaseURL(baseURL) ||
		!registrymsg.ValidSSHURL(sshURL) || !registrymsg.ValidTokenRef(tokenRef) {
		return ProviderView{}, false
	}
	return ProviderView{ID: id, Kind: kind, BaseURL: baseURL, SSHURL: sshURL, TokenRef: tokenRef}, true
}

// OrphanView is a GitLab project a failed creation left behind (public values
// only). The broker never deletes it; a person cleans it up in GitLab.
type OrphanView struct {
	Provider  string `json:"provider"`
	Project   string `json:"project"`
	ProjectID *int64 `json:"project_id"`
	Step      string `json:"step"`
	At        string `json:"at"`
	Origin    string `json:"origin"` // created | linked
}

func orphanView(o broker.OrphanRow) (OrphanView, bool) {
	if !registrymsg.ValidProviderID(o.Provider) || !(broker.ValidGitLabProjectPath(o.Project) || registrymsg.ValidRemotePath(o.Project)) || !broker.ValidOrphanStep(o.Step) ||
		!broker.ValidOrphanOrigin(o.Origin) || (o.ProjectID != nil && *o.ProjectID <= 0) {
		return OrphanView{}, false
	}
	if t, err := time.Parse(registryTimeFormat, o.At); err != nil || t.Location() != time.UTC || t.Format(registryTimeFormat) != o.At {
		return OrphanView{}, false
	}
	origin := o.Origin
	if origin == "" {
		origin = broker.OriginCreated
	}
	return OrphanView{Provider: o.Provider, Project: o.Project, ProjectID: o.ProjectID, Step: o.Step, At: o.At, Origin: origin}, true
}

// GroupView is one GitLab group the token can create projects in.
type GroupView struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Kind is set for GitHub owners: user (the token's account) or org.
	Kind string `json:"kind,omitempty"`
}

// ---- broker error mapping ----

// provableRefusal is the closed set of broker codes that mean "refused before
// anything was written" (the shape-stage and provider refusals of the broker
// contract). Only these let orchd delete a staged key that was forwarded; any
// other failure of a forwarded request (request_reused included) leaves it.
var provableRefusal = map[string]bool{
	"not_signed": true, "bad_signature": true, "unknown_signer": true, "stale_signature": true, "signers_unavailable": true,
	"change_mismatch": true, "bad_request": true, "too_large": true, "path_not_allowed": true, "provider_unknown": true,
	"deploy_key_mismatch": true, "visibility_required": true, "visibility_not_allowed": true, "not_supported_for_provider": true,
	"project_exists": true, "project_unknown": true, "already_linked": true, "project_name_taken": true, "group_unknown": true,
	"repo_not_found": true, "template_unknown": true, "token_ref_unknown": true, "tokens_unavailable": true,
	"github_refused": true, "gitlab_refused": true, "provider_rate_limited": true, "rate_limited": true,
}

// w3cBrokerError maps a broker failure to orchd's fixed refusal (GitLab
// wording where the sentence names the provider). See w3cBrokerErrorKind.
func w3cBrokerError(action string, e *broker.OpError, write bool) *httpError {
	return w3cBrokerErrorKind(action, "", e, write)
}

// w3cBrokerErrorKind maps a broker failure to orchd's fixed refusal. Only the
// code (and, for create_partial/link_partial, the validated detail) is looked
// at. write says the call was a write: then a failure that may have happened is
// outcome_unknown, never a plain retry. The codes of the older ops are mapped
// by projectsBrokerError. kind picks the wording of the sentences that name
// the provider: "github" for GitHub, "neutral" for link/attach (the provider's
// kind is not known to this function), anything else is the W3c GitLab text.
func w3cBrokerErrorKind(action, kind string, e *broker.OpError, write bool) *httpError {
	return providerWording(kind, w3cBrokerErrorBase(action, kind, e, write))
}

func w3cBrokerErrorBase(action, kind string, e *broker.OpError, write bool) *httpError {
	if isDeleteAction(action) {
		return deleteBrokerError(action, e)
	}
	unknown := errProvUnknownRes
	switch action {
	case registrymsg.ActionCreateProject:
		unknown = errCreateUnknown
	case registrymsg.ActionLinkProject, registrymsg.ActionAttachRemote:
		unknown = errLinkUnknown
	}
	if write && e.UnknownOutcome() {
		return unknown
	}
	switch e.Code {
	case "provider_unknown":
		return errProvUnknown
	case "provider_exists":
		return errProvExists
	case "provider_in_use":
		return errProvInUse
	case "bad_url":
		return errBadURL
	case "token_ref_unknown":
		return errTokenRefGone
	case "provider_unreachable":
		return errGLUnreachable
	case "gitlab_refused":
		return errGLRefused
	case "github_refused":
		return errGHRefused
	case "provider_rate_limited":
		return errProviderRateLimited
	case "not_supported_for_provider":
		return errNotSupportedForProvider
	case "visibility_required":
		return errVisibilityRequired
	case "visibility_not_allowed":
		return errVisibilityNotAllowed
	case "repo_not_found":
		return errRepoNotFound
	case "already_linked":
		return errAlreadyLinked
	case "group_unknown":
		return errGroupUnknown
	case "group_exists":
		return errGroupExists
	case "project_name_taken":
		return errNameTaken
	case "deploy_key_mismatch":
		return errKeyMismatch
	case "template_unknown":
		return errTemplateGone
	case "rate_limited":
		return errBrokerLimited
	case "tokens_unavailable":
		return errTokensGone
	case broker.CodeCreatePartial:
		if write && action == registrymsg.ActionCreateProject {
			return partialErrorWith(providerWording(kind, errCreatePartial), e.Partial)
		}
	case broker.CodeLinkPartial:
		if write && (action == registrymsg.ActionLinkProject || action == registrymsg.ActionAttachRemote) {
			return linkPartialError(e.Partial)
		}
	}
	he := projectsBrokerError(e, write)
	if he == errProjUnknownResult {
		return unknown
	}
	return he
}

// partialError is create_partial with the orphan facts when the broker gave
// valid ones (public values only: no token, no provider text).
func partialError(d *broker.PartialDetail) *httpError { return partialErrorWith(errCreatePartial, d) }

// detailOrigin is the origin an origin-less broker detail means: a create made
// the repository, a link or attach only joined an existing one.
func detailOrigin(base *httpError) string {
	if base == errLinkPartial {
		return broker.OriginLinked
	}
	return broker.OriginCreated
}

// partialErrorWith is partialError with the sentence of a given provider wording.
func partialErrorWith(base *httpError, d *broker.PartialDetail) *httpError {
	he := *base
	if m := partialDetail(d, detailOrigin(base)); m != nil {
		he.detail = m
	}
	return &he
}

// linkPartialError is link_partial (a link or attach that stopped after the
// repository was read: a deploy key of this server may be attached to an
// EXISTING repository) with the same public orphan facts.
func linkPartialError(d *broker.PartialDetail) *httpError {
	he := *errLinkPartial
	if m := partialDetail(d, detailOrigin(errLinkPartial)); m != nil {
		he.detail = m
	}
	return &he
}

func partialDetail(d *broker.PartialDetail, defaultOrigin string) map[string]any {
	if d == nil {
		return nil
	}
	origin := d.Origin
	if origin == "" || !d.OriginGiven {
		origin = defaultOrigin
	}
	return map[string]any{"provider": d.Provider, "project": d.Project, "project_id": d.ProjectID, "step": d.Step,
		"orphan_recorded": d.Recorded, "orphan_dropped": d.Dropped, "origin": origin}
}

// ---- reads ----

// providersList is GET /api/v1/providers (the broker's `providers` read).
func (h *handler) providersList(w http.ResponseWriter, r *http.Request) {
	if h.broker == nil || h.broker.Socket == "" {
		writeErr(w, errBrokerUnavailable)
		return
	}
	pl, oerr := h.broker.Providers(r.Context())
	if oerr != nil {
		writeErr(w, w3cBrokerError("", oerr, false))
		return
	}
	if len(pl.Providers) > maxProviderRows {
		writeErr(w, errProjBrokerBad)
		return
	}
	out := make([]ProviderView, 0, len(pl.Providers))
	for _, p := range pl.Providers {
		v, ok := providerView(p.ID, p.Kind, p.BaseURL, p.SSHURL, p.TokenRef)
		if !ok {
			writeErr(w, errProjBrokerBad)
			return
		}
		inUse := p.InUse
		v.InUse = &inUse
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// providerGroups is GET /api/v1/providers/{id}/groups: the broker asks GitLab
// (read only) for the groups the token can create projects in. It reaches
// GitLab, so it is limited per actor (D9).
func (h *handler) providerGroups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !registrymsg.ValidProviderID(id) {
		writeErr(w, errProjBadBody)
		return
	}
	if h.broker == nil || h.broker.Socket == "" {
		writeErr(w, errBrokerUnavailable)
		return
	}
	actor := store.ActorFrom(r.Context())
	slotAt := h.clock()
	if !h.groupsReadLimit.take(actor, slotAt) {
		writeErr(w, errProjRateLimited)
		return
	}
	res, oerr := h.broker.Do(r.Context(), broker.OpListGroups, map[string]any{"provider": id})
	if oerr != nil {
		if oerr.Unsent || oerr.Code == "busy" || oerr.Code == "forbidden" {
			h.groupsReadLimit.refund(actor, slotAt)
		}
		writeErr(w, w3cBrokerError("", oerr, false))
		return
	}
	var reply struct {
		Groups    []struct{ Path, Name, Kind string } `json:"groups"`
		Truncated bool                                `json:"truncated"`
	}
	if err := json.Unmarshal(res, &reply); err != nil || len(reply.Groups) > maxGroupRows {
		writeErr(w, errProjBrokerBad)
		return
	}
	out := make([]GroupView, 0, len(reply.Groups))
	for _, g := range reply.Groups {
		if !registrymsg.ValidOwnerField(g.Path) || g.Name == "" || len(g.Name) > maxNameBytes || hostileText(g.Name) ||
			(g.Kind != "" && g.Kind != "user" && g.Kind != "org") {
			writeErr(w, errProjBrokerBad)
			return
		}
		out = append(out, GroupView{Path: g.Path, Name: g.Name, Kind: g.Kind})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "truncated": reply.Truncated})
}

// ---- the change ----

// hasServerHash says the server computes deploy_key_sha256 for the action (the
// client never sends it in a proposal).
func hasServerHash(action string) bool {
	return action == registrymsg.ActionCreateProject || action == registrymsg.ActionLinkProject || action == registrymsg.ActionAttachRemote
}

// parseW3cChange validates action, subject and the raw fields of a provider,
// group or project-creation change: registrymsg's strict field parser and
// schema, plus the text rules a signing screen needs. withHash says whether
// deploy_key_sha256 belongs to the fields (create_project: apply yes, propose
// no: the server computes it from the .pub file). Nothing here touches the
// disk or the broker.
func parseW3cChange(rawAction, rawSubject, rawFields json.RawMessage, allowed []string, withHash bool) (registryChange, error) {
	action, ok1 := rawString(rawAction)
	subject, ok2 := rawString(rawSubject)
	if !ok1 || !ok2 || hostileText(action) || hostileText(subject) {
		return registryChange{}, errProjBadBody
	}
	okAction := false
	for _, a := range allowed {
		okAction = okAction || a == action
	}
	if !okAction {
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
	check := fields
	if hasServerHash(action) && !withHash {
		if _, has := fields["deploy_key_sha256"]; has {
			return registryChange{}, errProjBadBody // the server alone computes it
		}
		check = map[string]any{}
		for k, v := range fields {
			check[k] = v
		}
		check["deploy_key_sha256"] = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	if _, err := registrymsg.CanonicalChange(action, subject, check); err != nil {
		return registryChange{}, errProjBadBody
	}
	// create_project: the signed owner field is a superset of both providers'
	// rules, and the provider's kind is not known to this pure function. A
	// change that carries `visibility` is a GitHub create (visibility is
	// required for GitHub and forbidden for GitLab: the broker refuses the
	// wrong combination in its shape stage, before any signature is looked at
	// and without burning the request id), so its owner is one GitHub login;
	// without it the W3c GitLab group-path rule applies, exactly as before.
	if action == registrymsg.ActionCreateProject {
		g, _ := fields["group_path"].(string)
		if _, gh := fields["visibility"]; gh {
			if !registrymsg.ValidOwnerForKind("github", g) {
				return registryChange{}, errProjBadBody
			}
		} else if !registrymsg.ValidOwnerForKind("gitlab", g) {
			return registryChange{}, errProjBadBody
		}
	}
	return registryChange{Action: action, Subject: subject, Fields: fields}, nil
}

const w3cStamp = "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"

// providerPreview is the fixed Korean text of the signing screen for the
// provider and group actions. The variable parts are the values that passed
// registrymsg's schema and the hostile-text rules.
func providerPreview(c registryChange) []string {
	str := func(k string) string { s, _ := c.Fields[k].(string); return s }
	switch c.Action {
	case registrymsg.ActionSetProvider:
		if str("kind") == "github" {
			return []string{
				"제공자 '" + c.Subject + "'를 저장합니다(종류: github). 같은 id가 있으면 이 값으로 바뀝니다",
				"GitHub API 주소: " + str("base_url"),
				"ssh 주소: " + str("ssh_url"),
				"사용할 토큰 이름: " + str("token_ref") + " (토큰 값은 이 화면에 나오지 않습니다)",
				"[중요] 이 토큰의 권한으로 위 주소의 GitHub에 저장소를 만들고 저장소별 배포 키를 등록합니다",
				"이 제공자를 쓰는 프로젝트가 있으면 주소는 바꿀 수 없습니다",
				w3cStamp,
			}
		}
		return []string{
			"제공자 '" + c.Subject + "'를 저장합니다(종류: " + str("kind") + "). 같은 id가 있으면 이 값으로 바뀝니다",
			"GitLab 주소: " + str("base_url"),
			"ssh 주소: " + str("ssh_url"),
			"사용할 토큰 이름: " + str("token_ref") + " (토큰 값은 이 화면에 나오지 않습니다)",
			"[중요] 이 토큰의 권한으로 위 주소의 GitLab에 그룹과 프로젝트를 만듭니다",
			"이 제공자를 쓰는 프로젝트가 있으면 주소는 바꿀 수 없습니다",
			w3cStamp,
		}
	case registrymsg.ActionDeleteProvider:
		return []string{
			"제공자 '" + c.Subject + "'를 삭제합니다",
			"이 제공자를 쓰는 프로젝트가 있으면 삭제되지 않습니다",
			"GitLab의 그룹과 프로젝트는 지워지지 않습니다",
			w3cStamp,
		}
	default: // create_group
		parent := str("parent_path")
		if parent == "" {
			parent = "(최상위)"
		}
		return []string{
			"제공자 '" + c.Subject + "'의 GitLab에 새 그룹을 만듭니다",
			"그룹 이름: " + str("name"),
			"그룹 경로: " + str("path"),
			"상위 그룹: " + parent,
			"비공개 그룹으로 만들어집니다. 이 도구는 그룹을 삭제하지 않습니다",
			w3cStamp,
		}
	}
}

var providerActions = []string{registrymsg.ActionSetProvider, registrymsg.ActionDeleteProvider, registrymsg.ActionCreateGroup}

// proposeCore is the shared body of the propose endpoints. check runs after
// the body is understood and before a slot is taken (it may add the
// server-computed fields and response members); stage, when set, runs once the
// slot is taken and before the change is hashed: it is where a per-repository
// deploy key is made (a refused or rate-limited proposal makes none).
func (h *handler) proposeCore(w http.ResponseWriter, r *http.Request, allowed []string, check func(c *registryChange) (extra map[string]any, err error),
	stage func(c *registryChange) (extra map[string]any, err error), preview func(c registryChange) []string) {
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
	c, err := parseW3cChange(top["action"], top["subject"], top["fields"], allowed, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	var extra map[string]any
	if check != nil {
		if extra, err = check(&c); err != nil {
			writeErr(w, err)
			return
		}
	}
	// Proposing sends nothing anywhere, so the slot is never given back.
	if !h.w3cProposeLimit.take(store.ActorFrom(r.Context()), h.clock()) {
		writeErr(w, errProjRateLimited)
		return
	}
	stagedHash := ""
	// a key staged for this proposal is deleted when the proposal then fails
	discard := func() {
		if stagedHash != "" && h.keys != nil {
			h.keys.Discard(stagedHash)
		}
	}
	if stage != nil {
		more, err := stage(&c)
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(more) > 0 {
			stagedHash, _ = c.Fields["deploy_key_sha256"].(string)
		}
		if extra == nil {
			extra = map[string]any{}
		}
		for k, v := range more {
			extra[k] = v
		}
	}
	change, err := c.hash()
	if err != nil {
		discard()
		writeErr(w, errProjBadBody)
		return
	}
	msg := signing.RegistryMessage{Action: c.Action, Subject: c.Subject, Change: change,
		Request: registrymsg.NewRequestID(), Issued: h.clock().UTC().Truncate(time.Second)}
	text := msg.Text()
	if text == "" {
		discard()
		writeErr(w, errProjBadBody)
		return
	}
	body := map[string]any{
		"action":  c.Action,
		"subject": c.Subject,
		"fields":  c.Fields,
		"message": text,
		"change":  change,
		"request": msg.Request,
		"issued":  msg.Issued.Format(registryTimeFormat),
		"preview": preview(c),
	}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
}

// providersPropose is POST /api/v1/providers/propose (set_provider,
// delete_provider, create_group). Read-only: nothing is stored.
func (h *handler) providersPropose(w http.ResponseWriter, r *http.Request) {
	h.proposeCore(w, r, providerActions, nil, nil, providerPreview)
}

// providersApply is POST /api/v1/providers/apply.
func (h *handler) providersApply(w http.ResponseWriter, r *http.Request) {
	h.applyCore(w, r, providerActions, nil, nil, nil)
}

// w3cLimiter is the bucket of an action.
func (h *handler) w3cLimiter(action string) *actorLimiter {
	switch action {
	case registrymsg.ActionCreateProject:
		return h.createLimit
	case registrymsg.ActionCreateGroup:
		return h.groupLimit
	case registrymsg.ActionLinkProject, registrymsg.ActionAttachRemote:
		return h.linkLimit // one bucket for both
	case registrymsg.ActionRemoveProject:
		return h.removeLimit
	case registrymsg.ActionDeleteRemoteRepo:
		return h.deleteRemoteLimit
	case registrymsg.ActionRemoveDeployKey:
		return h.removeKeyLimit
	case registrymsg.ActionClearOrphan:
		return h.orphanLimit
	}
	return h.providerLimit
}

// applyCore is the shared body of the two apply endpoints. The server
// recomputes the change from the fields, checks the request is
// self-consistent, runs `pre` (local checks that cost nothing and may add
// broker arguments), takes a slot, and forwards the signed change to the
// broker unchanged (one attempt, never retried). `finish` turns an ok result
// into the response (nil: the generic one).
//
// settle, when set, hears how the call ended (for the per-repository key of a
// GitHub project): keyKept when nothing reached the broker or the broker
// provably did nothing, keyRefused for a known refusal, keyPartial for a
// create_partial/link_partial and keyUnknown for an unknown outcome or an ok
// result that could not be trusted. A fully trusted success is settled by
// `finish` itself (it moves the key before it clones).
func (h *handler) applyCore(w http.ResponseWriter, r *http.Request, allowed []string,
	pre func(c registryChange, args map[string]any) error,
	finish func(r *http.Request, c registryChange, res json.RawMessage, wall time.Time) (body any, err error),
	settle func(c registryChange, o keyOutcome)) {
	started := h.clock()
	wall := time.Now()
	// settle is told once. A panic anywhere below leaves the staged key as
	// safe as the point it happened at allows: before the request left, the
	// key is kept for the same signed change; after it, the outcome is unknown.
	var cur *registryChange
	sent, settled := false, false
	sf := func(c registryChange, o keyOutcome) {
		if settle == nil || settled {
			return
		}
		settled = true
		settle(c, o)
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("orchd: an apply request panicked (the key, if any, is kept)")
			if cur != nil {
				if sent {
					sf(*cur, keyUnknown)
				} else {
					sf(*cur, keyKept)
				}
			}
			writeErr(w, errProjUnknownResult)
		}
	}()
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
	c, err := parseW3cChange(top["action"], top["subject"], top["fields"], allowed, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	cur = &c
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
	args := map[string]any{"id": c.Subject, "change": change, "request": request, "issued": issuedText, "signature": sig}
	for k, v := range c.Fields {
		args[k] = v
	}
	if pre != nil {
		if err := pre(c, args); err != nil {
			writeErr(w, err)
			return
		}
	}
	actor := store.ActorFrom(r.Context())
	limiter := h.w3cLimiter(c.Action)
	if !limiter.take(actor, started) {
		sf(c, keyKept)
		writeErr(w, errProjRateLimited)
		return
	}
	// The write goes on even if the browser goes away: closing the socket
	// early would leave a change that may have happened unrecorded. The
	// client's own time limit bounds it. One attempt, never retried.
	sent = true
	res, oerr := h.broker.Do(context.WithoutCancel(r.Context()), c.Action, args)
	if oerr != nil {
		// Only a failure that provably cost nothing gives the slot back (same
		// rule as the registry and token endpoints): the request never
		// completely left, or the broker refused before looking at it.
		provablyNothing := oerr.Unsent || oerr.Code == "busy" || oerr.Code == "forbidden"
		if provablyNothing {
			limiter.refund(actor, started)
		}
		he := w3cBrokerErrorKind(c.Action, fieldKind(c), oerr, true)
		changed := he.v1Code == "outcome_unknown" || he.v1Code == broker.CodeCreatePartial || he.v1Code == broker.CodeLinkPartial || he.v1Code == "delete_partial"
		switch {
		case provablyNothing:
			sf(c, keyKept)
		case he.v1Code == broker.CodeCreatePartial || he.v1Code == broker.CodeLinkPartial:
			sf(c, keyPartial)
		case he.v1Code == "outcome_unknown":
			sf(c, keyUnknown)
		case provableRefusal[oerr.Code]:
			sf(c, keyRefused)
		default:
			// anything else (request_reused, internal_error, an unregistered code):
			// the request was forwarded, so the key may be in use over there
			sf(c, keyUnknown)
		}
		if changed {
			h.registryChanged()
		}
		code := he.v1Code
		if he.v1Code == "outcome_unknown" && (oerr.TimedOut || oerr.Code == broker.CodeTimeout) {
			code = "timeout"
		}
		h.recordW3c(actor, c.Action, c.Subject, false, code)
		writeErr(w, he)
		return
	}
	h.registryChanged()
	var body any
	if finish != nil {
		body, err = finish(r, c, res, wall)
	} else {
		body, err = h.providerResult(c, res)
	}
	if err != nil {
		he, _ := err.(*httpError)
		code := errProjUnknownResult.v1Code
		if he != nil {
			code = he.v1Code
		}
		sf(c, keyUnknown) // the broker said ok but the result was not what was asked for
		h.recordW3c(actor, c.Action, c.Subject, false, code)
		writeErr(w, err)
		return
	}
	h.recordW3c(actor, c.Action, c.Subject, true, "ok")
	writeJSON(w, http.StatusOK, body)
}

// registryChanged tells the publish resolver to drop its cache.
func (h *handler) registryChanged() {
	if h.onRegistryChange != nil {
		h.onRegistryChange()
	}
}

// providerResult re-bounds the broker's ok result of a provider or group
// action. A result that is not what was asked for is an unknown outcome: the
// broker said ok, so the change may well have happened.
func (h *handler) providerResult(c registryChange, res json.RawMessage) (any, error) {
	unknown := errProvUnknownRes
	str := func(k string) string { s, _ := c.Fields[k].(string); return s }
	switch c.Action {
	case registrymsg.ActionSetProvider:
		var p broker.ProviderRow
		if json.Unmarshal(res, &p) != nil {
			return nil, unknown
		}
		v, ok := providerView(p.ID, p.Kind, p.BaseURL, p.SSHURL, p.TokenRef)
		if !ok || v.ID != c.Subject || v.BaseURL != str("base_url") || v.SSHURL != str("ssh_url") || v.TokenRef != str("token_ref") {
			return nil, unknown
		}
		return map[string]any{"provider": v}, nil
	case registrymsg.ActionDeleteProvider:
		var d struct {
			ID      string `json:"id"`
			Deleted bool   `json:"deleted"`
		}
		if json.Unmarshal(res, &d) != nil || d.ID != c.Subject || !d.Deleted {
			return nil, unknown
		}
		return map[string]any{"id": d.ID, "deleted": true}, nil
	default: // create_group
		var g struct {
			Provider string `json:"provider"`
			Group    struct{ Path, Name string }
		}
		if json.Unmarshal(res, &g) != nil || g.Provider != c.Subject || !registrymsg.ValidGroupPath(g.Group.Path) ||
			g.Group.Name == "" || len(g.Group.Name) > maxNameBytes || hostileText(g.Group.Name) {
			return nil, unknown
		}
		return map[string]any{"provider": g.Provider, "group": GroupView{Path: g.Group.Path, Name: g.Group.Name}}, nil
	}
}

// recordW3c appends the ledger event of a provider, group or project-creation
// request: who, which action, which subject (a provider id or a project id),
// the outcome and its code. Never a field value, a path, a key or any part of
// the signature.
func (h *handler) recordW3c(actor, action, subject string, ok bool, code string) {
	typ := "provider_change"
	switch action {
	case registrymsg.ActionCreateProject:
		typ = "project_create"
	case registrymsg.ActionLinkProject:
		typ = "project_link"
	case registrymsg.ActionAttachRemote:
		typ = "project_attach"
	case registrymsg.ActionRemoveProject:
		typ = "project_remove"
	case registrymsg.ActionDeleteRemoteRepo:
		typ = "remote_delete"
	case registrymsg.ActionRemoveDeployKey:
		typ = "key_remove"
	case registrymsg.ActionClearOrphan:
		typ = "orphan_clear"
	}
	home, err := gate.Home(h.grants.Home)
	if err != nil {
		log.Printf("orchd: %s not recorded in the ledger: home unknown", typ)
		return
	}
	err = ledger.Append(policy.OrchLedgerPath(home), typ, policy.OrchRepo, map[string]any{
		"actor": actor, "action": action, "subject": subject, "ok": ok, "code": code,
	}, h.clock().UTC())
	if err != nil {
		log.Printf("orchd: %s not recorded in the ledger", typ)
	}
}
