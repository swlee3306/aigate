// new.js: the submit screen (#/new). The four requirement fields are put into
// a FIXED English template by the browser (no AI), the final prompt is shown
// exactly as it will be sent, and the submit carries a random Idempotency-Key
// so a double click, Enter spam or a retry after a network error never makes
// two tasks. Repositories, their HEADs and the class list come from the
// server (GET /repos, GET /policy); nothing else can be submitted.

import {
  el, clear, inline, idOf, errorPane, findHostile, sanitize,
} from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';

// MaxPromptBytes of the server (api.MaxPromptBytes): UTF-8 bytes, headers included.
export const PROMPT_MAX_BYTES = 64 * 1024;

// The template headers are fixed English text; the agent prompt format does
// not depend on the UI language.
const FIELDS = [
  { key: 'goal', header: 'Goal:', label: '목표', id: 'f-goal', hint: '무엇을 이루려는지 한두 문장으로 적습니다.' },
  { key: 'scope', header: 'Scope:', label: '수정 범위', id: 'f-scope', hint: '고치거나 추가할 파일·디렉터리.' },
  { key: 'done', header: 'Done when:', label: '완료 조건', id: 'f-done', hint: '끝났다고 볼 검증 기준(테스트 통과 등).' },
  { key: 'avoid', header: 'Do not:', label: '하지 말 것', id: 'f-avoid', hint: '건드리면 안 되는 것, 피할 방식.' },
];

const AGENTS = ['claude', 'codex'];
// The same shapes the server enforces (api.go classRe, baseRe); the server
// stays the authority, these only stop an obviously refused submit.
const CLASS_RE = /^[a-z0-9_가-힣-]{1,32}$/;
const BASE_RE = /^[A-Za-z0-9][A-Za-z0-9._/~^@{}+-]{0,199}$/;
const KEY_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
const KEY_LEN = 32;

export class PromptError extends Error {
  constructor(code, message, extra) {
    super(message);
    this.name = 'PromptError';
    this.code = code; // 'empty' | 'hostile' | 'too_long'
    Object.assign(this, extra || {});
  }
}

const utf8Bytes = (s) => new TextEncoder().encode(s).length;

// assemblePrompt builds the prompt from the four fields: each non-empty field
// (trimmed) becomes `<header> <text>` on its own line, in the order Goal,
// Scope, Done when, Do not; empty fields are omitted. Control, bidi and
// invisible characters (newline and tab excepted) are REJECTED, never
// stripped, so the text the user sees is the text that is sent. The result
// must be non-empty and at most PROMPT_MAX_BYTES UTF-8 bytes.
export function assemblePrompt(fields) {
  fields = fields && typeof fields === 'object' ? fields : {};
  const lines = [];
  for (const f of FIELDS) {
    const raw = typeof fields[f.key] === 'string' ? fields[f.key] : '';
    const bad = findHostile(raw, { allowNewline: true, allowTab: true });
    if (bad) {
      throw new PromptError('hostile', '「' + f.label + '」에 보이지 않는 문자나 제어 문자가 있습니다 (' + bad + '). 지우고 다시 입력하세요.', { field: f.key, char: bad });
    }
    const text = raw.trim();
    if (text !== '') lines.push(f.header + ' ' + text);
  }
  if (lines.length === 0) throw new PromptError('empty', '요구사항을 한 칸 이상 입력하세요.');
  const prompt = lines.join('\n');
  const bytes = utf8Bytes(prompt);
  if (bytes > PROMPT_MAX_BYTES) {
    throw new PromptError('too_long', '요구사항이 64 KiB(' + PROMPT_MAX_BYTES + '바이트)를 넘습니다 (현재 ' + bytes + '바이트). 줄여 주세요.', { bytes });
  }
  return prompt;
}

// newIdempotencyKey: 32 characters of [A-Za-z0-9] from the browser's CSPRNG.
// Bytes >= 248 are redrawn so every character is equally likely (no modulo bias).
export function newIdempotencyKey() {
  const c = globalThis.crypto;
  if (!c || typeof c.getRandomValues !== 'function') throw new Error('crypto.getRandomValues is not available');
  let out = '';
  while (out.length < KEY_LEN) {
    const buf = c.getRandomValues(new Uint8Array(KEY_LEN));
    for (let i = 0; i < buf.length && out.length < KEY_LEN; i++) {
      if (buf[i] < 248) out += KEY_ALPHABET[buf[i] % 62];
    }
  }
  return out;
}

// Fixed texts by error code: the server's message is never shown, with one
// exception: queue_full, whose two fixed sentences (my queue is full / the
// server queue is full) the server owns; they are shown as sanitized text
// before the fixed advice, and the fixed text below is the fallback.
const QUEUE_FULL_ADVICE = '앞의 작업이 진행된 뒤 다시 제출하세요. 같은 내용으로 다시 눌러도 중복되지 않습니다.';
const SUBMIT_ERRORS = {
  unknown_class: '정책에 없는 작업 종류입니다. 종류를 다시 고르세요.',
  queue_full: '대기 중인 작업이 가득 찼습니다(queue full). 앞의 작업이 진행된 뒤 다시 제출하세요. 같은 내용으로 다시 눌러도 중복되지 않습니다.',
  bad_request: '요청 형식이 올바르지 않습니다. 저장소, 기준 커밋, 종류, 요구사항을 확인하세요.',
  idempotency_conflict: '같은 제출 키로 다른 내용이 이미 처리되었습니다. 이 화면을 새로 열어 다시 제출하세요.',
  body_too_large: '요구사항이 너무 큽니다(64 KiB 초과). 줄여서 다시 제출하세요.',
  csrf_failed: '보안 확인에 실패했습니다. 다시 로그인한 뒤 제출하세요.',
  unauthenticated: '로그인이 필요합니다.',
};

const QUEUE_FULL_MESSAGE_MAX = 80;

export function submitErrorText(err) {
  const st = err && typeof err.status === 'number' ? err.status : 0;
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (code === 'queue_full' && typeof err.message === 'string') {
    const m = sanitize(err.message).trim().slice(0, QUEUE_FULL_MESSAGE_MAX);
    if (m) return m + '. ' + QUEUE_FULL_ADVICE;
  }
  if (Object.prototype.hasOwnProperty.call(SUBMIT_ERRORS, code)) return SUBMIT_ERRORS[code];
  if (st === 413) return SUBMIT_ERRORS.body_too_large;
  if (st === 429) return '요청이 많아 잠시 쉬는 중입니다. 잠시 후 같은 내용으로 다시 제출하세요.';
  if (st === 0) return '서버에 연결하지 못했습니다. 같은 내용으로 다시 눌러도 중복 제출되지 않습니다.';
  return '서버에서 오류가 발생했습니다. 잠시 후 다시 시도하세요.';
}

function defaultNavigate(hash) {
  globalThis.location.hash = hash;
}

// usable list entries: shape-checked, repos need a string path.
// REPO_LABEL_MAX: the longest repository label in the select. Paths that
// share a long prefix differ at the END, so a long path keeps its head and
// its tail and drops the middle (two different paths never look the same
// because of a cut at the end).
export const REPO_LABEL_MAX = 120;
export function repoLabel(path, max) {
  max = max || REPO_LABEL_MAX;
  const cps = Array.from(sanitize(path)); // code points: never split a pair
  if (cps.length <= max) return cps.join('');
  const head = Math.floor((max - 1) / 3);
  const tail = max - 1 - head;
  return cps.slice(0, head).join('') + '…' + cps.slice(cps.length - tail).join('');
}

function cleanRepos(list) {
  return (Array.isArray(list) ? list : []).filter((r) => r && typeof r === 'object' && typeof r.path === 'string' && r.path !== '');
}

// classChoices: the keys of policy.classes that are valid class names,
// sorted; empty means "free text" (the server accepts any regex-valid class).
function classChoices(policy) {
  const c = policy && typeof policy === 'object' && policy.classes && typeof policy.classes === 'object' && !Array.isArray(policy.classes)
    ? policy.classes : {};
  return Object.keys(c).filter((k) => CLASS_RE.test(k)).sort();
}

const note = (t, cls) => el('p', { class: cls ? ['note', cls] : 'note' }, t);

// renderNewForm builds the form and wires it. env: {repos, policy, post,
// navigate, signal}. All state lives in this closure.
function renderNewForm(env) {
  const repos = cleanRepos(env.repos);
  const classes = classChoices(env.policy);
  const st = {
    busy: false,
    done: false,
    shown: null, // the prompt currently displayed (string) or null
    problem: null, // why the form can not be submitted right now
    keys: new Map(), // content fingerprint -> idempotency key (cleared on success)
    msg: null, // result of the last submit {text, kind}
  };
  const h = {};

  const repoByPath = (p) => repos.find((r) => r.path === p) || null;
  const headOf = (r) => (r && typeof r.head === 'string' && /^[0-9a-f]{7,64}$/.test(r.head) ? r.head : '');

  // --- controls ---
  h.repo = el('select', { id: 'f-repo', 'data-focus-key': 'repo', on: { change: () => { setBaseFromRepo(); recompute(); } } },
    repos.map((r) => el('option', { value: r.path }, repoLabel(r.path))));
  h.repo.value = repos[0].path;
  h.base = el('input', {
    type: 'text', id: 'f-base', autocomplete: 'off', spellcheck: 'false', maxlength: '200', class: 'mono',
    'data-focus-key': 'base', on: { input: () => recompute(), change: () => recompute() },
  });
  h.baseChips = el('div', { class: 'row chips', 'aria-label': '기준 커밋 빠른 선택' });
  if (classes.length > 0) {
    h.cls = el('select', { id: 'f-class', 'data-focus-key': 'class', on: { change: () => recompute() } },
      classes.map((c) => el('option', { value: c }, c)));
    h.cls.value = classes[0];
  } else {
    h.cls = el('input', {
      type: 'text', id: 'f-class', autocomplete: 'off', spellcheck: 'false', maxlength: '32', placeholder: '예: docs',
      'data-focus-key': 'class', on: { input: () => recompute(), change: () => recompute() },
    });
    h.cls.value = '';
  }
  h.agent = el('select', { id: 'f-agent', 'data-focus-key': 'agent', on: { change: () => recompute() } },
    AGENTS.map((a) => el('option', { value: a }, a)));
  h.agent.value = AGENTS[0];
  h.fields = {};
  for (const f of FIELDS) {
    h.fields[f.key] = el('textarea', {
      id: f.id, 'data-focus-key': f.id, autocomplete: 'off', spellcheck: 'false',
      'aria-describedby': f.id + '-hint', on: { input: () => recompute(), change: () => recompute() },
    });
    h.fields[f.key].value = '';
  }
  h.preview = el('pre', { id: 'f-preview', class: 'block-pre', 'aria-label': '전송될 프롬프트' });
  h.count = el('p', { class: 'note', id: 'f-count' });
  // A polite live region, rewritten only when the message changes: an
  // alert refilled on every keystroke would interrupt a screen reader each time.
  h.problem = el('p', { class: ['note', 'err'], 'aria-live': 'polite', id: 'f-problem' });
  h.result = el('div', { class: 'form-result', role: 'status' });
  h.submit = el('button', { type: 'button', class: ['btn', 'primary'], id: 'f-submit', 'data-focus-key': 'submit', on: { click: () => onSubmit() } }, '제출');

  // --- state derivation ---
  function setBaseFromRepo() {
    const r = repoByPath(h.repo.value);
    h.base.value = headOf(r);
    paintChips(r);
  }
  function paintChips(r) {
    clear(h.baseChips);
    const head = headOf(r);
    const chips = [];
    if (head) {
      chips.push(el('button', { type: 'button', class: 'btn', 'data-base': 'HEAD', 'data-focus-key': 'chip-head',
        on: { click: () => { h.base.value = head; recompute(); } } }, '현재 HEAD ' + head.slice(0, 12)));
    }
    const branches = r && Array.isArray(r.branches) ? r.branches : [];
    let n = 0;
    for (const b of branches) {
      if (typeof b !== 'string' || !BASE_RE.test(b) || n >= 10) continue;
      n++;
      chips.push(el('button', { type: 'button', class: 'btn', 'data-base': b, 'data-focus-key': 'chip-' + n,
        on: { click: () => { h.base.value = b; recompute(); } } }, b));
    }
    chips.forEach((c) => h.baseChips.appendChild(c));
  }

  // current reads the form into a request body, or the reason it can not be sent.
  function current() {
    const repo = repoByPath(h.repo.value);
    if (!repo) return { problem: '저장소 목록에 없는 값입니다. 목록에서 고르세요.' };
    const base = typeof h.base.value === 'string' ? h.base.value.trim() : '';
    if (base === '') return { problem: '이 저장소에는 아직 커밋이 없어 기준 커밋을 정할 수 없습니다.' };
    if (!BASE_RE.test(base)) return { problem: '기준 커밋 형식이 올바르지 않습니다. 영문자·숫자와 . _ / ~ ^ @ { } + - 만 쓸 수 있습니다.' };
    const cls = typeof h.cls.value === 'string' ? h.cls.value : '';
    if (classes.length > 0 ? !classes.includes(cls) : !CLASS_RE.test(cls)) {
      return { problem: classes.length > 0 ? '정책에 없는 작업 종류입니다. 목록에서 고르세요.' : '종류는 소문자·숫자·밑줄·하이픈·한글 1~32자로 적으세요.' };
    }
    const agent = h.agent.value;
    if (!AGENTS.includes(agent)) return { problem: '에이전트는 claude 또는 codex 중에서 고르세요.' };
    const input = {};
    for (const f of FIELDS) input[f.key] = h.fields[f.key].value;
    let prompt;
    try {
      prompt = assemblePrompt(input);
    } catch (err) {
      if (err instanceof PromptError) return { problem: err.message, empty: err.code === 'empty', bytes: err.bytes };
      throw err;
    }
    return { body: { repo: repo.path, base, prompt, class: cls, agent }, prompt };
  }

  // recompute refreshes the preview, the byte count and the button. The
  // preview text is set from the SAME string that submit sends.
  function recompute() {
    const cur = current();
    st.problem = cur.problem || null;
    st.shown = cur.prompt === undefined ? null : cur.prompt;
    clear(h.preview);
    if (st.shown !== null) h.preview.appendChild(globalThis.document.createTextNode(st.shown));
    h.preview.hidden = st.shown === null;
    clear(h.count);
    if (st.shown !== null) h.count.appendChild(globalThis.document.createTextNode(utf8Bytes(st.shown) + ' / ' + PROMPT_MAX_BYTES + ' 바이트'));
    const showProblem = st.problem && !(cur.empty && !touched());
    const want = showProblem ? st.problem : '';
    if (want !== h.problem.textContent) {
      clear(h.problem);
      if (want) h.problem.appendChild(globalThis.document.createTextNode(want));
    }
    h.problem.hidden = !showProblem;
    syncButton();
    return cur;
  }
  const touched = () => FIELDS.some((f) => h.fields[f.key].value !== '');
  // aria-disabled (not disabled) like the task page: a real disabled attribute
  // would throw the keyboard focus off the button the user just pressed.
  // onSubmit guards the same conditions, so a click on it does nothing.
  const blocked = () => st.busy || st.done || !!st.problem;
  function syncButton() {
    h.submit.setAttribute('aria-disabled', blocked() ? 'true' : 'false');
    h.submit.setAttribute('aria-busy', st.busy ? 'true' : 'false');
  }
  function showResult(text, kind, extra) {
    clear(h.result);
    if (!text) return;
    h.result.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    h.result.appendChild(el('p', { class: ['note', kind === 'error' ? 'err' : 'ok'], 'data-focus-key': 'result' }, text));
    if (extra) h.result.appendChild(extra);
  }

  const fingerprint = (b) => JSON.stringify([b.repo, b.base, b.prompt, b.class, b.agent]);

  async function onSubmit() {
    if (st.busy || st.done) return; // double click, Enter spam: one request
    // st.problem is re-derived below (recompute): a stale problem must not
    // block a form the user has just fixed without an input event.
    const before = st.shown;
    const cur = recompute();
    if (cur.problem) return;
    if (cur.prompt !== before) return; // the user has not seen this text yet: show it first
    const body = cur.body;
    const fp = fingerprint(body);
    // The same content keeps its key (a retry after queue_full or a network
    // error is the same submit); changed content is a different submit.
    let key = st.keys.get(fp);
    if (!key) {
      key = newIdempotencyKey();
      st.keys.set(fp, key);
    }
    st.busy = true;
    syncButton();
    showResult('제출하는 중…', 'info');
    try {
      const task = await env.post('/api/v1/tasks', body, { headers: { 'Idempotency-Key': key } });
      if (env.signal && env.signal.aborted) return;
      const id = idOf(task && typeof task === 'object' ? task.id : null);
      st.busy = false;
      st.keys.clear();
      if (!id) {
        st.done = true; // a task may exist: do not offer a second submit
        syncButton();
        showResult('작업이 접수됐지만 작업 번호를 확인할 수 없습니다. 작업 목록에서 확인하세요.', 'error',
          el('a', { href: '#/tasks' }, '작업 목록으로 →'));
        return;
      }
      st.done = true;
      syncButton();
      showResult('작업 #' + id + '을(를) 접수했습니다. 상세 화면으로 이동합니다.', 'ok');
      env.navigate('#/tasks/' + id);
    } catch (err) {
      if (env.signal && env.signal.aborted) return;
      st.busy = false;
      recompute();
      showResult(submitErrorText(err), 'error');
    }
  }

  // --- layout ---
  const labeled = (id, label, control, hint) => el('div', { class: 'field' }, [
    el('label', { for: id }, label),
    control,
    hint ? el('small', { class: 'hint', id: id + '-hint' }, hint) : null,
  ]);
  const root = el('div', { class: 'ws' }, [
    el('div', { class: ['panel', 'canvas', 'form'] }, [
      el('h1', { id: 'h-new' }, '새 작업'),
      el('p', { class: 'lead' }, '요구사항을 네 칸에 나누어 적으면 고정 템플릿으로 프롬프트를 만듭니다. 아래 미리보기가 그대로 전송됩니다.'),
      el('div', { class: 'sec' }, [
        el('h2', {}, '어디에'),
        labeled('f-repo', '저장소', h.repo),
        el('div', { class: 'field' }, [
          el('label', { for: 'f-base' }, '기준 커밋(base)'),
          h.base,
          h.baseChips,
          el('small', { class: 'hint' }, '기본값은 저장소의 현재 HEAD입니다. 직접 입력하거나 브랜치를 고를 수 있습니다.'),
        ]),
        el('div', { class: 'row fields2' }, [
          labeled('f-class', '종류', h.cls, classes.length > 0 ? '정책에 있는 종류만 고를 수 있습니다.' : '정책에 종류 목록이 없어 직접 적습니다.'),
          labeled('f-agent', '에이전트', h.agent),
        ]),
      ]),
      el('div', { class: 'sec' }, [
        el('h2', {}, '무엇을'),
        ...FIELDS.map((f) => labeled(f.id, f.label, h.fields[f.key], f.hint)),
      ]),
      el('div', { class: 'sec' }, [
        el('h2', {}, '전송될 프롬프트'),
        h.preview,
        h.count,
        h.problem,
        el('div', { class: 'row' }, [h.submit, el('a', { class: 'btn', href: '#/tasks' }, '취소')]),
        h.result,
      ]),
    ]),
    el('aside', { class: ['panel', 'insp'], 'aria-label': '안내' }, [
      el('h2', {}, '안내'),
      el('ul', { class: 'rows' }, [
        el('li', {}, [el('span', {}, '프롬프트 상한'), el('span', { class: 'mono' }, '64 KiB')]),
        el('li', {}, [el('span', {}, '중복 제출'), el('span', {}, '같은 내용은 한 번만')]),
      ]),
      el('p', { class: 'note' }, '제출하면 작업이 대기열에 들어가고, 승인이 필요한 변경은 승인 화면에서 사람이 확인합니다.'),
    ]),
  ]);
  setBaseFromRepo();
  recompute();
  return root;
}

export async function newView(ctx) {
  ctx.setCrumb('새 작업');
  const navigate = ctx.navigate || defaultNavigate;
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  let busy = false;
  async function load() {
    if (busy) return;
    busy = true;
    try {
      const [repos, policy] = await Promise.all([ctx.get('/api/v1/repos'), ctx.get('/api/v1/policy')]);
      if (ctx.signal && ctx.signal.aborted) return;
      if (cleanRepos(repos).length === 0) {
        replaceContent(ctx.root, el('div', { class: ['panel', 'canvas'] }, [
          el('h1', { id: 'h-new' }, '새 작업'),
          el('p', { class: 'lead', role: 'status' }, '제출할 수 있는 저장소가 없습니다. 서버의 허용 경로 아래에 git 저장소가 있는지 확인하세요.'),
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'retry', on: { click: load } }, '다시 불러오기'),
        ]));
        return;
      }
      replaceContent(ctx.root, renderNewForm({ repos, policy, post: ctx.post, navigate, signal: ctx.signal }));
    } catch (err) {
      if (ctx.signal && ctx.signal.aborted) return;
      const pane = errorPane(err, '#/tasks', '← 작업 목록');
      pane.appendChild(el('button', { type: 'button', class: 'btn', 'data-focus-key': 'retry', on: { click: load } }, '다시 불러오기'));
      replaceContent(ctx.root, pane);
    } finally {
      busy = false;
    }
  }
  await load();
}

registerView('new', newView);
