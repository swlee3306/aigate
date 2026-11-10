// tokens.js: the token menu (#/tokens): the list, "add or replace a token" and
// "delete a token". Every change is a SIGNED change, in four steps:
//
//   begin   -> the server hands out a one-time request id (no value)
//   message -> the browser sends the fields plus value_hash, the server builds
//              the canonical message, `change` and its sentences (no value)
//   sign    -> confirmation screen, then the passkey signs the message
//   apply   -> the ONLY request that carries the token value
//
// value_hash = SHA-256(request ':' value) over UTF-8 bytes, the same bytes as
// Go's registrymsg.ValueHash. The signature covers that hash, never the value.
//
// The value is checked here BEFORE anything is requested (the broker's rules:
// 32..512 printable ASCII characters, no whitespace anywhere, never trimmed),
// so nobody spends a one-time signature on a value the broker would refuse.
//
// Value hygiene: the password field is emptied the moment the value moves to
// the one holder variable below; the holder is overwritten with '' when the
// body of the apply request has been built, on cancel, on every error and when
// the page is left. The value is never put into DOM text, an attribute, a
// URL, an event, a message or a log. LIMIT: JavaScript strings are immutable,
// so overwriting a variable only drops the reference; the characters stay in
// memory until the garbage collector reuses it, and the browser's own copies
// (the input widget, the request body, the network stack) cannot be wiped
// from here at all.
//
// Every string from the server is a text node (el()); the server's own error
// words are never shown, only the fixed sentences below chosen by code.

import { el, inline, clear, findHostile } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';
import { checkRegistryMessage, sha256Hex, sha256HexBytes, signRegistryChange, supported, waText } from '../webauthn.js';
import { resolveSigner } from './projects.js';
import { expiryBanner } from '../expiry.js';

// --- constants shared with the server (TestConstantsAgreeWithWeb compares them) ---

export const TOKEN_REF_RE = /^[a-z][a-z0-9-]{0,39}$/;
export const TOKEN_REF_MAX = 40;
export const TOKEN_KINDS = ['openai', 'gitlab', 'github'];
export const TOKEN_SINGLETON_KIND = 'openai';
export const TOKEN_VALUE_MIN = 32;
export const TOKEN_VALUE_MAX = 512;
export const TOKEN_VALUE_RE = /^[\x21-\x7e]+$/;
export const TOKEN_APPLY_MAX_BYTES = 4096;
export const EXPIRES_RE = /^[0-9]{4}-[0-9]{2}-[0-9]{2}$/;
export const EXPIRES_YEAR_MIN = 2000;
export const EXPIRES_YEAR_MAX = 9999;
export const ACTION_SET_TOKEN = 'set_token';
export const ACTION_DELETE_TOKEN = 'delete_token';
export const VALUE_HASH_RE = /^[0-9a-f]{64}$/;
export const REQUEST_ID_RE = /^[0-9a-f]{32}$/;
const ISSUED_RE = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$/;

const MAX_PREVIEW_LINES = 12;
const MAX_PREVIEW_CHARS = 600;
const MAX_ROWS = 200;
const KINDS_WITH_HOST = ['gitlab', 'github'];

// The browser waits longer than orchd's worst case for a signed change (the
// broker write limit is 15 s): the 15 s default of the api layer would let the
// browser give up first. A token write is never repeated on a 429.
const TOKEN_TIMEOUT_MS = 25000;
const WRITE_OPTS = { noRetry429: true, timeoutMs: TOKEN_TIMEOUT_MS };

// --- the clock (the day an expiry is compared with is the browser's UTC date) ---

const deps = { now: () => new Date() };

// configure swaps the clock (tests). Pass nothing to restore the real one.
export function configure(over) {
  deps.now = over && typeof over.now === 'function' ? over.now : () => new Date();
}

function todayUTC() {
  return deps.now().toISOString().slice(0, 10);
}

// --- rules ---------------------------------------------------------------------

export class ChangeError extends Error {
  constructor(why) {
    super(why);
    this.name = 'ChangeError';
  }
}

// validExpires: the broker's rule: YYYY-MM-DD, a real calendar date, year 2000..9999.
export function validExpires(s) {
  if (typeof s !== 'string' || !EXPIRES_RE.test(s)) return false;
  const y = Number(s.slice(0, 4));
  const m = Number(s.slice(5, 7));
  const d = Number(s.slice(8, 10));
  if (y < EXPIRES_YEAR_MIN || y > EXPIRES_YEAR_MAX) return false;
  const t = new Date(Date.UTC(y, m - 1, d));
  return t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d;
}

// checkValue: null when the value is acceptable, else the code the broker would
// answer. Characters first, then length (the broker's order). Nothing is trimmed.
export function checkValue(v) {
  if (typeof v !== 'string') return 'token_bad_format';
  if (v !== '' && !TOKEN_VALUE_RE.test(v)) return 'token_bad_format';
  if (v.length < TOKEN_VALUE_MIN) return 'token_too_short';
  if (v.length > TOKEN_VALUE_MAX) return 'token_bad_format';
  return null;
}

const SCHEMA = {
  [ACTION_SET_TOKEN]: { kind: 's', expires: 's', value_hash: 's' },
  [ACTION_DELETE_TOKEN]: {},
};

// canonicalChange returns {"action":..,"fields":{..sorted..},"subject":..}: the
// same bytes as Go's registrymsg.CanonicalChange. It refuses whatever Go
// refuses. Every string here is ASCII by its own rule, so there is no escaping.
export function canonicalChange(action, subject, fields) {
  if (typeof action !== 'string' || !Object.prototype.hasOwnProperty.call(SCHEMA, action)) throw new ChangeError('unknown action');
  const spec = SCHEMA[action];
  if (typeof subject !== 'string' || !TOKEN_REF_RE.test(subject)) throw new ChangeError('bad subject');
  if (fields === null || typeof fields !== 'object' || Array.isArray(fields)) throw new ChangeError('bad fields');
  const proto = Object.getPrototypeOf(fields);
  if (proto !== Object.prototype && proto !== null) throw new ChangeError('bad fields');
  const names = Object.keys(fields);
  if (names.length !== Object.keys(spec).length) throw new ChangeError('fields do not match the action');
  for (const name of names) {
    if (!Object.prototype.hasOwnProperty.call(spec, name)) throw new ChangeError('unknown field');
    const v = fields[name];
    if (typeof v !== 'string') throw new ChangeError('wrong type');
    if (name === 'kind' && !TOKEN_KINDS.includes(v)) throw new ChangeError('bad kind');
    if (name === 'expires' && v !== '' && !validExpires(v)) throw new ChangeError('bad expires'); // only expires may be empty
    if (name === 'value_hash' && !VALUE_HASH_RE.test(v)) throw new ChangeError('bad value_hash');
  }
  names.sort();
  const body = names.map((n) => JSON.stringify(n) + ':' + JSON.stringify(fields[n])).join(',');
  return '{"action":' + JSON.stringify(action) + ',"fields":{' + body + '},"subject":' + JSON.stringify(subject) + '}';
}

// computeChange is the `change` hash: lowercase hex SHA-256 of canonicalChange.
export async function computeChange(action, subject, fields) {
  return sha256Hex(canonicalChange(action, subject, fields));
}

// computeValueHash is lowercase hex SHA-256 of the UTF-8 bytes of request ':' value
// (Go: registrymsg.ValueHash). The request must be a 32-character lowercase hex
// id, as in Go. The byte arrays made here are zeroed before returning.
export async function computeValueHash(request, value) {
  if (typeof request !== 'string' || !REQUEST_ID_RE.test(request) || typeof value !== 'string') throw new ChangeError('bad request id');
  const enc = new TextEncoder();
  const salt = enc.encode(request + ':');
  const raw = enc.encode(value);
  const all = new Uint8Array(salt.length + raw.length);
  all.set(salt, 0);
  all.set(raw, salt.length);
  raw.fill(0);
  try {
    return await sha256HexBytes(all);
  } finally {
    all.fill(0);
  }
}

// --- fixed texts -------------------------------------------------------------

// ERROR_TEXT: one fixed sentence per refusal code (broker codes, orchd codes).
// The server's own words are never shown.
export const ERROR_TEXT = {
  not_signed: '서명이 없는 요청이라 거부되었습니다. 변경은 적용되지 않았습니다.',
  bad_signature: '서명이 올바르지 않아 거부되었습니다. 변경은 적용되지 않았습니다.',
  unknown_signer: '등록되지 않은 서명자의 서명이라 거부되었습니다. 변경은 적용되지 않았습니다.',
  stale_signature: '서명이 만료되었습니다(5분 안에만 쓸 수 있습니다). 변경은 적용되지 않았습니다. 처음부터 다시 진행하세요.',
  signers_unavailable: '서버의 서명자 목록을 쓸 수 없어 처리하지 못했습니다. 변경은 적용되지 않았습니다. 관리자에게 알려 주세요.',
  request_reused: '이미 사용된 요청이라 거부되었습니다. 변경은 다시 적용되지 않았습니다. 목록을 확인하고 필요하면 새로 시작하세요.',
  change_mismatch: '서명한 내용과 서버가 받은 내용이 달라 거부되었습니다. 변경은 적용되지 않았습니다.',
  bad_request: '요청 형식이 올바르지 않거나 만료일이 이미 지나 거부되었습니다. 변경은 적용되지 않았습니다.',
  token_unknown: '등록되지 않은 토큰입니다(설정 파일의 키는 여기서 지울 수 없습니다). 변경은 적용되지 않았습니다. 목록을 새로 읽어 확인하세요.',
  bad_kind: '토큰 종류가 올바르지 않습니다. openai, gitlab, github 중 하나여야 합니다. 변경은 적용되지 않았습니다.',
  token_too_short: '토큰 값이 너무 짧습니다(32자 이상). 변경은 적용되지 않았습니다.',
  token_bad_format: '토큰 값의 형식이 올바르지 않습니다. 앞뒤를 포함해 공백이 없는 인쇄 가능한 ASCII 문자 32~512자여야 합니다. 변경은 적용되지 않았습니다.',
  value_mismatch: '서명한 토큰 값과 보낸 토큰 값이 달라 거부되었습니다. 값을 다시 입력하고 처음부터 다시 진행하세요. 변경은 적용되지 않았습니다.',
  openai_singleton: 'openai 종류는 이름이 openai인 토큰 하나만 둘 수 있고, 다른 종류는 openai라는 이름을 쓸 수 없습니다. 변경은 적용되지 않았습니다.',
  token_exists_other_kind: '같은 이름의 토큰이 다른 종류로 이미 있습니다. 다른 이름을 쓰거나 먼저 삭제하세요. 변경은 적용되지 않았습니다.',
  tokens_unavailable: '토큰 저장소를 쓸 수 없어 처리하지 못했습니다. 관리자에게 알려 주세요.',
  outcome_unknown: '변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 목록을 다시 읽어 확인한 뒤 필요하면 다시 변경하세요.',
  broker_unavailable: '토큰 저장 서비스에 연결하지 못했습니다. 변경은 적용되지 않았습니다.',
  token_rate_limited: '토큰 변경 요청이 너무 잦습니다. 잠시 후 다시 시도하세요.',
  deploy_key_unavailable: '배포 키의 공개 키를 읽을 수 없습니다. 관리자에게 알려 주세요.',
  internal_error: '서버에서 내부 오류가 발생했습니다. 변경이 적용되었는지 알 수 없습니다. 목록을 다시 읽어 확인하세요.',
  broker_error: '토큰 저장 서비스가 예상하지 못한 응답을 했습니다. 변경이 적용되었는지 알 수 없습니다. 목록을 다시 읽어 확인하세요.',
  ai_key_missing: 'AI 키가 등록되어 있지 않습니다. 토큰 화면에서 openai 키를 등록하세요.',
  body_too_large: '요청이 너무 커서 거부되었습니다. 변경은 적용되지 않았습니다.',
};

const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);

const STATUS_TEXT = {
  403: '이 변경을 할 권한이 없습니다. 변경은 적용되지 않았습니다.',
  404: '찾을 수 없습니다. 변경은 적용되지 않았습니다.',
  429: ERROR_TEXT.token_rate_limited,
};

// checkList: the fixed text for this failure tells the user to look at the list,
// and the list is read once more.
function checkList(err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (['outcome_unknown', 'broker_error', 'internal_error', 'tokens_unavailable', 'token_unknown'].includes(code)) return true;
  if (has(ERROR_TEXT, code)) return false;
  const st = err && typeof err.status === 'number' ? err.status : 0;
  return st === 0 || st >= 500;
}

// failureText: the fixed sentence for an ApiError. Unknown codes fall back to
// the HTTP status, never to the server's message.
function failureText(err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (has(ERROR_TEXT, code)) return ERROR_TEXT[code];
  const st = err && typeof err.status === 'number' ? err.status : 0;
  if (has(STATUS_TEXT, st)) return STATUS_TEXT[st];
  if (st >= 500) return '서버에서 오류가 발생했습니다. 변경이 적용되었는지 목록에서 확인하세요.';
  return '요청을 처리하지 못했습니다. 변경은 적용되지 않았습니다.';
}

const TEXT = {
  mismatch: '서버가 보낸 변경 내용이 화면의 내용과 맞지 않아 서명할 수 없습니다. 아무것도 서명하거나 보내지 않았습니다. 입력한 값은 지웠습니다. 처음부터 다시 하세요. 계속 같으면 관리자에게 알려 주세요.',
  badAnswer: '서버가 예상하지 못한 응답을 했습니다. 아무것도 서명하거나 보내지 않았습니다. 입력한 값은 지웠습니다.',
  noPasskeySession: '패스키로 로그인한 세션에서만 웹 서명을 할 수 있습니다.',
  noSigner: '로그인한 서명자의 활성 패스키를 찾지 못했습니다.',
  signersFailed: '패스키 목록을 불러오지 못했습니다. 잠시 후 다시 시도하세요.',
  connect: '서버에 연결하지 못했습니다. 아무것도 서명하거나 보내지 않았습니다. 입력한 값은 지웠습니다.',
  unknownOutcome: ERROR_TEXT.outcome_unknown,
  guard: '입력한 토큰 값이 아직 전송되지 않았습니다. 이동하지 않았습니다. 이 화면의 "취소"를 눌러 값을 지운 뒤 나가세요.',
  badRef: '이름은 소문자로 시작하고 소문자·숫자·하이픈만 써서 40자 이내로 정확히 적으세요(앞뒤 공백도 안 됩니다).',
  badKind: ERROR_TEXT.bad_kind,
  badExpires: '만료일은 YYYY-MM-DD 형식의 실제 날짜(2000~9999년)이어야 하고, 적지 않으려면 비워 두세요.',
  pastExpires: '만료일이 이미 지났습니다. 오늘(UTC, 이 브라우저의 시계 기준) 이후의 날짜를 쓰거나 비워 두세요.',
  hintHost: 'GitLab·GitHub 토큰은 만료일을 적는 것을 권장합니다(임박하면 화면 위쪽에서 알려 드립니다). 직접 적은 날짜이며 서버가 GitLab·GitHub에 확인하지 않습니다. 날짜는 UTC로 셉니다.',
  hintOther: '비워 두면 만료일 없음으로 저장됩니다. 직접 적은 날짜이며 서버가 확인하지 않습니다. 날짜는 UTC로 셉니다.',
  delName: '삭제할 토큰의 이름을 정확히 입력하세요(대소문자·공백까지 같아야 합니다).',
};

// --- helpers -------------------------------------------------------------------

const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);

// The holder of the token value of the view on screen. The value lives ONLY
// here (never in the view state) and only between the click on "check" and
// the moment the body of the apply request is built.
let current = null;

// heldValueLength: how many characters the holder keeps (a test aid: it shows
// the length, never the value).
export function heldValueLength() {
  return current ? current.value.length : 0;
}

// problemOfFields checks the add/replace form before ANYTHING is requested.
// It returns the fixed sentence, or null. The value is not echoed.
function problemOfFields(kind, ref, expires, value) {
  if (typeof kind !== 'string' || !TOKEN_KINDS.includes(kind)) return TEXT.badKind;
  if (typeof ref !== 'string' || !TOKEN_REF_RE.test(ref)) return TEXT.badRef;
  if ((kind === TOKEN_SINGLETON_KIND) !== (ref === TOKEN_SINGLETON_KIND)) return ERROR_TEXT.openai_singleton;
  if (expires !== '') {
    if (!validExpires(expires)) return TEXT.badExpires;
    if (expires < todayUTC()) return TEXT.pastExpires;
  }
  const code = checkValue(value);
  return code ? ERROR_TEXT[code] : null;
}

// previewProblem: the server's sentences are a bounded list of plain text that
// names the same token, kind and expiry the screen shows.
function previewProblem(sent, pv) {
  if (!Array.isArray(pv) || pv.length === 0 || pv.length > MAX_PREVIEW_LINES) return true;
  if (pv.some((x) => typeof x !== 'string' || x === '' || x.length > MAX_PREVIEW_CHARS || findHostile(x) !== null)) return true;
  if (!pv.some((x) => x.includes("'" + sent.subject + "'"))) return true;
  if (sent.action === ACTION_DELETE_TOKEN) return !pv.some((x) => x.includes('삭제합니다'));
  const exp = sent.fields.expires === '' ? '만료일: 없음' : '만료일: ' + sent.fields.expires;
  return !pv.some((x) => x.includes('(종류: ' + sent.fields.kind + ')')) || !pv.some((x) => x.startsWith(exp));
}

// analyze checks the server's answer to `message` against what the browser asked
// for. It returns the list of problems (empty = the answer says what the screen
// says). `sent` is {action, subject, fields, request, issued}.
async function analyze(sent, resp) {
  if (!isObj(resp)) return ['shape'];
  const problems = [];
  let change;
  try {
    change = await computeChange(sent.action, sent.subject, sent.fields);
  } catch {
    return ['own_fields'];
  }
  if (resp.change !== change) problems.push('change');
  // the message to sign is exactly the canonical text of THIS change and request
  const expected = 'aigate-registry-v1\naction: ' + sent.action + '\nsubject: ' + sent.subject + '\nchange: ' + change +
    '\nrequest: ' + sent.request + '\nissued: ' + sent.issued + '\n';
  let ok = resp.message === expected;
  try {
    const m = checkRegistryMessage(resp.message);
    ok = ok && m.action === sent.action && m.subject === sent.subject && m.change === change && m.request === sent.request && m.issued === sent.issued;
  } catch {
    ok = false;
  }
  if (!ok) problems.push('message');
  if (resp.request !== sent.request || resp.issued !== sent.issued) problems.push('echo');
  if (previewProblem(sent, resp.preview)) problems.push('preview');
  return problems;
}

// --- the view ----------------------------------------------------------------

export function tokensView(ctx) {
  ctx.setCrumb('토큰');
  const held = { value: '' };
  current = held;
  const st = {
    step: 'loading', // loading | list | form | delform | confirm | result
    list: null,
    warn: 14,
    listFailed: null,
    note: null, // {text, error}
    form: null, // persistent controls of the add/replace form
    del: null, // persistent controls of the delete screen
    back: 'list', // where a failed begin/message goes back to
    sent: null,
    resp: null,
    problems: [],
    last4: '',
    hash8: '',
    signer: null,
    signerNote: null,
    busy: false,
    typed: false, // something is typed in the password field
    result: null, // {error, text, pending}
  };
  let signerP = null;
  let listSeq = 0; // list loads: a newer load or an apply makes older answers stale
  let propSeq = 0; // proposals: a cancel or a newer proposal makes older answers stale
  const gone = () => !!(ctx.signal && ctx.signal.aborted);

  function paint() {
    replaceContent(ctx.root, render());
  }

  // ---- the value and the leave guard ----
  function syncGuard() {
    const want = held.value !== '' || st.typed;
    ctx.setLeaveGuard(want ? () => (held.value !== '' || st.typed ? TEXT.guard : false) : null);
  }

  // wipe overwrites the holder and empties the password field.
  function wipe() {
    held.value = '';
    st.last4 = '';
    st.hash8 = '';
    st.typed = false;
    if (st.form && st.form.value) st.form.value.value = '';
    syncGuard();
  }

  ctx.onLeave(() => {
    propSeq++;
    wipe();
    ctx.setLeaveGuard(null);
  });

  // ---- list ----
  async function loadList() {
    const my = ++listSeq;
    try {
      const r = await ctx.get('/api/v1/settings/tokens');
      if (gone() || my !== listSeq) return;
      st.list = isObj(r) && Array.isArray(r.tokens) ? r.tokens.filter(isObj).slice(0, MAX_ROWS) : [];
      st.warn = isObj(r) ? r.warn_days : undefined;
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

  // backToList is the way out of a result: it always shows a freshly read list.
  async function backToList() {
    toList();
    st.step = 'loading';
    paint();
    await loadList();
  }

  // toList drops whatever is in progress: the value, the proposal, the forms.
  function toList() {
    propSeq++; // a proposal still in flight is for an abandoned change
    wipe();
    st.step = 'list';
    st.note = null;
    st.sent = null;
    st.resp = null;
    st.busy = false;
    st.form = null;
    st.del = null;
    paint();
  }

  // ---- add / replace: the form ----
  function hintText(kind) {
    return KINDS_WITH_HOST.includes(kind) ? TEXT.hintHost : TEXT.hintOther;
  }

  function buildForm(pre) {
    const f = { replace: !!pre };
    const kind0 = pre ? pre.kind : 'gitlab';
    f.kind = el('select', { id: 't-kind', 'data-focus-key': 'kind', disabled: !!pre, on: { change: onKind } },
      TOKEN_KINDS.map((k) => el('option', { value: k }, k)));
    f.kind.value = kind0;
    f.ref = el('input', {
      type: 'text', id: 't-ref', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(TOKEN_REF_MAX + 1),
      disabled: !!pre, 'data-focus-key': 'ref',
    });
    f.ref.value = pre ? pre.ref : '';
    if (!pre && kind0 === TOKEN_SINGLETON_KIND) f.ref.value = TOKEN_SINGLETON_KIND;
    f.value = el('input', {
      type: 'password', id: 't-value', class: 'mono', autocomplete: 'off', spellcheck: 'false',
      maxlength: String(TOKEN_VALUE_MAX + 1), 'data-focus-key': 'value',
      on: { input: onTyped },
    });
    f.value.value = '';
    f.expires = el('input', { type: 'text', id: 't-expires', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: '10', placeholder: 'YYYY-MM-DD', 'data-focus-key': 'expires' });
    f.expires.value = '';
    f.hint = el('span', { class: 'hint', 'data-hint': 'expires' }, hintText(kind0));
    f.problem = el('p', { class: ['note', 'err'], role: 'alert', 'data-problem': 'form' });
    f.problem.hidden = true;
    return f;
  }

  function onKind() {
    const f = st.form;
    if (!f) return;
    const k = f.kind.value;
    if (k === TOKEN_SINGLETON_KIND) f.ref.value = TOKEN_SINGLETON_KIND;
    else if (f.ref.value === TOKEN_SINGLETON_KIND) f.ref.value = '';
    clear(f.hint).append(hintText(k));
  }

  // onTyped only notes THAT something is typed (for the leave guard); the value
  // is read once, when the person asks for the confirmation.
  function onTyped() {
    const f = st.form;
    st.typed = !!f && typeof f.value.value === 'string' && f.value.value.length > 0;
    syncGuard();
  }

  function formProblem(box, text) {
    clear(box);
    if (text) box.append(text);
    box.hidden = !text;
  }

  function openForm(pre) {
    if (st.busy) return;
    st.note = null;
    st.form = buildForm(pre);
    st.back = 'form';
    st.step = 'form';
    paint();
  }

  function submitForm() {
    const f = st.form;
    if (!f || st.busy) return;
    const kind = f.kind.value;
    const ref = f.ref.value;
    const expires = f.expires.value;
    const value = f.value.value; // the one place the value is read
    const problem = problemOfFields(kind, ref, expires, value);
    if (problem) {
      formProblem(f.problem, problem); // nothing is requested; the value stays in the field only
      return;
    }
    formProblem(f.problem, '');
    held.value = value;
    f.value.value = ''; // from here on the value is in the holder only
    st.typed = false;
    syncGuard();
    propose(ACTION_SET_TOKEN, ref, { kind, expires }, true);
  }

  // ---- delete: the name screen ----
  function openDelete(ref) {
    if (st.busy) return;
    st.note = null;
    const d = { ref };
    d.input = el('input', { type: 'text', id: 't-confirm-ref', class: 'mono', autocomplete: 'off', spellcheck: 'false', maxlength: String(TOKEN_REF_MAX + 1), 'data-focus-key': 'confirm-ref' });
    d.input.value = '';
    d.problem = el('p', { class: ['note', 'err'], role: 'alert', 'data-problem': 'form' });
    d.problem.hidden = true;
    st.del = d;
    st.back = 'delform';
    st.step = 'delform';
    paint();
  }

  function submitDelete() {
    const d = st.del;
    if (!d || st.busy) return;
    if (d.input.value !== d.ref) {
      formProblem(d.problem, TEXT.delName);
      return;
    }
    formProblem(d.problem, '');
    propose(ACTION_DELETE_TOKEN, d.ref, {}, false);
  }

  // ---- begin -> message -> confirmation ----
  // fail: a refusal before anything was signed: the value is wiped and the
  // person is back on the form (or the name screen) with a fixed sentence.
  function fail(text, my) {
    if (gone() || my !== propSeq) return;
    wipe();
    st.busy = false;
    st.step = st.back === 'delform' && st.del ? 'delform' : st.form ? 'form' : 'list';
    const box = st.step === 'delform' ? st.del.problem : st.step === 'form' ? st.form.problem : null;
    if (box) formProblem(box, text);
    else st.note = { error: true, text };
    paint();
  }

  async function propose(action, subject, beginFields, withValue) {
    if (st.busy) return;
    st.busy = true;
    st.note = null;
    const my = ++propSeq;
    paint();
    let b;
    try {
      b = await ctx.post('/api/v1/tokens/begin', { action, subject, fields: beginFields }, WRITE_OPTS);
    } catch (err) {
      return fail(err && err.status === 0 ? TEXT.connect : failureText(err), my);
    }
    if (gone() || my !== propSeq) return;
    if (!isObj(b) || typeof b.request !== 'string' || !REQUEST_ID_RE.test(b.request) || typeof b.issued !== 'string' || !ISSUED_RE.test(b.issued)) {
      return fail(TEXT.badAnswer, my);
    }
    const fields = { ...beginFields };
    if (withValue) {
      try {
        fields.value_hash = await computeValueHash(b.request, held.value);
      } catch {
        return fail(TEXT.badAnswer, my);
      }
      if (gone() || my !== propSeq) return;
    }
    const sent = { action, subject, fields, request: b.request, issued: b.issued };
    let resp;
    try {
      resp = await ctx.post('/api/v1/tokens/message', { action, subject, fields, request: b.request, issued: b.issued }, WRITE_OPTS);
    } catch (err) {
      return fail(err && err.status === 0 ? TEXT.connect : failureText(err), my);
    }
    if (gone() || my !== propSeq) return;
    const problems = await analyze(sent, resp);
    if (gone() || my !== propSeq) return;
    st.problems = problems;
    st.sent = sent;
    st.resp = resp;
    st.signer = null;
    st.signerNote = null;
    st.busy = false;
    st.step = 'confirm';
    if (problems.length > 0 || !withValue) {
      // A proposal that can not be signed (or a delete) never needs the value.
      st.last4 = '';
      if (problems.length > 0) wipe();
    } else {
      st.last4 = held.value.slice(-4);
      st.hash8 = fields.value_hash.slice(0, 8);
    }
    if (st.form && st.form.value) st.form.value.value = '';
    paint();
    if (problems.length === 0) await resolveSignerOnce(sent);
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
    // The user left or cancelled while the passkey prompt was open: send nothing.
    if (gone() || st.resp !== cur.resp) return;
    const body = {
      action: cur.sent.action, subject: cur.sent.subject, fields: cur.sent.fields,
      change: cur.resp.change, request: cur.resp.request, issued: cur.resp.issued, signature,
    };
    if (cur.sent.action === ACTION_SET_TOKEN) body.value = held.value; // the value goes into this body and nowhere else
    held.value = '';
    st.last4 = '';
    st.hash8 = '';
    syncGuard(); // the value has left the holder: the person is free to go
    // From here the change is spent: whatever the answer, it is never signed or sent again.
    listSeq++; // a list load that started before this change must not paint over the next one
    st.step = 'result';
    st.note = null;
    st.result = { error: false, text: '서버로 변경을 보내는 중…', pending: true };
    paint();
    let answer;
    try {
      answer = await ctx.post('/api/v1/tokens/apply', body, WRITE_OPTS);
    } catch (err) {
      body.value = '';
      if (gone()) return;
      const unknown = err && err.status === 0;
      st.busy = false;
      st.result = { error: true, text: unknown ? TEXT.unknownOutcome : failureText(err) };
      paint();
      // Every fixed text that tells the user to check the list also reads it again.
      if (unknown || checkList(err)) await loadList();
      return;
    }
    body.value = '';
    if (gone()) return;
    st.busy = false;
    const isSet = cur.sent.action === ACTION_SET_TOKEN;
    const okAnswer = isSet
      ? isObj(answer) && answer.ref === cur.sent.subject && answer.kind === cur.sent.fields.kind
      : isObj(answer) && answer.ref === cur.sent.subject && answer.deleted === true;
    if (!okAnswer) {
      // The server said ok but not about this token: the outcome is not known.
      st.result = { error: true, text: TEXT.unknownOutcome };
      paint();
      await loadList();
      return;
    }
    st.result = { error: false, text: isSet ? '토큰을 저장했습니다.' : '토큰을 삭제했습니다.' };
    paint();
    await loadList();
  }

  // ---- rendering ----
  function noteNode() {
    return st.note ? el('p', { class: st.note.error ? ['note', 'err'] : ['note', 'ok'], role: st.note.error ? 'alert' : 'status' }, st.note.text) : null;
  }

  function render() {
    switch (st.step) {
      case 'form':
        return renderForm();
      case 'delform':
        return renderDelete();
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

  function daysCell(r) {
    const d = r.days_left;
    if (!Number.isInteger(d)) return '-';
    const w = Number.isInteger(st.warn) ? st.warn : 14;
    if (d < 0) return el('span', { class: ['chip', 'c-fail'] }, -d + '일 지남');
    return el('span', d <= w ? { class: ['chip', 'c-wait'] } : {}, d + '일');
  }

  function renderList() {
    if (st.list === null) {
      return el('div', { class: ['panel', 'canvas'] }, [
        el('h1', { id: 'h-tokens', 'data-focus-key': 'err' }, '불러오지 못했습니다'),
        el('p', { class: 'lead', role: 'alert' }, failureText(st.listFailed)),
        el('p', {}, [el('button', { type: 'button', class: 'btn', 'data-focus-key': 'retry', on: { click: reload } }, '다시 시도'), ' ', el('a', { href: '#/overview' }, '← 개요')]),
      ]);
    }
    const rows = st.list.map((r, i) => tokenRow(r, i));
    const table = el('div', { class: 'tscroll' }, [
      el('table', { class: 'ptable' }, [
        el('caption', { class: 'sr' }, '저장된 토큰'),
        el('thead', {}, [el('tr', {}, [
          el('th', { scope: 'col' }, '이름'), el('th', { scope: 'col' }, '종류'), el('th', { scope: 'col' }, '마지막 4자'),
          el('th', { scope: 'col' }, '만료일'), el('th', { scope: 'col' }, '남은 일수'), el('th', { scope: 'col' }, '출처'),
          el('th', { scope: 'col' }, '변경'),
        ])]),
        el('tbody', {}, rows),
      ]),
    ]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-tokens' }, '토큰'),
      expiryBanner(st.list, st.warn),
      el('p', { class: 'lead' }, st.list.length === 0 ? '저장된 토큰이 없습니다.'
        : '토큰 ' + st.list.length + '개. 토큰 값은 한 번 저장하면 다시 볼 수 없습니다. 추가·교체·삭제는 패스키 서명이 필요합니다.'),
      el('div', { class: 'row' }, [
        el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'add', on: { click: () => openForm(null) } }, '토큰 추가'),
        ' ',
        el('button', { type: 'button', class: 'btn', 'data-focus-key': 'reload', disabled: st.busy, on: { click: reload } }, '목록 새로고침'),
      ]),
      noteNode(),
      st.list.length > 0 ? el('div', { class: 'card' }, [table]) : null,
      el('p', { class: 'note' }, '남은 일수는 UTC 날짜로 셉니다. 만료일은 사용자가 직접 적은 날짜이며 서버가 GitLab·GitHub에 확인하지 않습니다. 설정 파일의 키는 여기서 지울 수 없고, 같은 이름의 저장된 키로 교체할 수 있습니다.'),
    ]);
  }

  async function reload() {
    if (st.busy) return;
    st.note = null;
    await loadList();
  }

  function tokenRow(r, i) {
    const refOk = typeof r.ref === 'string' && TOKEN_REF_RE.test(r.ref);
    const kindOk = typeof r.kind === 'string' && TOKEN_KINDS.includes(r.kind);
    const source = r.source === 'store' ? '저장소' : r.source === 'config' ? '설정 파일' : inline(r.source, 20);
    const acts = [];
    if (refOk && kindOk) {
      acts.push(el('button', {
        type: 'button', class: 'btn', 'data-focus-key': 'replace-' + i,
        on: { click: () => openForm({ ref: r.ref, kind: r.kind }) },
      }, ['교체', el('span', { class: 'sr' }, ' (' + inline(r.ref, 64) + ')')]));
      if (r.source === 'store') {
        acts.push(' ', el('button', {
          type: 'button', class: 'btn', 'data-focus-key': 'delete-' + i,
          on: { click: () => openDelete(r.ref) },
        }, ['삭제', el('span', { class: 'sr' }, ' (' + inline(r.ref, 64) + ')')]));
      }
    } else {
      acts.push(el('span', { class: 'note' }, '-'));
    }
    return el('tr', {}, [
      el('td', { class: 'mono', 'data-label': '이름' }, [inline(r.ref, 64), r.usable === false ? [' ', el('span', { class: ['chip', 'c-fail'] }, '사용 불가')] : null]),
      el('td', { class: 'mono', 'data-label': '종류' }, inline(r.kind, 32)),
      el('td', { class: 'mono', 'data-label': '마지막 4자' }, typeof r.last4 === 'string' && r.last4 !== '' ? inline(r.last4, 8) : '-'),
      el('td', { class: 'mono', 'data-label': '만료일' }, typeof r.expires === 'string' && r.expires !== '' ? inline(r.expires, 16) : '없음'),
      el('td', { 'data-label': '남은 일수' }, daysCell(r)),
      el('td', { 'data-label': '출처' }, source),
      el('td', {}, acts),
    ]);
  }

  function renderForm() {
    const f = st.form;
    const field = (labelText, id, control, hint) => el('div', { class: 'field' }, [
      el('label', { for: id }, labelText), control, hint || null,
    ]);
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-tokens' }, f.replace ? '토큰 교체' : '토큰 추가'),
      el('p', { class: 'lead' }, '값은 이 화면에서만 입력하고, 다음 화면에서 바뀔 내용을 확인한 뒤 패스키로 서명합니다. 서명에는 값의 해시만 묶이고, 값은 마지막 전송 한 번에만 실립니다.'),
      el('div', { class: 'form' }, [
        field('종류', 't-kind', f.kind, f.replace ? el('span', { class: 'hint' }, '교체할 때는 종류와 이름을 바꿀 수 없습니다.') : null),
        field('이름', 't-ref', f.ref, el('span', { class: 'hint' }, '소문자로 시작, 소문자·숫자·하이픈, 40자 이내. openai 종류는 이름이 openai 하나뿐입니다.')),
        field('토큰 값', 't-value', f.value, el('span', { class: 'hint' }, '32~512자의 인쇄 가능한 ASCII 문자, 공백 없음. 붙여넣어도 됩니다. 입력한 값은 가려지고, 확인 화면에는 마지막 4자만 보입니다.')),
        field('만료일 (선택)', 't-expires', f.expires, f.hint),
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

  function renderDelete() {
    const d = st.del;
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-tokens' }, '토큰 삭제'),
      el('p', { class: 'lead' }, '삭제하면 저장된 값이 지워집니다. 삭제할 토큰의 이름을 그대로 입력하면 다음 화면에서 패스키로 서명합니다.'),
      el('div', { class: 'form' }, [
        el('p', {}, ['삭제할 토큰: ', el('span', { class: 'mono' }, d.ref)]),
        el('div', { class: 'field' }, [el('label', { for: 't-confirm-ref' }, '이름 확인'), d.input, el('span', { class: 'hint' }, '위 이름과 똑같이 입력하세요.')]),
        d.problem,
        noteNode(),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm', disabled: st.busy, on: { click: submitDelete } }, '삭제 내용 확인'),
          ' ',
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'cancel', on: { click: toList } }, '취소'),
        ]),
      ]),
    ]);
  }

  function renderConfirm() {
    const s = st.sent;
    const li = (k, v, field) => el('li', { 'data-field': field }, [el('b', {}, k), ': ', el('span', { class: 'mono' }, v)]);
    const isSet = s.action === ACTION_SET_TOKEN;
    const facts = [];
    if (isSet) {
      facts.push(li('종류', s.fields.kind, 'kind'), li('이름', s.subject, 'ref'),
        li('만료일', s.fields.expires === '' ? '없음 (만료일을 적지 않음)' : s.fields.expires, 'expires'));
      if (st.problems.length === 0) facts.push(li('값의 마지막 4자', st.last4, 'last4'), li('값 해시의 앞 8자', st.hash8, 'hash'));
    } else {
      facts.push(li('삭제할 토큰', s.subject, 'ref'));
    }
    let controls;
    if (st.problems.length > 0) {
      controls = el('div', { class: 'trunc', role: 'alert', 'data-warn': 'mismatch' }, TEXT.mismatch);
    } else {
      controls = el('div', { class: 'confirm' }, [
        st.signerNote ? el('p', { class: 'note' }, st.signerNote) : null,
        st.signer && supported()
          ? el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'sign', disabled: st.busy, on: { click: onSign } }, '패스키로 서명하고 적용')
          : null,
      ]);
    }
    const msgText = st.problems.length === 0 ? st.resp.message : '';
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-tokens' }, '변경 내용 확인'),
      el('p', { class: 'lead' }, '아래 내용이 전부입니다. 서명하면 바로 적용됩니다.'),
      el('ul', { class: 'pfacts', 'aria-label': '바뀌는 내용' }, facts),
      isSet && st.problems.length === 0
        ? el('p', { class: 'note' }, '토큰 값 전체는 이 화면에 다시 표시되지 않습니다. 위의 마지막 4자와 해시 앞 8자로 방금 입력한 값이 맞는지만 확인하세요.')
        : null,
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
          el('p', { class: 'note' }, '이 화면은 위 내용으로 변경의 해시와 메시지를 직접 다시 만들어 서버가 보낸 것과 같을 때만 서명 단추를 보여 줍니다. 5분 안에 서명해야 합니다.'),
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
      el('h1', { id: 'h-tokens' }, r.pending ? '적용하는 중' : r.error ? '변경하지 못했습니다' : '완료'),
      el('p', { class: r.error ? ['note', 'err'] : ['note', 'ok'], role: r.error ? 'alert' : 'status', 'data-result': r.error ? 'error' : r.pending ? 'pending' : 'ok' }, r.text),
      r.pending ? null : el('p', {}, [el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'back', on: { click: backToList } }, '목록으로')]),
    ]);
  }

  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  // One read per visit and after each change: the page never polls, so a
  // refresh can not wipe a confirmation.
  return loadList();
}

registerView('tokens', tokensView);
