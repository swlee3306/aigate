package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"aigate/internal/gitx"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/keys"
	"aigate/internal/orch/remove"
	"aigate/internal/orch/store"
	"aigate/internal/registrymsg"
)

// W3f: deleting a project. Four signed actions, same shape as the W3e
// endpoints: propose builds the signing message and sends nothing, apply
// recomputes the change, checks everything orchd can check, and forwards the
// signed change to the broker, which verifies the signature and does the
// registry and provider work. orchd's own part is the LOCAL files (the clone
// under the repository roots and the project's key folder), and it runs only
// AFTER the broker has answered ok to a signed change that asks for it:
//
//   - remove_project: with the SIGNED detach_key (W3g) the broker first removes
//     this server's deploy key from the repository (orchd supplies the public
//     key line, read from the project's key folder or the GitLab shared .pub
//     BEFORE anything local is removed; never from the request), then it
//     removes the registry row; orchd then removes the clone and the key folder;
//   - delete_remote_repo: broker deletes the remote repository. Without
//     also_local the row stays (remote null, remote_deleted_at set, Provider
//     kept). With the SIGNED also_local (W3g) the broker also removes the
//     registry row in the same signed op, and orchd removes the clone and key
//     folder afterwards; if that local part fails an unregistered folder is
//     left (recovery: register the path again, then a signed remove_project);
//   - remove_deploy_key: broker removes this server's key from the repository;
//   - clear_orphan: broker drops one orphan record (no provider call).
//
// No local file is ever removed on an unsigned path, on a broker refusal or on
// an unknown outcome. Nothing the broker or a provider said is shown: fixed
// sentences only.

const (
	// Per-actor, per hour (F-D5). The broker has its own broker-wide caps.
	RemoveProjectPerActorHour  = 10
	DeleteRemotePerActorHour   = 3
	RemoveKeyPerActorHour      = 10
	ClearOrphanPerActorHour    = 20
	RemovalPreviewPerActorHour = 30

	// previewBudget bounds one removal preview (git queries and the size walk).
	previewBudget = 25 * time.Second
	// previewMaxEntries bounds the size walk.
	previewMaxEntries = 200000
	// localBudget bounds the busy re-check of the local tail.
	localBudget = 15 * time.Second
)

var (
	errProjectBusy       = apiErr(http.StatusConflict, "project_busy", "이 프로젝트에 대기 중이거나 실행 중이거나 게시·승인 대기 중인 작업이 있어 지울 수 없습니다. 작업이 끝나거나 취소된 뒤 다시 시도하세요. 변경 없음")
	errBusyCheckFailed   = apiErr(http.StatusServiceUnavailable, "busy_check_failed", "작업 목록을 읽을 수 없어 이 프로젝트에 진행 중인 작업이 있는지 확인하지 못했습니다. 변경 없음")
	errLegacyAckRequired = apiErr(http.StatusBadRequest, "legacy_ack_required", "처음부터 있던(legacy) 프로젝트는 두 번째 확인(legacy_ack)이 있어야 지울 수 있습니다. 변경 없음")
	errOriginUnknownDel  = apiErr(http.StatusConflict, "origin_unknown", "이 도구가 만든 저장소인지 알 수 없는 프로젝트라 원격 저장소는 지울 수 없습니다. 필요하면 GitHub·GitLab 화면에서 직접 지우세요. 변경 없음")
	errNotCreatedByTool  = apiErr(http.StatusConflict, "not_created_by_tool", "이 도구가 만들지 않고 연결만 한 저장소라 지울 수 없습니다. 연결한 저장소에서는 이 서버의 배포 키만 뗄 수 있습니다. 변경 없음")
	errRemoteChanged     = apiErr(http.StatusConflict, "remote_changed", "등록된 원격 저장소 정보와 요청이 다르거나, 제공자의 저장소가 바뀌었거나, 이미 원격이 없습니다. 프로젝트 목록을 다시 읽어 확인하세요. 변경 없음")
	errConfirmMismatch   = apiErr(http.StatusBadRequest, "confirm_mismatch", "입력한 저장소 이름이 등록된 저장소 이름과 글자 그대로(대소문자 포함) 같지 않습니다. 변경 없음")
	errDeletePartial     = apiErr(http.StatusGatewayTimeout, "delete_partial", "원격 저장소 삭제가 일부만 처리되었거나 결과를 알 수 없습니다. 자동으로 다시 시도하지 않았고 서버의 복제본도 지우지 않았습니다. 제공자 화면과 프로젝트 목록을 확인한 뒤 판단하세요")
	// W3g: remove_project's delete_partial only comes from the key detach
	errRemovePartial     = apiErr(http.StatusGatewayTimeout, "delete_partial", "배포 키 떼기가 일부만 처리되었거나 결과를 알 수 없습니다(키는 이미 떼어졌을 수 있습니다). 프로젝트 등록과 서버의 복제본은 지우지 않았고 자동으로 다시 시도하지 않았습니다. 제공자 화면에서 이 서버의 배포 키를 확인한 뒤 새로 서명하세요")
	errKeyNotFound       = apiErr(http.StatusNotFound, "key_not_found", "제공자의 저장소에서 이 서버의 배포 키(GitHub은 제목 'orchd <프로젝트>'와 공개 키가 둘 다 같은 것, GitLab은 공개 키가 같은 것)를 찾지 못했습니다. 이미 제거되었을 수 있습니다. 변경 없음")
	errRemoveRefused     = apiErr(http.StatusBadGateway, "remove_refused", "제공자가 삭제 요청을 받아들이지 않았거나 대상이 하나로 정해지지 않아 아무것도 지우지 않았습니다. 토큰 권한과 제공자 화면을 확인하세요")
	errOrphanNone        = apiErr(http.StatusNotFound, "orphan_unknown", "그런 고아 기록이 없습니다(이미 지워졌을 수 있습니다). 목록을 다시 읽으세요. 변경 없음")
	errDeleteLockBusy    = apiErr(http.StatusConflict, "busy", "같은 프로젝트나 저장소에 대한 다른 변경이 진행 중입니다. 잠시 후 다시 시도하세요. 변경 없음")
	errDeleteRateLimited = apiErr(http.StatusTooManyRequests, "delete_rate_limited", "서버 전체의 삭제 횟수 한도(시간당·하루)에 도달했습니다. 나중에 다시 시도하세요. 변경 없음")
	errKeyUnavailRemoval = apiErr(http.StatusConflict, "key_unavailable_for_removal", "이 프로젝트의 배포 키 공개 키를 서버에서 읽을 수 없어 이 도구로 뗄 수 없습니다. 제공자 화면(저장소 설정의 배포 키)에서 제목이 orchd로 시작하는 키를 직접 제거하세요. 변경 없음")
	// W3g mixed versions: a broker that does not know the op or the detach_key
	// argument refuses before looking at the signature. Nothing was deleted.
	errDeleteBrokerShape = apiErr(http.StatusServiceUnavailable, "broker_unavailable", "등록부 서비스(broker)가 이 삭제 요청의 형식을 받아들이지 않아 처리하지 않았습니다. 서버 구성 요소의 버전이 서로 맞지 않거나, 또는 그사이 프로젝트 상태가 바뀌었을 수 있습니다. 목록을 다시 읽으세요. 아무것도 지우지 않았습니다")
	// W3g remove_project with detach_key: the key line cannot be read (G-D5)
	errKeyUnavailDetach = apiErr(http.StatusConflict, "key_unavailable_for_removal", "이 프로젝트의 배포 키 공개 키를 서버에서 읽을 수 없어 배포 키를 함께 뗄 수 없습니다. 키를 원격에 남기고 지우려면 \"배포 키도 함께 떼기\"를 끄고 다시 진행하세요(그 뒤 제공자 화면에서 제목이 orchd로 시작하는 키를 직접 제거하세요). 변경 없음")
	errDeleteBrokerOld  = apiErr(http.StatusServiceUnavailable, "broker_unavailable", "등록부 서비스(broker)가 이 삭제 요청을 지원하지 않아 처리하지 않았습니다. 서버 구성 요소의 버전이 서로 맞지 않을 수 있습니다. 아무것도 지우지 않았습니다. 서버 관리자가 확인해야 합니다")
	errNoRemote         = apiErr(http.StatusConflict, "no_remote", "이 프로젝트에는 연결된 원격 저장소가 없어 뗄 배포 키가 없습니다. 변경 없음")
	errLocalUnsafe      = apiErr(http.StatusConflict, "local_unsafe", "서버의 복제본 폴더가 이 도구가 지울 수 있는 모양이 아닙니다(심볼릭 링크이거나, .git 폴더가 없거나, 허용된 저장소 루트 바로 아래의 프로젝트 이름 폴더가 아닙니다). 안전을 위해 아무것도 지우지 않았습니다. 서버 관리자가 확인해야 합니다. 변경 없음")
	errRepoBeingRemoved = apiErr(http.StatusConflict, "repo_being_removed", "이 프로젝트의 서버 폴더를 지우는 중이라 지금은 작업을 내거나 복제할 수 없습니다. 잠시 후 다시 시도하세요. 변경 없음")
	errDeleteNeutralBad = apiErr(http.StatusBadRequest, "bad_url", "제공자 주소가 허용되지 않습니다. 주소 모양과 접속 대상을 확인하세요. 변경 없음")
	errDeleteGHRefused  = apiErr(http.StatusBadGateway, "github_refused", "제공자(GitHub)가 요청을 받아들이지 않았습니다. 토큰 권한과 제공자 설정을 확인하세요. 아무것도 지우지 않았습니다")
	errDeleteGLRefused  = apiErr(http.StatusBadGateway, "gitlab_refused", "제공자(GitLab)가 요청을 받아들이지 않았습니다. 토큰 권한과 제공자 설정을 확인하세요. 아무것도 지우지 않았습니다")

	// Outcome unknown, one sentence per action (each says what was NOT done).
	errRemoveUnknownDetach = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "배포 키 떼기와 프로젝트 등록 지우기가 끝났는지 알 수 없습니다. 배포 키는 제공자에서 이미 떼어졌을 수 있습니다. 자동으로 다시 시도하지 않았고 서버의 복제본은 지우지 않았습니다. 프로젝트 목록과 제공자 화면을 확인하세요")
	errRemoveUnknownRes    = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "프로젝트 등록이 지워졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았고 서버의 복제본은 지우지 않았습니다. 프로젝트 목록을 다시 읽어 확인하세요")
	errDeleteUnknownRes    = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "원격 저장소가 지워졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았고 서버의 복제본도 지우지 않았습니다. 제공자 화면에서 저장소가 남아 있는지 확인하세요")
	errKeyUnknownRes       = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "배포 키가 제거되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 제공자 화면에서 이 서버의 배포 키(제목이 orchd로 시작)를 확인하세요")
	errOrphanUnknownRes    = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "고아 기록이 지워졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 프로젝트 목록(알림 포함)을 다시 읽어 확인하세요")

	// The "files only partly removed" answers (a copy gets the detail: the state
	// of each item). Kept: nothing of the remaining items was touched. Moved: a
	// folder was moved aside and its deletion stopped (the restart sweep cleans it).
	errDeletePartialKept  = apiErr(http.StatusBadGateway, "delete_partial_local", "서버의 파일 지우기가 일부만 끝났습니다. 남은 항목은 원래 자리에 그대로 있습니다(항목별 상태는 함께 표시됩니다). 자동으로 다시 시도하지 않았습니다. 서버 관리자가 확인해야 합니다")
	errDeletePartialMoved = apiErr(http.StatusBadGateway, "delete_partial_local", "서버의 파일 지우기가 일부만 끝났습니다. 지우던 폴더는 프로젝트 이름에서 떼어 내 .deleting- 이름으로 옮겨졌고 아직 다 지워지지 않았습니다(항목별 상태는 함께 표시됩니다). 자동으로 다시 시도하지 않았습니다. 서버를 다시 시작하면 정리되며, 관리자가 직접 지울 수도 있습니다")
)

var deleteActions = []string{registrymsg.ActionRemoveProject, registrymsg.ActionDeleteRemoteRepo, registrymsg.ActionRemoveDeployKey, registrymsg.ActionClearOrphan}

func isDeleteAction(a string) bool {
	for _, d := range deleteActions {
		if d == a {
			return true
		}
	}
	return false
}

// deleteUnknown is the outcome-unknown sentence of an action.
func deleteUnknown(action string) *httpError {
	switch action {
	case registrymsg.ActionRemoveProject:
		return errRemoveUnknownRes
	case registrymsg.ActionDeleteRemoteRepo:
		return errDeleteUnknownRes
	case registrymsg.ActionRemoveDeployKey:
		return errKeyUnknownRes
	}
	return errOrphanUnknownRes
}

// deleteErrorFor narrows a deletion error to the signed change: a
// remove_project whose signature asked to detach the deploy key gets the
// unknown-outcome sentence that says the key may already be gone.
func deleteErrorFor(c registryChange, he *httpError) *httpError {
	if detach, _ := c.Fields["detach_key"].(bool); detach && c.Action == registrymsg.ActionRemoveProject && he == errRemoveUnknownRes {
		return errRemoveUnknownDetach
	}
	return he
}

// deleteBrokerError maps a broker failure of a deletion op to a fixed sentence.
// Only the code is looked at. All four ops are writes: a failure that may have
// happened, and any code orchd does not know, is an unknown outcome (never a
// plain error the console might retry).
func deleteBrokerError(action string, e *broker.OpError) *httpError {
	if e.UnknownOutcome() {
		return deleteUnknown(action)
	}
	switch e.Code {
	case "unknown_op", "not_implemented":
		return errDeleteBrokerOld
	case "bad_request":
		// orchd checks every shape rule of remove_project before sending, so a
		// broker refusing its shape is one that does not know detach_key (W3f)
		if action == registrymsg.ActionRemoveProject {
			return errDeleteBrokerShape
		}
	case "legacy_ack_required":
		return errLegacyAckRequired
	case "origin_unknown":
		return errOriginUnknownDel
	case "not_created_by_tool":
		return errNotCreatedByTool
	case "remote_changed":
		return errRemoteChanged
	case "confirm_mismatch":
		return errConfirmMismatch
	case "delete_partial":
		if action == registrymsg.ActionRemoveProject {
			return errRemovePartial
		}
		return errDeletePartial
	case "key_not_found":
		return errKeyNotFound
	case "remove_refused":
		return errRemoveRefused
	case "orphan_unknown":
		return errOrphanNone
	case "busy":
		return errDeleteLockBusy
	case "rate_limited":
		return errDeleteRateLimited
	case "provider_unknown":
		return errProvUnknown
	case "token_ref_unknown":
		return errTokenRefGone
	case "provider_unreachable":
		return errNeutralUnreachable
	case "provider_rate_limited":
		return errProviderRateLimited
	case "github_refused":
		return errDeleteGHRefused
	case "gitlab_refused":
		return errDeleteGLRefused
	case "bad_url":
		return errDeleteNeutralBad
	}
	he := projectsBrokerError(e, true)
	if he == errProjUnknownResult {
		return deleteUnknown(action)
	}
	return he
}

// ---- checks shared by propose, apply and the preview ----

// projectBusy refuses when the orchd DB holds a task that is not in a final
// state for the project's repository path (or a path below it).
func (h *handler) projectBusy(ctx context.Context, path string) error {
	// a clone running into the folder or a submit in flight counts as busy
	if h.guard.inUse(path) {
		return errProjectBusy
	}
	if h.st == nil {
		return errBusyCheckFailed
	}
	n, err := h.st.CountActiveByRepo(ctx, path)
	if err != nil {
		return errBusyCheckFailed
	}
	if n > 0 {
		return errProjectBusy
	}
	return nil
}

// delInfo is what the checks learned about the project of one request.
type delInfo struct {
	view    ProjectView
	path    string
	kind    string // the provider's kind ("" when the project has none)
	legacy  bool
	static  bool
	keyLine string
	keyFP   string
	// W3g remove_project: detach is the signed detach_key; remote the
	// registry's remote path (for the preview line).
	detach bool
	remote string
	// detachAvailable: detach_key=true would pass the checks (for the
	// propose answer, whatever was asked)
	detachAvailable bool
}

// detachKeyInfo is what remove_project's detach_key needs (G-D3, G-D5):
// eligible when the row has a provider of a known kind and a remote that was
// not deleted; ok when, in addition, the remote has its number (the broker
// checks the repository by it) and orchd can read a public key line the broker
// accepts. The line is read from the project's key folder (GitHub) or the
// shared key's .pub (GitLab), never from a request.
func (h *handler) detachKeyInfo(id string, p *broker.Project, kind string) (eligible, ok bool, line, fp string) {
	if p.Provider == nil || p.Remote == nil || p.RemoteDeletedAt != nil || kind == "" {
		return false, false, "", ""
	}
	if p.Remote.ID == nil {
		return true, false, "", ""
	}
	line, fp, ok = h.deployKeyLine(id, kind)
	return true, ok, line, fp
}

// keyTitle is the title this tool gives a project's deploy key at the provider.
// GitLab's shared key keeps the title of the first project that registered it,
// so for GitLab the fingerprint is the identity (deploy_key_shared).
func keyTitle(id string) string { return "orchd " + id }

// localPlan is the dry run of the local removal: the registry path must be the
// project's own folder directly under a root, and the clone and key folder (if
// they exist) must be removable by internal/orch/remove. Nothing is changed.
func (h *handler) localPlan(id, path string, rows []broker.Project) error {
	if !h.directUnderRoot(path) || !safeProjectPath(path) || filepath.Base(path) != id {
		return errLocalUnsafe
	}
	// No other registry row may share the folder, live below it, or contain it:
	// removing this project's folder would take another project's files along.
	for _, o := range rows {
		if o.ID == id {
			continue
		}
		if under(o.Path, path) || under(path, o.Path) {
			return errLocalUnsafe
		}
	}
	if _, err := remove.CheckClone(h.roots, path, id); err != nil {
		return errLocalUnsafe
	}
	if h.keys != nil {
		if _, err := remove.CheckKeyDir(h.keys.KeysDir(), id); err != nil {
			return errLocalUnsafe
		}
	}
	return nil
}

// deployKeyLine is the public key line the broker needs to find this server's
// key on the provider: the project's own key for GitHub, the shared key for
// GitLab. Read fresh from the files, never from a request.
func (h *handler) deployKeyLine(id, kind string) (line, fp string, ok bool) {
	if kind == "github" {
		if h.keys == nil {
			return "", "", false
		}
		k, err := keys.ProjectPublic(h.keys.KeysDir(), id)
		if err != nil || !brokerDeployKeyRE.MatchString(k.Public) {
			return "", "", false
		}
		return k.Public, k.Fingerprint, true
	}
	return h.currentDeployKey()
}

// deleteChecks is everything a deletion request can be checked for from the
// registry (one read), orchd's configuration, the DB and the files before
// anything is signed or sent. It mirrors the broker's shape rules (the broker
// still decides) and adds what only orchd knows: busy projects, the local
// folder, the key line.
func (h *handler) deleteChecks(ctx context.Context, c registryChange) (*delInfo, error) {
	rv, err := h.readRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if c.Action == registrymsg.ActionClearOrphan {
		project, _ := c.Fields["project"].(string)
		for _, o := range rv.Orphans {
			if o.Provider == c.Subject && o.Project == project {
				return &delInfo{}, nil
			}
		}
		return nil, errOrphanNone
	}
	var p *broker.Project
	for i := range rv.Projects {
		if rv.Projects[i].ID == c.Subject {
			p = &rv.Projects[i]
		}
	}
	if p == nil {
		return nil, errProjUnknown
	}
	view, ok := projectView(*p)
	if !ok {
		return nil, errProjBrokerBad
	}
	info := &delInfo{view: view, path: p.Path, legacy: view.Origin == "legacy", static: h.staticRepos[filepath.Clean(p.Path)]}
	if p.Provider != nil {
		for _, pr := range rv.Providers {
			if pr.ID == *p.Provider {
				info.kind = pr.Kind
			}
		}
	}
	str := func(k string) string { s, _ := c.Fields[k].(string); return s }
	switch c.Action {
	case registrymsg.ActionRemoveProject:
		ack, _ := c.Fields["legacy_ack"].(bool)
		if info.legacy && !ack {
			return nil, errLegacyAckRequired
		}
		if !info.legacy && ack {
			return nil, errProjBadBody
		}
		eligible, ok, line, fp := h.detachKeyInfo(c.Subject, p, info.kind)
		info.detachAvailable = ok
		if detach, _ := c.Fields["detach_key"].(bool); detach {
			if !eligible {
				return nil, errProjBadBody // G-D3: nothing to detach (the broker says bad_request too)
			}
			if p.Remote.ID == nil {
				return nil, errRemoteChanged // what the broker would answer
			}
			if !ok {
				return nil, errKeyUnavailDetach
			}
			// read here, at propose AND again at apply, always before the broker
			// call and therefore before the key folder can be removed
			info.detach, info.keyLine, info.keyFP, info.remote = true, line, fp, p.Remote.Path
		}
		if err := h.projectBusy(ctx, p.Path); err != nil {
			return nil, err
		}
		if err := h.localPlan(c.Subject, p.Path, rv.Projects); err != nil {
			return nil, err
		}
	case registrymsg.ActionDeleteRemoteRepo:
		switch view.Origin {
		case "created":
		case "linked":
			return nil, errNotCreatedByTool
		default: // unknown and legacy
			return nil, errOriginUnknownDel
		}
		if p.Provider == nil || p.Remote == nil || p.RemoteDeletedAt != nil || p.Remote.ID == nil || info.kind == "" {
			return nil, errRemoteChanged
		}
		if !sameRemote(info.kind, p.Remote.Path, str("remote_path")) || str("remote_id") != strconv.FormatInt(*p.Remote.ID, 10) {
			return nil, errRemoteChanged
		}
		if name, ok := registrymsg.RemoteLastSegment(p.Remote.Path); !ok || str("confirm_name") != name {
			return nil, errConfirmMismatch
		}
		if err := h.projectBusy(ctx, p.Path); err != nil {
			return nil, err
		}
		if also, _ := c.Fields["also_local"].(bool); also {
			if err := h.localPlan(c.Subject, p.Path, rv.Projects); err != nil {
				return nil, err
			}
		}
	case registrymsg.ActionRemoveDeployKey:
		if p.Provider == nil || p.Remote == nil || info.kind == "" {
			return nil, errNoRemote
		}
		if !sameRemote(info.kind, p.Remote.Path, str("remote_path")) {
			return nil, errRemoteChanged
		}
		line, fp, ok := h.deployKeyLine(c.Subject, info.kind)
		if !ok {
			return nil, errKeyUnavailRemoval
		}
		info.keyLine, info.keyFP = line, fp
		if err := h.projectBusy(ctx, p.Path); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// ---- propose ----

const deleteStamp = "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"

func deletePreview(c registryChange, info *delInfo) []string {
	str := func(k string) string { s, _ := c.Fields[k].(string); return s }
	switch c.Action {
	case registrymsg.ActionRemoveProject:
		out := []string{
			"프로젝트 '" + c.Subject + "'를 이 서버에서 지웁니다: 프로젝트 등록, 서버의 복제본 폴더(" + info.path + "), 이 프로젝트의 배포 키 폴더",
			"원격 저장소는 지워지지 않고 그대로 남습니다",
		}
		if detach, _ := c.Fields["detach_key"].(bool); detach {
			out = append(out, "먼저 원격 저장소 '"+info.remote+"'에서 이 서버의 배포 키(지문 "+info.keyFP+")를 뗍니다. 키를 떼지 못하거나 결과를 알 수 없으면 프로젝트 등록과 서버의 파일을 지우지 않습니다")
		}
		out = append(out,
			"[중요] 되돌릴 수 없습니다. 원격에 올라가지 않은 커밋과 저장하지 않은 변경은 이 서버의 복제본에만 있으므로 함께 사라집니다",
			"이 서버의 작업 기록은 남습니다")
		if info.legacy {
			out = append(out, "[중요] 처음부터 있던(legacy) 프로젝트입니다. 지우면 이 저장소로는 더 작업을 낼 수 없습니다(두 번째 확인을 했습니다)")
		}
		if info.static {
			out = append(out, "이 프로젝트에는 서버 설정(--git-remote-for)에 정해진 원격이 있습니다. 그 설정은 바뀌지 않으니 관리자가 따로 정리해야 합니다")
		}
		return append(out, deleteStamp)
	case registrymsg.ActionDeleteRemoteRepo:
		out := []string{
			"[중요] 원격 저장소 '" + str("remote_path") + "'를 제공자에서 삭제합니다. 되돌릴 수 없습니다(제공자 설정에 따라 복구 가능 기간이 있을 수 있으나 이 도구는 복구하지 않습니다)",
			"이 도구가 만든 저장소만 지울 수 있으며, 저장소 이름 '" + str("confirm_name") + "'을 직접 입력해 확인했습니다",
			"삭제가 확실히 끝난 뒤에만 다음 단계를 합니다. 결과를 알 수 없으면 아무것도 더 하지 않고 자동으로 다시 시도하지 않습니다",
		}
		if also, _ := c.Fields["also_local"].(bool); also {
			out = append(out,
				"원격 삭제가 끝나면 이 서버의 복제본 폴더("+info.path+")와 이 프로젝트의 배포 키 폴더도 지웁니다",
				"[중요] 원격이 지워지면 이 서버의 복제본이 유일한 사본일 수 있습니다. 원격에 올라가지 않은 커밋과 변경은 복제본과 함께 사라집니다",
				"원격 삭제가 끝나면 프로젝트 등록도 같은 서명으로 지웁니다. 서버의 파일을 지우지 못하면 등록 없는 폴더가 남을 수 있습니다(관리자가 정리)")
		} else {
			out = append(out, "이 서버의 복제본과 프로젝트 등록은 그대로 남습니다(등록은 '원격 없음' 상태가 됩니다)")
		}
		return append(out, deleteStamp)
	case registrymsg.ActionRemoveDeployKey:
		return []string{
			"프로젝트 '" + c.Subject + "'의 저장소 '" + str("remote_path") + "'에서 이 서버의 배포 키만 뗍니다(지문 " + info.keyFP + ")",
			"저장소와 다른 키는 지워지지 않습니다. 프로젝트 등록과 서버의 복제본은 그대로입니다",
			"[중요] 키를 떼면 이 서버는 그 저장소를 복제하거나 게시하지 못합니다",
			deleteStamp,
		}
	}
	return []string{
		"제공자 '" + c.Subject + "'의 고아 기록 하나('" + str("project") + "')를 지웁니다",
		"외부에는 아무것도 보내지 않습니다. 제공자에 남아 있을 수 있는 프로젝트는 지워지지 않으니 제공자 화면에서 직접 정리하세요",
		deleteStamp,
	}
}

func (h *handler) deletePropose(w http.ResponseWriter, r *http.Request, action string) {
	var info *delInfo
	h.proposeCore(w, r, []string{action}, func(c *registryChange) (map[string]any, error) {
		i, err := h.deleteChecks(r.Context(), *c)
		if err != nil {
			return nil, err
		}
		info = i
		extra := map[string]any{}
		if c.Action != registrymsg.ActionClearOrphan {
			extra["origin"], extra["legacy"] = i.view.Origin, i.legacy
		}
		if c.Action == registrymsg.ActionRemoveDeployKey {
			extra["deploy_key_fingerprint"] = i.keyFP
		}
		if c.Action == registrymsg.ActionRemoveProject {
			extra["detach_key_available"] = i.detachAvailable
			if i.detach {
				extra["deploy_key_title"], extra["deploy_key_fingerprint"], extra["deploy_key_shared"] = keyTitle(c.Subject), i.keyFP, i.kind != "github"
			}
		}
		return extra, nil
	}, nil, func(c registryChange) []string { return deletePreview(c, info) })
}

// ---- apply ----

func (h *handler) deleteApply(w http.ResponseWriter, r *http.Request, action string) {
	var info *delInfo
	h.applyCore(w, r, []string{action}, func(c registryChange, args map[string]any) error {
		i, err := h.deleteChecks(r.Context(), c)
		if err != nil {
			return err
		}
		info = i
		if c.Action == registrymsg.ActionRemoveDeployKey || (c.Action == registrymsg.ActionRemoveProject && i.detach) {
			// an argument of the broker op, not a signed field: orchd's own key
			// line, fresh (for remove_project: read before the folder can go)
			args["deploy_key"] = i.keyLine
		}
		return nil
	}, func(r *http.Request, c registryChange, res json.RawMessage, wall time.Time) (any, error) {
		return h.finishDelete(r, c, res, info)
	}, nil)
}

// finishDelete turns the broker's ok result into the response. The reply is
// checked against what was asked BEFORE any local file is touched; the local
// removal is decided by the SIGNED fields only (never by anything in the reply).
func (h *handler) finishDelete(r *http.Request, c registryChange, res json.RawMessage, info *delInfo) (any, error) {
	unknown := deleteErrorFor(c, deleteUnknown(c.Action))
	switch c.Action {
	case registrymsg.ActionRemoveProject:
		var d struct {
			ID       string  `json:"id"`
			Removed  bool    `json:"removed"`
			KeyState *string `json:"key_state"`
		}
		if json.Unmarshal(res, &d) != nil || d.ID != c.Subject || !d.Removed {
			return nil, unknown
		}
		// a signed detach must say how the key ended; without one the reply
		// must not talk about a key (an answer to something else)
		detach, _ := c.Fields["detach_key"].(bool)
		if detach != (d.KeyState != nil) || (detach && *d.KeyState != "removed" && *d.KeyState != "already_gone") {
			return nil, unknown
		}
		local, err := h.removeLocal(r.Context(), c.Subject, info.path, true, false)
		if err != nil {
			if !detach {
				return nil, err
			}
			// the key WAS detached and the row removed: say so in the ledger
			// before the request is recorded as partial, and in the answer
			h.recordW3c(store.ActorFrom(r.Context()), c.Action, c.Subject, true, "ok")
			return nil, detachedPartial(err, *d.KeyState)
		}
		body := map[string]any{"id": c.Subject, "removed": true, "detach_key": detach, "local": local}
		if detach {
			body["key_state"] = *d.KeyState
		}
		return body, nil
	case registrymsg.ActionDeleteRemoteRepo:
		var d struct {
			ID              string `json:"id"`
			RemoteState     string `json:"remote_state"`
			RemoteDeletedAt string `json:"remote_deleted_at"`
			RegistryRemoved *bool  `json:"registry_removed"`
		}
		if json.Unmarshal(res, &d) != nil || d.ID != c.Subject || (d.RemoteState != "deleted" && d.RemoteState != "requested") {
			return nil, unknown
		}
		if t, err := time.Parse(registryTimeFormat, d.RemoteDeletedAt); err != nil || t.Location() != time.UTC || t.Format(registryTimeFormat) != d.RemoteDeletedAt {
			return nil, unknown
		}
		// also_local signed: the broker removed the row in the same op and must
		// say so (a W3f broker does not: unknown, nothing local is touched);
		// without it the row stays and the reply must not say otherwise
		also, _ := c.Fields["also_local"].(bool)
		if also != (d.RegistryRemoved != nil) || (also && !*d.RegistryRemoved) {
			return nil, unknown
		}
		registry := "kept"
		if also {
			registry = "removed"
		}
		body := map[string]any{"id": c.Subject, "remote_state": d.RemoteState, "remote_deleted_at": d.RemoteDeletedAt,
			"also_local": also, "registry": registry}
		if also {
			local, err := h.removeLocal(r.Context(), c.Subject, info.path, true, true)
			if err != nil {
				// the remote delete DID happen: say so in the ledger before the
				// request is recorded as partial
				h.recordW3c(store.ActorFrom(r.Context()), c.Action, c.Subject, true, "ok")
				return nil, err
			}
			body["local"] = local
		}
		return body, nil
	case registrymsg.ActionRemoveDeployKey:
		var d struct {
			ID         string `json:"id"`
			KeyRemoved bool   `json:"key_removed"`
		}
		if json.Unmarshal(res, &d) != nil || d.ID != c.Subject || !d.KeyRemoved {
			return nil, unknown
		}
		return map[string]any{"id": c.Subject, "key_removed": true}, nil
	}
	var d struct {
		Provider string `json:"provider"`
		Project  string `json:"project"`
		Cleared  bool   `json:"cleared"`
	}
	want, _ := c.Fields["project"].(string)
	if json.Unmarshal(res, &d) != nil || d.Provider != c.Subject || d.Project != want || !d.Cleared {
		return nil, unknown
	}
	return map[string]any{"provider": d.Provider, "project": d.Project, "cleared": true}, nil
}

// Item states of a local removal: removed and already_gone are success;
// kept_in_place (nothing of the item was touched) and moved_aside (the folder
// left its project name and its deletion stopped) are what remains.
const (
	itemKept  = "kept_in_place"
	itemMoved = "moved_aside"
)

func itemState(err error) string {
	if errors.Is(err, remove.ErrPartial) {
		return itemMoved
	}
	return itemKept
}

// localPartial builds the "files only partly removed" error with its detail:
// the state of each item, which remain, and what already happened elsewhere
// (kinds and states only, never a path).
func localPartial(clone, key string, registryRemoved, remoteDeleted bool, reason string) *httpError {
	base := errDeletePartialKept
	if clone == itemMoved || key == itemMoved {
		base = errDeletePartialMoved
	}
	he := *base
	remaining := []string{}
	if clone == itemKept || clone == itemMoved {
		remaining = append(remaining, "clone")
	}
	if key == itemKept || key == itemMoved {
		remaining = append(remaining, "key")
	}
	he.detail = map[string]any{"remaining": remaining, "clone": clone, "key": key,
		"registry_removed": registryRemoved, "remote_deleted": remoteDeleted, "reason": reason}
	return &he
}

// detachedPartial adds to a "files only partly removed" answer of a
// remove_project that detached the key: how the key ended at the provider,
// and that the folder left behind is no longer registered.
func detachedPartial(err error, keyState string) error {
	he, ok := err.(*httpError)
	if !ok {
		return err
	}
	det, ok := he.detail.(map[string]any)
	if !ok {
		return err
	}
	out := *he
	out.msg += ". 이 서버의 배포 키는 제공자에서 이미 떼어졌고(또는 이미 없었고) 프로젝트 등록도 지워졌습니다. 남은 폴더는 등록되지 않은 상태입니다(복구: 경로를 다시 등록한 뒤 서명된 삭제)"
	d := map[string]any{}
	for k, v := range det {
		d[k] = v
	}
	d["key_state"], d["key_detached"] = keyState, true
	out.detail = d
	return &out
}

// removeLocal removes the clone and the key folder of project id. It runs only
// from finishDelete, after the broker accepted a signed change that asks for it.
// The path guard is held from the final busy re-check until both items were
// handled: no task can be submitted and no clone can run in between. The clone
// and the key folder are attempted INDEPENDENTLY; a failure of one never skips
// the other, and each item is reported on its own.
func (h *handler) removeLocal(ctx context.Context, id, path string, registryRemoved, remoteDeleted bool) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), localBudget)
	defer cancel()
	if !h.guard.tryRemove(path) {
		return nil, localPartial(itemKept, itemKept, registryRemoved, remoteDeleted, "in_progress")
	}
	defer h.guard.endRemove(path)
	if err := h.projectBusy(ctx, path); err != nil {
		reason := "project_busy"
		if err == errBusyCheckFailed {
			reason = "busy_check_failed"
		}
		return nil, localPartial(itemKept, itemKept, registryRemoved, remoteDeleted, reason)
	}
	h.localMu.Lock()
	defer h.localMu.Unlock()
	local := map[string]string{}
	failed := false
	cloneState, keyState := "", ""
	out, err := remove.Clone(h.roots, path, id)
	if err != nil {
		log.Printf("orchd: the local removal of a project did not complete (clone)")
		cloneState, failed = itemState(err), true
	} else {
		cloneState = string(out)
	}
	if h.keys == nil {
		keyState = "not_managed"
	} else if out, err := remove.KeyDir(h.keys.KeysDir(), id); err != nil {
		log.Printf("orchd: the local removal of a project did not complete (key)")
		keyState, failed = itemState(err), true
	} else {
		keyState = string(out)
	}
	if failed {
		return nil, localPartial(cloneState, keyState, registryRemoved, remoteDeleted, "remove_failed")
	}
	local["clone"], local["key"] = cloneState, keyState
	return local, nil
}

// ---- endpoints ----

func (h *handler) removePropose(w http.ResponseWriter, r *http.Request) {
	h.deletePropose(w, r, registrymsg.ActionRemoveProject)
}
func (h *handler) removeApply(w http.ResponseWriter, r *http.Request) {
	h.deleteApply(w, r, registrymsg.ActionRemoveProject)
}
func (h *handler) deleteRemotePropose(w http.ResponseWriter, r *http.Request) {
	h.deletePropose(w, r, registrymsg.ActionDeleteRemoteRepo)
}
func (h *handler) deleteRemoteApply(w http.ResponseWriter, r *http.Request) {
	h.deleteApply(w, r, registrymsg.ActionDeleteRemoteRepo)
}
func (h *handler) removeKeyPropose(w http.ResponseWriter, r *http.Request) {
	h.deletePropose(w, r, registrymsg.ActionRemoveDeployKey)
}
func (h *handler) removeKeyApply(w http.ResponseWriter, r *http.Request) {
	h.deleteApply(w, r, registrymsg.ActionRemoveDeployKey)
}
func (h *handler) orphanClearPropose(w http.ResponseWriter, r *http.Request) {
	h.deletePropose(w, r, registrymsg.ActionClearOrphan)
}
func (h *handler) orphanClearApply(w http.ResponseWriter, r *http.Request) {
	h.deleteApply(w, r, registrymsg.ActionClearOrphan)
}

// ---- the read-only removal preview ----

// removalPreview is GET /api/v1/projects/{id}/removal-preview: what a removal
// of this project would lose, as COUNTS and flags only. It never returns a file
// name, a branch name, a commit message or any content. It reads the registry
// (broker down: broker_unavailable, nothing else is guessed from the request).
func (h *handler) removalPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !registrymsg.ValidSubject(id) {
		writeErr(w, errProjBadBody)
		return
	}
	actor := store.ActorFrom(r.Context())
	slotAt := h.clock()
	if !h.previewRemovalLimit.take(actor, slotAt) {
		writeErr(w, errProjRateLimited)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), previewBudget)
	defer cancel()
	rv, err := h.readRegistry(ctx)
	if err != nil {
		// nothing was read from any project: the slot is free again
		h.previewRemovalLimit.refund(actor, slotAt)
		writeErr(w, err)
		return
	}
	var p *broker.Project
	for i := range rv.Projects {
		if rv.Projects[i].ID == id {
			p = &rv.Projects[i]
		}
	}
	if p == nil {
		writeErr(w, errProjUnknown)
		return
	}
	view, ok := projectView(*p)
	if !ok {
		writeErr(w, errProjBrokerBad)
		return
	}
	kind := ""
	if p.Provider != nil {
		for _, pr := range rv.Providers {
			if pr.ID == *p.Provider {
				kind = pr.Kind
			}
		}
	}
	// W3g: whether remove_project may detach the key (the same checks as its
	// propose), and the key's public identity; null when it may not
	_, detachOK, _, keyFP := h.detachKeyInfo(id, p, kind)
	var keyTitleV, keyFPV any
	if detachOK {
		keyTitleV, keyFPV = keyTitle(id), keyFP
	}
	body := map[string]any{
		"id": id, "origin": view.Origin, "legacy": view.Origin == "legacy", "has_remote": p.Remote != nil, "remote_key_may_remain": p.Remote != nil,
		"detach_key_available": detachOK, "deploy_key_title": keyTitleV, "deploy_key_fingerprint": keyFPV, "deploy_key_shared": detachOK && kind != "github",
		"remote_deleted_at": view.RemoteDeletedAt, "static_remote": h.staticRepos[filepath.Clean(p.Path)],
		"busy": false, "busy_unknown": false,
		"folder_exists": false, "is_git_repo": false, "local_removable": false, "key_dir_exists": false,
		"size_bytes": 0, "size_known": false,
		"unpushed_commits": 0, "unpushed_known": false, "changed_files": 0, "changed_known": false,
	}
	switch err := h.projectBusy(ctx, p.Path); err {
	case nil:
	case errProjectBusy:
		body["busy"] = true
	default:
		body["busy_unknown"] = true
	}
	// Local facts are only looked at for the shape removal itself would accept.
	shapeOK := h.directUnderRoot(p.Path) && safeProjectPath(p.Path) && filepath.Base(p.Path) == id
	if h.keys != nil {
		if have, kerr := remove.CheckKeyDir(h.keys.KeysDir(), id); kerr == nil {
			body["key_dir_exists"] = have
		}
	}
	if shapeOK {
		have, cerr := remove.CheckClone(h.roots, p.Path, id)
		body["local_removable"] = cerr == nil && h.localPlan(id, p.Path, rv.Projects) == nil
		if fi, lerr := os.Lstat(p.Path); lerr == nil {
			body["folder_exists"] = true
			if gi, gerr := os.Lstat(filepath.Join(p.Path, ".git")); gerr == nil && gi.IsDir() && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
				body["is_git_repo"] = true
			}
		}
		if cerr == nil && have {
			size, complete := dirSize(ctx, p.Path)
			body["size_bytes"], body["size_known"] = size, complete
			// git runs only for a clone whose git directories stay under the roots
			if h.ownRepo(ctx, p.Path) {
				wc := gitx.ReadWorkCounts(ctx, p.Path)
				body["unpushed_commits"], body["unpushed_known"] = wc.Unpushed, wc.UnpushedKnown
				body["changed_files"], body["changed_known"] = wc.Changed, wc.ChangedKnown
			}
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// dirSize adds up the sizes of the regular files below root without following
// any symlink, within the entry and time caps; complete is false when a cap
// stopped the walk (the size is then a lower bound).
func dirSize(ctx context.Context, root string) (size int64, complete bool) {
	n := 0
	complete = true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			complete = false
			return nil
		}
		if ctx.Err() != nil || n >= previewMaxEntries {
			complete = false
			return filepath.SkipAll
		}
		n++
		if d.Type().IsRegular() {
			if fi, ierr := d.Info(); ierr == nil {
				size += fi.Size()
			} else {
				complete = false
			}
		}
		return nil
	})
	return size, complete
}
