// render.js: safe DOM construction, the stage menu, the overview and the
// polling helper. Nothing here parses HTML: every string from the server (or
// an agent) becomes a text node, so markup in data stays literal text.
// Keep this file free of top-level document/window access (tests stub them).

const doc = () => globalThis.document;

// --- el ---------------------------------------------------------------

const TAGS = new Set([
  'a', 'article', 'aside', 'b', 'br', 'button', 'caption', 'code', 'dd', 'div', 'dl', 'dt', 'em',
  'footer', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'header', 'hr', 'i', 'input', 'label', 'li', 'main',
  'nav', 'ol', 'option', 'p', 'pre', 'section', 'select', 'small', 'span', 'strong', 'sub', 'sup',
  'table', 'tbody', 'td', 'textarea', 'th', 'thead', 'tr', 'ul',
]);
const PLAIN_ATTRS = new Set([
  'class', 'id', 'role', 'type', 'disabled', 'title', 'tabindex', 'for', 'name', 'value', 'href',
  'hidden', 'scope', 'colspan', 'placeholder', 'maxlength', 'autocomplete', 'spellcheck', 'inputmode', 'autocapitalize', 'autocorrect', 'readonly',
  'target', 'rel',
]);
const PATTERN_ATTRS = [/^aria-[a-z]+$/, /^data-[a-z0-9-]+$/];
// ID_RE: a numeric id (task, approval). Views must validate ids with it
// before building a `#/tasks/<id>` href.
export const ID_RE = /^[0-9]{1,18}$/;
// SAFE_TOKEN_RE: the only shape accepted for id/for/name values and for each
// segment of a hash route (no dots, so `..` is impossible).
export const SAFE_TOKEN_RE = /^[A-Za-z0-9_-]{1,64}$/;
// href is only ever `#/` + safe segments separated by `/`.
const HREF_RE = /^#\/[A-Za-z0-9_-]{1,64}(\/[A-Za-z0-9_-]{1,64})*$/;
const EVENT_RE = /^[a-z]+$/;

// External links (a merge request on GitLab) are the one place an href leaves
// the console. They pass safeExternalHref, and on an <a> only the FIXED
// target and rel below are accepted.
const EXTERNAL_TARGET = '_blank';
const EXTERNAL_REL = 'noopener noreferrer';
const EXTERNAL_HREF_MAX = 500;
const EXTERNAL_HREF_RE = /^https?:\/\/[\x21-\x2e\x30-\x7e][\x21-\x7e]*$/i; // printable ASCII, a host right after //

// safeExternalHref returns url when it is a plain http(s) address that is safe
// to put in an href, else null: a string of at most 500 printable ASCII
// characters (no whitespace, control, bidi or non-ASCII character), with a
// host and without user:password. Nothing is normalised: what passes is
// what is linked.
export function safeExternalHref(url) {
  if (typeof url !== 'string' || url.length === 0 || url.length > EXTERNAL_HREF_MAX) return null;
  if (!EXTERNAL_HREF_RE.test(url) || url.includes('\\')) return null;
  let u;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
  if (u.username !== '' || u.password !== '' || u.hostname === '') return null;
  return url;
}

function isPlainObject(x) {
  if (x === null || typeof x !== 'object') return false;
  const p = Object.getPrototypeOf(x);
  return p === Object.prototype || p === null;
}

function setAttr(node, tag, name, value) {
  const ok = PLAIN_ATTRS.has(name) || PATTERN_ATTRS.some((re) => re.test(name));
  if (!ok) throw new Error('el: attribute not allowed: ' + name);
  if (value === false || value === null || value === undefined) return;
  if (name === 'href') {
    if (tag !== 'a' || typeof value !== 'string' || !(HREF_RE.test(value) || safeExternalHref(value) !== null)) {
      throw new Error('el: href must be a #/ route or a safe http(s) address on an <a>');
    }
  }
  if (name === 'target' && !(tag === 'a' && value === EXTERNAL_TARGET)) {
    throw new Error('el: target must be _blank on an <a>');
  }
  if (name === 'rel' && !(tag === 'a' && value === EXTERNAL_REL)) {
    throw new Error('el: rel must be "noopener noreferrer" on an <a>');
  }
  if ((name === 'id' || name === 'for' || name === 'name') && !(typeof value === 'string' && SAFE_TOKEN_RE.test(value))) {
    throw new Error('el: ' + name + ' must match SAFE_TOKEN_RE');
  }
  if (name === 'class' && Array.isArray(value)) value = value.filter(Boolean).join(' ');
  if (value === true) value = '';
  node.setAttribute(name, String(value));
}

function appendChild(node, child) {
  if (child === null || child === undefined || child === false || child === true) return;
  if (Array.isArray(child)) {
    child.forEach((c) => appendChild(node, c));
  } else if (typeof child === 'object' && typeof child.nodeType === 'number') {
    node.appendChild(child);
  } else {
    node.appendChild(doc().createTextNode(String(child)));
  }
}

// el builds one element. attrs is an optional plain object (or omitted);
// children are strings/numbers (text nodes only), nodes, or arrays of those.
// attrs.on = {click: fn} registers listeners; only functions are accepted.
export function el(tag, attrs, children) {
  if (typeof tag !== 'string' || !TAGS.has(tag)) throw new Error('el: tag not allowed: ' + tag);
  if (attrs === null || attrs === undefined) {
    attrs = null;
  } else if (!isPlainObject(attrs)) {
    children = attrs; // el(tag, children)
    attrs = null;
  }
  const node = doc().createElement(tag);
  if (attrs) {
    for (const [name, value] of Object.entries(attrs)) {
      if (name === 'on') {
        if (!isPlainObject(value)) throw new Error('el: on must be an object of functions');
        for (const [ev, fn] of Object.entries(value)) {
          if (!EVENT_RE.test(ev) || typeof fn !== 'function') throw new Error('el: bad listener ' + ev);
          node.addEventListener(ev, fn);
        }
        continue;
      }
      setAttr(node, tag, name, value);
    }
    if (node.hasAttribute('target') && node.getAttribute('rel') !== EXTERNAL_REL) {
      throw new Error('el: rel must be "noopener noreferrer" when target is set');
    }
    // an address outside the console is only ever linked with the fixed
    // target and rel (never a bare external href)
    const href = node.getAttribute('href');
    if (href !== null && !HREF_RE.test(href) && node.getAttribute('target') !== EXTERNAL_TARGET) {
      throw new Error('el: an external href needs target=_blank and rel="noopener noreferrer"');
    }
  }
  appendChild(node, children);
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

// --- labels and formatting --------------------------------------------

export const STATE_LABELS = {
  queued: '대기', preparing: '준비', agent_running: '에이전트', collecting: '수집',
  verifying: '검증', needs_approval: '승인 대기', passed: '통과', publishing: '게시',
  done: '완료', failed: '실패', cancelled: '취소', error: '오류',
};

const STAGE_STATUS = {
  '✅': { text: '완료', cls: 'c-done' },
  '🔄': { text: '진행 중', cls: 'c-run' },
  '⬜': { text: '시작 전', cls: 'c-idle' },
};

// Where a stage leads. Stages not listed here are shown without a link.
export const STAGE_ROUTES = { implement: '#/tasks', verify: '#/tasks', approve: '#/approvals' };

export function formatSeconds(s) {
  if (typeof s !== 'number' || !Number.isFinite(s) || s < 0) return '-';
  if (s < 10) return (Math.round(s * 10) / 10).toFixed(1) + '초';
  if (s < 600) return Math.round(s) + '초';
  const m = Math.floor(s / 60);
  return m + '분 ' + Math.round(s - m * 60) + '초';
}

function count(by, key) {
  const n = by && typeof by === 'object' ? by[key] : 0;
  return Number.isFinite(n) && n > 0 ? n : 0;
}

// --- stage menu --------------------------------------------------------

// renderStageMenu renders GET /api/v1/stages as a list (id, name, status,
// next); unknown fields are ignored and entries without an id and a name are
// skipped. opts.pending adds the pending-approvals badge to the approve stage
// (taken from the overview, never parsed out of the server's text).
export function renderStageMenu(stages, opts) {
  const pending = opts && Number.isFinite(opts.pending) ? opts.pending : 0;
  const items = [];
  for (const s of Array.isArray(stages) ? stages : []) {
    if (!s || typeof s.id !== 'string' || typeof s.name !== 'string') continue;
    const st = STAGE_STATUS[s.status] || { text: typeof s.status === 'string' ? s.status : '', cls: 'c-idle' };
    const route = Object.prototype.hasOwnProperty.call(STAGE_ROUTES, s.id) ? STAGE_ROUTES[s.id] : null;
    const label = [
      el('span', { class: ['chip', st.cls] }, st.text),
      el('span', { class: 'stg-name' }, s.name),
      s.id === 'approve' && pending > 0
        ? el('span', { class: 'badge', 'aria-label': '대기 ' + pending + '건' }, String(pending))
        : null,
    ];
    const head = route ? el('a', { class: 'stg', href: route }, label) : el('span', { class: 'stg' }, label);
    const next = typeof s.next === 'string' && s.next ? el('small', { class: 'stg-next' }, s.next) : null;
    items.push(el('li', { 'data-stage': s.id }, [head, next]));
  }
  return el('ul', { class: 'stg-list', 'aria-label': '작업 단계' }, items);
}

// --- overview ----------------------------------------------------------

const CHAIN = ['queued', 'preparing', 'agent_running', 'collecting', 'verifying'];

function node(state, by, avg) {
  const timed = avg && Number.isFinite(avg[state]);
  const n = count(by, state);
  const metric = timed ? '평균 ' + formatSeconds(avg[state]) : n > 0 ? n + '건' : '-';
  return el('div', { class: 'tnode', 'data-node': state }, [
    el('span', { class: 't' }, STATE_LABELS[state] || state),
    el('span', { class: 's' }, state),
    el('span', { class: 'm' }, metric),
  ]);
}

function link() {
  return el('div', { class: 'tlink', 'aria-hidden': 'true' });
}

// renderOverview renders GET /api/v1/overview: a one-line summary, ONE
// emphasised "needs a person" bar (only when something awaits approval) and
// the pipeline as a vertical tree. Everything but the approval gate is quiet.
export function renderOverview(ov) {
  ov = ov && typeof ov === 'object' ? ov : {};
  const by = ov.by_state && typeof ov.by_state === 'object' ? ov.by_state : {};
  const avg = ov.stage_avg && typeof ov.stage_avg === 'object' ? ov.stage_avg : {};
  const total = Number.isFinite(ov.total) ? ov.total : 0;
  const todo = Array.isArray(ov.todo) ? ov.todo : [];
  const pending = Math.max(Number.isFinite(ov.pending_approvals) ? ov.pending_approvals : 0, todo.length);
  const done = count(by, 'done');
  const failed = count(by, 'failed') + count(by, 'error');

  const lead = total === 0
    ? '아직 작업이 없습니다.'
    : '작업 ' + total + '건 중 ' + done + '건 완료' + (failed > 0 ? ', ' + failed + '건 실패' : '') + '.';

  const first = todo.find((t) => t && typeof t.text === 'string');
  const todoBar = pending > 0
    ? el('div', { class: 'todo', role: 'status' }, [
      el('span', {}, [
        el('b', {}, '사람이 볼 것 ' + pending + '건'),
        first ? ' · ' + first.text : null,
      ]),
      el('a', { href: '#/approvals', 'data-focus-key': 'todo-approvals' }, '승인 보기 →'),
    ])
    : null;

  const gate = el('div', { class: ['tnode', 'gate'], 'data-node': 'needs_approval' }, [
    el('span', { class: 't' }, '승인 대기'),
    el('span', { class: 's' }, '사람이 승인'),
    pending > 0 ? el('span', { class: 'one' }, pending + '건') : el('span', { class: 'm' }, '-'),
  ]);
  const failNode = el('div', { class: ['tnode', 'bad'], 'data-node': 'failed' }, [
    el('span', { class: 't' }, '실패'),
    el('span', { class: 's' }, 'failed · error'),
    el('span', { class: 'm' }, failed > 0 ? failed + '건' : '-'),
  ]);

  const chain = [];
  CHAIN.forEach((s, i) => {
    if (i > 0) chain.push(link());
    chain.push(node(s, by, avg));
  });

  const tree = el('div', { class: 'tree', role: 'group', 'aria-label': '작업 파이프라인' }, [
    chain,
    el('div', { class: 'tbranch' }, [
      el('div', { class: 'tcol' }, [failNode]),
      el('div', { class: 'tcol' }, [
        node('publishing', by, avg), link(),
        el('div', { class: ['tnode', 'ok'], 'data-node': 'done' }, [
          el('span', { class: 't' }, '완료'),
          el('span', { class: 's' }, 'orch/N 브랜치'),
          el('span', { class: 'm' }, done > 0 ? done + '건' : '-'),
        ]),
      ]),
      el('div', { class: 'tcol' }, [gate]),
    ]),
  ]);

  const rows = Object.keys(by).filter((k) => count(by, k) > 0).sort().map((k) =>
    el('li', {}, [el('span', {}, STATE_LABELS[k] || k), el('span', { class: 'mono' }, String(by[k]))]));
  const insp = el('aside', { class: ['panel', 'insp'], 'aria-label': '상태별 건수' }, [
    el('h2', {}, '상태별 건수'),
    rows.length ? el('ul', { class: 'rows' }, rows) : el('p', { class: 'note' }, '집계할 작업이 없습니다.'),
  ]);

  return el('div', { class: 'ws' }, [
    el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-overview' }, '개요'),
      el('p', { class: 'lead' }, lead),
      todoBar,
      tree,
    ]),
    insp,
  ]);
}

// --- poll --------------------------------------------------------------

const MAX_BACKOFF_MS = 60000;
const MAX_BACKOFF_STEPS = 5;

// poll calls fn every ms with setTimeout chaining: the next run is scheduled
// only after the previous one settled, so requests never overlap. It stops
// while paused or while the document is hidden (resuming refreshes at once)
// and backs off exponentially after failures. Never point it at auth endpoints.
// opts: onUpdate({state, lastUpdated, failures, error}), now(), document.
export function poll(fn, ms, opts) {
  opts = opts || {};
  const d = opts.document || doc();
  const now = opts.now || (() => new Date());
  let timer = null;
  let inFlight = false;
  let paused = false;
  let stopped = false;
  let failures = 0;
  let lastUpdated = null;
  let lastError = null;

  const isHidden = () => !!d && d.visibilityState === 'hidden';
  const stateOf = () => (stopped ? 'stopped' : paused ? 'paused' : isHidden() ? 'hidden' : 'running');
  const notify = () => {
    if (opts.onUpdate) opts.onUpdate({ state: stateOf(), lastUpdated, failures, error: lastError });
  };
  const cancel = () => {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  };
  const schedule = (delay) => {
    cancel();
    if (stopped || paused || isHidden() || inFlight) return;
    timer = setTimeout(run, delay);
  };
  const interval = () => (failures === 0 ? ms : Math.min(MAX_BACKOFF_MS, ms * 2 ** Math.min(failures, MAX_BACKOFF_STEPS)));

  async function run() {
    timer = null;
    if (inFlight || stopped || paused || isHidden()) return;
    inFlight = true;
    try {
      await fn();
      failures = 0;
      lastError = null;
      lastUpdated = now();
    } catch (err) {
      failures++;
      lastError = err;
    } finally {
      inFlight = false;
    }
    if (stopped) return; // a late result of a left view must not touch the UI
    notify();
    schedule(interval());
  }

  const onVisibility = () => {
    notify();
    if (!isHidden()) schedule(0);
    else cancel();
  };
  if (d && d.addEventListener) d.addEventListener('visibilitychange', onVisibility);

  const handle = {
    get state() { return stateOf(); },
    get lastUpdated() { return lastUpdated; },
    get failures() { return failures; },
    pause() {
      if (stopped || paused) return;
      paused = true;
      cancel();
      notify();
    },
    resume() {
      if (stopped || !paused) return;
      paused = false;
      notify();
      schedule(0);
    },
    stop() {
      if (stopped) return;
      stopped = true;
      cancel();
      if (d && d.removeEventListener) d.removeEventListener('visibilitychange', onVisibility);
      notify();
    },
  };
  schedule(0);
  return handle;
}

// formatLive is the text of the "live" pill.
export function formatLive(state, lastUpdated, failures) {
  const pad = (n) => String(n).padStart(2, '0');
  const t = lastUpdated instanceof Date && !Number.isNaN(lastUpdated.getTime())
    ? pad(lastUpdated.getHours()) + ':' + pad(lastUpdated.getMinutes()) + ':' + pad(lastUpdated.getSeconds())
    : null;
  if (state === 'paused') return '일시정지됨' + (t ? ' · 마지막 갱신 ' + t : '');
  if (state === 'hidden') return '탭이 가려져 멈춤' + (t ? ' · 마지막 갱신 ' + t : '');
  if (state === 'stopped') return '갱신 중지';
  if (failures > 0) return '갱신 실패, 다시 시도 중' + (t ? ' · 마지막 성공 ' + t : '');
  return t ? '실시간 · ' + t + ' 갱신' : '갱신 대기 중';
}

// --- text safety -------------------------------------------------------
// Everything from the server (prompts, paths, diff lines, notes) is hostile.
// textContent already prevents markup injection; these helpers also stop
// terminal escapes, bidi/zero-width tricks and unbounded lengths from
// reaching the page by replacing them with VISIBLE placeholders.

// bidi controls, zero-width characters, BOM, line/paragraph separators
const INVISIBLE_RE = /[\u00ad\u034f\u061c\u115f\u1160\u17b4\u17b5\u180b-\u180e\u200b-\u200f\u2028-\u202e\u2060-\u2069\u3164\ufe00-\ufe0f\ufeff\ufff9-\ufffb\u{e0000}-\u{e007f}]/gu;
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f]/g;

// sanitize makes s safe to show. opts.keepTab keeps U+0009 (diff/pre blocks),
// opts.keepNewline keeps U+000A (multi-line blocks); otherwise both become
// visible symbols. Carriage returns always become a visible symbol.
export function sanitize(s, opts) {
  opts = opts || {};
  s = typeof s === 'string' ? s : s === null || s === undefined ? '' : String(s);
  return s
    .replace(CONTROL_RE, (c) => {
      const cp = c.codePointAt(0);
      if (cp === 0x09 && opts.keepTab) return c;
      if (cp === 0x0a) return opts.keepNewline ? c : '⏎';
      if (cp === 0x7f) return '␡';
      if (cp >= 0x80) return '�';
      return String.fromCodePoint(0x2400 + cp); // C0 control pictures (ESC -> U+241B)
    })
    .replace(INVISIBLE_RE, (c) => '⟨U+' + c.codePointAt(0).toString(16).toUpperCase().padStart(4, '0') + '⟩');
}

const INVISIBLE_ONE = new RegExp(INVISIBLE_RE.source, 'u');

// findHostile returns the first character of s that must never be sent as
// input (C0/C1 controls, bidi and zero-width characters, lone surrogates) as
// 'U+XXXX', or null. opts.allowNewline / opts.allowTab permit U+000A / U+0009.
// Unlike sanitize it only looks: callers reject the input instead of stripping.
export function findHostile(s, opts) {
  opts = opts || {};
  for (const ch of typeof s === 'string' ? s : '') {
    const cp = ch.codePointAt(0);
    let bad;
    if (cp === 0x0a) bad = !opts.allowNewline;
    else if (cp === 0x09) bad = !opts.allowTab;
    else bad = cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || (cp >= 0xd800 && cp <= 0xdfff) || INVISIBLE_ONE.test(ch);
    if (bad) return 'U+' + cp.toString(16).toUpperCase().padStart(4, '0');
  }
  return null;
}

// clip cuts s to at most max UTF-16 units without splitting a surrogate pair
// and says how much was dropped.
export function clip(s, max) {
  if (s.length <= max) return { text: s, dropped: 0 };
  let end = max;
  const c = s.charCodeAt(end - 1);
  if (c >= 0xd800 && c <= 0xdbff) end--;
  return { text: s.slice(0, end), dropped: s.length - end };
}

// inline: one line of display text (labels, table cells).
export function inline(s, max) {
  const c = clip(sanitize(s), max || 200);
  return c.dropped ? c.text + '…' : c.text;
}

export const MAX_LINE = 500;

// capLine sanitizes one line of a block and caps its length with a marker.
export function capLine(s, max) {
  max = max || MAX_LINE;
  // clip first (with a margin for placeholders that widen text), so a huge
  // line is never sanitized whole
  const pre = clip(typeof s === 'string' ? s : String(s === null || s === undefined ? '' : s), max + 64);
  const c = clip(sanitize(pre.text, { keepTab: true }), max);
  const dropped = pre.dropped + c.dropped;
  return dropped ? c.text + ' …(+' + dropped + '자)' : c.text;
}

// splitLines splits text on \n (CRLF counts as one break). At most `limit`
// lines are materialised; total is the full line count.
export function splitLines(text, limit) {
  text = typeof text === 'string' ? text : '';
  const lines = [];
  let pos = 0;
  let total = 0;
  while (pos <= text.length) {
    let nl = text.indexOf('\n', pos);
    if (nl < 0) nl = text.length;
    total++;
    if (lines.length < limit) {
      let end = nl;
      if (end > pos && text.charCodeAt(end - 1) === 13) end--;
      lines.push(text.slice(pos, end));
    }
    pos = nl + 1;
  }
  if (text.endsWith('\n')) total--; // a trailing newline ends the last line
  return { lines: lines.slice(0, Math.min(lines.length, total)), total };
}

// chunked renders items in chunks of `size` into container and adds a
// "더 보기" button that appends the next chunk, so unbounded data is never
// rendered at once. render(item, index) returns a node.
export function chunked(container, items, size, render, label) {
  let shown = 0;
  let more = null;
  const step = () => {
    const end = Math.min(items.length, shown + size);
    const nodes = [];
    for (; shown < end; shown++) nodes.push(render(items[shown], shown));
    if (more) container.removeChild(more);
    more = null;
    for (const n of nodes) container.appendChild(n);
    if (shown < items.length) {
      more = el('button', { type: 'button', class: 'btn more', on: { click: step } },
        '더 보기 (' + (label || '항목') + ' ' + (items.length - shown) + '개 남음)');
      container.appendChild(more);
    }
  };
  step();
  return container;
}

// --- shared view helpers -------------------------------------------------

const STATE_CLASS = {
  queued: 'c-idle', preparing: 'c-run', agent_running: 'c-run', collecting: 'c-run', verifying: 'c-run',
  needs_approval: 'c-wait', passed: 'c-done', publishing: 'c-run', done: 'c-done', failed: 'c-fail',
  cancelled: 'c-idle', error: 'c-fail',
};

const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);

// stateLabel: the Korean label of a known state, else sanitized raw text.
export function stateLabel(state) {
  return typeof state === 'string' && has(STATE_LABELS, state) ? STATE_LABELS[state] : inline(state, 40) || '?';
}

// stateChip: text AND color. The class comes only from a fixed table; an
// unknown (hostile) state is shown as sanitized text with the neutral class.
export function stateChip(state) {
  const known = typeof state === 'string' && has(STATE_LABELS, state);
  return el('span', { class: ['chip', known ? STATE_CLASS[state] : 'c-idle'] },
    known ? STATE_LABELS[state] : inline(state, 40) || '?');
}

// idOf returns the id as a string when it is a valid numeric id, else null.
// Views build `#/tasks/<id>` only from its result.
export function idOf(x) {
  const s = typeof x === 'number' && Number.isSafeInteger(x) ? String(x) : typeof x === 'string' ? x : '';
  return ID_RE.test(s) && Number(s) > 0 ? s : null;
}

export function formatTime(iso) {
  const d = new Date(iso);
  if (typeof iso !== 'string' || Number.isNaN(d.getTime())) return '-';
  const p = (n) => String(n).padStart(2, '0');
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

// errorText: FIXED text chosen by status; the server's message (which can
// echo agent or git output) is never shown.
export function errorText(err) {
  const st = err && typeof err.status === 'number' ? err.status : 0;
  if (st === 404) return '찾을 수 없습니다. 없는 항목이거나 삭제되었습니다.';
  if (st === 409) return '아직 볼 수 없는 상태입니다. 작업이 진행되면 다시 확인하세요.';
  if (st === 413) return '내용이 너무 커서 웹에서 볼 수 없습니다.';
  if (st === 429) return '요청이 많아 잠시 쉬는 중입니다. 잠시 후 다시 시도하세요.';
  if (st === 403) return '이 내용을 볼 권한이 없습니다.';
  if (st === 0) return '서버에 연결하지 못했습니다.';
  return '서버에서 오류가 발생했습니다.';
}

export function errorPane(err, backHref, backLabel) {
  return el('div', { class: ['panel', 'canvas'] }, [
    el('h1', { 'data-focus-key': 'err' }, '불러오지 못했습니다'),
    el('p', { class: 'lead', role: 'alert' }, errorText(err)),
    backHref ? el('a', { href: backHref }, backLabel || '← 돌아가기') : null,
  ]);
}

// textBlock: a bounded, read-only block of text lines (white-space: pre): the first
// `chunk` lines now, "더 보기" for the rest, and a note when the text was
// cut before even loading all lines. The caller supplies raw text.
export function textBlock(text, label, opts) {
  opts = opts || {};
  const { lines, total } = splitLines(text, opts.maxLines || 5000);
  if (typeof text !== 'string' || text === '') return el('p', { class: 'note' }, '(비어 있음)');
  const pre = el('div', { class: 'block', role: 'region', tabindex: '0', 'aria-label': label });
  chunked(pre, lines, opts.chunk || 200, (l) => el('div', {}, capLine(l) || ' '), '줄');
  return el('div', {}, [
    pre,
    total > lines.length
      ? el('p', { class: 'note' }, '너무 길어 처음 ' + lines.length + '줄까지만 불러왔습니다 (전체 ' + total + '줄).')
      : null,
  ]);
}
