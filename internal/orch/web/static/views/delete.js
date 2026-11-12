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
//  * `also_local` is a signed field of ONE signature; the follow-up "등록 지우기" is a second,
//    separately signed remove_project that is never started or signed automatically;
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
const PARTIAL_REASONS = ['project_busy', 'busy_check_failed', 'in_progress', 'remove_failed'];

// The members of the removal-preview answer (internal/orch/api removalPreview; TestConstantsAgreeWithWeb
// reads them from the Go source).
export const PREVIEW_KEYS = ['id', 'origin', 'legacy', 'has_remote', 'remote_key_may_remain', 'remote_deleted_at', 'static_remote', 'busy', 'busy_unknown',
  'folder_exists', 'is_git_repo', 'local_removable', 'key_dir_exists', 'size_bytes', 'size_known', 'unpushed_commits', 'unpushed_known', 'changed_files', 'changed_known'];

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
  keyHint: '이 프로젝트에는 원격 저장소 연결이 있어, 서버의 복제본과 등록을 지워도 이 서버의 배포 키는 제공자 저장소에 남을 수 있습니다. 배포 키를 먼저 뗄까요?',
  ackLabel: '원격에 올라가지 않은 커밋이나 변경이 있을 수 있고, 그것이 함께 사라지는 것을 알고 지웁니다.',
  ackNeeded: '위 항목을 체크해야 다음으로 갈 수 있습니다.',
  typedLabel: '저장소 이름을 직접 입력하세요(대소문자까지 같아야 합니다)',
  typedHint: '입력한 이름이 저장소 이름과 글자 그대로 같을 때만 다음 단계로 갈 수 있습니다. 붙여넣어도 됩니다.',
  alsoLabel: '원격 저장소를 지운 뒤 이 서버의 복제본과 배포 키 폴더도 이어서 지웁니다(같은 서명 한 번)',
  alsoHint: '프로젝트 등록은 "원격 없음" 상태로 남습니다. 등록까지 지우려면 별도로 서명하는 "등록 지우기"가 한 번 더 필요하며 자동으로 하지 않습니다.',
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
  partialRemoteDo: 'GitHub·GitLab 화면에서 저장소가 남아 있는지 확인하세요. 서버의 복제본과 등록은 지우지 않았습니다. 확인한 뒤 서버의 항목은 "복제본과 등록 지우기"로 정리하세요(원격이 이미 지워진 경우 그 복제본이 유일한 사본일 수 있습니다). 자동으로 다시 시도하지 않았습니다.',
  noResult: '결과를 읽지 못했습니다. 목록을 다시 확인하세요.',
  orphanCreated: '이 도구가 만든 저장소가 제공자에 남아 있을 수 있습니다. 이 도구는 원격 저장소를 지우지 않습니다: 먼저 GitHub·GitLab에서 직접 지운 뒤에 이 기록을 지우세요. 이 기록만 지우고 저장소를 그대로 두면 같은 이름으로 다시 만들 수 없습니다.',
  orphanLinked: '원래 있던 저장소입니다. 절대 지우지 마세요. 이 도구는 제공자에서 아무것도 지우지 않고 이 서버의 기록 한 줄만 지웁니다. 이 서버의 배포 키가 붙어 있을 수 있으니 필요하면 제공자 저장소 설정(Deploy keys)에서 제목이 orchd로 시작하는 키를 직접 제거하세요.',
  orphanUnknown: '출처를 알 수 없는 기록입니다. 지우기 전에 제공자 화면에서 저장소가 있는지, 이 도구가 만든 것인지 먼저 확인하세요. 이 도구는 제공자에서 아무것도 지우지 않고 이 서버의 기록 한 줄만 지웁니다.',
};

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
  return { origin, remote, gone, local: true, remoteOffered: remoteWhy === null, remoteWhy, key: remote !== null && !gone };
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
  return { remaining: d.remaining, clone: d.clone, key: d.key, registryRemoved: d.registry_removed, remoteDeleted: d.remote_deleted, reason: d.reason };
}

// --- the controller ---------------------------------------------------------------

// host: {paint(), rows(), providers(), reload(): Promise, toList()}
export function makeDelete(ctx, host) {
  const d = {
    step: 'idle', // idle | choose | loading | preview | legacy | flow | orphan | fail
    row: null, opts: null, choice: null, pv: null, note: null, followUp: false, orphan: null, seq: 0,
    typed: null, ack: null, also: null, legacyTyped: null, blocked: null,
  };
  const gone = () => !!(ctx.signal && ctx.signal.aborted);
  let resultErr = null;

  const flow = makeFlow(ctx, { onChange: () => { if (!gone() && d.step === 'flow') host.paint(); }, onResult, kindOf: () => undefined });

  const api = {
    get active() { return d.step !== 'idle'; },
    get busy() { return flow.busy || d.step === 'loading'; },
    flow,
    state: d,
  };

  function reset() {
    d.seq++;
    flow.reset();
    Object.assign(d, { step: 'idle', row: null, opts: null, choice: null, pv: null, note: null, followUp: false, orphan: null, typed: null, ack: null, also: null, legacyTyped: null, blocked: null });
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
    d.choice = choice;
    d.followUp = !!followUp;
    d.note = null;
    d.typed = null;
    d.ack = null;
    d.also = null;
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
      d.note = { error: true, text: err && err.status === 0 ? '서버에 연결하지 못했습니다. 아무것도 서명하거나 보내지 않았습니다.' : failureText(err) };
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
    const key = g.ok + '|' + g.needAck + '|' + g.why + '|' + (d.choice === ACTION_DELETE_REMOTE_REPO && d.also ? d.also.checked : '') + '|' + (d.step === 'legacy' && legacyTypedOk());
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
  const touchesLocal = () => d.choice === ACTION_REMOVE_PROJECT || alsoLocal();

  // ackNeeded: files may be lost with the local clone (something unpushed, changed, unknown, or the remote is gone).
  function ackNeeded() {
    const pv = d.pv;
    if (!pv || !touchesLocal() || !pv.folder_exists) return false;
    return pv.unpushed_commits > 0 || !pv.unpushed_known || pv.changed_files > 0 || !pv.changed_known || d.opts.gone;
  }

  // gate: may the propose button be on? {ok, why}
  function gate() {
    const pv = d.pv;
    if (!pv) return { ok: false, why: 'preview' };
    if (pv.busy) return { ok: false, why: 'busy' };
    if (pv.busy_unknown) return { ok: false, why: 'busy_unknown' };
    const needAck = ackNeeded();
    if (touchesLocal() && pv.folder_exists && !pv.local_removable) return { ok: false, why: 'local_unsafe', needAck };
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
      sent = { action: ACTION_REMOVE_PROJECT, subject: row.id, fields: { scope: 'local', legacy_ack: false } };
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
    startFlow({ action: ACTION_REMOVE_PROJECT, subject: d.row.id, fields: { scope: 'local', legacy_ack: true } });
  }

  const legacyTypedOk = () => !!d.legacyTyped && d.legacyTyped.value === d.row.id;

  function proposeOrphan() {
    if (flow.busy || !d.orphan) return;
    startFlow({ action: ACTION_CLEAR_ORPHAN, subject: d.orphan.provider, fields: { project: d.orphan.project } });
  }

  async function startFlow(sent) {
    d.prevStep = d.step;
    d.step = 'flow';
    host.paint();
    await flow.propose(sent);
    if (gone()) return;
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
  async function startFollowUp() {
    if (api.busy || !d.row) return;
    const id = d.row.id;
    flow.reset();
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
    await pickChoice(ACTION_REMOVE_PROJECT, true);
  }

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
    if (d.choice === ACTION_REMOVE_PROJECT || alsoLocal()) {
      if (d.choice === ACTION_REMOVE_PROJECT) items.push(['등록', '프로젝트 등록(' + inline(row.id, 64) + ')을 등록부에서 지웁니다']);
      items.push(['복제본', '서버의 복제본 폴더: ' + (pv.folder_exists ? inline(row.path, 200) : inline(row.path, 200) + ' (이미 없음)')]);
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
      items.push('이 서버의 작업 기록은 남습니다');
    } else if (d.choice === ACTION_DELETE_REMOTE_REPO) {
      items.push(alsoLocal() ? '프로젝트 등록은 "원격 없음" 상태로 남습니다(등록 지우기는 별도 서명)' : '이 서버의 복제본, 배포 키 폴더와 프로젝트 등록은 그대로 남습니다');
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
      d.followUp ? el('p', { class: 'note', 'data-note': 'follow-up' }, '원격 저장소 삭제는 이미 끝났습니다. 이것은 서버의 복제본과 등록을 지우는 별도의 변경이며 다시 서명해야 합니다.') : null,
      blockText ? el('p', { class: ['note', 'err'], role: 'alert', 'data-block': g.why }, blockText) : null,
      el('section', { class: 'sec' }, [el('h2', {}, '지울 대상'), targetList()]),
      el('section', { class: 'sec' }, [el('h2', {}, '그대로 남는 것'), keptList()]),
      touchesLocal() || isRemote ? el('section', { class: 'sec' }, [el('h2', {}, '지우면 사라질 수 있는 것'), countsList()]) : null,
      pv.static_remote && !isKey ? el('p', { class: 'note', 'data-note': 'static' }, DEL_TEXT.staticRemote) : null,
      o.origin !== 'legacy' && !isRemote && !isKey ? el('p', { class: 'note' }, o.origin === 'created' ? '이 도구가 만든 프로젝트입니다.' : '') : null,
      isRemote ? warnBox(DEL_TEXT.irreversibleRemote, 'remote-irreversible') : isKey ? null : warnBox(DEL_TEXT.irreversible, 'irreversible'),
      touchesLocal() && o.gone ? warnBox(DEL_TEXT.remoteAlreadyGone, 'remote-gone') : null,
      isRemote ? el('div', { class: 'form' }, [
        el('div', { class: 'field' }, [el('div', { class: 'row' }, [d.also, ' ', el('label', { for: 'del-also' }, DEL_TEXT.alsoLabel)]), el('span', { class: 'hint' }, DEL_TEXT.alsoHint)]),
        el('div', { class: 'field' }, [el('label', { for: 'del-typed' }, DEL_TEXT.typedLabel + ': ' + o.remote.name), d.typed, el('span', { class: 'hint', id: 'del-gate-hint' }, DEL_TEXT.typedHint)]),
      ]) : null,
      pv.remote_key_may_remain && d.choice === ACTION_REMOVE_PROJECT && o.key
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
      lead = '아래 내용이 전부입니다. 서명하면 이 서버의 프로젝트 등록, 복제본, 배포 키 폴더가 바로 지워집니다. 원격 저장소는 지워지지 않습니다.';
      facts.push(factItem('프로젝트', s.subject, { 'data-field': 'id' }), factItem('범위', '서버의 복제본과 등록만(원격은 그대로)', { 'data-field': 'scope' }),
        factItem('처음부터 있던 프로젝트 확인(legacy_ack)', f.legacy_ack ? '예 — 두 번째 확인을 했습니다' : '아니오', { 'data-field': 'legacy_ack' }));
      warn = warnBox(DEL_TEXT.irreversible, 'irreversible');
    } else if (s.action === ACTION_DELETE_REMOTE_REPO) {
      lead = '아래 내용이 전부입니다. 서명하면 제공자의 원격 저장소가 바로 지워집니다. 되돌릴 수 없습니다.';
      facts.push(factItem('프로젝트', s.subject, { 'data-field': 'id' }), factItem('원격 저장소', f.remote_path, { 'data-field': 'remote_path' }),
        factItem('저장소 번호', f.remote_id, { 'data-field': 'remote_id' }), factItem('입력한 저장소 이름', f.confirm_name, { 'data-field': 'confirm_name' }),
        factItem('이어서 서버의 복제본과 키 폴더도 지움(also_local)', f.also_local ? '예' : '아니오', { 'data-field': 'also_local' }));
      warn = warnBox(DEL_TEXT.irreversibleRemote, 'remote-irreversible');
    } else if (s.action === ACTION_REMOVE_DEPLOY_KEY) {
      lead = '아래 내용이 전부입니다. 서명하면 제공자 저장소에서 이 서버의 배포 키만 떼어집니다. 저장소와 다른 키는 그대로입니다.';
      facts.push(factItem('프로젝트', s.subject, { 'data-field': 'id' }), factItem('원격 저장소', f.remote_path, { 'data-field': 'remote_path' }));
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
    if (code === 'delete_partial') {
      return panel([el('h1', { id: 'h-projects' }, '원격 저장소 삭제 결과를 알 수 없습니다'),
        el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'unknown' }, ERROR_TEXT.delete_partial),
        el('p', { class: 'note', 'data-note': 'partial-remote' }, DEL_TEXT.partialRemoteDo), back]);
    }
    if (unknown) {
      return panel([el('h1', { id: 'h-projects' }, '결과를 알 수 없습니다'),
        el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'unknown' }, has(DEL_TEXT.unknown, s.action) ? DEL_TEXT.unknown[s.action] : ERROR_TEXT.outcome_unknown), back]);
    }
    return panel([el('h1', { id: 'h-projects' }, '지우지 못했습니다'),
      el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'error' }, failureText(err)), back]);
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

  function renderOk(data, s, back) {
    const kids = [el('h1', { id: 'h-projects' }, '완료')];
    if (s.action === ACTION_REMOVE_PROJECT) {
      kids.push(el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, '프로젝트 ' + inline(s.subject, 64) + '를 이 서버에서 지웠습니다. 원격 저장소는 그대로입니다.'),
        stepList([['registry', '프로젝트 등록', 'removed', '지웠습니다'], ...localRows(isObj(data) ? data.local : null)]));
    } else if (s.action === ACTION_DELETE_REMOTE_REPO) {
      const requested = isObj(data) && data.remote_state === 'requested';
      const known = isObj(data) && (data.remote_state === 'deleted' || requested);
      kids.push(el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, '원격 저장소 삭제를 요청했습니다' + (requested ? '(제공자가 지연 삭제로 처리할 수 있습니다).' : '.')));
      const rows = [['remote', '원격 저장소 삭제', known ? 'done' : 'unknown', known ? (requested ? '요청됨' : '완료') : '결과를 알 수 없음']];
      if (s.fields.also_local) rows.push(...localRows(isObj(data) ? data.local : null, true));
      rows.push(['registry', '프로젝트 등록', 'kept', '남음("원격 없음" 상태)']);
      kids.push(stepList(rows));
      kids.push(el('div', { class: 'trunc', role: 'note', 'data-followup': 'remove' }, [
        el('p', {}, s.fields.also_local ? '프로젝트 등록은 남아 있습니다. 등록까지 지우려면 별도의 서명이 한 번 더 필요하며 자동으로 하지 않습니다.'
          : '서버의 복제본과 등록은 그대로 남아 있습니다. 지우려면 별도의 서명이 필요하며 자동으로 하지 않습니다.'),
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'follow-up', on: { click: startFollowUp } }, '등록 지우기…')]));
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
        ['registry', '프로젝트 등록', det.registryRemoved ? 'removed' : 'kept', det.registryRemoved ? '지웠습니다' : '남아 있습니다'],
        ['remote', '원격 저장소', det.remoteDeleted ? 'removed' : 'kept', det.remoteDeleted ? '이 요청에서 지웠습니다' : '그대로 있습니다'],
        ['clone', '서버의 복제본 폴더', det.clone, CLONE_STATE_TEXT[det.clone]],
        ['key', '배포 키 폴더', det.key, CLONE_STATE_TEXT[det.key]],
      ]));
      kids.push(el('p', { class: 'note', 'data-note': 'reason' }, PARTIAL_REASON_TEXT[det.reason]));
      const todo = det.registryRemoved
        ? '할 일: 등록은 이미 지워져 서버에 폴더가 등록 없이 남았을 수 있습니다. 서버 관리자가 남은 폴더를 직접 지우거나, 같은 경로를 다시 등록한 뒤 "복제본과 등록 지우기"를 새로 서명해 정리하세요(처음부터 있던 프로젝트는 두 번째 확인이 다시 필요합니다).'
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
