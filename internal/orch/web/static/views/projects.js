// projects.js: the project registry (#/projects): the list, "add a project"
// and the AI-transfer switch of a row. Every change is a SIGNED change:
//
//   form -> POST /projects/propose (stores nothing) -> confirmation screen ->
//   passkey signature -> POST /projects/apply -> result
//
// The confirmation screen shows what will change in the browser's own words
// (the fields THIS page sent) next to the server's sentences, and offers the
// signature only when the server's proposal says the same thing: the echoed
// fields equal the ones sent, the server's `change` equals the SHA-256 the
// browser computes itself over the canonical JSON of those fields (the same
// bytes as Go's registrymsg), and the message to sign carries that change.
// Otherwise there is a fixed warning and no sign button.
//
// Every string from the server is a text node (el()); the server's error
// words are never shown, only fixed sentences chosen by code. Nothing is
// stored in the browser, and this view never navigates or reads the clock.

import { el, inline, clear, findHostile, errorPane } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';
import { expiryBanner } from '../expiry.js';
import {
  signRegistryChange, checkRegistryMessage, sha256Hex, activeSigners, rememberedPasskey, supported, waText,
} from '../webauthn.js';
import {
  PROVIDER_ID_RE, TEMPLATES, PUBLISH_MODES, VISIBILITIES, PROVIDER_KINDS, MAX_GROUP_DEPTH, PARTIAL_STEPS, ORPHAN_ORIGINS, ACTION_CREATE_GROUP, ACTION_CREATE_PROJECT,
  ACTION_LINK_PROJECT, ACTION_ATTACH_REMOTE, validName, validGroupPath, validGitHubOwner, validOwnerForKind, validDescription, validRemotePath, validRemotePathForKind, validBaseUrl,
  makeFlow, confirmPanel, li as factItem, stepStates, stepList, linkStepList, stepLabel, partialInfo, orphanBlock, orphanNotice, hostName, kindOfProvider,
  ERROR_TEXT as W3C_ERROR_TEXT, failureText as w3cFailureText, isUnknown, cloneText, CLONE_TEXT, CLONE_TIMEOUT_MS, PROVIDER_TIMEOUT_MS, FLOW_TEXT, GROUP_SETTLE_MS,
} from './providers.js';

// --- constants shared with the server (TestConstantsAgreeWithWeb compares them) ---

export const SUBJECT_RE = /^[a-z][a-z0-9-]{0,39}$/;
export const KIND_RE = /^[a-z][a-z0-9_-]{0,31}$/;
export const FLAG_RE = /^[a-z][a-z0-9_]{0,31}$/;
// A project path is shown on a signing screen: printable ASCII and Hangul syllables only.
export const PATH_TEXT_RE = /^[\x20-\x7e\uac00-\ud7a3]+$/;
export const MAX_PATH_BYTES = 4096;
export const SUBJECT_MAX = 40;
export const KIND_MAX = 32;
export const ACTION_REGISTER = 'register_project';
export const ACTION_SET_FLAG = 'set_flag';
export const FLAG_AI_CONTEXT = 'ai_context';

// Marks of the server's two AI sentences (internal/orch/api previewSentences;
// TestConstantsAgreeWithWeb pins them): the preview must say what the browser's
// own AI line says.
export const AI_ON_MARK = '전송될 수 있게 허용합니다';
export const AI_OFF_MARK = 'AI 서비스로 전송되지 않습니다';

const MAX_PREVIEW_LINES = 12;
const MAX_PREVIEW_CHARS = 600;
const MAX_KINDS = 64;
const MAX_GROUPS = 500;
const NAME_MAX_CHARS = 40;
const KEY_PREFIX_CHARS = 64;
const TEMPLATE_TEXT = { basic: 'basic — README, .gitignore, .aigate.yml', go: 'go — Go 모듈, main.go, 시험, .aigate.yml' };
const PUBLISH_TEXT = { write: '쓰기 — 이 서버가 브랜치를 올릴 수 있음', read: '읽기 — 읽기 전용' };

const utf8Len = (s) => new TextEncoder().encode(s).length;

export function isSafeProjectPath(s) {
  return typeof s === 'string' && s.length > 0 && PATH_TEXT_RE.test(s) && utf8Len(s) <= MAX_PATH_BYTES;
}

// The browser waits longer than orchd's worst case for a signed change (the
// broker write limit is 15 s, plus the repository checks): the 15 s default of
// the api layer would let the browser give up first.
const PROJECT_TIMEOUT_MS = 25000;

// --- canonical change (the same bytes as Go's registrymsg.CanonicalChange) ---

export class ChangeError extends Error {
  constructor(why) {
    super(why);
    this.name = 'ChangeError';
  }
}

const SCHEMA = {
  [ACTION_REGISTER]: { path: 's', kind: 's', ai_context: 'b' },
  [ACTION_SET_FLAG]: { flag: 's', value: 'b' },
};

// A string whose canonical JSON could differ between Go and JSON.stringify:
// control characters, DEL, U+2028/9 and lone surrogates.
function checkString(s) {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x20 || c === 0x7f || c === 0x2028 || c === 0x2029) throw new ChangeError('control character');
    if (c >= 0xd800 && c <= 0xdbff) {
      const d = s.charCodeAt(i + 1);
      if (!(d >= 0xdc00 && d <= 0xdfff)) throw new ChangeError('lone surrogate');
      i++;
    } else if (c >= 0xdc00 && c <= 0xdfff) {
      throw new ChangeError('lone surrogate');
    }
  }
}

// canonicalChange returns {"action":..,"fields":{..sorted..},"subject":..}: no
// whitespace, no HTML escaping, only strings and booleans. It refuses
// whatever registrymsg refuses.
export function canonicalChange(action, subject, fields) {
  if (typeof action !== 'string' || !Object.prototype.hasOwnProperty.call(SCHEMA, action)) throw new ChangeError('unknown action');
  const spec = SCHEMA[action];
  if (typeof subject !== 'string' || !SUBJECT_RE.test(subject)) throw new ChangeError('bad subject');
  if (fields === null || typeof fields !== 'object' || Array.isArray(fields)) throw new ChangeError('bad fields');
  const proto = Object.getPrototypeOf(fields);
  if (proto !== Object.prototype && proto !== null) throw new ChangeError('bad fields');
  const names = Object.keys(fields);
  if (names.length !== Object.keys(spec).length) throw new ChangeError('fields do not match the action');
  for (const name of names) {
    if (!Object.prototype.hasOwnProperty.call(spec, name)) throw new ChangeError('unknown field');
    const v = fields[name];
    if (spec[name] === 'b') {
      if (typeof v !== 'boolean') throw new ChangeError('wrong type');
      continue;
    }
    if (typeof v !== 'string' || v === '') throw new ChangeError('wrong type');
    checkString(v);
    if (name === 'kind' && !KIND_RE.test(v)) throw new ChangeError('bad kind');
    if (name === 'flag' && !FLAG_RE.test(v)) throw new ChangeError('bad flag');
  }
  names.sort();
  const body = names.map((n) => JSON.stringify(n) + ':' + JSON.stringify(fields[n])).join(',');
  return '{"action":' + JSON.stringify(action) + ',"fields":{' + body + '},"subject":' + JSON.stringify(subject) + '}';
}

// computeChange is the `change` hash of a change: lowercase hex SHA-256 of
// canonicalChange. Async because WebCrypto is.
export async function computeChange(action, subject, fields) {
  return sha256Hex(canonicalChange(action, subject, fields));
}

// --- fixed texts -------------------------------------------------------------

// ERROR_TEXT: one fixed sentence per refusal code (D5 plus the orchd ones).
// The server's own words are never shown.
export const ERROR_TEXT = {
  not_signed: '서명이 없는 요청이라 거부되었습니다. 변경은 적용되지 않았습니다.',
  bad_signature: '서명이 올바르지 않아 거부되었습니다. 변경은 적용되지 않았습니다.',
  unknown_signer: '등록되지 않은 서명자의 서명이라 거부되었습니다. 변경은 적용되지 않았습니다.',
  stale_signature: '서명이 만료되었습니다(5분 안에만 쓸 수 있습니다). 변경은 적용되지 않았습니다. 처음부터 다시 진행하세요.',
  signers_unavailable: '서버의 서명자 목록을 쓸 수 없어 처리하지 못했습니다. 변경은 적용되지 않았습니다. 관리자에게 알려 주세요.',
  request_reused: '이미 사용된 요청이라 거부되었습니다. 변경은 다시 적용되지 않았습니다. 목록을 확인하고 필요하면 새로 시작하세요.',
  change_mismatch: '서명한 내용과 서버가 받은 내용이 달라 거부되었습니다. 변경은 적용되지 않았습니다.',
  project_exists: '같은 ID 또는 같은 경로의 프로젝트가 이미 있습니다. 변경은 적용되지 않았습니다.',
  project_unknown: '등록되지 않은 프로젝트입니다. 변경은 적용되지 않았습니다. 목록을 새로 읽어 확인하세요.',
  path_not_allowed: '이 경로는 등록할 수 없습니다(허용된 저장소 폴더 바로 아래의 git 저장소만 가능합니다). 변경은 적용되지 않았습니다.',
  registry_unavailable: '프로젝트 등록부를 쓸 수 없어 처리하지 못했습니다. 관리자에게 알려 주세요.',
  not_supported: '이 버전에서는 지원하지 않는 변경입니다(AI 전송 허용 여부만 바꿀 수 있습니다).',
  outcome_unknown: '변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 목록을 다시 읽어 확인한 뒤 필요하면 다시 변경하세요.',
  project_rate_limited: '프로젝트 변경 요청이 너무 잦습니다. 잠시 후 다시 시도하세요.',
  bad_request: '요청 형식이 올바르지 않아 거부되었습니다. 변경은 적용되지 않았습니다.',
  broker_unavailable: '프로젝트 등록부 서비스에 연결하지 못했습니다. 변경은 적용되지 않았습니다.',
  broker_error: '프로젝트 등록부 서비스가 예상하지 못한 응답을 했습니다. 변경이 적용되었는지 목록에서 확인하세요.',
  repos_unavailable: '저장소 목록을 지금 읽을 수 없습니다. 잠시 후 다시 시도하세요.',
};

const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);

const STATUS_TEXT = {
  403: '이 변경을 할 권한이 없습니다. 변경은 적용되지 않았습니다.',
  404: '찾을 수 없습니다. 변경은 적용되지 않았습니다.',
  429: ERROR_TEXT.project_rate_limited,
};

// checkList: the fixed text for this failure tells the user to look at the list.
export function checkList(err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (code === 'outcome_unknown' || code === 'broker_error' || code === 'project_unknown') return true;
  if (has(ERROR_TEXT, code)) return false;
  const st = err && typeof err.status === 'number' ? err.status : 0;
  return st >= 500;
}

// failureText: the fixed sentence for an ApiError. Unknown codes fall back to
// the HTTP status, never to the server's message.
export function failureText(err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (has(ERROR_TEXT, code)) return ERROR_TEXT[code];
  const st = err && typeof err.status === 'number' ? err.status : 0;
  if (has(STATUS_TEXT, st)) return STATUS_TEXT[st];
  if (st >= 500) return '서버에서 오류가 발생했습니다. 변경이 적용되었는지 목록에서 확인하세요.';
  return '요청을 처리하지 못했습니다. 변경은 적용되지 않았습니다.';
}

const TEXT = {
  mismatch: '서버가 보낸 변경 내용이 화면의 내용과 맞지 않아 서명할 수 없습니다. 아무것도 서명하거나 보내지 않았습니다. 처음부터 다시 하세요. 계속 같으면 관리자에게 알려 주세요.',
  noPasskeySession: '패스키로 로그인한 세션에서만 웹 서명을 할 수 있습니다.',
  noSigner: '로그인한 서명자의 활성 패스키를 찾지 못했습니다.',
  signersFailed: '패스키 목록을 불러오지 못했습니다. 잠시 후 다시 시도하세요.',
  connect: '서버에 연결하지 못했습니다. 아무것도 서명하거나 보내지 않았습니다.',
  unknownOutcome: ERROR_TEXT.outcome_unknown,
  aiOn: 'AI 전송 허용: 예. 이 저장소의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있습니다.',
};

// --- defaults ----------------------------------------------------------------

// defaultId derives a project id from the folder name of a path.
export function defaultId(path) {
  const base = String(path).split('/').filter(Boolean).pop() || '';
  let id = base.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '');
  if (id === '') id = 'project';
  if (!/^[a-z]/.test(id)) id = 'p-' + id;
  return id.slice(0, SUBJECT_MAX);
}

const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);

// --- analysis of a proposal --------------------------------------------------

// analyze checks the server's proposal against what the browser asked for.
// It returns the list of problems (empty = the proposal says what the screen
// says). `sent` is {action, subject, fields}: the page's own values.
async function analyze(sent, resp) {
  const problems = [];
  if (!isObj(resp)) return ['shape'];
  let canon;
  let change;
  try {
    canon = canonicalChange(sent.action, sent.subject, sent.fields);
    change = await computeChange(sent.action, sent.subject, sent.fields);
  } catch {
    return ['own_fields'];
  }
  // the server echoes what was asked, nothing else
  let echoed = '';
  try {
    echoed = canonicalChange(resp.action, resp.subject, resp.fields);
  } catch {
    problems.push('echo');
  }
  if (echoed !== canon) problems.push('echo');
  // its hash is the browser's hash of the fields on screen
  if (resp.change !== change) problems.push('change');
  // the message to sign is canonical and says the same
  let msg = null;
  try {
    msg = checkRegistryMessage(resp.message);
  } catch {
    problems.push('message');
  }
  if (msg) {
    if (msg.action !== sent.action || msg.subject !== sent.subject || msg.change !== change) problems.push('message');
    if (resp.request !== msg.request || resp.issued !== msg.issued) problems.push('message');
  }
  // sentences: a bounded list of plain text
  const pv = resp.preview;
  if (!Array.isArray(pv) || pv.length === 0 || pv.length > MAX_PREVIEW_LINES ||
      pv.some((x) => typeof x !== 'string' || x === '' || x.length > MAX_PREVIEW_CHARS || findHostile(x) !== null)) {
    problems.push('preview');
  }
  // the server's AI sentences agree with the browser's own AI line
  if (Array.isArray(pv)) {
    const aiNew = sent.action === ACTION_REGISTER ? sent.fields.ai_context : sent.fields.value;
    const on = pv.filter((x) => typeof x === 'string' && x.includes(AI_ON_MARK)).length;
    const off = pv.filter((x) => typeof x === 'string' && x.includes(AI_OFF_MARK)).length;
    if (aiNew ? !(on === 1 && off === 0) : !(off === 1 && on === 0)) problems.push('preview_ai');
  }
  return problems;
}

// --- the view ----------------------------------------------------------------

const MAX_ORPHANS = 50;
const ORPHAN_AT_RE = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$/;

// validOrphan: a half-made project row of the registry list, every member checked
// (they are shown as text, never as markup, and never become an attribute).
function validOrphan(o) {
  return isObj(o) && typeof o.provider === 'string' && PROVIDER_ID_RE.test(o.provider) && validRemotePath(o.project) &&
    (o.project_id === null || (Number.isSafeInteger(o.project_id) && o.project_id > 0)) && PARTIAL_STEPS.includes(o.step) &&
    typeof o.at === 'string' && ORPHAN_AT_RE.test(o.at);
}

// validCloneResult: the `clone` member of a create answer.
function validCloneResult(c) {
  if (!isObj(c) || typeof c.status !== 'string' || typeof c.code !== 'string' || !/^[a-z_]{1,40}$/.test(c.code)) return { status: 'failed', code: 'other' };
  return { status: c.status, code: c.code };
}

export function projectsView(ctx) {
  ctx.setCrumb('프로젝트');
  const st = {
    step: 'loading', // loading | list | candidates | form | confirm | result
    list: null,
    listFailed: null,
    note: null, // {text, error}
    cands: null,
    kinds: [],
    form: null, // persistent controls of the register form
    sent: null, // {action, subject, fields}: what this page asked for
    view: null, // for a set_flag: {path, old} of the list row it came from
    resp: null,
    problems: [],
    signer: null,
    signerNote: null,
    busy: false,
    result: null, // {error, text}
    orphans: [], // half-made or half-linked repositories the server remembers (shape-checked rows only)
    regProviders: [], // the providers of the registry list: the kind of an orphan's provider
  };
  // W3c: a new group or project (providers.js holds the signed-change flow). W3e: `lk` is the
  // link ("기존 저장소 연결") or attach ("원격 연결") form.
  const cp = { providers: [], f: null, g: null, groups: null, truncated: false, groupsErr: null, groupsSeq: 0, which: 'create', clone: null, cloneBusy: false, pendingGroup: null, keyFailed: false };
  const lk = { mode: 'link', f: null, row: null };
  let groupTimer = null; // the settle timer of the provider select
  // The kind of a provider (gitlab | github) from the lists this page has read; undefined when not known.
  const kindFor = (id) => kindOfProvider(cp.providers, id) || kindOfProvider(st.regProviders, id);
  const flow = makeFlow(ctx, {
    onChange: onFlowChange,
    onResult: onFlowResult,
    prepare: prepareResult,
    kindOf: (sent) => kindFor(sent.action === ACTION_CREATE_GROUP ? sent.subject : sent.fields.provider),
  });
  let signerP = null;
  let expiry = null; // {rows, warn}: the tokens that are due, for the banner on top of the list
  let listSeq = 0; // list loads: a newer load or an apply makes older answers stale
  let propSeq = 0; // proposals: a cancel or a newer proposal makes older answers stale
  const gone = () => !!(ctx.signal && ctx.signal.aborted);

  function paint() {
    replaceContent(ctx.root, render());
  }

  // ---- list ----
  async function loadList() {
    const my = ++listSeq;
    try {
      const r = await ctx.get('/api/v1/projects/registry');
      if (gone() || my !== listSeq) return;
      st.list = r && Array.isArray(r.projects) ? r.projects.filter(isObj) : [];
      st.orphans = r && Array.isArray(r.orphans) ? r.orphans.filter(validOrphan).slice(0, MAX_ORPHANS) : [];
      st.regProviders = r && Array.isArray(r.providers) ? r.providers.filter((x) => isObj(x) && PROVIDER_KINDS.includes(x.kind)).slice(0, 50) : [];
      st.listFailed = null;
    } catch (err) {
      if (gone() || my !== listSeq) return;
      st.listFailed = err;
    }
    if (st.step === 'loading' || st.step === 'list') {
      st.step = 'list';
      paint();
    }
  }

  // loadExpiry reads the token list once per visit for the expiry banner. The
  // banner is a convenience: a failure here is silent and never blocks the page.
  async function loadExpiry() {
    try {
      const r = await ctx.get('/api/v1/settings/tokens');
      if (gone() || !isObj(r) || !Array.isArray(r.tokens)) return;
      expiry = { rows: r.tokens.filter(isObj).slice(0, 200), warn: r.warn_days };
      if (st.step === 'list') paint();
    } catch {
      // no banner
    }
  }

  // backToList is the way out of a result: it always shows a freshly read list.
  async function backToList() {
    toList();
    st.step = 'loading';
    paint();
    await loadList();
  }

  function toList() {
    propSeq++; // a proposal still in flight is for an abandoned change
    flow.reset();
    cp.f = null;
    cp.g = null;
    lk.f = null;
    lk.row = null;
    cp.groups = null;
    cp.groupsSeq++;
    if (groupTimer !== null) {
      clearTimeout(groupTimer);
      groupTimer = null;
    }
    cp.clone = null;
    st.step = 'list';
    st.note = null;
    st.sent = null;
    st.resp = null;
    st.busy = false;
    st.form = null;
    paint();
  }

  // ---- register: candidates and form ----
  async function startRegister() {
    if (st.busy) return;
    st.busy = true;
    st.note = null;
    st.step = 'candidates';
    paint();
    let r;
    try {
      r = await ctx.get('/api/v1/projects/candidates');
    } catch (err) {
      if (gone()) return;
      st.busy = false;
      st.step = 'list';
      st.note = { error: true, text: failureText(err) };
      paint();
      return;
    }
    if (gone()) return;
    st.busy = false;
    const cands = r && Array.isArray(r.candidates) ? r.candidates.filter((c) => isObj(c) && isSafeProjectPath(c.path)) : [];
    const kinds = r && Array.isArray(r.kinds) ? r.kinds.filter((k) => typeof k === 'string' && KIND_RE.test(k)).slice(0, MAX_KINDS) : [];
    st.cands = cands;
    st.kinds = kinds;
    if (cands.length === 0 || kinds.length === 0) {
      st.step = 'list';
      st.note = {
        error: false,
        text: cands.length === 0 ? '등록할 수 있는 저장소가 없습니다. 허용된 저장소 폴더 바로 아래의 git 저장소만 나타납니다.'
          : '고를 수 있는 종류가 없습니다. 정책에 종류(클래스)가 있어야 합니다.',
      };
      paint();
      return;
    }
    st.form = buildForm();
    st.step = 'form';
    paint();
  }

  function buildForm() {
    const f = {};
    f.cand = el('select', { id: 'p-cand', 'data-focus-key': 'cand', on: { change: () => { f.id.value = defaultId(st.cands[Number(f.cand.value)].path); } } },
      st.cands.map((c, i) => el('option', { value: String(i) }, inline(c.path, 200))));
    f.cand.value = '0';
    f.id = el('input', { type: 'text', id: 'p-id', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(SUBJECT_MAX), 'data-focus-key': 'id' });
    f.id.value = defaultId(st.cands[0].path);
    f.kind = el('select', { id: 'p-kind', 'data-focus-key': 'kind' }, st.kinds.map((k, i) => el('option', { value: String(i) }, k)));
    f.kind.value = '0';
    f.ai = el('input', { type: 'checkbox', id: 'p-ai', 'data-focus-key': 'ai' });
    f.ai.checked = false; // default: the repository's contents are NOT sent to an AI service
    f.problem = el('p', { class: ['note', 'err'], role: 'alert' });
    f.problem.hidden = true;
    return f;
  }

  function formProblem(f, text) {
    clear(f.problem);
    if (text) f.problem.append(text);
    f.problem.hidden = !text;
  }

  function submitForm() {
    const f = st.form;
    if (!f || st.busy) return;
    const cand = st.cands[Number(f.cand.value)];
    const kind = st.kinds[Number(f.kind.value)];
    const subject = typeof f.id.value === 'string' ? f.id.value.trim() : '';
    if (!cand || !isSafeProjectPath(cand.path)) return formProblem(f, '저장소를 목록에서 고르세요.');
    if (!SUBJECT_RE.test(subject)) {
      return formProblem(f, '프로젝트 ID는 소문자로 시작하고 소문자·숫자·하이픈만 써서 40자 이내로 적으세요.');
    }
    if (kind === undefined) return formProblem(f, '종류를 목록에서 고르세요.');
    formProblem(f, '');
    propose({ action: ACTION_REGISTER, subject, fields: { path: cand.path, kind, ai_context: f.ai.checked === true } });
  }

  // ---- propose -> confirmation ----
  async function propose(sent, view) {
    if (st.busy) return;
    st.busy = true;
    st.note = null;
    const my = ++propSeq;
    st.view = view || null;
    if (st.step === 'list') paint(); // disables the reload button while the proposal is out
    let resp;
    try {
      resp = await ctx.post('/api/v1/projects/propose', sent, { noRetry429: true, timeoutMs: PROJECT_TIMEOUT_MS });
    } catch (err) {
      if (gone() || my !== propSeq) return; // left or cancelled: toList already cleared busy
      st.busy = false;
      const text = err && err.status === 0 ? TEXT.connect : failureText(err);
      if (st.form && st.step === 'form') {
        formProblem(st.form, text);
        paint();
      } else {
        st.note = { error: true, text };
        paint();
      }
      return;
    }
    if (gone() || my !== propSeq) return;
    st.problems = await analyze(sent, resp);
    if (gone() || my !== propSeq) return;
    st.sent = sent;
    st.resp = resp;
    st.step = 'confirm';
    st.busy = false;
    st.signer = null;
    st.signerNote = null;
    paint();
    if (st.problems.length === 0) await resolveSignerOnce(sent);
  }

  async function resolveSignerOnce(sent) {
    if (!supported()) {
      st.signerNote = waText('unsupported');
    } else {
      try {
        if (!signerP) signerP = resolveSigner(ctx);
        const s = await signerP;
        if (s.error) {
          signerP = null;
          st.signerNote = s.error;
        } else {
          st.signer = s;
          st.signerNote = '서명에 쓸 패스키: ' + inline(s.id, 64);
        }
      } catch {
        signerP = null;
        st.signerNote = TEXT.signersFailed;
      }
    }
    if (gone() || st.sent !== sent || st.step !== 'confirm') return;
    paint();
  }

  const canSign = () => st.step === 'confirm' && !st.busy && st.problems.length === 0 && !!st.signer && !!st.resp && supported();

  async function onSign() {
    if (!canSign()) return; // also a double click: busy is set below, before any await
    const cur = { sent: st.sent, resp: st.resp };
    st.busy = true;
    st.note = { error: false, text: '패스키 확인을 기다리는 중…' };
    paint();
    let signature;
    try {
      signature = await signRegistryChange(cur.resp.message, st.signer);
    } catch (err) {
      if (gone() || st.resp !== cur.resp) return;
      st.busy = false;
      st.note = { error: true, text: err && err.name === 'WebAuthnError' ? waText(err.code) : waText('authenticator_failed') };
      paint();
      return;
    }
    // The user left while the passkey prompt was open: drop the signature, send nothing.
    if (gone() || st.resp !== cur.resp) return;
    const body = {
      action: cur.sent.action, subject: cur.sent.subject, fields: cur.sent.fields,
      change: cur.resp.change, request: cur.resp.request, issued: cur.resp.issued, signature,
    };
    // From here the proposal is spent: whatever the answer, it is never signed or sent again.
    listSeq++; // a list load that started before this change must not paint over the next one
    st.step = 'result';
    st.note = null;
    st.result = { error: false, text: '서버로 변경을 보내는 중…', pending: true };
    paint();
    try {
      await ctx.post('/api/v1/projects/apply', body, { noRetry429: true, timeoutMs: PROJECT_TIMEOUT_MS });
    } catch (err) {
      if (gone()) return;
      const unknown = (err && err.code === 'outcome_unknown') || (err && err.status === 0);
      st.busy = false;
      st.result = { error: true, text: unknown ? TEXT.unknownOutcome : failureText(err) };
      paint();
      // Every fixed text that tells the user to check the list also reads it again.
      if (unknown || checkList(err)) await loadList();
      return;
    }
    if (gone()) return;
    st.busy = false;
    st.result = {
      error: false,
      text: cur.sent.action === ACTION_REGISTER ? '프로젝트를 등록했습니다.' : 'AI 전송 허용 설정을 변경했습니다.',
    };
    paint();
    await loadList();
  }

  // ---- W3c: new project and new group ----
  function onFlowChange() {
    if (st.step === 'x_confirm' && (flow.phase === 'sending' || flow.phase === 'done')) st.step = 'x_result';
    if (st.step === 'x_confirm' || st.step === 'x_result' || st.step === 'cp_form' || st.step === 'cg_form' || st.step === 'lk_form') paint();
  }

  // prepareResult runs BEFORE the result screen is first drawn: what the server answered about the clone
  // (or the attach key) is known by then, so the screen never shows a default while the lists are re-read.
  function prepareResult(result, sent) {
    if (sent.action === ACTION_CREATE_GROUP) return;
    cp.clone = null;
    cp.keyFailed = false;
    if (result.ok && sent.action === ACTION_ATTACH_REMOTE) {
      // an attach has no clone; its answer may say that the key could not be put in place
      const k = isObj(result.data) ? result.data.key : null;
      cp.keyFailed = isObj(k) && k.status === 'failed' && k.code === 'key_unavailable';
    } else if (result.ok) {
      const c = validCloneResult(isObj(result.data) ? result.data.clone : null);
      // the folder was already there: the project exists but what this server holds under that name is unknown
      cp.clone = { state: c.status === 'cloned' ? 'ok' : c.status === 'already_cloned' ? 'check' : 'failed', code: c.code };
    }
  }

  // After a signed change ended, whatever the answer.
  async function onFlowResult(result, sent) {
    if (sent.action === ACTION_CREATE_GROUP) {
      const g = result.ok && isObj(result.data) ? result.data.group : null;
      const made = isObj(g) && validGroupPath(g.path) ? g.path : (sent.fields.parent_path === '' ? '' : sent.fields.parent_path + '/') + sent.fields.path;
      await loadGroups(sent.subject, result.ok ? made : null);
      if (!result.ok && result.unknown) await reloadProviders();
      if (result.ok && st.step === 'x_result') {
        flow.reset();
        st.step = 'cp_form';
        st.note = { error: false, text: '그룹을 만들었습니다.' };
        paint();
      }
      return;
    }
    // a project: the lists are read again (an unknown outcome certainly changed what they say, a half-made project adds an alert)
    const reads = [loadList()];
    if (!result.ok && result.unknown) reads.push(reloadProviders());
    await Promise.all(reads);
    if (!gone() && st.step === 'x_result') paint();
  }

  async function reloadProviders() {
    try {
      const r = await ctx.get('/api/v1/providers');
      if (!gone() && r && Array.isArray(r.providers)) cp.providers = r.providers.filter(usableProvider).slice(0, 50);
    } catch {
      // the next screen reads them again
    }
  }

  // A provider this page can offer: a known kind and an address of the right shape.
  const usableProvider = (p) => isObj(p) && PROVIDER_ID_RE.test(p.id) && PROVIDER_KINDS.includes(p.kind) && validBaseUrl(p.base_url);

  // readProviders reads the provider list for a form; null (with a note on the list) when there is none to offer.
  async function readProviders() {
    st.busy = true;
    st.note = null;
    paint();
    let r;
    try {
      r = await ctx.get('/api/v1/providers');
    } catch (err) {
      if (gone()) return null;
      st.busy = false;
      st.note = { error: true, text: w3cFailureText(err) };
      paint();
      return null;
    }
    if (gone()) return null;
    st.busy = false;
    const provs = r && Array.isArray(r.providers) ? r.providers.filter(usableProvider).slice(0, 50) : [];
    if (provs.length === 0) {
      st.note = { error: false, text: '등록된 제공자가 없습니다. 먼저 제공자를 추가하세요.', link: '#/providers' };
      paint();
      return null;
    }
    cp.providers = provs;
    return provs;
  }

  async function startCreate() {
    if (st.busy || flow.busy) return;
    const provs = await readProviders();
    if (provs === null) return;
    cp.f = buildCreateForm();
    st.step = 'cp_form';
    paint();
    loadGroups(provs[0].id, null);
  }

  function buildCreateForm() {
    const f = {};
    f.provider = el('select', { id: 'cp-provider', 'data-focus-key': 'provider', on: { change: onProviderChange, blur: commitGroups } },
      cp.providers.map((p) => el('option', { value: p.id }, inline(p.id, 64) + ' — ' + hostName(p.kind) + ' — ' + inline(p.base_url, 80))));
    f.provider.value = cp.providers[0].id;
    f.group = el('select', { id: 'cp-group', 'data-focus-key': 'group' }, [el('option', { value: '' }, '그룹을 불러오는 중…')]);
    f.group.value = '';
    // GitHub only: the repository is public or private, and there is NO default: the first option is
    // a placeholder and the propose button stays off until a real choice is made.
    f.visibility = el('select', { id: 'cp-visibility', 'data-focus-key': 'visibility', on: { change: () => { setProblem(f, ''); paint(); } } }, [
      el('option', { value: '' }, '선택하세요(필수)'),
      el('option', { value: 'public' }, '공개(public) — 인터넷의 누구나 읽을 수 있음'),
      el('option', { value: 'private' }, '비공개(private) — 초대받은 사람만'),
    ]);
    f.visibility.value = '';
    f.newGroup = el('button', { type: 'button', class: 'btn', 'data-focus-key': 'newgroup', on: { click: openGroupForm } }, '새 그룹 만들기');
    f.name = el('input', { type: 'text', id: 'cp-name', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(NAME_MAX_CHARS), 'data-focus-key': 'name' });
    f.desc = el('input', { type: 'text', id: 'cp-desc', autocomplete: 'off', 'data-focus-key': 'desc' });
    f.template = el('select', { id: 'cp-template', 'data-focus-key': 'template' }, TEMPLATES.map((t) => el('option', { value: t }, TEMPLATE_TEXT[t])));
    f.template.value = TEMPLATES[0];
    f.ai = el('input', { type: 'checkbox', id: 'cp-ai', 'data-focus-key': 'ai' });
    f.ai.checked = false; // default: the repository's contents are NOT sent to an AI service
    f.publish = el('select', { id: 'cp-publish', 'data-focus-key': 'publish' }, PUBLISH_MODES.map((m) => el('option', { value: m }, PUBLISH_TEXT[m])));
    f.publish.value = PUBLISH_MODES[0];
    f.problem = el('p', { class: ['note', 'err'], role: 'alert', 'data-problem': 'form' });
    f.problem.hidden = true;
    return f;
  }

  const setProblem = (f, text) => {
    clear(f.problem);
    if (text) f.problem.append(text);
    f.problem.hidden = !text;
  };

  // The provider select changed: a new provider is a new form, so a visibility chosen for the
  // previous one is never carried over (no default, ever).
  function onProviderChange() {
    if (cp.f) cp.f.visibility.value = '';
    scheduleGroups();
  }

  // The kind of the provider chosen in the create form (gitlab when it is not known).
  const createKind = () => (cp.f && kindFor(cp.f.provider.value)) || 'gitlab';

  // scheduleGroups: the select fired a change (an arrow key fires one each). The old
  // list is dropped at once, the provider call is made once the select is quiet.
  function scheduleGroups() {
    cp.groups = null;
    cp.groupsErr = null;
    cp.groupsSeq++;
    fillGroupSelect(null);
    if (groupTimer !== null) clearTimeout(groupTimer);
    groupTimer = setTimeout(() => {
      groupTimer = null;
      if (!gone() && cp.f) loadGroups(cp.f.provider.value, null);
    }, GROUP_SETTLE_MS);
  }

  function commitGroups() {
    if (groupTimer === null) return;
    clearTimeout(groupTimer);
    groupTimer = null;
    if (cp.f && !gone()) loadGroups(cp.f.provider.value, null);
  }

  function retryGroups() {
    if (!cp.f || !cp.groupsErr) return; // a second click on a button that is already gone
    loadGroups(cp.f.provider.value, null);
  }

  // loadGroups reads the groups of a provider from GitLab (a call that costs a
  // slot of the hourly limit and is never waited out or repeated on its own).
  async function loadGroups(provId, select) {
    if (typeof provId !== 'string' || !PROVIDER_ID_RE.test(provId)) return;
    const my = ++cp.groupsSeq;
    cp.groups = null;
    cp.groupsErr = null;
    cp.truncated = false;
    fillGroupSelect(null);
    let r;
    try {
      r = await ctx.get('/api/v1/providers/' + provId + '/groups', { noRetry429: true, timeoutMs: PROVIDER_TIMEOUT_MS });
    } catch (err) {
      if (gone() || my !== cp.groupsSeq) return;
      cp.groupsErr = err && err.status === 0 ? FLOW_TEXT.connect : w3cFailureText(err);
      fillGroupSelect(null);
      return;
    }
    if (gone() || my !== cp.groupsSeq) return;
    // GitLab rows: a lowercase group path. GitHub rows: one login (case kept) of the account or an organisation.
    const gh = kindFor(provId) === 'github';
    cp.groups = r && Array.isArray(r.groups)
      ? r.groups.filter((g) => isObj(g) && (gh ? validGitHubOwner(g.path) && (g.kind === 'user' || g.kind === 'org') : validGroupPath(g.path)))
        .slice(0, MAX_GROUPS).map((g) => ({ path: g.path, kind: gh ? g.kind : undefined }))
      : [];
    cp.truncated = !!r && r.truncated === true;
    fillGroupSelect(select);
  }

  // fillGroupSelect rebuilds the group choices in place (the control survives repaints).
  function fillGroupSelect(select) {
    const f = cp.f;
    if (!f) return;
    clear(f.group);
    const gh = createKind() === 'github';
    const what = gh ? '소유자' : '그룹';
    const first = cp.groups === null ? (cp.groupsErr ? what + ' 목록을 읽지 못했습니다' : what + '를 불러오는 중…') : cp.groups.length === 0 ? '고를 ' + what + '가 없습니다' : what + '를 고르세요';
    f.group.appendChild(el('option', { value: '' }, first));
    for (const g of cp.groups || []) f.group.appendChild(el('option', { value: g.path }, gh ? g.path + ' — ' + (g.kind === 'user' ? '내 계정' : '조직') : g.path));
    f.group.value = select && (cp.groups || []).some((g) => g.path === select) ? select : '';
    f.newGroup.disabled = cp.groups === null;
    if (st.step === 'cp_form') paint();
  }

  function submitCreate() {
    const f = cp.f;
    if (!f || flow.busy) return;
    const provider = f.provider.value;
    const group = f.group.value;
    const name = typeof f.name.value === 'string' ? f.name.value.trim() : '';
    const desc = typeof f.desc.value === 'string' ? f.desc.value : '';
    if (!validName(name)) return setProblem(f, '프로젝트 이름은 소문자로 시작하고 소문자·숫자·하이픈만 써서 2자 이상 40자 이내로 적으세요. (GitLab 프로젝트 이름, 프로젝트 ID, 폴더 이름으로 함께 쓰입니다.)');
    if (!PROVIDER_ID_RE.test(provider) || !cp.providers.some((p) => p.id === provider)) return setProblem(f, '제공자를 목록에서 고르세요.');
    const kind = kindFor(provider) || 'gitlab';
    const gh = kind === 'github';
    if (cp.groups === null || group === '' || !cp.groups.some((g) => g.path === group) || !validOwnerForKind(kind, group)) {
      return setProblem(f, gh ? '소유자(내 계정 또는 조직)를 목록에서 고르세요. GitHub에서는 새 조직을 만들 수 없습니다.' : '그룹을 목록에서 고르세요. 그룹이 없으면 "새 그룹 만들기"를 쓰세요.');
    }
    if (gh && !VISIBILITIES.includes(f.visibility.value)) return setProblem(f, '공개(public) 또는 비공개(private)를 반드시 고르세요. 기본값이 없습니다.');
    if (!validDescription(desc)) return setProblem(f, '설명은 200자(글자 수) 이내여야 하고 줄바꿈·제어 문자·보이지 않는 문자·방향 전환 문자를 쓸 수 없습니다.');
    if (!TEMPLATES.includes(f.template.value) || !PUBLISH_MODES.includes(f.publish.value)) return setProblem(f, '템플릿과 게시 권한을 목록에서 고르세요.');
    setProblem(f, '');
    const fields = { provider, group_path: group, template: f.template.value, description: desc, ai_context: f.ai.checked === true, publish: f.publish.value };
    if (gh) fields.visibility = f.visibility.value; // GitLab never gets this field (the server refuses it)
    startFlow({ action: ACTION_CREATE_PROJECT, subject: name, fields }, 'create');
  }

  function openGroupForm() {
    if (!cp.f || cp.groups === null) return;
    const g = {};
    g.parent = el('select', { id: 'cg-parent', 'data-focus-key': 'parent' }, [
      el('option', { value: '' }, '(최상위)'),
      ...cp.groups.filter((x) => x.path.split('/').length < MAX_GROUP_DEPTH).map((x) => el('option', { value: x.path }, x.path)),
    ]);
    g.parent.value = '';
    g.pathAuto = true;
    g.name = el('input', {
      type: 'text', id: 'cg-name', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(NAME_MAX_CHARS), 'data-focus-key': 'gname',
      on: { input: () => { if (g.pathAuto) g.path.value = g.name.value; } },
    });
    g.path = el('input', {
      type: 'text', id: 'cg-path', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(NAME_MAX_CHARS), 'data-focus-key': 'gpath',
      on: { input: () => { g.pathAuto = false; } },
    });
    g.problem = el('p', { class: ['note', 'err'], role: 'alert', 'data-problem': 'group' });
    g.problem.hidden = true;
    cp.g = g;
    st.step = 'cg_form';
    st.note = null;
    paint();
  }

  function submitGroup() {
    const g = cp.g;
    if (!g || flow.busy) return;
    const name = g.name.value.trim();
    const path = g.path.value.trim();
    const parent = g.parent.value;
    if (!validName(name)) return setProblem(g, '그룹 이름은 소문자로 시작하고 소문자·숫자·하이픈만 써서 2자 이상 40자 이내로 적으세요.');
    if (!validName(path)) return setProblem(g, '그룹 경로는 한 마디(슬래시 없이)이고 이름과 같은 규칙입니다: 소문자로 시작, 소문자·숫자·하이픈, 2~40자.');
    if (parent !== '' && !(validGroupPath(parent) && parent.split('/').length < MAX_GROUP_DEPTH && (cp.groups || []).some((x) => x.path === parent))) {
      return setProblem(g, '상위 그룹을 목록에서 고르세요(그룹은 4단계까지만 만들 수 있습니다).');
    }
    setProblem(g, '');
    startFlow({ action: ACTION_CREATE_GROUP, subject: cp.f.provider.value, fields: { name, path, parent_path: parent } }, 'group');
  }

  async function startFlow(sent, which) {
    if (flow.busy) return;
    cp.which = which;
    await flow.propose(sent);
    if (flow.phase === 'idle' && flow.note) {
      const note = flow.note;
      flow.note = null;
      const f = which === 'group' ? cp.g : which === 'create' ? cp.f : lk.f;
      if (f) setProblem(f, note.text);
      paint();
      return;
    }
    if (flow.phase === 'confirm') {
      st.step = 'x_confirm';
      paint();
    }
  }

  function backFromFlow() {
    flow.reset();
    st.step = cp.which === 'group' ? 'cg_form' : cp.which === 'create' ? 'cp_form' : 'lk_form';
    paint();
  }

  // The clone of a registered project that has no folder yet. No signature: it
  // only clones what the registry already says. A timeout is "unknown", never retried.
  const kindOfProject = (id) => {
    const row = (st.list || []).find((x) => isObj(x) && x.id === id);
    return row && typeof row.provider === 'string' ? kindFor(row.provider) : flow.sent && flow.sent.subject === id ? kindFor(flow.sent.fields.provider) : undefined;
  };

  // The sentence of a failed clone: GitHub's words for a GitHub project, else the clone text, else the shared text.
  function cloneFailureText(code, kind) {
    if (kind === 'github' && Object.prototype.hasOwnProperty.call(CLONE_TEXT, code)) return cloneText(code, 'github');
    if (Object.prototype.hasOwnProperty.call(CLONE_TEXT, code)) return CLONE_TEXT[code];
    if (Object.prototype.hasOwnProperty.call(W3C_ERROR_TEXT, code)) return W3C_ERROR_TEXT[code];
    return cloneText(code);
  }

  async function retryClone(id, where) {
    if (cp.cloneBusy || !SUBJECT_RE.test(id)) return;
    const cloneKind = kindOfProject(id);
    cp.cloneBusy = true;
    if (where === 'result') cp.clone = { state: 'busy' };
    paint();
    let outcome; // {ok} | {code} | {unknown}
    try {
      await ctx.post('/api/v1/projects/' + id + '/clone', {}, { noRetry429: true, timeoutMs: CLONE_TIMEOUT_MS });
      outcome = { ok: true };
    } catch (err) {
      outcome = isUnknown(err) ? { unknown: true } : { code: err && typeof err.code === 'string' ? err.code : '', status: err && err.status };
    }
    if (gone()) return;
    cp.cloneBusy = false;
    let text;
    if (outcome.ok) text = '이 서버에 복제했습니다.';
    else if (outcome.unknown) text = CLONE_TEXT.unknown;
    else text = cloneFailureText(outcome.code, cloneKind);
    if (where === 'result') {
      cp.clone = outcome.ok ? { state: 'ok', code: 'ok', text }
        : outcome.code === 'already_cloned' ? { state: 'check', code: outcome.code, text }
          : { state: 'failed', code: outcome.code || 'other', text, unknown: !!outcome.unknown };
    } else {
      st.note = { error: !outcome.ok, text: outcome.ok ? '복제했습니다: ' + id : text };
    }
    paint();
    // A success, an existing folder and an unknown outcome all change what the list says.
    if (outcome.ok || outcome.unknown || outcome.code === 'already_cloned') await loadList();
  }

  function cloneCell(p, idOk) {
    if (typeof p.provider !== 'string') return '-';
    if (p.cloned === true) return el('span', { class: ['chip', 'c-done'] }, '복제됨');
    return el('span', {}, [
      el('span', { class: ['chip', 'c-idle'] }, '복제 안 됨'),
      idOk ? [' ', el('button', {
        type: 'button', class: 'btn', 'data-focus-key': 'clone-' + p.id, disabled: cp.cloneBusy, on: { click: () => retryClone(p.id, 'list') },
      }, ['다시 복제', el('span', { class: 'sr' }, ' (' + inline(p.id, 64) + ')')])] : null,
    ]);
  }

  function orphansBlock() {
    if (st.orphans.length === 0) return null;
    return el('div', { class: 'trunc', role: 'note', 'data-orphans': 'list' }, [
      el('p', {}, '제공자(GitLab/GitHub)에 일부만 만들어졌거나 연결이 중간에 멈춘 항목 ' + st.orphans.length + '개가 있을 수 있습니다.'),
      el('ul', {}, st.orphans.map((o) => {
        const kind = kindFor(o.provider);
        // a missing or unknown origin is never guessed: it says "origin unknown" and does not tell anyone to delete
        const origin = ORPHAN_ORIGINS.includes(o.origin) ? o.origin : 'unknown';
        return el('li', { 'data-origin': origin }, [
          el('span', { class: 'mono' }, o.project),
          ' (프로젝트 ID ' + (o.project_id === null ? '알 수 없음' : String(o.project_id)) + ', 멈춘 단계: ' + stepLabel(o.step, kind) + ', ' + o.at + ')',
          el('div', { class: 'note' }, orphanNotice(origin, kind)),
        ]);
      })),
    ]);
  }

  function renderCreateForm() {
    const f = cp.f;
    const kind = createKind();
    const gh = kind === 'github';
    const field = (labelText, id, control, hint) => el('div', { class: 'field' }, [el('label', { for: id }, labelText), control, hint ? el('span', { class: 'hint', id: id + '-hint' }, hint) : null]);
    const needVisibility = gh && !VISIBILITIES.includes(f.visibility.value);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, '새 프로젝트 만들기'),
      el('p', { class: 'lead' }, gh
        ? 'GitHub에 빈 저장소를 만들고(첫 커밋, main 보호, 이 저장소 전용 배포 키 포함) 이 서버에 복제해 등록합니다. 다음 화면에서 바뀔 내용을 모두 확인한 뒤 패스키로 서명합니다.'
        : 'GitLab에 빈 프로젝트를 만들고(첫 커밋, main 보호, 배포 키 포함) 이 서버에 복제해 등록합니다. 다음 화면에서 바뀔 내용을 모두 확인한 뒤 패스키로 서명합니다.'),
      el('div', { class: 'form' }, [
        field('제공자', 'cp-provider', f.provider),
        field(gh ? '소유자(내 계정 또는 조직)' : '그룹', 'cp-group', f.group,
          cp.groupsErr ? cp.groupsErr : cp.truncated ? (gh ? '소유자가 많아 일부만 보입니다. 찾는 소유자가 없으면 GitHub에서 확인하세요.' : '그룹이 많아 일부만 보입니다. 찾는 그룹이 없으면 GitLab에서 확인하세요.')
            : gh ? 'GitHub에 실제로 물어 받은 목록입니다(토큰의 계정과 속한 조직). GitHub에서는 새 조직을 이 도구로 만들 수 없습니다.' : 'GitLab에 실제로 물어 받은 그룹입니다(프로젝트를 만들 권한이 있는 것만).'),
        el('div', { class: ['row', 'rowgap'] }, [
          gh ? null : f.newGroup, // GitHub has no group creation: the control is not there at all
          cp.groupsErr ? [' ', el('button', { type: 'button', class: 'btn', 'data-focus-key': 'regroups', on: { click: retryGroups } }, '다시 불러오기')] : null,
        ]),
        field('프로젝트 이름', 'cp-name', f.name, gh ? '소문자·숫자·하이픈, 2~40자. GitHub 저장소 이름·프로젝트 ID·폴더 이름이 모두 이것입니다.'
          : '소문자·숫자·하이픈, 2~40자. GitLab 프로젝트 이름·프로젝트 ID·폴더 이름이 모두 이것입니다. GitLab이 예약한 이름은 GitLab이 거부할 수 있습니다.'),
        gh ? field('공개 여부(필수)', 'cp-visibility', f.visibility, '기본값이 없습니다. 공개 저장소는 인터넷의 누구나 코드를 읽을 수 있습니다. 비공개는 초대받은 사람만 봅니다. 브랜치 보호는 공개 저장소이거나 유료 요금제일 때만 켜집니다.') : null,
        field('설명(선택)', 'cp-desc', f.desc, '200자 이내.'),
        field('템플릿', 'cp-template', f.template, '코드에 고정된 파일들이 첫 커밋으로 들어갑니다.'),
        el('div', { class: 'field' }, [
          el('div', { class: 'row' }, [f.ai, ' ', el('label', { for: 'cp-ai' }, 'AI 전송 허용 (기본: 꺼짐)')]),
          el('span', { class: 'hint' }, '켜면 AI 계획 기능이 이 프로젝트 파일의 일부 내용을 외부 AI 서비스(OpenAI)로 보낼 수 있습니다. 공개 여부와는 별개의 설정입니다.'),
        ]),
        field('게시 권한', 'cp-publish', f.publish, '쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다. 읽기: 읽기 전용 키로 등록합니다.'),
        f.problem,
        noteNode(),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm', disabled: flow.busy || needVisibility, 'aria-describedby': needVisibility ? 'cp-visibility-hint' : null, on: { click: submitCreate } }, '변경 내용 확인'),
          ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: toList } }, '취소'),
        ]),
      ]),
    ]);
  }

  function renderGroupForm() {
    const g = cp.g;
    const field = (labelText, id, control, hint) => el('div', { class: 'field' }, [el('label', { for: id }, labelText), control, hint ? el('span', { class: 'hint' }, hint) : null]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, '새 그룹 만들기'),
      el('p', { class: 'lead' }, 'GitLab에 비공개 그룹을 만듭니다. 별도의 패스키 서명이 필요하며, 이 도구는 그룹을 삭제하지 않습니다.'),
      el('div', { class: 'form' }, [
        field('상위 그룹', 'cg-parent', g.parent, '"(최상위)"를 고르면 최상위 그룹이 됩니다. 그룹은 4단계까지입니다.'),
        field('그룹 이름', 'cg-name', g.name, '소문자·숫자·하이픈, 2~40자'),
        field('그룹 경로', 'cg-path', g.path, '주소에 쓰이는 한 마디. 기본은 이름과 같습니다.'),
        g.problem,
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm', disabled: flow.busy, on: { click: submitGroup } }, '변경 내용 확인'),
          ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: () => { cp.which = 'create'; st.step = 'cp_form'; paint(); } } }, '취소'),
        ]),
      ]),
    ]);
  }

  // The provider's GitLab address from this page's own provider list (not from the server's proposal).
  function providerUrlFact(id) {
    const prov = cp.providers.find((p) => p.id === id);
    const label = prov && prov.kind === 'github' ? '제공자 GitHub API 주소 (제공자 목록 기준)' : '제공자 GitLab 주소 (제공자 목록 기준)';
    return factItem(label, prov ? inline(prov.base_url, 200) : '(목록에 없음)', { 'data-field': 'provider_url' });
  }

  function renderXConfirm() {
    const s = flow.sent;
    if (s.action === ACTION_CREATE_GROUP) {
      return confirmPanel(flow, {
        title: '변경 내용 확인',
        lead: '아래 내용이 전부입니다. 서명하면 GitLab에 그룹이 바로 만들어집니다.',
        facts: [
          factItem('제공자', s.subject, { 'data-field': 'provider' }),
          providerUrlFact(s.subject),
          factItem('그룹 이름', s.fields.name, { 'data-field': 'name' }),
          factItem('그룹 경로', s.fields.path, { 'data-field': 'path' }),
          factItem('상위 그룹', s.fields.parent_path === '' ? '(최상위)' : s.fields.parent_path, { 'data-field': 'parent_path' }),
        ],
        onSign: () => flow.sign(),
        onCancel: backFromFlow,
      });
    }
    if (s.action === ACTION_LINK_PROJECT || s.action === ACTION_ATTACH_REMOTE) return renderRemoteConfirm(s);
    const f = flow.full || s.fields; // a refused proposal shows only what this page itself sent
    const key = flow.key;
    const yn = (b) => (b ? '예' : '아니오');
    const gh = kindFor(s.fields.provider) === 'github';
    const factsList = [
      factItem('제공자', s.fields.provider, { 'data-field': 'provider' }),
      providerUrlFact(s.fields.provider),
      factItem(gh ? '소유자' : '그룹 경로', s.fields.group_path, { 'data-field': 'group_path' }),
      factItem('프로젝트 이름', s.subject, { 'data-field': 'name' }),
    ];
    if (has(s.fields, 'visibility')) factsList.push(visibilityFact(s.fields.visibility));
    factsList.push(
      factItem('템플릿', f.template, { 'data-field': 'template' }),
      factItem('설명', f.description === '' ? '(없음)' : f.description, { 'data-field': 'description' }),
      factItem('AI 전송 허용', yn(f.ai_context), { 'data-field': 'ai_context' }),
      factItem('게시 권한', f.publish === 'write' ? '쓰기 — 이 서버가 브랜치를 올릴 수 있는 배포 키' : '읽기 — 읽기 전용 배포 키', { 'data-field': 'publish' }),
    );
    if (key) keyFacts(factsList, key);
    return confirmPanel(flow, {
      title: '변경 내용 확인',
      lead: gh ? '아래 내용이 전부입니다. 서명하면 GitHub에 저장소가 바로 만들어지고, 이 도구는 만든 것을 삭제하지 못합니다.'
        : '아래 내용이 전부입니다. 서명하면 GitLab에 프로젝트가 바로 만들어지고, 이 도구는 만든 것을 삭제하지 못합니다.',
      facts: factsList,
      extra: [
        key ? keyHint(gh) : null,
        s.fields.visibility === 'public' ? publicWarning() : null,
        f.ai_context ? el('div', { class: 'trunc', 'data-emph': 'ai-on', role: 'note' }, '[중요] AI 전송 허용: 예. 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있습니다.') : null,
      ],
      onSign: () => flow.sign(),
      onCancel: backFromFlow,
    });
  }

  // The deploy key as the browser verified it: fingerprint (computed here), the front of the line, its comment, the signed hash.
  function keyFacts(list, key) {
    list.push(
      factItem('배포 키 지문(ssh-keygen -l 형식)', key.fingerprint, { 'data-field': 'fingerprint' }),
      factItem('공개 키(.pub) 앞부분', key.line.slice(0, KEY_PREFIX_CHARS) + '…', { 'data-field': 'key_prefix' }),
      factItem('공개 키(.pub) 주석', key.line.split(' ').slice(2).join(' ') || '(없음)', { 'data-field': 'key_comment' }),
      factItem('서명에 묶이는 키 해시(앞 16자)', key.hash.slice(0, 16) + '…', { 'data-field': 'key_hash' }),
    );
  }

  function keyHint(gh) {
    return el('p', { class: 'note', 'data-hint': 'fingerprint' }, gh
      ? '이 키는 이 저장소 전용입니다: 제안할 때마다 서버가 새로 만들며 다른 저장소에는 쓰이지 않습니다. 이 화면은 서버가 보낸 공개 키에서 지문을 스스로 계산했습니다. 만든 뒤 GitHub 저장소의 Settings → Deploy keys에 같은 지문의 키가 있어야 합니다.'
      : '이 지문이 서버의 배포 키와 같은지 서버에서 ssh-keygen -l -f <.pub 파일 경로> 로 직접 비교해 확인하세요. 이 화면은 서버가 보낸 공개 키에서 지문을 스스로 계산했습니다.');
  }

  // The visibility of a new GitHub repository, in bold plain text (never colour alone).
  function visibilityFact(v) {
    return el('li', { 'data-field': 'visibility' }, [el('b', {}, '공개 여부'), ': ',
      el('strong', {}, v === 'public' ? '공개(public) — 인터넷의 누구나 읽을 수 있음' : '비공개(private) — 초대받은 사람만')]);
  }

  function publicWarning() {
    return el('div', { class: 'trunc', 'data-emph': 'public-warning', role: 'note' },
      '[중요] 공개 저장소입니다: 인터넷의 누구나(로그인하지 않아도) 이 저장소의 코드를 읽을 수 있습니다. 이 도구는 만든 저장소를 비공개로 바꾸거나 삭제하지 못합니다. 코드에 비밀 값이 들어가지 않게 하세요.');
  }

  // The confirmation of a link or an attach: a repository that already exists gets this server's deploy key.
  function renderRemoteConfirm(s) {
    const f = flow.full || s.fields;
    const key = flow.key;
    const link = s.action === ACTION_LINK_PROJECT;
    const gh = kindFor(s.fields.provider) === 'github';
    const factsList = [
      factItem('제공자', s.fields.provider, { 'data-field': 'provider' }),
      providerUrlFact(s.fields.provider),
      factItem('원격 저장소 경로', s.fields.remote_path, { 'data-field': 'remote_path' }),
      factItem(link ? '새 프로젝트 ID(저장소 이름에서 정해짐)' : '연결할 프로젝트 ID', s.subject, { 'data-field': 'id' }),
    ];
    if (link) factsList.push(factItem('AI 전송 허용', f.ai_context ? '예' : '아니오', { 'data-field': 'ai_context' }));
    factsList.push(factItem('게시 권한', f.publish === 'write' ? '쓰기 — 이 서버가 브랜치를 올릴 수 있는 배포 키' : '읽기 — 읽기 전용 배포 키', { 'data-field': 'publish' }));
    if (key) keyFacts(factsList, key);
    return confirmPanel(flow, {
      title: '변경 내용 확인',
      lead: link ? '아래 내용이 전부입니다. 서명하면 이미 있는 저장소를 이 서버의 새 프로젝트로 등록하고 복제합니다.'
        : '아래 내용이 전부입니다. 서명하면 이 프로젝트의 게시와 복제가 이 원격 저장소를 쓰게 됩니다.',
      warn: el('div', { class: 'trunc', role: 'note', 'data-warn': 'key-on-existing' },
        '[중요] 이미 있는 저장소의 설정(Deploy keys)에 이 서버의 배포 키가 등록됩니다. 이 도구는 저장소를 만들거나 지우거나 다른 설정을 바꾸지 않으며, 등록한 키도 지우지 않습니다.'),
      facts: factsList,
      extra: [
        key ? keyHint(gh) : null,
        link && f.ai_context ? el('div', { class: 'trunc', 'data-emph': 'ai-on', role: 'note' }, '[중요] AI 전송 허용: 예. 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있습니다.') : null,
      ],
      onSign: () => flow.sign(),
      onCancel: backFromFlow,
    });
  }

  function renderXResult() {
    const r = flow.result;
    const s = flow.sent;
    if (s.action === ACTION_CREATE_GROUP) {
      const kind = r === null ? 'pending' : r.ok ? 'ok' : 'error';
      const body = r === null ? '서버로 변경을 보내는 중…' : r.ok ? '그룹을 만들었습니다. 그룹 목록을 다시 읽는 중…'
        : r.unknown ? W3C_ERROR_TEXT.outcome_unknown : w3cFailureText(r.err, kindFor(s.subject));
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { id: 'h-projects' }, r === null ? '만드는 중' : r.ok ? '완료' : '그룹을 만들지 못했습니다'),
        el('p', { class: kind === 'error' ? ['note', 'err'] : ['note', 'ok'], role: kind === 'error' ? 'alert' : 'status', 'data-result': kind }, body),
        kind === 'error' ? el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backFromFlow } }, '돌아가기')]) : null,
      ]);
    }
    if (s.action === ACTION_LINK_PROJECT || s.action === ACTION_ATTACH_REMOTE) return renderRemoteResult(r, s);
    const kind = kindFor(s.fields.provider);
    const host = hostName(kind);
    if (r === null) {
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { id: 'h-projects' }, '만드는 중'),
        el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'pending' }, '서버가 ' + host + '에 ' + (kind === 'github' ? '저장소' : '프로젝트') + '를 만들고 있습니다(최대 약 90초 걸릴 수 있습니다). 끝나면 단계가 나옵니다. 이 화면을 닫아도 서버의 작업은 멈추지 않습니다.'),
      ]);
    }
    if (r.ok) {
      const c = cp.clone || { state: 'checking', code: 'other' };
      const cloneDone = c.state === 'ok';
      const cloneLine = cloneDone ? '이 서버에 복제했습니다.'
        : c.state === 'check' ? CLONE_TEXT.check
        : c.state === 'checking' ? '복제 결과를 확인하는 중…' : c.state === 'busy' ? '복제하는 중입니다(최대 2분)…' : (c.text || cloneText(c.code, kind));
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { id: 'h-projects' }, '완료'),
        el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, kind === 'github' ? '저장소를 만들고 등록했습니다.' : '프로젝트를 만들고 등록했습니다.'),
        stepList(stepStates('ok', null, cloneDone), kind),
        el('p', { class: cloneDone ? ['note', 'ok'] : c.state === 'busy' || c.state === 'checking' ? ['note'] : ['note', 'err'], role: 'status', 'data-clone': c.state }, cloneLine),
        c.state === 'failed'
          ? el('p', {}, [el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reclone', disabled: cp.cloneBusy, on: { click: () => retryClone(s.subject, 'result') } }, '다시 복제'), ' ',
            el('span', { class: 'note' }, '서명 없이 이미 등록된 내용대로 복제만 다시 합니다.')])
          : null,
        el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
      ]);
    }
    // a failure
    const partial = r.err && r.err.code === 'create_partial';
    const info = partial ? partialInfo(r.err, s) : null;
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, '프로젝트를 만들지 못했습니다'),
      el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'error' }, r.unknown ? W3C_ERROR_TEXT.outcome_unknown : w3cFailureText(r.err, kind)),
      (partial || r.unknown) && has(s.fields, 'visibility') ? el('p', { class: 'note', 'data-field': 'visibility' }, [el('strong', {}, '공개 여부: '),
        s.fields.visibility === 'public' ? '공개로 만들려던 저장소가 이미 만들어졌을 수 있습니다. 만들어졌다면 인터넷의 누구나 읽을 수 있으니 GitHub에서 바로 확인하세요.'
          : '비공개로 만들려던 저장소였습니다(만들어졌다면 초대받은 사람만 볼 수 있습니다).']) : null,
      partial && info.step !== null ? stepList(stepStates('partial', info.step, false), kind) : null,
      partial ? orphanBlock(info, kind) : null,
      partial && info.step === null ? el('p', { class: 'note' }, '어느 단계에서 멈췄는지 알 수 없습니다. ' + host + '에서 위 경로를 확인하세요.') : null,
      r.unknown ? el('p', { class: 'note' }, host + '에 같은 이름의 ' + (kind === 'github' ? '저장소' : '프로젝트') + '가 생겼는지, 목록의 알림에 올라왔는지 확인하세요. 같은 이름으로 다시 만들려면 먼저 그것을 확인하세요.') : null,
      el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
    ]);
  }

  // The result of a link or an attach. Nothing here is the server's word: the id and the remote path
  // are the ones this page sent, the sentences are fixed by code.
  function renderRemoteResult(r, s) {
    const kind = kindFor(s.fields.provider);
    const host = hostName(kind);
    const link = s.action === ACTION_LINK_PROJECT;
    if (r === null) {
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { id: 'h-projects' }, link ? '연결하는 중' : '원격을 연결하는 중'),
        el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'pending' }, '서버가 ' + host + '의 저장소를 확인하고 배포 키를 등록하고 있습니다(최대 약 2분 걸릴 수 있습니다). 이 화면을 닫아도 서버의 작업은 멈추지 않습니다.'),
      ]);
    }
    if (r.ok) {
      const c = cp.clone || { state: 'checking', code: 'other' };
      const cloneDone = c.state === 'ok';
      const cloneLine = cloneDone ? '이 서버에 복제했습니다.' : c.state === 'check' ? CLONE_TEXT.check
        : c.state === 'checking' ? '복제 결과를 확인하는 중…' : c.state === 'busy' ? '복제하는 중입니다(최대 2분)…' : (c.text || cloneText(c.code, kind));
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { id: 'h-projects' }, '완료'),
        el('p', { class: ['note', 'ok'], role: 'status', 'data-result': 'ok' }, link ? '이미 있는 저장소를 프로젝트로 등록했습니다.' : '프로젝트에 원격 저장소를 연결했습니다.'),
        el('ul', { class: 'pfacts', 'aria-label': '연결 결과' }, [
          factItem('프로젝트 ID', s.subject, { 'data-field': 'id' }),
          factItem('원격 저장소', s.fields.remote_path, { 'data-field': 'remote_path' }),
        ]),
        link ? el('p', { class: cloneDone ? ['note', 'ok'] : c.state === 'busy' || c.state === 'checking' ? ['note'] : ['note', 'err'], role: 'status', 'data-clone': c.state }, cloneLine) : null,
        !link && cp.keyFailed ? el('p', { class: ['note', 'err'], role: 'alert', 'data-note': 'key' }, W3C_ERROR_TEXT.key_unavailable) : null,
        link && c.state === 'failed'
          ? el('p', {}, [el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reclone', disabled: cp.cloneBusy, on: { click: () => retryClone(s.subject, 'result') } }, '다시 복제'), ' ',
            el('span', { class: 'note' }, '서명 없이 이미 등록된 내용대로 복제만 다시 합니다.')])
          : null,
        el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
      ]);
    }
    const partial = r.err && r.err.code === 'link_partial';
    const info = partial ? partialInfo(r.err, s) : null;
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, link ? '연결하지 못했습니다' : '원격을 연결하지 못했습니다'),
      el('p', { class: ['note', 'err'], role: 'alert', 'data-result': 'error' }, r.unknown ? W3C_ERROR_TEXT.outcome_unknown : w3cFailureText(r.err, kind)),
      partial && info.step !== null ? linkStepList(info.step) : null,
      partial ? orphanBlock(info, kind) : null,
      partial && info.step === null ? el('p', { class: 'note' }, '어느 단계에서 멈췄는지 알 수 없습니다. ' + host + '에서 위 저장소의 배포 키를 확인하세요.') : null,
      r.unknown ? el('p', { class: 'note' }, host + ' 저장소 설정(Deploy keys)에 이 서버의 배포 키(제목이 orchd로 시작)가 붙었는지와 목록을 확인하세요. 확인하기 전에는 같은 저장소를 다시 연결하지 마세요.') : null,
      el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
    ]);
  }

  // ---- link ("기존 저장소 연결") and attach ("원격 연결") ----
  async function startRemote(mode, row) {
    if (st.busy || flow.busy) return;
    const provs = await readProviders();
    if (provs === null) return;
    lk.mode = mode;
    lk.row = row || null;
    lk.f = buildRemoteForm();
    st.step = 'lk_form';
    paint();
  }

  // remoteCheck: what the typed path means for the chosen provider: {id} when it can be signed, else {why}.
  function remoteCheck() {
    const f = lk.f;
    const kind = kindFor(f.provider.value) || 'gitlab';
    const remote = f.remote.value.trim();
    if (!validRemotePathForKind(kind, remote)) return { why: 'path', kind };
    const name = remote.slice(remote.lastIndexOf('/') + 1);
    if (lk.mode === 'attach') {
      const same = kind === 'github' ? name.toLowerCase() === lk.row.id : name === lk.row.id;
      return same ? { id: lk.row.id, kind, remote } : { why: 'mismatch', kind };
    }
    // the project id is the repository name (GitHub names are case-insensitive: lower case)
    const id = kind === 'github' ? name.toLowerCase() : name;
    return validName(id) ? { id, kind, remote } : { why: 'id', id, kind };
  }

  function updateDerived() {
    const f = lk.f;
    if (!f || !f.derived) return;
    const c = remoteCheck();
    clear(f.derived);
    f.derived.append(f.remote.value.trim() === '' ? '(저장소 경로를 적으면 나타납니다)' : c.id !== undefined && c.why === undefined ? c.id : c.why === 'id' ? '(사용할 수 없음: ' + c.id + ')' : '(경로를 확인하세요)');
  }

  function buildRemoteForm() {
    const f = {};
    const link = lk.mode === 'link';
    f.provider = el('select', { id: 'lk-provider', 'data-focus-key': 'provider', on: { change: () => { setProblem(f, ''); updateDerived(); paint(); } } },
      cp.providers.map((p) => el('option', { value: p.id }, inline(p.id, 64) + ' — ' + hostName(p.kind) + ' — ' + inline(p.base_url, 80))));
    f.provider.value = cp.providers[0].id;
    f.remote = el('input', { type: 'text', id: 'lk-remote', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: '255', 'data-focus-key': 'remote', on: { input: updateDerived } });
    f.remote.value = '';
    if (link) {
      f.ai = el('input', { type: 'checkbox', id: 'lk-ai', 'data-focus-key': 'ai' });
      f.ai.checked = false; // default: the repository's contents are NOT sent to an AI service
    }
    f.publish = el('select', { id: 'lk-publish', 'data-focus-key': 'publish' }, PUBLISH_MODES.map((m) => el('option', { value: m }, PUBLISH_TEXT[m])));
    f.publish.value = 'read'; // an existing repository gets the least right unless the person asks for more
    f.derived = el('span', { class: 'mono' }, '(저장소 경로를 적으면 나타납니다)');
    f.problem = el('p', { class: ['note', 'err'], role: 'alert', 'data-problem': 'form' });
    f.problem.hidden = true;
    return f;
  }

  function submitRemote() {
    const f = lk.f;
    if (!f || flow.busy) return;
    const c = remoteCheck();
    if (c.why === 'path') {
      return setProblem(f, c.kind === 'github' ? '원격 저장소 경로는 소유자/저장소 형태여야 합니다(예: octo/Hello-World). 점 경로, .git 끝, % 기호, 한글은 쓸 수 없습니다.'
        : '원격 저장소 경로는 그룹/하위그룹/프로젝트 형태여야 합니다(소문자·숫자·하이픈 이름, 예: team/sub/app-one). 점 경로, .git 끝, % 기호는 쓸 수 없습니다.');
    }
    if (c.why === 'id') return setProblem(f, '저장소 이름에서 정해지는 프로젝트 ID(' + c.id + ')가 규칙에 맞지 않습니다. 프로젝트 ID는 소문자로 시작하고 소문자·숫자·하이픈만 써서 2~40자여야 합니다. 이 이름의 저장소는 연결할 수 없습니다.');
    if (c.why === 'mismatch') return setProblem(f, w3cFailureText({ code: 'remote_name_mismatch' }, c.kind));
    if (!PUBLISH_MODES.includes(f.publish.value)) return setProblem(f, '게시 권한을 목록에서 고르세요.');
    setProblem(f, '');
    const provider = f.provider.value;
    if (lk.mode === 'link') {
      startFlow({ action: ACTION_LINK_PROJECT, subject: c.id, fields: { provider, remote_path: c.remote, ai_context: f.ai.checked === true, publish: f.publish.value } }, 'link');
    } else {
      startFlow({ action: ACTION_ATTACH_REMOTE, subject: c.id, fields: { provider, remote_path: c.remote, publish: f.publish.value } }, 'attach');
    }
  }

  function renderRemoteForm() {
    const f = lk.f;
    const link = lk.mode === 'link';
    const kind = kindFor(f.provider.value) || 'gitlab';
    const field = (labelText, id, control, hint) => el('div', { class: 'field' }, [el('label', { for: id }, labelText), control, hint ? el('span', { class: 'hint' }, hint) : null]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, link ? '기존 저장소 연결' : '원격 연결'),
      el('p', { class: 'lead' }, link
        ? '제공자에 이미 있는 저장소를 이 서버의 새 프로젝트로 가져옵니다. 저장소를 만들거나 지우거나 설정을 바꾸지 않고, 이 서버의 배포 키만 등록합니다. 다음 화면에서 바뀔 내용을 모두 확인한 뒤 패스키로 서명합니다.'
        : '이미 등록된 프로젝트에 제공자의 저장소를 원격으로 연결합니다. 프로젝트의 파일과 AI 설정은 바뀌지 않고, 이 서버의 배포 키가 그 저장소에 등록됩니다. 다음 화면에서 바뀔 내용을 모두 확인한 뒤 패스키로 서명합니다.'),
      el('div', { class: 'form' }, [
        link ? null : el('div', { class: 'field', 'data-field': 'subject' }, [el('div', {}, [el('b', {}, '연결할 프로젝트'), ': ', el('span', { class: 'mono' }, inline(lk.row.id, 64))]),
          el('span', { class: 'hint' }, '이미 원격이 있는 프로젝트와, 서버 설정에 원격이 정해진 프로젝트는 연결할 수 없습니다(서버가 거부합니다).')]),
        field('제공자', 'lk-provider', f.provider),
        field('원격 저장소 경로', 'lk-remote', f.remote, kind === 'github' ? '소유자/저장소, 예: octo/Hello-World' : '그룹/하위그룹/프로젝트, 예: team/sub/app-one'),
        link ? el('div', { class: 'field', 'data-field': 'derived-id' }, [el('div', {}, [el('b', {}, '프로젝트 ID(저장소 이름에서 정해집니다)'), ': ', f.derived]),
          el('span', { class: 'hint' }, '프로젝트 ID는 원격 저장소 이름과 같아야 합니다' + (kind === 'github' ? '(GitHub: 소문자로 바꿉니다)' : '') + '. 그렇지 않은 저장소는 연결할 수 없습니다.')]) : null,
        link ? el('div', { class: 'field' }, [
          el('div', { class: 'row' }, [f.ai, ' ', el('label', { for: 'lk-ai' }, 'AI 전송 허용 (기본: 꺼짐)')]),
          el('span', { class: 'hint' }, '켜면 AI 계획 기능이 이 저장소 파일의 일부 내용을 외부 AI 서비스(OpenAI)로 보낼 수 있습니다.'),
        ]) : null,
        field('게시 권한', 'lk-publish', f.publish, '기본은 읽기입니다. 쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다.'),
        f.problem,
        noteNode(),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm', disabled: flow.busy, on: { click: submitRemote } }, '변경 내용 확인'),
          ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: toList } }, '취소'),
        ]),
      ]),
    ]);
  }

  // ---- rendering ----
  function noteNode() {
    return st.note ? el('p', { class: st.note.error ? ['note', 'err'] : ['note', 'ok'], role: st.note.error ? 'alert' : 'status' }, [
      st.note.text, st.note.link ? [' ', el('a', { href: st.note.link }, '제공자 화면')] : null,
    ]) : null;
  }

  function render() {
    switch (st.step) {
      case 'candidates':
        return el('div', { class: ['panel', 'canvas'] }, [el('h1', { id: 'h-projects' }, '프로젝트 추가'), el('p', { class: 'lead' }, '저장소 목록을 불러오는 중…')]);
      case 'form':
        return renderForm();
      case 'confirm':
        return renderConfirm();
      case 'result':
        return renderResult();
      case 'cp_form':
        return renderCreateForm();
      case 'cg_form':
        return renderGroupForm();
      case 'lk_form':
        return renderRemoteForm();
      case 'x_confirm':
        return renderXConfirm();
      case 'x_result':
        return renderXResult();
      case 'list':
        return renderList();
      default:
        return el('div', { class: ['panel', 'canvas'] }, [el('p', { class: 'lead' }, '불러오는 중…')]);
    }
  }

  function renderList() {
    if (st.list === null) return errorPane(st.listFailed, '#/overview', '← 개요');
    const rows = st.list.map((p, i) => projectRow(p, i));
    const table = el('div', { class: 'tscroll' }, [
      el('table', { class: 'ptable' }, [
        el('caption', { class: 'sr' }, '등록된 프로젝트'),
        el('thead', {}, [el('tr', {}, [
          el('th', { scope: 'col' }, 'ID'), el('th', { scope: 'col' }, '경로'), el('th', { scope: 'col' }, '종류'),
          el('th', { scope: 'col' }, 'AI 전송 허용'), el('th', { scope: 'col' }, '원격 연결'), el('th', { scope: 'col' }, '복제'),
          el('th', { scope: 'col' }, '자동 병합'), el('th', { scope: 'col' }, '변경'),
        ])]),
        el('tbody', {}, rows),
      ]),
    ]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, '프로젝트'),
      expiry ? expiryBanner(expiry.rows, expiry.warn, { link: true }) : null,
      el('p', { class: 'lead' }, st.list.length === 0 ? '등록된 프로젝트가 없습니다.'
        : '등록된 프로젝트 ' + st.list.length + '개. AI 전송 허용을 바꾸거나 새로 등록하려면 패스키 서명이 필요합니다.'),
      el('div', { class: 'row' }, [
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'add', on: { click: startRegister } }, '프로젝트 추가'),
        ' ',
        el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'create', disabled: st.busy || flow.busy, on: { click: startCreate } }, '새 프로젝트 만들기'),
        ' ',
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'link', disabled: st.busy || flow.busy, on: { click: () => startRemote('link', null) } }, '기존 저장소 연결'),
        ' ',
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reload', disabled: st.busy, on: { click: reload } }, '목록 새로고침'),
      ]),
      noteNode(),
      orphansBlock(),
      st.list.length > 0 ? el('div', { class: 'card' }, [table]) : null,
      el('p', { class: 'note' }, '원격 연결(GitLab·GitHub)과 자동 병합 설정은 이 표에서 바꾸지 않습니다. 원격이 없는 프로젝트에는 "원격 연결" 단추가 있습니다.'),
    ]);
  }

  async function reload() {
    if (st.busy) return;
    st.note = null;
    await loadList();
  }

  function projectRow(p, i) {
    const idOk = typeof p.id === 'string' && SUBJECT_RE.test(p.id);
    const ai = p.ai_context === true;
    const toggle = idOk
      ? el('button', {
        type: 'button', class: 'btn', 'data-focus-key': 'toggle-' + i,
        on: {
          click: () => {
            if (!st.busy) propose({ action: ACTION_SET_FLAG, subject: p.id, fields: { flag: FLAG_AI_CONTEXT, value: !ai } }, { path: typeof p.path === 'string' ? p.path : '', old: ai });
          },
        },
      }, [ai ? 'AI 전송 끄기' : 'AI 전송 켜기', el('span', { class: 'sr' }, ' (' + inline(p.id, 64) + ')')])
      : el('span', { class: 'note' }, '-');
    const gl = isObj(p.gitlab) && typeof p.gitlab.project === 'string' ? inline(p.gitlab.project, 120) : '연결 없음';
    // a project with no remote of its own can get one (a static --git-remote-for entry is invisible here: the server refuses it)
    const canAttach = idOk && !isObj(p.gitlab) && (p.provider === null || p.provider === undefined);
    return el('tr', {}, [
      el('td', { class: 'mono', 'data-label': 'ID' }, inline(p.id, 64)),
      el('td', { class: 'mono', 'data-label': '경로' }, inline(p.path, 200)),
      el('td', { class: 'mono', 'data-label': '종류' }, inline(p.kind, 64)),
      el('td', { 'data-label': 'AI 전송 허용' }, el('span', { class: ['chip', ai ? 'c-wait' : 'c-idle'] }, ai ? '예' : '아니오')),
      el('td', { 'data-label': '원격 연결' }, gl),
      el('td', { 'data-label': '복제' }, cloneCell(p, idOk)),
      el('td', { 'data-label': '자동 병합' }, isObj(p.auto_merge) && p.auto_merge.enabled === true ? '켜짐' : '꺼짐'),
      el('td', {}, [toggle, canAttach ? [' ', el('button', {
        type: 'button', class: 'btn', 'data-focus-key': 'attach-' + i, disabled: st.busy || flow.busy,
        on: { click: () => startRemote('attach', { id: p.id }) },
      }, ['원격 연결', el('span', { class: 'sr' }, ' (' + inline(p.id, 64) + ')')])] : null]),
    ]);
  }

  function renderForm() {
    const f = st.form;
    const field = (labelText, id, control, hint) => el('div', { class: 'field' }, [
      el('label', { for: id }, labelText), control, hint ? el('span', { class: 'hint' }, hint) : null,
    ]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, '프로젝트 추가'),
      el('p', { class: 'lead' }, '허용된 폴더 안의 저장소를 프로젝트로 등록합니다. 다음 화면에서 바뀔 내용을 모두 확인한 뒤 패스키로 서명합니다.'),
      el('div', { class: 'form' }, [
        field('저장소', 'p-cand', f.cand, '아직 등록되지 않은 저장소만 나옵니다.'),
        field('프로젝트 ID', 'p-id', f.id, '소문자·숫자·하이픈, 40자 이내'),
        field('종류(정책 클래스)', 'p-kind', f.kind),
        el('div', { class: 'field' }, [
          el('div', { class: 'row' }, [f.ai, ' ', el('label', { for: 'p-ai' }, 'AI 전송 허용 (기본: 꺼짐)')]),
          el('span', { class: 'hint' }, '켜면 AI 계획 기능이 이 저장소의 일부 파일 내용을 외부 AI 서비스(OpenAI)로 보낼 수 있습니다.'),
        ]),
        f.problem,
        noteNode(),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm', disabled: st.busy, on: { click: submitForm } }, '변경 내용 확인'),
          ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: toList } }, '취소'),
        ]),
      ]),
    ]);
  }

  function renderConfirm() {
    const s = st.sent;
    const aiNew = s.action === ACTION_REGISTER ? s.fields.ai_context : s.fields.value;
    const li = (k, v, attrs) => el('li', attrs || {}, [el('b', {}, k), ': ', el('span', { class: 'mono' }, v)]);
    const facts = [li('프로젝트 ID', s.subject)];
    if (s.action === ACTION_REGISTER) {
      facts.push(li('경로', s.fields.path), li('종류', s.fields.kind));
    } else {
      facts.push(li('바꿀 설정', 'AI 전송 허용'));
      if (st.view && st.view.path) facts.push(li('경로', inline(st.view.path, MAX_PATH_BYTES)));
    }
    const yn = (b) => (b ? '예' : '아니오');
    const aiValue = s.action === ACTION_SET_FLAG && st.view ? yn(st.view.old) + ' → ' + yn(aiNew) : yn(aiNew);
    facts.push(li('AI 전송 허용', aiValue, { 'data-field': 'ai_context' }));

    let controls;
    if (st.problems.length > 0) {
      controls = el('div', { class: 'trunc', role: 'alert', 'data-warn': 'mismatch' }, TEXT.mismatch);
    } else {
      controls = el('div', { class: 'confirm' }, [
        st.signerNote ? el('p', { class: 'note' }, st.signerNote) : null,
        st.signer && supported()
          ? el('button', {
            type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'sign', disabled: st.busy, on: { click: onSign },
          }, '패스키로 서명하고 적용')
          : null,
      ]);
    }
    const msgText = st.problems.length === 0 ? st.resp.message : '';
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, '변경 내용 확인'),
      el('p', { class: 'lead' }, '아래 내용이 전부입니다. 서명하면 바로 적용됩니다.'),
      el('ul', { class: 'pfacts', 'aria-label': '바뀌는 내용' }, facts),
      aiNew ? el('div', { class: 'trunc', 'data-emph': 'ai-on', role: 'note' }, TEXT.aiOn) : null,
      st.problems.length === 0
        ? el('section', { class: 'sec', 'aria-labelledby': 'h-says' }, [
          el('h2', { id: 'h-says' }, '서버의 설명'),
          el('ul', {}, st.resp.preview.map((x) => el('li', { 'data-preview': 'item' }, x))),
        ])
        : null,
      msgText
        ? el('section', { class: 'sec', 'aria-labelledby': 'h-msg' }, [
          el('h2', { id: 'h-msg' }, '서명할 메시지 (서버가 만든 원문)'),
          el('pre', { class: 'msg', role: 'region', tabindex: '0', 'aria-label': '서명할 메시지' }, msgText),
          el('p', { class: 'note' }, '이 화면은 위 내용으로 변경의 해시를 직접 다시 계산해 서버가 보낸 값과 같을 때만 서명 단추를 보여 줍니다. 5분 안에 서명해야 합니다.'),
        ])
        : null,
      controls,
      noteNode(),
      el('p', {}, [el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: toList } }, '취소')]),
    ]);
  }

  function renderResult() {
    const r = st.result;
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-projects' }, r.pending ? '적용하는 중' : r.error ? '변경하지 못했습니다' : '완료'),
      el('p', { class: r.error ? ['note', 'err'] : ['note', 'ok'], role: r.error ? 'alert' : 'status', 'data-result': r.error ? 'error' : r.pending ? 'pending' : 'ok' }, r.text),
      r.pending ? null : el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
    ]);
  }

  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  // One read per visit and after each change: the page never polls, so a
  // refresh can not wipe a confirmation.
  loadExpiry();
  return loadList();
}

// resolveSigner: the passkey of the logged-in principal (this tab's login, or
// GET /auth/signers once per page load).
export async function resolveSigner(ctx) {
  const s = ctx.session;
  if (!s || s.kind !== 'session' || typeof s.principal !== 'string') return { error: TEXT.noPasskeySession };
  const rem = rememberedPasskey();
  if (rem && rem.signer === s.principal) return { id: rem.signer, credentialId: rem.credentialId, rpId: rem.rpId };
  const list = await activeSigners({ get: ctx.get });
  const found = list.find((x) => x.id === s.principal);
  if (!found) return { error: TEXT.noSigner };
  return { id: found.id, credentialId: found.credential_id };
}

registerView('projects', projectsView);
