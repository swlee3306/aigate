// delete.js: the deletion screens of the project list (W3f). They are drawn by projects.js (one
// `del` step) and never navigate; every change is a SIGNED change run by providers.js's makeFlow:
//
//   "삭제…" -> choose what to delete -> removal preview (counts) -> [legacy: second confirmation]
//   -> POST .../propose (stores nothing) -> confirmation (the browser recomputes `change` from its
//   OWN values; no sign button unless the server says the same) -> passkey -> apply -> result
//
// Rules this file keeps (tests pin each one):
//  * a remote repository is offered for deletion only for a project this tool created (`origin`
//    created), with a remote link, a repository number and no remote_deleted_at; its name must be
//    typed exactly (case kept) before the propose button turns on;
//  * a legacy project gets a SEPARATE second confirmation before anything is proposed, and the signed
//    `legacy_ack` is true exactly then;
//  * `also_local` is a signed field of ONE signature that also removes the registration (W3g); the
//    follow-up "등록 지우기" (a second, separately signed remove_project, never started or signed
//    automatically) stays only for a remote deletion without also_local and for rows already marked
//    remote_deleted_at;
//  * W3g: "배포 키도 함께 떼기" (signed `detach_key`) is offered on the local removal only when the
//    removal preview says the key can be detached; it is checked by default, and unchecking it says
//    plainly that the key stays at the remote;
//  * an unknown outcome is never retried and never signed again: the lists are read again and a fixed
//    sentence is shown; every sentence is chosen by code, the server's words are never shown.
// Every string from the server is a text node (el()); nothing is stored in the browser.

import { el, inline } from '../render.js';
import {
  makeFlow, confirmPanel, li as factItem, failureText, isUnknown, ERROR_TEXT, hostName, validRemotePath, validConfirmName, validOrphanProject,
  PROVIDER_ID_RE, REMOTE_ID_RE, ACTION_REMOVE_PROJECT, ACTION_DELETE_REMOTE_REPO, ACTION_REMOVE_DEPLOY_KEY, ACTION_CLEAR_ORPHAN,
  REMOVAL_PREVIEW_TIMEOUT_MS,
} from './providers.js';

const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);
const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);
const ID_RE = /^[a-z][a-z0-9-]{0,39}$/;
const ORIGINS = ['created', 'linked', 'unknown', 'legacy'];
const KEEP_STATES = ['kept_in_place', 'moved_aside'];
const DONE_STATES = ['removed', 'already_gone', 'not_managed'];
const KEY_STATES = ['removed', 'already_gone'];
const KEY_STATE_TEXT = { removed: '뗐습니다', already_gone: '이미 없었습니다(찾은 키가 없어 지울 것이 없었음)' };
const PARTIAL_REASONS = ['project_busy', 'busy_check_failed', 'in_progress', 'remove_failed'];

// The members of the removal-preview answer (internal/orch/api removalPreview; TestConstantsAgreeWithWeb
// reads them from the Go source).
export const PREVIEW_KEYS = ['id', 'origin', 'legacy', 'has_remote', 'remote_key_may_remain', 'remote_deleted_at', 'static_remote', 'busy', 'busy_unknown',
  'folder_exists', 'is_git_repo', 'local_removable', 'key_dir_exists', 'size_bytes', 'size_known', 'unpushed_commits', 'unpushed_known', 'changed_files', 'changed_known',
  'detach_key_available', 'deploy_key_title', 'deploy_key_fingerprint', 'deploy_key_shared'];

// --- fixed sentences ---------------------------------------------------------

export const DEL_TEXT = {
  menuBusy: '다른 작업이 진행 중이라 지금은 지울 수 없습니다. 끝난 뒤 다시 시도하세요.',
  menu: '삭제…',
  previewLoading: '지우면 사라질 것을 서버에서 읽는 중입니다…',
  previewBad: '서버가 알려 준 미리보기를 확인할 수 없거나 화면의 프로젝트 정보와 맞지 않아 중단했습니다. 아무것도 서명하거나 보내지 않았습니다. 목록을 새로 읽고 다시 시도하세요.',
  busy: '이 프로젝트에 대기 중이거나 실행 중이거나 게시·승인 대기 중인 작업이 있어 지금은 지울 수 없습니다. 작업이 끝나거나 취소된 뒤 다시 시도하세요.',
  busyUnknown: '진행 중인 작업이 있는지 확인하지 못했습니다. 안전을 위해 지금은 지울 수 없습니다. 잠시 후 다시 시도하세요.',
  localUnsafe: '서버의 복제본 폴더가 이 도구가 지울 수 있는 모양이 아니어서 서버의 복제본은 지울 수 없습니다. 서버 관리자가 확인해야 합니다.',
  irreversible: '[중요] 되돌릴 수 없습니다. 지운 서버의 복제본과 그 안의 원격에 올라가지 않은 커밋·저장하지 않은 변경은 복구할 수 없습니다.',
  irreversibleRemote: '[중요] 원격 저장소 삭제는 되돌릴 수 없습니다. 이 도구는 지운 저장소를 복구하지 않습니다(제공자 설정에 따라 복구 가능 기간이 있을 수 있으나 이 화면은 그 상태를 알지 못합니다).',
  remoteAlreadyGone: '[중요] 원격 저장소는 이미 지워졌습니다. 이 서버의 복제본이 그 코드의 유일한 사본일 수 있습니다. 지우기 전에 꼭 필요한 것을 다른 곳에 옮겨 두세요.',
  noRemoteDelete: {
    legacy: '처음부터 있던 프로젝트의 원격 저장소는 이 도구로 지울 수 없습니다(서버의 복제본과 등록만 지울 수 있습니다). 원격이 필요 없다면 GitHub·GitLab 화면에서 직접 지우세요.',
    unknown: '이 도구가 만든 저장소인지 알 수 없는 프로젝트라 원격 저장소는 이 도구로 지울 수 없습니다. 필요하면 GitHub·GitLab 화면에서 직접 지우세요.',
    linked: '이 도구가 만들지 않고 연결만 한 저장소라 원격 저장소는 지울 수 없습니다. 이 서버의 배포 키만 뗄 수 있습니다.',
    gone: '원격 저장소는 이미 지워졌습니다.',
    none: '연결된 원격 저장소가 없습니다.',
    id: '원격 저장소 번호를 알 수 없어(이 프로젝트의 등록 정보에 없음) 원격 저장소는 지울 수 없습니다.',
  },
  alsoWarn: '[중요] 같은 서명으로 원격 저장소 삭제와 이 서버의 복제본 삭제가 모두 일어납니다. 원격이 지워지면 이 서버의 복제본이 그 코드의 유일한 사본일 수 있고, 그 복제본도 함께 사라져 복구할 수 없습니다.',
  remoteMaybeGone: '[중요] 이 프로젝트의 원격 저장소 삭제가 방금 결과를 알 수 없는 상태로 끝났습니다. 원격 저장소는 이미 지워졌을 수 있고, 그러면 이 서버의 복제본이 그 코드의 유일한 사본일 수 있습니다. 지우기 전에 꼭 필요한 것을 다른 곳에 옮겨 두세요.',
  createdLocalOnly: '이 프로젝트의 원격 저장소는 이 도구가 만든 것입니다. 등록을 지우면 이 도구는 더 이상 그 원격 저장소를 지울 수 없게 됩니다. 원격 저장소도 지우려면 먼저 "원격 저장소 지우기"를 하세요.',
  keyHint: '이 프로젝트에는 원격 저장소 연결이 있어, 서버의 복제본과 등록을 지워도 이 서버의 배포 키는 제공자 저장소에 남을 수 있습니다. 배포 키를 먼저 뗄까요?',
  ackLabel: '원격에 올라가지 않은 커밋이나 변경이 있을 수 있고, 그것이 함께 사라지는 것을 알고 지웁니다.',
  ackNeeded: '위 항목을 체크해야 다음으로 갈 수 있습니다.',
  typedLabel: '저장소 이름을 직접 입력하세요(대소문자까지 같아야 합니다)',
  typedHint: '입력한 이름이 저장소 이름과 글자 그대로 같을 때만 다음 단계로 갈 수 있습니다. 붙여넣어도 됩니다.',
  alsoLabel: '원격 저장소를 지운 뒤 이 서버의 복제본과 배포 키 폴더도 이어서 지웁니다(같은 서명 한 번)',
  alsoHint: '원격 삭제가 끝나면 프로젝트 등록도 같은 서명으로 지웁니다(두 번째 서명은 없습니다). 서버의 파일을 지우지 못하면 등록 없는 폴더가 남을 수 있고, 그때는 관리자가 정리합니다.',
  detachLabel: '배포 키도 함께 떼기',
  detachHint: '지우기 전에 먼저 원격 저장소에서 이 서버의 배포 키를 뗍니다(같은 서명 한 번). 키를 떼지 못하거나 결과를 알 수 없으면 등록과 파일은 지우지 않습니다.',
  detachShared: 'GitLab 공용 키: 제공자 화면의 키 제목은 처음 등록한 프로젝트의 이름일 수 있어, 지문으로 이 서버의 키를 확인합니다.',
  keyRemains: '[중요] 키가 원격에 남습니다. 직접 지우세요. 원격 저장소 설정(Deploy keys)에서 이 서버의 배포 키를 제거해야 합니다.',
  legacyBody: '이것은 처음부터 있던 프로젝트입니다. 서버의 복제본과 등록이 지워지고, 이 서버의 작업 기록은 남지만 이 저장소로는 더 작업을 낼 수 없습니다. 원격 저장소는 이 도구가 지우지 않고 그대로 남습니다.',
  legacyTyped: '프로젝트 이름을 한 번 더 직접 입력하세요',
  legacyStatic: '이 프로젝트에는 서버 설정(--git-remote-for)에 정해진 원격이 있습니다. 그 설정은 바뀌지 않으니 관리자가 따로 정리해야 합니다.',
  staticRemote: '서버 설정(--git-remote-for)에 정해진 원격이 있는 프로젝트입니다. 그 설정은 이 도구가 바꾸지 않으므로 지운 뒤 관리자가 따로 정리해야 합니다.',
  gate: '서명 단추는 위 내용을 이 화면이 직접 다시 계산해 서버가 보낸 값과 같을 때만 나타납니다.',
  sending: '서버가 지우는 중입니다(최대 약 2분 걸릴 수 있습니다). 이 화면을 닫아도 서버의 작업은 멈추지 않습니다.',
  unknown: {
    [ACTION_REMOVE_PROJECT]: '프로젝트 등록이 지워졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았고 서버의 복제본은 지우지 않았을 수 있습니다. 목록을 다시 읽었으니 확인하세요. 필요하면 처음부터 새로 서명해야 합니다.',
    [ACTION_DELETE_REMOTE_REPO]: '원격 저장소가 지워졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았고 다시 서명을 요구하지도 않습니다. GitHub·GitLab 화면에서 저장소가 남아 있는지 확인하고 목록을 확인하세요.',
    [ACTION_REMOVE_DEPLOY_KEY]: '배포 키가 제거되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 제공자 저장소 설정(Deploy keys)에서 제목이 orchd로 시작하는 키가 남았는지 확인하세요.',
    [ACTION_CLEAR_ORPHAN]: '고아 기록이 지워졌는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 목록(알림 포함)을 다시 읽었으니 확인하세요.',
  },
  partialRemote: '원격 저장소 삭제가 일부만 처리되었거나 결과를 알 수 없습니다. 저장소가 지워졌을 수도, 아닐 수도 있습니다.',
  partialRemoteDo: '아래 단추: 저장소가 아직 있으면 "원격 저장소 다시 지우기"(새 서명이 필요하고 서버가 번호와 이름을 다시 확인합니다), 이미 없으면 "배포 키도 함께 떼기"를 끄고 "복제본과 등록 지우기"(그 복제본이 유일한 사본일 수 있습니다). 어느 쪽도 눌러야 시작하며 눌러도 서명 전에는 아무것도 지워지지 않습니다.',
  noResult: '서버의 답이 요청한 내용과 맞지 않아 결과를 확인하지 못했습니다. 목록을 다시 읽었으니 확인하세요. 자동으로 다시 시도하지 않았습니다.',
  orphanCreated: '이 도구가 만든 저장소가 제공자에 남아 있을 수 있습니다. 이 기록에 대해서는 이 도구가 원격 저장소를 지우지 않습니다: 먼저 GitHub·GitLab에서 직접 지운 뒤에 이 기록을 지우세요. 이 기록만 지우고 저장소를 그대로 두면 같은 이름으로 다시 만들 수 없습니다.',
  orphanLinked: '원래 있던 저장소입니다. 절대 지우지 마세요. 이 도구는 제공자에서 아무것도 지우지 않고 이 서버의 기록 한 줄만 지웁니다. 이 서버의 배포 키가 붙어 있을 수 있으니 필요하면 제공자 저장소 설정(Deploy keys)에서 제목이 orchd로 시작하는 키를 직접 제거하세요.',
  orphanUnknown: '출처를 알 수 없는 기록입니다. 지우기 전에 제공자 화면에서 저장소가 있는지, 이 도구가 만든 것인지 먼저 확인하세요. 이 도구는 제공자에서 아무것도 지우지 않고 이 서버의 기록 한 줄만 지웁니다.',
};

// The deletion screens' own sentences for the codes whose shared text talks about creating things: each says
// that nothing was deleted (true for every one of them: a refusal before the one DELETE request).
export const DEL_FAIL = {
  github_refused: '제공자(GitHub)가 요청을 받아들이지 않았습니다. 토큰 권한과 제공자 설정을 확인하세요. 아무것도 지우지 않았습니다.',
  gitlab_refused: '제공자(GitLab)가 요청을 받아들이지 않았습니다. 토큰 권한과 제공자 설정을 확인하세요. 아무것도 지우지 않았습니다.',
  provider_unreachable: '제공자(GitHub·GitLab)에 연결하지 못했거나 응답이 없었습니다. 삭제 요청이 나가기 전의 실패이므로 아무것도 지우지 않았습니다. 잠시 후 다시 시도하세요.',
  bad_url: '제공자 주소가 허용되지 않아 아무것도 지우지 않았습니다. 제공자 화면에서 주소와 접속 대상을 확인하세요.',
  token_ref_unknown: '제공자가 쓸 토큰이 없거나 쓸 수 없어 아무것도 지우지 않았습니다. 토큰 화면에서 확인하세요.',
  provider_rate_limited: '제공자가 요청 한도를 넘었다고 응답했습니다. 아무것도 지우지 않았습니다. 한참 뒤에 다시 시도하세요. 자동으로 다시 시도하지 않았습니다.',
  remove_refused: '제공자가 삭제 요청을 받아들이지 않았거나 지울 대상이 하나로 정해지지 않아 아무것도 지우지 않았습니다. 토큰 권한과 제공자 화면을 확인하세요.',
};
// W3g: the remove flow's own sentences (G-D6 reuses the codes; the page words them for this flow).
export const DEL_REMOVE_TEXT = {
  keyUnavailable: '이 프로젝트의 배포 키 공개 키를 서버에서 읽을 수 없어 키를 함께 뗄 수 없습니다. 아무것도 지우지 않았습니다. "배포 키도 함께 떼기"를 끄고 진행할 수 있으며, 그러면 키는 원격에 남으니 제공자 저장소 설정(Deploy keys)에서 직접 지우세요.',
  brokerUnavailable: '서버 구성 요소(broker와 orchd)의 버전이 서로 맞지 않거나 그사이 프로젝트 상태가 바뀌었거나 등록부 서비스에 연결하지 못해 처리하지 않았습니다. 아무것도 지우지 않았습니다. 목록을 다시 읽은 뒤 다시 시도하세요.',
  unknownDetach: '배포 키 떼기와 프로젝트 등록 지우기가 끝났는지 알 수 없습니다. 키는 이미 떼어졌을 수 있습니다. 자동으로 다시 시도하지 않았습니다. 제공자 저장소 설정(Deploy keys)과 목록을 확인하고, 필요하면 처음부터 새로 서명하세요.',
  partialDetach: '배포 키 떼기가 일부만 처리되었거나 결과를 알 수 없습니다(키는 이미 떼어졌을 수 있습니다). 프로젝트 등록과 서버의 복제본은 지우지 않았습니다. 자동으로 다시 시도하지 않았습니다. 제공자 저장소 설정(Deploy keys)에서 확인한 뒤 "복제본과 등록 지우기"를 새로 서명하세요(키가 이미 없으면 "배포 키도 함께 떼기"를 끄고).',
};
const failText = (err, sent) => {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (code === 'broker_unavailable') return DEL_REMOVE_TEXT.brokerUnavailable;
  if (code === 'key_unavailable_for_removal' && sent && sent.action === ACTION_REMOVE_PROJECT) return DEL_REMOVE_TEXT.keyUnavailable;
  return has(DEL_FAIL, code) ? DEL_FAIL[code] : null;
};

// The server's fixed preview lines (internal/orch/api deletePreview), as templates: {id} {path} {remote} {name} {fp}
// {provider} {project}. This page rebuilds the whole preview from its own values and a proposal is signable only
// when the server's list is exactly that (TestConstantsAgreeWithWeb pins every template against the real lines).
export const DEL_LINES = {
  stamp: '서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다',
  rp1: "프로젝트 '{id}'를 이 서버에서 지웁니다: 프로젝트 등록, 서버의 복제본 폴더({path}), 이 프로젝트의 배포 키 폴더",
  rp2: '원격 저장소는 지워지지 않고 그대로 남습니다',
  rpDetach: "먼저 원격 저장소 '{remote}'에서 이 서버의 배포 키(지문 {fp})를 뗍니다. 키를 떼지 못하거나 결과를 알 수 없으면 프로젝트 등록과 서버의 파일을 지우지 않습니다",
  rp3: '[중요] 되돌릴 수 없습니다. 원격에 올라가지 않은 커밋과 저장하지 않은 변경은 이 서버의 복제본에만 있으므로 함께 사라집니다',
  rp4: '이 서버의 작업 기록은 남습니다',
  rpLegacy: '[중요] 처음부터 있던(legacy) 프로젝트입니다. 지우면 이 저장소로는 더 작업을 낼 수 없습니다(두 번째 확인을 했습니다)',
  rpStatic: '이 프로젝트에는 서버 설정(--git-remote-for)에 정해진 원격이 있습니다. 그 설정은 바뀌지 않으니 관리자가 따로 정리해야 합니다',
  dr1: "[중요] 원격 저장소 '{remote}'를 제공자에서 삭제합니다. 되돌릴 수 없습니다(제공자 설정에 따라 복구 가능 기간이 있을 수 있으나 이 도구는 복구하지 않습니다)",
  dr2: "이 도구가 만든 저장소만 지울 수 있으며, 저장소 이름 '{name}'을 직접 입력해 확인했습니다",
  dr3: '삭제가 확실히 끝난 뒤에만 다음 단계를 합니다. 결과를 알 수 없으면 아무것도 더 하지 않고 자동으로 다시 시도하지 않습니다',
  drAlso1: '원격 삭제가 끝나면 이 서버의 복제본 폴더({path})와 이 프로젝트의 배포 키 폴더도 지웁니다',
  drAlso2: '[중요] 원격이 지워지면 이 서버의 복제본이 유일한 사본일 수 있습니다. 원격에 올라가지 않은 커밋과 변경은 복제본과 함께 사라집니다',
  drAlso3: '원격 삭제가 끝나면 프로젝트 등록도 같은 서명으로 지웁니다. 서버의 파일을 지우지 못하면 등록 없는 폴더가 남을 수 있습니다(관리자가 정리)',
  drKeep: "이 서버의 복제본과 프로젝트 등록은 그대로 남습니다(등록은 '원격 없음' 상태가 됩니다)",
  dk1: "프로젝트 '{id}'의 저장소 '{remote}'에서 이 서버의 배포 키만 뗍니다(지문 {fp})",
  dk2: '저장소와 다른 키는 지워지지 않습니다. 프로젝트 등록과 서버의 복제본은 그대로입니다',
  dk3: '[중요] 키를 떼면 이 서버는 그 저장소를 복제하거나 게시하지 못합니다',
  co1: "제공자 '{provider}'의 고아 기록 하나('{project}')를 지웁니다",
  co2: '외부에는 아무것도 보내지 않습니다. 제공자에 남아 있을 수 있는 프로젝트는 지워지지 않으니 제공자 화면에서 직접 정리하세요',
};
const FP_RE = /^SHA256:[A-Za-z0-9+/]{43}$/;
const fill = (t, v) => t.replace(/\{([a-z]+)\}/g, (_, k) => v[k]);

// expectedLines: the whole preview of a proposal, from the values this page sent and knows (null when a value is missing).
export function expectedLines(sent, row, pv, resp) {
  const L = DEL_LINES;
  const f = sent.fields;
  const path = isObj(row) ? row.path : null;
  let out;
  if (sent.action === ACTION_REMOVE_PROJECT) {
    if (!proposalKeyOk(sent, row, pv, resp)) return null;
    out = [fill(L.rp1, { id: sent.subject, path }), L.rp2];
    if (f.detach_key) out.push(fill(L.rpDetach, { remote: remoteOf(row).path, fp: resp.deploy_key_fingerprint }));
    out.push(L.rp3, L.rp4);
    if (f.legacy_ack) out.push(L.rpLegacy);
    if (pv && pv.static_remote) out.push(L.rpStatic);
  } else if (sent.action === ACTION_DELETE_REMOTE_REPO) {
    out = [fill(L.dr1, { remote: f.remote_path }), fill(L.dr2, { name: f.confirm_name }), L.dr3];
    out.push(...(f.also_local ? [fill(L.drAlso1, { path }), L.drAlso2, L.drAlso3] : [L.drKeep]));
  } else if (sent.action === ACTION_REMOVE_DEPLOY_KEY) {
    const fp = isObj(resp) ? resp.deploy_key_fingerprint : null;
    if (typeof fp !== 'string' || !FP_RE.test(fp)) return null;
    out = [fill(L.dk1, { id: sent.subject, remote: f.remote_path, fp }), L.dk2, L.dk3];
  } else {
    out = [fill(L.co1, { provider: sent.subject, project: f.project }), L.co2];
  }
  if (out.some((x) => typeof x !== 'string' || x.includes('{'))) return null;
  return [...out, L.stamp];
}

// proposalKeyOk: the key members of a remove_project proposal (W3g). detach_key_available is always a bool;
// the key's identity is there exactly when detach_key was asked, and it is the key the removal preview showed
// (title computed here, fingerprint equal to the preview's, shared a bool equal to the preview's).
function proposalKeyOk(sent, row, pv, resp) {
  if (!isObj(resp) || typeof resp.detach_key_available !== 'boolean') return false;
  const ident = ['deploy_key_title', 'deploy_key_fingerprint', 'deploy_key_shared'];
  if (!sent.fields.detach_key) return !ident.some((k) => has(resp, k));
  if (resp.detach_key_available !== true || remoteOf(row) === null || !isObj(pv) || pv.detach_key_available !== true) return false;
  return resp.deploy_key_title === 'orchd ' + sent.subject && typeof resp.deploy_key_fingerprint === 'string' && FP_RE.test(resp.deploy_key_fingerprint) &&
    resp.deploy_key_fingerprint === pv.deploy_key_fingerprint && typeof resp.deploy_key_shared === 'boolean' && resp.deploy_key_shared === pv.deploy_key_shared;
}

const CLONE_STATE_TEXT = {
  removed: '지웠습니다', already_gone: '이미 없었습니다', not_managed: '이 도구가 관리하는 폴더가 아니라 건드리지 않았습니다',
  kept_in_place: '지우지 못했습니다(원래 자리에 그대로 있음)', moved_aside: '프로젝트 이름에서 떼어 .deleting- 이름으로 옮겼으나 아직 다 지우지 못했습니다',
};
const PARTIAL_REASON_TEXT = {
  project_busy: '지우는 도중 이 프로젝트에 진행 중인 작업이 생겨 파일은 건드리지 않았습니다.',
  busy_check_failed: '진행 중인 작업이 있는지 확인하지 못해 파일은 건드리지 않았습니다.',
  in_progress: '같은 폴더에 복제 또는 작업 제출이 진행 중이라 파일은 건드리지 않았습니다.',
  remove_failed: '서버가 폴더를 지우지 못했습니다.',
};

// --- helpers --------------------------------------------------------------------

export const originOf = (row) => (isObj(row) && ORIGINS.includes(row.origin) ? row.origin : 'unknown');

// remoteOf: the remote link of a registry row as this page can verify it, else null.
export function remoteOf(row) {
  if (!isObj(row) || !isObj(row.gitlab) || typeof row.provider !== 'string' || !PROVIDER_ID_RE.test(row.provider) || !validRemotePath(row.gitlab.project)) return null;
  const idn = row.gitlab.project_id;
  const idStr = Number.isSafeInteger(idn) && idn > 0 ? String(idn) : null;
  return { provider: row.provider, path: row.gitlab.project, id: idStr !== null && REMOTE_ID_RE.test(idStr) ? idStr : null, name: row.gitlab.project.slice(row.gitlab.project.lastIndexOf('/') + 1) };
}

// deleteOptions: what may be offered for a row. `remoteWhy` names the fixed sentence when a remote deletion is not offered.
export function deleteOptions(row) {
  const origin = originOf(row);
  const remote = remoteOf(row);
  const gone = isObj(row) && typeof row.remote_deleted_at === 'string';
  let remoteWhy = null;
  if (origin === 'legacy') remoteWhy = 'legacy';
  else if (origin === 'unknown') remoteWhy = 'unknown';
  else if (origin === 'linked') remoteWhy = 'linked';
  else if (gone) remoteWhy = 'gone';
  else if (remote === null) remoteWhy = 'none';
  else if (remote.id === null) remoteWhy = 'id';
  return { origin, remote, gone, local: true, remoteOffered: remoteWhy === null, remoteWhy, key: remote !== null && remote.id !== null && !gone };
}

// checkPreview: the removal-preview body, every member checked; null when anything is off or when it
// does not describe the row this page started from.
export function checkPreview(pv, id, row) {
  if (!isObj(pv) || pv.id !== id) return null;
  const bools = ['legacy', 'has_remote', 'remote_key_may_remain', 'static_remote', 'busy', 'busy_unknown', 'folder_exists', 'is_git_repo', 'local_removable', 'key_dir_exists', 'size_known', 'unpushed_known', 'changed_known'];
  if (!bools.every((k) => typeof pv[k] === 'boolean')) return null;
  const ints = ['size_bytes', 'unpushed_commits', 'changed_files'];
  if (!ints.every((k) => Number.isSafeInteger(pv[k]) && pv[k] >= 0)) return null;
  if (!ORIGINS.includes(pv.origin) || pv.origin !== originOf(row) || pv.legacy !== (pv.origin === 'legacy')) return null;
  if (pv.has_remote !== (isObj(row) && isObj(row.gitlab))) return null;
  if (pv.remote_deleted_at !== null && typeof pv.remote_deleted_at !== 'string') return null;
  if ((pv.remote_deleted_at !== null) !== (isObj(row) && typeof row.remote_deleted_at === 'string')) return null;
  // W3g: whether the key can be detached with the removal, and which key (public identity only)
  if (typeof pv.detach_key_available !== 'boolean' || typeof pv.deploy_key_shared !== 'boolean') return null;
  if (pv.detach_key_available) {
    if (!deleteOptions(row).key || pv.deploy_key_title !== 'orchd ' + id || typeof pv.deploy_key_fingerprint !== 'string' || !FP_RE.test(pv.deploy_key_fingerprint)) return null;
  } else if (pv.deploy_key_title !== null || pv.deploy_key_fingerprint !== null || pv.deploy_key_shared !== false) return null;
  return pv;
}

export function sizeText(n) {
  if (n < 1024) return n + ' B';
  const u = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (v >= 10 ? v.toFixed(0) : v.toFixed(1)) + ' ' + u[i];
}

// partialDetail: the per-item facts of a delete_partial_local refusal, checked member by member (null when off).
export function partialDetail(err) {
  const d = err && err.detail;
  if (!isObj(d) || !Array.isArray(d.remaining) || d.remaining.length > 2 || !d.remaining.every((x) => x === 'clone' || x === 'key') ||
      new Set(d.remaining).size !== d.remaining.length) return null;
  const okState = (v) => typeof v === 'string' && (DONE_STATES.includes(v) || KEEP_STATES.includes(v));
  if (!okState(d.clone) || !okState(d.key) || typeof d.registry_removed !== 'boolean' || typeof d.remote_deleted !== 'boolean' ||
      typeof d.reason !== 'string' || !PARTIAL_REASONS.includes(d.reason)) return null;
  // W3g: the key was detached at the provider before the local removal failed (both members, or neither)
  if (has(d, 'key_state') || has(d, 'key_detached')) {
    if (d.key_detached !== true || !KEY_STATES.includes(d.key_state) || d.registry_removed !== true) return null;
  }
  return { remaining: d.remaining, clone: d.clone, key: d.key, registryRemoved: d.registry_removed, remoteDeleted: d.remote_deleted, reason: d.reason,
    keyState: has(d, 'key_state') ? d.key_state : null };
}

// --- the controller ---------------------------------------------------------------

// host: {paint(), rows(), providers(), reload(): Promise, toList()}
export function makeDelete(ctx, host) {
  const d = {
    prevStep: null,
    step: 'idle', // idle | choose | loading | preview | legacy | flow | orphan | fail
    row: null, opts: null, choice: null, pv: null, note: null, followUp: false, orphan: null, seq: 0,
    typed: null, ack: null, also: null, detach: null, legacyTyped: null, blocked: null,
  };
  const gone = () => !!(ctx.signal && ctx.signal.aborted);
  let resultErr = null;
  // ids whose remote deletion ended 'maybe done' in THIS page visit (memory only, never stored)
  const partialIds = new Set();

  const flow = makeFlow(ctx, { onChange: () => { if (!gone() && d.step === 'flow') host.paint(); }, onResult, kindOf: () => undefined, failText });

  const api = {
    get active() { return d.step !== 'idle'; },
    get busy() { return flow.busy || d.step === 'loading'; },
    flow,
    state: d,
  };

  function reset() {
    d.seq++;
    flow.reset();
    Object.assign(d, { step: 'idle', prevStep: null, row: null, opts: null, choice: null, pv: null, note: null, followUp: false, orphan: null, typed: null, ack: null, also: null, detach: null, legacyTyped: null, blocked: null });
    resultErr = null;
  }
  api.reset = reset;

  const rowFor = (id) => host.rows().find((r) => isObj(r) && r.id === id) || null;

  // ---- entry points ----
  api.start = (row) => {
    if (api.busy || !isObj(row) || typeof row.id !== 'string' || !ID_RE.test(row.id)) return;
    reset();
    d.row = row;
    d.opts = deleteOptions(row);
    d.step = 'choose';
    host.paint();
  };

  api.startOrphan = (o, kind) => {
    if (api.busy || !isObj(o) || typeof o.provider !== 'string' || !PROVIDER_ID_RE.test(o.provider) || !validOrphanProject(o.project)) return;
    reset();
    const origin = o.origin === 'created' || o.origin === 'linked' ? o.origin : 'unknown';
    d.orphan = { provider: o.provider, project: o.project, origin, kind };
    d.step = 'orphan';
    host.paint();
  };

  function cancel() {
    reset();
    host.toList();
  }

  // pickChoice: the person chose what to delete; the preview is read once (and reused when going back and forth).
  async function pickChoice(choice, followUp) {
    if (api.busy || !d.row) return;
    if (choice === ACTION_DELETE_REMOTE_REPO && !d.opts.remoteOffered) {
      d.step = 'fail';
      d.note = { error: false, text: DEL_TEXT.noRemoteDelete[d.opts.remoteWhy] };
      host.paint();
      return;
    }
    d.choice = choice;
    d.followUp = !!followUp;
    d.note = null;
    d.typed = null;
    d.ack = null;
    d.also = null;
    d.detach = null;
    d.legacyTyped = null;
    if (d.pv) {
      d.step = 'preview';
      buildControls();
      host.paint();
      return;
    }
    const my = ++d.seq;
    d.step = 'loading';
    host.paint();
    let body;
    try {
      body = await ctx.get('/api/v1/projects/' + d.row.id + '/removal-preview', { noRetry429: true, timeoutMs: REMOVAL_PREVIEW_TIMEOUT_MS, csrf: true });
    } catch (err) {
      if (gone() || my !== d.seq) return;
      d.step = 'fail';
      d.note = { error: true, text: err && err.status === 0 ? '서버에 연결하지 못했습니다. 아무것도 서명하거나 보내지 않았습니다.' : (failText(err) || failureText(err)) };
      host.paint();
      return;
    }
    if (gone() || my !== d.seq) return;
    const pv = checkPreview(body, d.row.id, d.row);
    if (pv === null) {
      d.step = 'fail';
      d.note = { error: true, text: DEL_TEXT.previewBad };
      host.paint();
      return;
    }
    d.pv = pv;
    d.step = 'preview';
    buildControls();
    host.paint();
  }

  // ---- the persistent controls of the preview and legacy screens ----
  function buildControls() {
    const remote = d.opts.remote;
    d.ack = el('input', { type: 'checkbox', id: 'del-ack', 'data-focus-key': 'ack', on: { change: gateChanged } });
    d.ack.checked = false;
    d.also = el('input', { type: 'checkbox', id: 'del-also', 'data-focus-key': 'also', on: { change: () => { gateChanged(true); } } });
    d.also.checked = false;
    // W3g: checked by default; shown (and sent as true) only when the preview says the key can be detached
    d.detach = el('input', { type: 'checkbox', id: 'del-detach', 'data-focus-key': 'detach', on: { change: () => { gateChanged(true); } } });
    d.detach.checked = true;
    d.typed = el('input', { type: 'text', id: 'del-typed', class: 'mono', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off', maxlength: '120', 'data-focus-key': 'typed', on: { input: gateChanged } });
    d.typed.value = '';
    d.legacyTyped = el('input', { type: 'text', id: 'del-legacy-typed', class: 'mono', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off', maxlength: '60', 'data-focus-key': 'ltyped', on: { input: gateChanged } });
    d.legacyTyped.value = '';
    d.blocked = null;
    void remote;
  }

  // The propose button's state is part of the render: a repaint happens only when it changes.
  function gateChanged(force) {
    const g = gate();
    const key = g.ok + '|' + g.needAck + '|' + g.why + '|' + (d.choice === ACTION_DELETE_REMOTE_REPO && d.also ? d.also.checked : '') + '|' + (d.step === 'legacy' && legacyTypedOk()) + '|' + detachKey();
    if (force === true || key !== d.blocked) {
      d.blocked = key;
      keepFocus(() => host.paint());
    }
  }

  // keepFocus: building the new screen moves the persistent controls out of the old one, which drops the
  // focus of the field being typed in. The focused control (by data-focus-key) and its caret are put back.
  function keepFocus(fn) {
    const doc = globalThis.document;
    const a = doc ? doc.activeElement : null;
    const key = a && typeof a.getAttribute === 'function' ? a.getAttribute('data-focus-key') : null;
    const from = a && typeof a.selectionStart === 'number' ? [a.selectionStart, a.selectionEnd] : null;
    fn();
    if (!key) return;
    const find = (n) => {
      if (n.nodeType !== 1) return null;
      if (n.getAttribute('data-focus-key') === key) return n;
      for (const c of n.childNodes) {
        const f = find(c);
        if (f) return f;
      }
      return null;
    };
    const t = find(ctx.root);
    if (t && typeof t.focus === 'function') {
      t.focus();
      if (from && typeof t.setSelectionRange === 'function') t.setSelectionRange(from[0], from[1]);
    }
  }

  const alsoLocal = () => d.choice === ACTION_DELETE_REMOTE_REPO && !!d.also && d.also.checked === true;
  // detachOffered: the local removal of a row whose key the server can detach (removal preview, checked against the row)
  const detachOffered = () => d.choice === ACTION_REMOVE_PROJECT && !!d.pv && d.pv.detach_key_available === true && !!d.opts && d.opts.key;
  const detachKey = () => detachOffered() && !!d.detach && d.detach.checked === true;
  const touchesLocal = () => d.choice === ACTION_REMOVE_PROJECT || alsoLocal();

  // maybeGone: the remote is already deleted, or its deletion ended "maybe done" in this page visit
  const maybeGone = () => d.opts.gone || (!!d.row && partialIds.has(d.row.id));

  // ackNeeded: files may be lost with the local clone: always with also_local (the same signature deletes the
  // remote and then the clone may be the only copy), when the remote is (maybe) gone, or when anything is
  // unpushed, changed or unknown.
  function ackNeeded() {
    const pv = d.pv;
    if (!pv || !touchesLocal()) return false;
    if (alsoLocal() || maybeGone()) return true;
    if (!pv.folder_exists) return false;
    return pv.unpushed_commits > 0 || !pv.unpushed_known || pv.changed_files > 0 || !pv.changed_known;
  }

  // gate: may the propose button be on? {ok, why}
  function gate() {
    const pv = d.pv;
    if (!pv) return { ok: false, why: 'preview' };
    if (pv.busy) return { ok: false, why: 'busy' };
    if (pv.busy_unknown) return { ok: false, why: 'busy_unknown' };
    const needAck = ackNeeded();
    if (touchesLocal() && !pv.local_removable) return { ok: false, why: 'local_unsafe', needAck };
    if (d.choice === ACTION_DELETE_REMOTE_REPO) {
      if (!d.opts.remoteOffered || pv.remote_deleted_at !== null) return { ok: false, why: 'not_offered', needAck };
      const name = d.opts.remote.name;
      const t = d.typed ? d.typed.value : '';
      if (t !== name || !validConfirmName(t)) return { ok: false, why: 'typed', needAck };
    }
    if (d.choice === ACTION_REMOVE_DEPLOY_KEY && !d.opts.key) return { ok: false, why: 'not_offered', needAck };
    if (needAck && !(d.ack && d.ack.checked === true)) return { ok: false, why: 'ack', needAck };
    return { ok: true, needAck };
  }

  // ---- propose ----
  function proposeFrom(choice) {
    if (flow.busy) return;
    const row = d.row;
    const g = gate();
    if (!g.ok) return;
    let sent;
    if (choice === ACTION_REMOVE_PROJECT) {
      if (d.opts.origin === 'legacy') return; // only the second confirmation screen proposes a legacy removal
      sent = { action: ACTION_REMOVE_PROJECT, subject: row.id, fields: { scope: 'local', legacy_ack: false, detach_key: detachKey() } };
    } else if (choice === ACTION_DELETE_REMOTE_REPO) {
      const r = d.opts.remote;
      sent = { action: ACTION_DELETE_REMOTE_REPO, subject: row.id, fields: { remote_path: r.path, remote_id: r.id, confirm_name: d.typed.value, also_local: alsoLocal() } };
    } else {
      sent = { action: ACTION_REMOVE_DEPLOY_KEY, subject: row.id, fields: { remote_path: d.opts.remote.path } };
    }
    startFlow(sent);
  }

  function proposeLegacy() {
    if (flow.busy || d.opts.origin !== 'legacy' || d.step !== 'legacy') return;
    const g = gate();
    if (!g.ok || !legacyTypedOk()) return;
    startFlow({ action: ACTION_REMOVE_PROJECT, subject: d.row.id, fields: { scope: 'local', legacy_ack: true, detach_key: detachKey() } });
  }

  const legacyTypedOk = () => !!d.legacyTyped && d.legacyTyped.value === d.row.id;

  function proposeOrphan() {
    if (flow.busy || !d.orphan) return;
    startFlow({ action: ACTION_CLEAR_ORPHAN, subject: d.orphan.provider, fields: { project: d.orphan.project } });
  }

  async function startFlow(sent) {
    const my = d.seq;
    d.prevStep = d.step;
    d.step = 'flow';
    host.paint();
    const pv = d.pv;
    const row = d.row;
    await flow.propose(sent, (resp) => expectedLines(sent, row, pv, resp));
    if (gone() || my !== d.seq) return;
    if (flow.phase === 'idle') {
      // the proposal was refused before anything was signed: back to the screen it came from, with the fixed sentence
      d.step = d.prevStep;
      d.note = flow.note;
      flow.note = null;
      host.paint();
    }
  }

  // backFromFlow: from the confirmation screen to the screen the proposal started from.
  function backFromFlow() {
    flow.reset();
    d.step = d.prevStep || 'choose';
    d.note = null;
    host.paint();
  }

  async function onResult(result, sent) {
    resultErr = result.ok ? null : result.err;
    if (!result.ok && result.err && result.err.code === 'delete_partial' && sent && sent.action === ACTION_DELETE_REMOTE_REPO) partialIds.add(sent.subject);
    // the lists are read again whatever the answer (an unknown outcome certainly changed what they say)
    try {
      await host.reload();
    } catch {
      // the list screen reads them again
    }
    if (!gone() && d.step === 'flow') host.paint();
    void sent;
  }

  // followUp: the second, separately signed removal of a project whose remote is gone. Never started by itself.
  // resume: the person pressed a button on a result screen: read the list again, then show the preview of the next change.
  async function resume(choice, followUp) {
    if (api.busy || !d.row) return;
    const id = d.row.id;
    flow.reset();
    try {
      await host.reload();
    } catch {
      // the row lookup below says it
    }
    if (gone()) return;
    const row = rowFor(id);
    if (row === null) {
      d.step = 'fail';
      d.note = { error: false, text: '프로젝트가 이미 목록에 없습니다. 목록을 확인하세요.' };
      host.paint();
      return;
    }
    d.row = row;
    d.opts = deleteOptions(row);
    d.pv = null;
    await pickChoice(choice, followUp);
  }
  const startFollowUp = () => resume(ACTION_REMOVE_PROJECT, true);

  // ---- rendering ----
  const panel = (kids) => el('div', { class: ['panel', 'canvas'] }, kids);
  const noteEl = () => (d.note ? el('p', { class: d.note.error ? ['note', 'err'] : ['note', 'ok'], role: d.note.error ? 'alert' : 'status', 'data-note': 'del' }, d.note.text) : null);
  const cancelBtn = (label) => el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: cancel } }, label || '취소');
  const warnBox = (text, key) => el('div', { class: 'trunc', role: 'note', 'data-emph': key }, [el('strong', {}, text)]);

  api.render = () => {
    switch (d.step) {
      case 'choose': return renderChoose();
      case 'loading': return panel([el('h1', { id: 'h-projects' }, '삭제'), el('p', { class: 'lead', role: 'status' }, DEL_TEXT.previewLoading)]);
      case 'preview': return renderPreview();
      case 'legacy': return renderLegacy();
      case 'orphan': return renderOrphan();
      case 'fail': return panel([el('h1', { id: 'h-projects' }, '삭제'), noteEl(), el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: cancel } }, '목록으로')])]);
      case 'flow': return renderFlow();
      default: return null;
    }
  };

  const remoteLine = (r) => {
    const prov = host.providers().find((p) => isObj(p) && p.id === r.provider);
    const addr = prov && typeof prov.ssh_url === 'string' ? inline(prov.ssh_url, 120) + '/' + r.path : r.path;
    return { host: prov && (prov.kind === 'github' || prov.kind === 'gitlab') ? hostName(prov.kind) : hostName(undefined), addr };
  };

  // the repository and key a detach names: name, address and provider of the row's remote; the key's title (not for a
  // shared GitLab key, whose provider title may be another project's) and fingerprint, as the removal preview gave them
  function keyIdentity() {
    const r = d.opts.remote;
    const rl = remoteLine(r);
    const pv = d.pv;
    return { repo: r.path + ' (주소 ' + rl.addr + ', ' + rl.host + ')', title: pv.deploy_key_shared ? null : inline(pv.deploy_key_title, 64), fp: inline(pv.deploy_key_fingerprint, 80), shared: pv.deploy_key_shared };
  }
  function keyTarget() {
    const k = keyIdentity();
    return '원격 저장소 ' + k.repo + '에서 제공자의 배포 키를 먼저 뗍니다: ' + (k.title !== null ? '제목 ' + k.title + ', ' : '') + '지문 ' + k.fp + (k.shared ? ' (GitLab 공용 키)' : '');
  }
  const keyRemainsBox = () => warnBox(DEL_TEXT.keyRemains + ' (저장소 ' + d.opts.remote.path + ')', 'key-remains');

  function renderChoose() {
    const o = d.opts;
    const id = d.row.id;
    const rl = o.remote ? remoteLine(o.remote) : null;
    const choice = (key, title, text, btnLabel, action) => el('section', { class: 'sec', 'data-choice': key }, [
      el('h2', {}, title), el('p', {}, text),
      el('button', { type: 'button', class: key === 'local' ? ['btn', 'primary'] : 'btn', 'data-focus-key': 'choice-' + key, on: { click: action } }, [btnLabel, el('span', { class: 'sr' }, ' (' + inline(id, 64) + ')')]),
    ]);
    return panel([
      el('h1', { id: 'h-projects' }, '프로젝트 삭제'),
      el('p', { class: 'lead' }, ['무엇을 지울지 고르세요. 프로젝트: ', el('span', { class: 'mono', 'data-field': 'id' }, inline(id, 64)),
        ' (', o.origin === 'legacy' ? '처음부터 있던 프로젝트' : o.origin === 'created' ? '이 도구가 만든 저장소' : o.origin === 'linked' ? '연결한 기존 저장소' : '출처 불명', ')']),
      choice('local', '서버의 복제본과 등록만 지우기', '이 서버의 복제본 폴더, 배포 키 폴더, 프로젝트 등록을 지웁니다. 원격 저장소는 지워지지 않고 그대로 남습니다.', '복제본과 등록 지우기…', () => pickChoice(ACTION_REMOVE_PROJECT, false)),
      o.remoteOffered
        ? choice('remote', '원격 저장소까지 지우기', ['이 도구가 만든 원격 저장소 ', el('span', { class: 'mono' }, rl.addr), '(' + rl.host + ')를 지웁니다. 되돌릴 수 없습니다. 다음 화면에서 저장소 이름을 직접 입력해야 합니다.'], '원격 저장소 지우기…', () => pickChoice(ACTION_DELETE_REMOTE_REPO, false))
        : el('section', { class: 'sec', 'data-choice': 'remote-not-offered' }, [el('h2', {}, '원격 저장소는 이 화면에서 지울 수 없습니다'), el('p', {}, DEL_TEXT.noRemoteDelete[o.remoteWhy])]),
      o.key
        ? choice('key', '배포 키만 떼기', '제공자 저장소에서 이 서버의 배포 키만 뗍니다. 저장소, 다른 키, 프로젝트 등록과 서버의 복제본은 그대로입니다. 키를 떼면 이 서버는 그 저장소를 복제하거나 게시하지 못합니다.', '배포 키만 떼기…', () => pickChoice(ACTION_REMOVE_DEPLOY_KEY, false))
        : null,
      noteEl(),
      el('p', {}, [cancelBtn('취소')]),
    ]);
  }

  function countsList() {
    const pv = d.pv;
    return el('ul', { class: 'pfacts', 'aria-label': '지우면 사라질 수 있는 것(개수만)', 'data-counts': 'list' }, [
      factItem('원격에 올라가지 않은 커밋', pv.unpushed_known ? pv.unpushed_commits + '개' : '알 수 없음 — 있을 수 있습니다', { 'data-field': 'unpushed' }),
      factItem('수정한 파일', pv.changed_known ? pv.changed_files + '개' : '알 수 없음 — 변경이 있을 수 있습니다', { 'data-field': 'changed' }),
      factItem('복제본 폴더 크기', pv.size_known ? sizeText(pv.size_bytes) : pv.folder_exists ? '알 수 없음' : '(폴더 없음)', { 'data-field': 'size' }),
    ]);
  }

  function targetList() {
    const pv = d.pv;
    const row = d.row;
    const r = d.opts.remote;
    const items = [];
    if (detachKey()) items.push(['원격 키', keyTarget()]);
    if (d.choice === ACTION_REMOVE_PROJECT || alsoLocal()) {
      items.push(['등록', '프로젝트 등록(' + inline(row.id, 64) + ')을 등록부에서 지웁니다' + (alsoLocal() ? '(원격 삭제가 끝난 뒤, 같은 서명)' : '')]);
      items.push(['복제본', '서버의 복제본 폴더: ' + inline(row.path, 200) + (pv.folder_exists ? '' : pv.local_removable ? ' (이미 없음)' : ' (확인하지 못함)')]);
      items.push(['키 폴더', '이 프로젝트의 배포 키 폴더(서버): ' + (pv.key_dir_exists ? '있음, 지웁니다' : '없음')]);
    }
    if (d.choice === ACTION_DELETE_REMOTE_REPO && r) {
      const rl = remoteLine(r);
      items.unshift(['원격', rl.host + '의 원격 저장소 ' + r.path + ' (주소 ' + rl.addr + ', 저장소 번호 ' + r.id + ')를 지웁니다. 공개 여부는 이 화면이 알지 못합니다']);
    }
    if (d.choice === ACTION_REMOVE_DEPLOY_KEY && r) {
      items.push(['키', '저장소 ' + r.path + '에서 이 서버의 배포 키(제목 orchd ' + row.id + ')만 뗍니다']);
    }
    return el('ul', { class: 'pfacts', 'aria-label': '지울 대상 전체', 'data-targets': 'list' }, items.map(([k, t]) => el('li', { 'data-target': k }, [el('b', {}, '지움'), ': ', t])));
  }

  function keptList() {
    const row = d.row;
    const r = d.opts.remote;
    const items = [];
    if (d.choice === ACTION_REMOVE_PROJECT) {
      items.push(r ? '원격 저장소 ' + r.path + ': 지워지지 않고 그대로 남습니다' : '원격 저장소: 연결이 없습니다');
      if (r && !d.opts.gone && !detachKey()) items.push('원격 저장소에 붙은 이 서버의 배포 키: 떼지 않으면 그대로 남습니다');
      items.push('이 서버의 작업 기록은 남습니다');
    } else if (d.choice === ACTION_DELETE_REMOTE_REPO) {
      if (!alsoLocal()) items.push('이 서버의 복제본, 배포 키 폴더와 프로젝트 등록은 그대로 남습니다(등록은 "원격 없음" 상태)');
      items.push('이 서버의 작업 기록은 남습니다');
    } else {
      items.push('원격 저장소와 그 안의 다른 키, 프로젝트 등록(' + inline(row.id, 64) + ')과 서버의 복제본은 그대로입니다');
    }
    return el('ul', { 'aria-label': '지워지지 않는 것', 'data-kept': 'list' }, items.map((t) => el('li', {}, t)));
  }

  function renderPreview() {
    const pv = d.pv;
    const row = d.row;
    const o = d.opts;
    const g = gate();
    const isRemote = d.choice === ACTION_DELETE_REMOTE_REPO;
    const isKey = d.choice === ACTION_REMOVE_DEPLOY_KEY;
    const legacy = o.origin === 'legacy' && d.choice === ACTION_REMOVE_PROJECT;
    const title = isRemote ? '원격 저장소 지우기' : isKey ? '배포 키만 떼기' : d.followUp ? '등록 지우기(두 번째 서명)' : '복제본과 등록 지우기';
    const blockText = g.why === 'busy' ? DEL_TEXT.busy : g.why === 'busy_unknown' ? DEL_TEXT.busyUnknown : g.why === 'local_unsafe' ? DEL_TEXT.localUnsafe : null;
    const needAck = g.needAck === true;
    const proposeLabel = '변경 내용 확인 (아직 서명하지 않음)';
    const nextBtn = legacy
      ? el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'legacy-next', disabled: !!blockText || (needAck && !(d.ack && d.ack.checked)), on: { click: () => { if (!gate().ok) return; d.step = 'legacy'; host.paint(); } } }, '다음: 두 번째 확인')
      : el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'propose', 'data-action': isRemote ? ACTION_DELETE_REMOTE_REPO : isKey ? ACTION_REMOVE_DEPLOY_KEY : ACTION_REMOVE_PROJECT,
        disabled: !g.ok || flow.busy, 'aria-describedby': !g.ok && !blockText ? 'del-gate-hint' : null, on: { click: () => proposeFrom(d.choice) } }, proposeLabel);
    return panel([
      el('h1', { id: 'h-projects' }, title),
      el('p', { class: 'lead' }, ['프로젝트 ', el('span', { class: 'mono' }, inline(row.id, 64)), '. 아래 내용이 지워지거나 그대로 남습니다. 이 화면에서는 아직 아무것도 지워지지 않습니다.']),
      d.followUp ? el('p', { class: 'note', 'data-note': 'follow-up' }, o.gone ? '원격 저장소 삭제는 이미 끝났습니다. 이것은 서버의 복제본과 등록을 지우는 별도의 변경이며 다시 서명해야 합니다.' : '원격 저장소가 지워졌는지 알 수 없는 상태입니다. 이것은 서버의 복제본과 등록을 지우는 별도의 변경이며 다시 서명해야 합니다.') : null,
      blockText ? el('p', { class: ['note', 'err'], role: 'alert', 'data-block': g.why }, blockText) : null,
      el('section', { class: 'sec' }, [el('h2', {}, '지울 대상'), targetList()]),
      el('section', { class: 'sec' }, [el('h2', {}, '그대로 남는 것'), keptList()]),
      touchesLocal() || isRemote ? el('section', { class: 'sec' }, [el('h2', {}, '지우면 사라질 수 있는 것'), countsList()]) : null,
      pv.static_remote && !isKey ? el('p', { class: 'note', 'data-note': 'static' }, DEL_TEXT.staticRemote) : null,
      isRemote ? warnBox(DEL_TEXT.irreversibleRemote, 'remote-irreversible') : isKey ? null : warnBox(DEL_TEXT.irreversible, 'irreversible'),
      touchesLocal() && maybeGone() ? warnBox(o.gone ? DEL_TEXT.remoteAlreadyGone : DEL_TEXT.remoteMaybeGone, 'remote-gone') : null,
      alsoLocal() ? warnBox(DEL_TEXT.alsoWarn, 'also-local') : null,
      alsoLocal() ? warnBox(DEL_TEXT.irreversible, 'irreversible') : null,
      d.choice === ACTION_REMOVE_PROJECT && o.origin === 'created' && o.remote !== null && !o.gone && !d.followUp ? el('p', { class: 'note', 'data-note': 'created-local-only' }, DEL_TEXT.createdLocalOnly) : null,
      isRemote ? el('div', { class: 'form' }, [
        el('div', { class: 'field' }, [el('div', { class: 'row' }, [d.also, ' ', el('label', { for: 'del-also' }, DEL_TEXT.alsoLabel)]), el('span', { class: 'hint' }, DEL_TEXT.alsoHint)]),
        el('div', { class: 'field' }, [el('label', { for: 'del-typed' }, DEL_TEXT.typedLabel + ': ' + o.remote.name), d.typed, el('span', { class: 'hint', id: 'del-gate-hint' }, DEL_TEXT.typedHint)]),
      ]) : null,
      detachOffered() ? (() => {
        const k = keyIdentity();
        return el('div', { class: 'form', 'data-detach': 'option' }, [el('div', { class: 'field' }, [
          el('div', { class: 'row' }, [d.detach, ' ', el('label', { for: 'del-detach' }, DEL_TEXT.detachLabel)]),
          el('span', { class: 'hint' }, DEL_TEXT.detachHint),
          el('ul', { class: 'pfacts', 'aria-label': '떼는 배포 키' }, [
            factItem('원격 저장소', k.repo, { 'data-key': 'repo' }),
            k.title !== null ? factItem('키 제목', k.title, { 'data-key': 'title' }) : null,
            factItem('키 지문', k.fp, { 'data-key': 'fingerprint' }),
          ].filter(Boolean)),
          k.shared ? el('span', { class: 'hint' }, DEL_TEXT.detachShared) : null,
        ].filter(Boolean))]);
      })() : null,
      detachOffered() && !detachKey() ? keyRemainsBox() : null,
      pv.remote_key_may_remain && d.choice === ACTION_REMOVE_PROJECT && o.key && !detachOffered()
        ? el('div', { class: 'trunc', role: 'note', 'data-hint': 'detach-key' }, [el('p', {}, DEL_TEXT.keyHint),
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'detach-first', on: { click: () => pickChoice(ACTION_REMOVE_DEPLOY_KEY, false) } }, '배포 키 먼저 떼기')]) : null,
      needAck ? el('div', { class: 'form' }, [el('div', { class: 'field' }, [el('div', { class: 'row' }, [d.ack, ' ', el('label', { for: 'del-ack' }, DEL_TEXT.ackLabel)]), g.why === 'ack' ? el('span', { class: 'hint' }, DEL_TEXT.ackNeeded) : null])]) : null,
      noteEl(),
      el('p', { class: 'note' }, DEL_TEXT.gate),
      el('div', { class: 'row' }, [nextBtn, ' ', el('button', { type: 'button', class: 'btn', 'data-focus-key': 'back-choose', on: { click: () => { d.step = 'choose'; d.note = null; host.paint(); } } }, '뒤로'), ' ', cancelBtn('취소')]),
    ].filter((x) => x !== null && x !== undefined));
  }

  function renderLegacy() {
    const pv = d.pv;
    const row = d.row;
    const ok = legacyTypedOk();
    return panel([
      el('h1', { id: 'h-projects', 'data-screen': 'legacy-confirm' }, '두 번째 확인: 처음부터 있던 프로젝트'),
      warnBox(DEL_TEXT.legacyBody, 'legacy'),
      el('ul', { class: 'pfacts', 'aria-label': '지우면 사라질 수 있는 것(개수만)', 'data-counts': 'legacy' }, [
        factItem('프로젝트', inline(row.id, 64), { 'data-field': 'id' }),
        factItem('복제본 폴더', inline(row.path, 200), { 'data-field': 'path' }),
        factItem('원격에 올라가지 않은 커밋', pv.unpushed_known ? pv.unpushed_commits + '개' : '알 수 없음 — 있을 수 있습니다', { 'data-field': 'unpushed' }),
        factItem('수정한 파일', pv.changed_known ? pv.changed_files + '개' : '알 수 없음 — 변경이 있을 수 있습니다', { 'data-field': 'changed' }),
      ]),
      pv.static_remote ? el('p', { class: 'note', 'data-note': 'static' }, DEL_TEXT.legacyStatic) : null,
      warnBox(DEL_TEXT.irreversible, 'irreversible'),
      el('div', { class: 'form' }, [el('div', { class: 'field' }, [el('label', { for: 'del-legacy-typed' }, DEL_TEXT.legacyTyped + ': ' + row.id), d.legacyTyped,
        el('span', { class: 'hint', id: 'del-gate-hint' }, '입력한 이름이 프로젝트 이름과 글자 그대로 같을 때만 다음 단계로 갈 수 있습니다.')])]),
      noteEl(),
      el('p', { class: 'note' }, DEL_TEXT.gate),
      el('div', { class: 'row' }, [
        el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'propose', 'data-action': ACTION_REMOVE_PROJECT, disabled: !ok || flow.busy, 'aria-describedby': ok ? null : 'del-gate-hint', on: { click: proposeLegacy } }, '변경 내용 확인 (아직 서명하지 않음)'),
        ' ', el('button', { type: 'button', class: 'btn', 'data-focus-key': 'back-preview', on: { click: () => { d.step = 'preview'; d.note = null; host.paint(); } } }, '뒤로'), ' ', cancelBtn('취소'),
      ]),
    ]);
  }

  function renderOrphan() {
    const o = d.orphan;
    const text = o.origin === 'created' ? DEL_TEXT.orphanCreated : o.origin === 'linked' ? DEL_TEXT.orphanLinked : DEL_TEXT.orphanUnknown;
    return panel([
      el('h1', { id: 'h-projects' }, '고아 기록 지우기'),
      el('p', { class: 'lead' }, ['제공자 ', el('span', { class: 'mono' }, inline(o.provider, 64)), '의 기록 ', el('span', { class: 'mono', 'data-field': 'project' }, inline(o.project, 120)), '를 이 서버에서 지웁니다.']),
      el('div', { class: 'trunc', role: 'note', 'data-orphan-origin': o.origin }, [el('strong', {}, text)]),
      el('p', { class: 'note' }, '이 도구는 이 기록에 대해 제공자(GitHub·GitLab)에서 아무것도 지우지 않습니다. 이 서버의 기록 한 줄만 지웁니다.'),
      noteEl(),
      el('div', { class: 'row' }, [
        el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'propose', 'data-action': ACTION_CLEAR_ORPHAN, disabled: flow.busy, on: { click: proposeOrphan } }, '변경 내용 확인 (아직 서명하지 않음)'),
        ' ', cancelBtn('취소'),
      ]),
    ]);
  }

  // ---- confirmation and result ----
  function renderFlow() {
    if (flow.phase === 'proposing' || flow.phase === 'idle') {
      return panel([el('h1', { id: 'h-projects' }, '변경 내용을 만드는 중'), el('p', { class: 'lead', role: 'status' }, '서버가 서명할 내용을 만들고 있습니다. 아직 아무것도 지워지지 않았습니다…')]);
    }
    if (flow.phase === 'confirm') return renderConfirm();
    return renderResult();
  }

  function renderConfirm() {
    const s = flow.sent;
    const f = s.fields;
    const facts = [];
    let lead;
    let warn = null;
    if (s.action === ACTION_REMOVE_PROJECT) {
      lead = f.detach_key
        ? '아래 내용이 전부입니다. 서명하면 먼저 원격 저장소에서 이 서버의 배포 키를 떼고, 그다음 이 서버의 프로젝트 등록, 복제본, 배포 키 폴더가 바로 지워집니다. 원격 저장소 자체는 지워지지 않습니다.'
        : '아래 내용이 전부입니다. 서명하면 이 서버의 프로젝트 등록, 복제본, 배포 키 폴더가 바로 지워집니다. 원격 저장소는 지워지지 않습니다.';
      facts.push(factItem('프로젝트', s.subject, { 'data-field': 'id' }), factItem('범위', '서버의 복제본과 등록만(원격 저장소는 그대로)', { 'data-field': 'scope' }),
        factItem('처음부터 있던 프로젝트 확인(legacy_ack)', f.legacy_ack ? '예 — 두 번째 확인을 했습니다' : '아니오', { 'data-field': 'legacy_ack' }));
      const remote = remoteOf(d.row);
      if (f.detach_key && remote !== null && d.pv && d.pv.detach_key_available === true) {
        const k = keyIdentity();
        facts.push(factItem('배포 키도 함께 떼기(detach_key)', '예 — 지우기 전에 먼저 뗍니다', { 'data-field': 'detach_key' }),
          factItem('키를 떼는 원격 저장소', k.repo, { 'data-field': 'key_repo' }));
        if (k.title !== null) facts.push(factItem('떼는 키 제목', k.title, { 'data-field': 'key_title' }));
        else facts.push(factItem('키 확인 방법', DEL_TEXT.detachShared, { 'data-field': 'key_shared' }));
        facts.push(factItem('떼는 키 지문(서버가 알린 값)', inline(flow.resp.deploy_key_fingerprint, 80), { 'data-field': 'key_fingerprint' }));
      } else {
        facts.push(factItem('배포 키도 함께 떼기(detach_key)', '아니오', { 'data-field': 'detach_key' }));
      }
      warn = el('div', {}, [warnBox(DEL_TEXT.irreversible, 'irreversible'), !f.detach_key && remote !== null && !d.opts.gone ? keyRemainsBox() : null].filter(Boolean));
    } else if (s.action === ACTION_DELETE_REMOTE_REPO) {
      lead = '아래 내용이 전부입니다. 서명하면 제공자의 원격 저장소가 바로 지워집니다. 되돌릴 수 없습니다.';
      facts.push(factItem('프로젝트', s.subject, { 'data-field': 'id' }), factItem('원격 저장소', f.remote_path, { 'data-field': 'remote_path' }),
        factItem('저장소 번호', f.remote_id, { 'data-field': 'remote_id' }), factItem('입력한 저장소 이름', f.confirm_name, { 'data-field': 'confirm_name' }),
        factItem('이어서 서버의 복제본, 키 폴더와 프로젝트 등록도 지움(also_local)', f.also_local ? '예 — 같은 서명으로' : '아니오 — 복제본과 등록은 남음', { 'data-field': 'also_local' }));
      warn = warnBox(DEL_TEXT.irreversibleRemote, 'remote-irreversible');
    } else if (s.action === ACTION_REMOVE_DEPLOY_KEY) {
      lead = '아래 내용이 전부입니다. 서명하면 제공자 저장소에서 이 서버의 배포 키만 떼어집니다. 저장소와 다른 키는 그대로입니다.';
      facts.push(factItem('프로젝트', s.subject, { 'data-field': 'id' }), factItem('원격 저장소', f.remote_path, { 'data-field': 'remote_path' }),
        factItem('떼는 배포 키 지문(서버가 알린 값)', inline(flow.resp.deploy_key_fingerprint, 80), { 'data-field': 'fingerprint' }));
      warn = el('div', { class: 'trunc', role: 'note', 'data-emph': 'key-detach' }, [el('strong', {}, '[중요] 키를 떼면 이 서버는 이 저장소를 복제하거나 게시하지 못합니다.')]);
    } else {
      lead = '아래 내용이 전부입니다. 서명하면 이 서버의 고아 기록 한 줄이 지워집니다. 제공자에서는 아무것도 지워지지 않습니다.';
      facts.push(factItem('제공자', s.subject, { 'data-field': 'provider' }), factItem('기록', f.project, { 'data-field': 'project' }));
    }
    return confirmPanel(flow, { title: '변경 내용 확인', lead, warn, facts, extra: [], onSign: () => flow.sign(), onCancel: backFromFlow });
  }

  const stepList = (rows) => el('ol', { class: 'steps', 'aria-label': '지우기 단계' }, rows.map(([k, label, status, text]) => el('li', { 'data-step': k, 'data-status': status }, [el('b', {}, label), ' — ', text])));

  function renderResult() {
    const r = flow.result;
    const s = flow.sent;
    const back = el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: cancel } }, '목록으로')]);
    if (r === null) {
      return panel([el('h1', { id: 'h-projects' }, '지우는 중'), el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'pending' }, DEL_TEXT.sending)]);
    }
    if (r.ok) return renderOk(r.data, s, back);
    const err = resultErr || r.err;
    const code = err && typeof err.code === 'string' ? err.code : '';
    if (code === 'delete_partial_local') return renderPartialLocal(err, s, back);
    const unknown = isUnknown(err) || code === 'delete_partial' || code === 'outcome_unknown';
    if (code === 'delete_partial' && s.action === ACTION_REMOVE_PROJECT) {
      // W3g: only the key detach of a remove_project ends this way; nothing local was touched, the registration stays
      return panel([el('h1', { id: 'h-projects' }, '배포 키 떼기 결과를 알 수 없습니다'),
        el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'unknown' }, DEL_REMOVE_TEXT.partialDetach), back]);
    }
    if (code === 'delete_partial') {
      return panel([el('h1', { id: 'h-projects' }, '원격 저장소 삭제 결과를 알 수 없습니다'),
        el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'unknown' }, ERROR_TEXT.delete_partial),
        el('p', { class: 'note', 'data-note': 'partial-remote' }, DEL_TEXT.partialRemoteDo),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'again-remote', on: { click: () => resume(ACTION_DELETE_REMOTE_REPO, false) } }, '원격 저장소 다시 지우기…'), ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'again-local', on: { click: () => resume(ACTION_REMOVE_PROJECT, true) } }, '복제본과 등록 지우기…'),
        ]), back]);
    }
    if (unknown) {
      return panel([el('h1', { id: 'h-projects' }, '결과를 알 수 없습니다'),
        el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'unknown' }, s.action === ACTION_REMOVE_PROJECT && s.fields.detach_key ? DEL_REMOVE_TEXT.unknownDetach
          : has(DEL_TEXT.unknown, s.action) ? DEL_TEXT.unknown[s.action] : ERROR_TEXT.outcome_unknown), back]);
    }
    return panel([el('h1', { id: 'h-projects' }, '지우지 못했습니다'),
      el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'error' }, failText(err, s) || failureText(err)), back]);
  }

  function localRows(local, also) {
    const c = isObj(local) && DONE_STATES.includes(local.clone) ? local.clone : null;
    const k = isObj(local) && DONE_STATES.includes(local.key) ? local.key : null;
    void also;
    return [
      ['clone', '서버의 복제본 폴더', c === null ? 'unknown' : c, c === null ? '결과를 알 수 없음' : CLONE_STATE_TEXT[c]],
      ['key', '배포 키 폴더', k === null ? 'unknown' : k, k === null ? '결과를 알 수 없음' : CLONE_STATE_TEXT[k]],
    ];
  }

  // okShape: the success answer describes what was asked (the same project, the expected state)
  function okShape(data, s) {
    if (!isObj(data)) return false;
    if (s.action === ACTION_REMOVE_PROJECT) {
      // W3g: the answer says whether the key was detached, and how, exactly as signed
      if (data.id !== s.subject || data.removed !== true || data.detach_key !== s.fields.detach_key) return false;
      return s.fields.detach_key ? KEY_STATES.includes(data.key_state) : !has(data, 'key_state');
    }
    if (s.action === ACTION_DELETE_REMOTE_REPO) {
      // W3g: also_local removed the registration in the same signature; without it the registration stays
      return data.id === s.subject && (data.remote_state === 'deleted' || data.remote_state === 'requested') && data.registry === (s.fields.also_local ? 'removed' : 'kept');
    }
    if (s.action === ACTION_REMOVE_DEPLOY_KEY) return data.id === s.subject && data.key_removed === true;
    return data.provider === s.subject && data.project === s.fields.project && data.cleared === true;
  }

  function renderOk(data, s, back) {
    if (!okShape(data, s)) {
      return panel([el('h1', { id: 'h-projects' }, '결과를 확인하세요'), el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'check' }, DEL_TEXT.noResult), back]);
    }
    const kids = [el('h1', { id: 'h-projects' }, '완료')];
    if (s.action === ACTION_REMOVE_PROJECT) {
      const detached = s.fields.detach_key === true;
      kids.push(el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, '프로젝트 ' + inline(s.subject, 64) + '를 이 서버에서 지웠습니다. ' +
        (detached ? '원격 저장소에서 이 서버의 배포 키도 뗐습니다(저장소 자체는 그대로입니다).' : '원격 저장소는 그대로입니다.')),
        stepList([...(detached ? [['remote-key', '원격 저장소의 배포 키', data.key_state, KEY_STATE_TEXT[data.key_state]]] : []),
          ['registry', '프로젝트 등록', 'removed', '지웠습니다'], ...localRows(isObj(data) ? data.local : null)]));
      const r = remoteOf(d.row);
      if (!detached && r !== null && !(d.opts && d.opts.gone)) kids.push(el('p', { class: 'note', 'data-note': 'key-remains' }, '이 서버의 배포 키는 원격 저장소 ' + r.path + '에 남아 있습니다. 필요 없으면 제공자 저장소 설정(Deploy keys)에서 직접 지우세요.'));
    } else if (s.action === ACTION_DELETE_REMOTE_REPO) {
      const requested = isObj(data) && data.remote_state === 'requested';
      const known = true;
      kids.push(el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, '원격 저장소 삭제를 요청했습니다' + (requested ? '(제공자가 지연 삭제로 처리할 수 있습니다).' : '.')));
      const rows = [['remote', '원격 저장소 삭제', known ? 'done' : 'unknown', known ? (requested ? '요청됨' : '완료') : '결과를 알 수 없음']];
      if (s.fields.also_local) {
        // W3g: the same signature removed the registration: nothing is left to sign
        rows.push(...localRows(isObj(data) ? data.local : null, true));
        rows.push(['registry', '프로젝트 등록', 'removed', '지웠습니다(같은 서명)']);
        kids.push(stepList(rows));
      } else {
        rows.push(['registry', '프로젝트 등록', 'kept', '남음("원격 없음" 상태)']);
        kids.push(stepList(rows));
        kids.push(el('div', { class: 'trunc', role: 'note', 'data-followup': 'remove' }, [
          el('p', {}, '서버의 복제본과 등록은 그대로 남아 있습니다. 지우려면 별도의 서명이 필요하며 자동으로 하지 않습니다.'),
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'follow-up', on: { click: startFollowUp } }, '복제본과 등록 지우기…')]));
      }
    } else if (s.action === ACTION_REMOVE_DEPLOY_KEY) {
      kids.push(el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, '이 서버의 배포 키를 제공자 저장소에서 뗐습니다. 저장소와 프로젝트는 그대로입니다.'),
        stepList([['key', '제공자 저장소의 배포 키', 'removed', '뗐습니다']]));
    } else {
      kids.push(el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, '고아 기록을 지웠습니다. 제공자에는 아무것도 하지 않았습니다.'),
        stepList([['orphan', '이 서버의 고아 기록', 'removed', '지웠습니다']]));
    }
    kids.push(back);
    return panel(kids);
  }

  // the per-item screen of delete_partial_local: what is gone, what remains, what to do
  function renderPartialLocal(err, s, back) {
    const det = partialDetail(err);
    const kids = [el('h1', { id: 'h-projects' }, '서버의 파일을 일부만 지웠습니다'),
      el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'partial-local' }, ERROR_TEXT.delete_partial_local)];
    if (det === null) {
      kids.push(el('p', { class: 'note', 'data-note': 'detail-invalid' }, '서버가 알려 준 항목별 상태를 확인할 수 없습니다. 서버 관리자가 서버의 폴더를 확인해야 합니다. 목록을 다시 읽었습니다.'));
    } else {
      kids.push(stepList([
        ...(det.keyState !== null ? [['remote-key', '원격 저장소의 배포 키', det.keyState, KEY_STATE_TEXT[det.keyState]]] : []),
        ['registry', '프로젝트 등록', det.registryRemoved ? 'removed' : 'kept', det.registryRemoved ? '지웠습니다' : '남아 있습니다'],
        ['remote', '원격 저장소', det.remoteDeleted ? 'removed' : 'kept', det.remoteDeleted ? '이 요청에서 지웠습니다' : '그대로 있습니다'],
        ['clone', '서버의 복제본 폴더', det.clone, CLONE_STATE_TEXT[det.clone]],
        ['key', '배포 키 폴더', det.key, CLONE_STATE_TEXT[det.key]],
      ]));
      kids.push(el('p', { class: 'note', 'data-note': 'reason' }, PARTIAL_REASON_TEXT[det.reason]));
      const todo = det.registryRemoved
        ? '할 일: 등록은 이미 지워져 서버에 폴더가 등록 없이 남았을 수 있습니다. 서버 관리자가 남은 폴더를 직접 지우거나, 같은 경로를 다시 등록한 뒤 "배포 키도 함께 떼기"를 끄고 "복제본과 등록 지우기"를 새로 서명해 정리하세요(' +
          (det.keyState !== null ? '배포 키는 이미 처리되었습니다. ' : det.remoteDeleted ? '원격 저장소는 이미 지워졌습니다. ' : '') + '처음부터 있던 프로젝트는 두 번째 확인이 다시 필요합니다).'
        : '할 일: 등록은 남아 있습니다. 목록을 확인한 뒤 "복제본과 등록 지우기"를 새로 서명해 다시 정리하세요. 자동으로 다시 시도하지 않았습니다.';
      kids.push(el('p', { class: 'note', 'data-note': 'todo' }, todo));
      if (det.clone === 'moved_aside' || det.key === 'moved_aside') kids.push(el('p', { class: 'note' }, '.deleting- 이름으로 옮겨진 폴더는 서버를 다시 시작하면 정리되며, 관리자가 직접 지울 수도 있습니다.'));
    }
    void s;
    kids.push(back);
    return panel(kids);
  }

  return api;
}
