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
  };
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

  // ---- rendering ----
  function noteNode() {
    return st.note ? el('p', { class: st.note.error ? ['note', 'err'] : ['note', 'ok'], role: st.note.error ? 'alert' : 'status' }, st.note.text) : null;
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
          el('th', { scope: 'col' }, 'AI 전송 허용'), el('th', { scope: 'col' }, 'GitLab 연결'),
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
        el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'add', on: { click: startRegister } }, '프로젝트 추가'),
        ' ',
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reload', disabled: st.busy, on: { click: reload } }, '목록 새로고침'),
      ]),
      noteNode(),
      st.list.length > 0 ? el('div', { class: 'card' }, [table]) : null,
      el('p', { class: 'note' }, 'GitLab 연결과 자동 병합은 지금은 읽기 전용입니다.'),
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
    return el('tr', {}, [
      el('td', { class: 'mono', 'data-label': 'ID' }, inline(p.id, 64)),
      el('td', { class: 'mono', 'data-label': '경로' }, inline(p.path, 200)),
      el('td', { class: 'mono', 'data-label': '종류' }, inline(p.kind, 64)),
      el('td', { 'data-label': 'AI 전송 허용' }, el('span', { class: ['chip', ai ? 'c-wait' : 'c-idle'] }, ai ? '예' : '아니오')),
      el('td', { 'data-label': 'GitLab 연결' }, gl),
      el('td', { 'data-label': '자동 병합' }, isObj(p.auto_merge) && p.auto_merge.enabled === true ? '켜짐' : '꺼짐'),
      el('td', {}, toggle),
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
