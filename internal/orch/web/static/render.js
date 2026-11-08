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
  'hidden', 'scope', 'colspan', 'placeholder',
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
    if (tag !== 'a' || typeof value !== 'string' || !HREF_RE.test(value)) {
      throw new Error('el: href must be a #/ route on an <a>');
    }
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
