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
	W3cProposePerActorHour    = 60

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
	errTokenRefGone   = apiErr(http.StatusBadRequest, "token_ref_unknown", "제공자가 쓸 gitlab 종류 토큰이 없거나 쓸 수 없습니다. 토큰 화면에서 확인하세요. 변경 없음")
	errGLUnreachable  = apiErr(http.StatusBadGateway, "provider_unreachable", "GitLab에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요")
	errGLRefused      = apiErr(http.StatusBadGateway, "gitlab_refused", "GitLab이 요청을 받아들이지 않았습니다. 토큰 권한과 GitLab 설정을 확인하세요")
	errGroupUnknown   = apiErr(http.StatusNotFound, "group_unknown", "GitLab에 그런 그룹이 없습니다. 변경 없음")
	errGroupExists    = apiErr(http.StatusConflict, "group_exists", "같은 경로의 그룹이 이미 있습니다. 변경 없음")
	errNameTaken      = apiErr(http.StatusConflict, "project_name_taken", "같은 이름의 프로젝트가 이미 등록되어 있거나 GitLab에 있습니다. 기존 프로젝트는 가져오지 않습니다. 변경 없음")
	errCreatePartial  = apiErr(http.StatusBadGateway, "create_partial", "GitLab에 일부만 만들어졌습니다. 이 도구는 삭제하지 않으니 GitLab에서 직접 정리한 뒤 다시 만드세요")
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
	if !registrymsg.ValidProviderID(id) || kind != "gitlab" || !registrymsg.ValidBaseURL(baseURL) ||
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
}

func orphanView(o broker.OrphanRow) (OrphanView, bool) {
	if !registrymsg.ValidProviderID(o.Provider) || !broker.ValidGitLabProjectPath(o.Project) || !broker.ValidOrphanStep(o.Step) ||
		(o.ProjectID != nil && *o.ProjectID <= 0) {
		return OrphanView{}, false
	}
	if t, err := time.Parse(registryTimeFormat, o.At); err != nil || t.Location() != time.UTC || t.Format(registryTimeFormat) != o.At {
		return OrphanView{}, false
	}
	return OrphanView{Provider: o.Provider, Project: o.Project, ProjectID: o.ProjectID, Step: o.Step, At: o.At}, true
}

// GroupView is one GitLab group the token can create projects in.
type GroupView struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// ---- broker error mapping ----

// w3cBrokerError maps a broker failure to orchd's fixed refusal. Only the code
// (and, for create_partial, the validated detail) is looked at. write says the
// call was a write: then a failure that may have happened is outcome_unknown,
// never a plain retry. The codes of the older ops are mapped by
// projectsBrokerError.
func w3cBrokerError(action string, e *broker.OpError, write bool) *httpError {
	unknown := errProvUnknownRes
	if action == registrymsg.ActionCreateProject {
		unknown = errCreateUnknown
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
			return partialError(e.Partial)
		}
	}
	he := projectsBrokerError(e, write)
	if he == errProjUnknownResult {
		return unknown
	}
	return he
}

// partialError is create_partial with the orphan facts when the broker gave
// valid ones (public values only: no token, no GitLab text).
func partialError(d *broker.PartialDetail) *httpError {
	he := *errCreatePartial
	if d != nil {
		he.detail = map[string]any{"provider": d.Provider, "project": d.Project, "project_id": d.ProjectID, "step": d.Step,
			"orphan_recorded": d.Recorded, "orphan_dropped": d.Dropped}
	}
	return &he
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
		Groups    []struct{ Path, Name string } `json:"groups"`
		Truncated bool                          `json:"truncated"`
	}
	if err := json.Unmarshal(res, &reply); err != nil || len(reply.Groups) > maxGroupRows {
		writeErr(w, errProjBrokerBad)
		return
	}
	out := make([]GroupView, 0, len(reply.Groups))
	for _, g := range reply.Groups {
		if !registrymsg.ValidGroupPath(g.Path) || g.Name == "" || len(g.Name) > maxNameBytes || hostileText(g.Name) {
			writeErr(w, errProjBrokerBad)
			return
		}
		out = append(out, GroupView{Path: g.Path, Name: g.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "truncated": reply.Truncated})
}

// ---- the change ----

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
	if action == registrymsg.ActionCreateProject && !withHash {
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

// proposeCore is the shared body of the two propose endpoints. check runs
// after the body is understood and before a slot is taken; fill may add the
// server-computed fields; extra adds response members.
func (h *handler) proposeCore(w http.ResponseWriter, r *http.Request, allowed []string, check func(c *registryChange) (extra map[string]any, err error), preview func(c registryChange) []string) {
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
	h.proposeCore(w, r, providerActions, nil, providerPreview)
}

// providersApply is POST /api/v1/providers/apply.
func (h *handler) providersApply(w http.ResponseWriter, r *http.Request) {
	h.applyCore(w, r, providerActions, nil, nil)
}

// w3cLimiter is the bucket of an action.
func (h *handler) w3cLimiter(action string) *actorLimiter {
	switch action {
	case registrymsg.ActionCreateProject:
		return h.createLimit
	case registrymsg.ActionCreateGroup:
		return h.groupLimit
	}
	return h.providerLimit
}

// applyCore is the shared body of the two apply endpoints. The server
// recomputes the change from the fields, checks the request is
// self-consistent, runs `pre` (local checks that cost nothing and may add
// broker arguments), takes a slot, and forwards the signed change to the
// broker unchanged (one attempt, never retried). `finish` turns an ok result
// into the response (nil: the generic one).
func (h *handler) applyCore(w http.ResponseWriter, r *http.Request, allowed []string,
	pre func(c registryChange, args map[string]any) error,
	finish func(r *http.Request, c registryChange, res json.RawMessage, wall time.Time) (body any, err error)) {
	started := h.clock()
	wall := time.Now()
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
		writeErr(w, errProjRateLimited)
		return
	}
	// The write goes on even if the browser goes away: closing the socket
	// early would leave a change that may have happened unrecorded. The
	// client's own time limit bounds it. One attempt, never retried.
	res, oerr := h.broker.Do(context.WithoutCancel(r.Context()), c.Action, args)
	if oerr != nil {
		// Only a failure that provably cost nothing gives the slot back (same
		// rule as the registry and token endpoints): the request never
		// completely left, or the broker refused before looking at it.
		if oerr.Unsent || oerr.Code == "busy" || oerr.Code == "forbidden" {
			limiter.refund(actor, started)
		}
		he := w3cBrokerError(c.Action, oerr, true)
		changed := he.v1Code == "outcome_unknown" || he.v1Code == broker.CodeCreatePartial
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
	if action == registrymsg.ActionCreateProject {
		typ = "project_create"
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
