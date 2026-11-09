// plan.js: the AI plan screen (#/new/plan). Steps: (1) repository and ONE line
// of requirement, (2) a preview of exactly which files would be sent to the AI
// (nothing leaves before the person confirms), (3) the plan as a checklist the
// person edits, (4) the final request text, assembled HERE by a fixed English
// template (D5), shown read-only and submitted as ONE ordinary task.
//
// Everything the AI or the server returns is hostile text: it is cut to the
// server's own bounds, made visible with sanitize(), and put on the page as
// text nodes or input values, never as markup, attributes or links. Plan
// files are shown as text. The plan lives only in this closure (a refresh
// loses it, by design); nothing is written to any browser storage.

import {
  el, clear, inline, clip, sanitize, findHostile, errorPane, errorText, idOf, SAFE_TOKEN_RE,
} from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';
import {
  PromptError, PROMPT_MAX_BYTES, newIdempotencyKey, submitErrorText, repoLabel, CLASS_RE, BASE_RE, AGENTS,
  cleanRepos, classChoices,
} from './new.js';

// Bounds of the server (D4) and of the form. Text from the AI is cut to them.
export const LIMITS = {
  requirement: 2000, // code points (refine.MaxRequirementRunes)
  summary: 600, title: 120, description: 800, file: 200, question: 300, outOfScope: 200,
  comment: 500, answer: 500, notes: 2000,
  items: 30, files: 10, questions: 10, outOfScopeItems: 15, extraItems: 10, listed: 100,
};
const CONFIRM_TEXT = '이 파일들을 AI로 보내 계획서 만들기';
const PREVIEW_TIMEOUT_MS = 25000;
const REFINE_TIMEOUT_MS = 90000; // the broker's own limit is 60 s; the page waits a little longer
const R_ID = /^r[0-9]{1,2}$/;
const Q_ID = /^q[0-9]{1,2}$/;

const utf8Bytes = (s) => new TextEncoder().encode(s).length;
const str = (x) => (typeof x === 'string' ? x : '');
const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);

// show: text from outside made safe and bounded. The input is cut first so a
// huge string is never sanitized whole; newlines survive only when asked.
function show(s, max, newline) {
  const pre = clip(str(s), max + 64).text;
  return clip(sanitize(pre, { keepNewline: !!newline }), max).text;
}

// --- plan state --------------------------------------------------------

// newPlanState turns the /refine response into the screen state (browser
// memory only): {summary, items:[{id,title,description,files,checked,comment,
// edited,origTitle,origDescription}], extraItems:[{id,title,description}],
// questions:[{id,text,answer}], outOfScope, notes}. Texts are cut to the
// server bounds and made visible, so what the person sees and edits is what is
// assembled. Server ids are used as DOM ids only when they have the exact
// r<n>/q<n> shape; anything else gets a client id.
export function newPlanState(plan) {
  plan = plan && typeof plan === 'object' ? plan : {};
  const seen = new Set();
  const pick = (raw, re, prefix, i) => {
    const id = typeof raw === 'string' && re.test(raw) && !seen.has(raw) ? raw : prefix + (i + 1);
    seen.add(id);
    return id;
  };
  const items = (Array.isArray(plan.items) ? plan.items : []).slice(0, LIMITS.items)
    .filter((x) => x && typeof x === 'object')
    .map((x, i) => {
      const title = show(x.title, LIMITS.title);
      const description = show(x.description, LIMITS.description, true);
      return {
        id: pick(x.id, R_ID, 'i', i), title, description,
        files: (Array.isArray(x.files) ? x.files : []).slice(0, LIMITS.files).map((f) => show(f, LIMITS.file)),
        checked: true, comment: '', edited: false, origTitle: title, origDescription: description,
      };
    });
  const questions = (Array.isArray(plan.questions) ? plan.questions : []).slice(0, LIMITS.questions)
    .filter((x) => x && typeof x === 'object')
    .map((x, i) => ({ id: pick(x.id, Q_ID, 'j', i), text: show(x.text, LIMITS.question), answer: '' }));
  return {
    summary: show(plan.summary, LIMITS.summary, true),
    items,
    extraItems: [],
    questions,
    outOfScope: (Array.isArray(plan.out_of_scope) ? plan.out_of_scope : []).slice(0, LIMITS.outOfScopeItems).map((o) => show(o, LIMITS.outOfScope)),
    notes: '',
  };
}

// --- assembly (D5) -----------------------------------------------------

// assemblePlanPrompt builds the final request text:
//
//   Goal: <summary>
//   Requirements (implement ALL of these):   numbered; checked items, then added items
//   Do not (the user excluded these; do not implement them):   unchecked items (titles)
//   Out of scope:
//   Notes from the user:
//   Answers to open questions:   Q: <text> A: <answer>
//
// An item's description and "User comment:" go under its line, indented by
// three spaces. Empty sections are left out. Plan `files` are not part of the
// text. Control, bidi and invisible characters are REJECTED (PromptError
// 'hostile'), never stripped; no checked item is 'empty'; more than 64 KiB
// of UTF-8 is 'too_long'.
export function assemblePlanPrompt(state) {
  state = state && typeof state === 'object' ? state : {};
  const check = (value, label, newline) => {
    const raw = str(value);
    const bad = findHostile(raw, { allowNewline: !!newline, allowTab: !!newline });
    if (bad) {
      throw new PromptError('hostile', '「' + label + '」에 ' + (bad === 'U+000A' ? '줄바꿈' : '보이지 않는 문자나 제어 문자') + '가 있습니다 (' + bad + '). 지우고 다시 입력하세요.', { field: label, char: bad });
    }
    return raw.trim();
  };
  const indent = (t) => t.split('\n').join('\n   ');
  const items = Array.isArray(state.items) ? state.items : [];
  const extras = Array.isArray(state.extraItems) ? state.extraItems : [];
  const questions = Array.isArray(state.questions) ? state.questions : [];
  const outOfScope = Array.isArray(state.outOfScope) ? state.outOfScope : [];

  const sections = [];
  const goal = check(state.summary, '목표 요약', true);
  if (goal) sections.push('Goal: ' + goal);

  const reqs = [];
  const dont = [];
  items.forEach((it, i) => {
    const label = '항목 ' + (i + 1);
    const title = check(it.title, label + ' 제목');
    const desc = check(it.description, label + ' 설명', true);
    const comment = check(it.comment, label + ' 의견', true);
    if (it.checked === true) {
      if (!title) throw new PromptError('empty', label + '의 제목이 비어 있습니다. 제목을 적거나 체크를 푸세요.');
      let t = title;
      if (desc) t += '\n   ' + indent(desc);
      if (comment) t += '\n   User comment: ' + indent(comment);
      reqs.push(t);
    } else if (title) {
      dont.push('- ' + title);
    }
  });
  extras.forEach((it, i) => {
    const label = '추가 항목 ' + (i + 1);
    const title = check(it.title, label + ' 제목');
    const desc = check(it.description, label + ' 설명', true);
    if (!title) return; // an empty added row is ignored
    reqs.push(desc ? title + '\n   ' + indent(desc) : title);
  });
  if (reqs.length === 0) throw new PromptError('empty', '체크된 항목이 없습니다. 하나 이상 체크하거나 항목을 추가하세요.');
  sections.push('Requirements (implement ALL of these):\n' + reqs.map((t, i) => (i + 1) + '. ' + t).join('\n'));
  if (dont.length) sections.push('Do not (the user excluded these; do not implement them):\n' + dont.join('\n'));

  const oos = outOfScope.map((o, i) => check(o, '범위 밖 ' + (i + 1))).filter(Boolean);
  if (oos.length) sections.push('Out of scope:\n' + oos.map((o) => '- ' + o).join('\n'));
  const notes = check(state.notes, '전체 추가 의견', true);
  if (notes) sections.push('Notes from the user:\n' + notes);
  const qa = [];
  questions.forEach((q, i) => {
    const text = check(q.text, '질문 ' + (i + 1));
    const answer = check(q.answer, '질문 ' + (i + 1) + ' 답');
    if (text && answer) qa.push('Q: ' + text + ' A: ' + answer);
  });
  if (qa.length) sections.push('Answers to open questions:\n' + qa.join('\n'));

  const prompt = sections.join('\n\n');
  const bytes = utf8Bytes(prompt);
  if (bytes > PROMPT_MAX_BYTES) {
    throw new PromptError('too_long', '요청 문장이 64 KiB(' + PROMPT_MAX_BYTES + '바이트)를 넘습니다 (현재 ' + bytes + '바이트). 줄여 주세요.', { bytes });
  }
  return prompt;
}

// --- fixed texts -------------------------------------------------------

const REFINE_ERRORS = {
  repo_not_allowed: '이 저장소는 AI 전송이 허용되어 있지 않습니다.',
  broker_unavailable: 'AI 계획 서비스에 연결할 수 없습니다. 잠시 후 다시 시도하세요.',
  refine_rate_limited: 'AI 계획 요청 횟수 한도를 넘었습니다. 잠시 후 다시 시도하세요.',
  selection_changed: '미리보기 이후 저장소나 요구사항 글이 바뀌었습니다. 미리보기를 다시 하세요.',
  selection_expired: '미리보기가 만료되었습니다. 미리보기를 다시 하세요.',
  budget_exceeded: 'AI 사용 예산 또는 호출 한도를 넘었습니다. 관리자에게 알려 주세요.',
  upstream_error: 'AI 서비스 호출에 실패했습니다. 잠시 후 다시 시도하세요.',
  bad_plan: 'AI가 계획서 형식을 지키지 않았습니다. 다시 시도하거나 요구사항 글을 바꿔 보세요.',
  bad_request: '요청을 처리할 수 없습니다. 저장소와 요구사항 글을 확인하세요.',
};
const STALE_CODES = new Set(['selection_changed', 'selection_expired']);

// refineErrorText: FIXED text by error code; the server's message is never shown.
export function refineErrorText(err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (has(REFINE_ERRORS, code)) return REFINE_ERRORS[code];
  return errorText(err);
}

const EXCLUDE_REASONS = {
  secret_name: '비밀 파일 이름이라 제외',
  symlink: '심볼릭 링크라 제외',
  submodule: '서브모듈이라 제외',
  binary: '이진 파일이라 제외',
  not_utf8: 'UTF-8 글이 아니라 제외',
  too_large: '파일이 너무 커서 제외',
  looks_like_secret: '비밀 값처럼 보여 제외',
  unsafe_path: '안전하지 않은 경로라 제외',
  file_cap: '파일 수 한도 때문에 제외',
  total_cap: '전체 크기 한도 때문에 제외',
  unreadable: '읽을 수 없어 제외',
};
const WHY = { readme: 'README', name_match: '이름이 요구사항과 겹침' };

// --- the view ----------------------------------------------------------

const note = (t, cls) => el('p', { class: cls ? ['note', cls] : 'note' }, t);
const headOf = (r) => (r && typeof r.head === 'string' && /^[0-9a-f]{7,64}$/.test(r.head) ? r.head : '');

function usablePreview(p) {
  p = p && typeof p === 'object' ? p : {};
  const files = (Array.isArray(p.files) ? p.files : []).filter((f) => f && typeof f === 'object').slice(0, LIMITS.listed);
  const excluded = (Array.isArray(p.excluded) ? p.excluded : []).filter((f) => f && typeof f === 'object').slice(0, LIMITS.listed);
  const allowed = p.allowed === true && typeof p.selection_id === 'string' && p.selection_id !== '';
  return {
    allowed,
    selectionId: allowed ? p.selection_id : '',
    files: allowed ? files : [],
    excluded: allowed ? excluded : [],
    total: Number.isSafeInteger(p.total_bytes) && p.total_bytes >= 0 ? p.total_bytes : 0,
  };
}

function renderPlan(env) {
  const { ctx } = env;
  const repos = cleanRepos(env.repos);
  const classes = classChoices(env.policy);
  const st = {
    stage: 'input', // input | preview | loading | checklist
    repo: repos[0].path,
    text: '', // the requirement exactly as typed, frozen at the preview
    problem: null, // input-step message {text, kind}
    preview: null,
    err: null, // preview-step message {text, stale}
    plan: null,
    busy: false,
    discard: false,
    released: false, // the person agreed to leave, or the task was submitted
    finalOpen: false,
    shown: null,
    xn: 0,
    submit: { base: headOf(repos[0]), cls: classes[0] || '', agent: AGENTS[0] },
    keys: new Map(),
    done: false,
    result: null, // {text, kind}
    submitProblem: null,
  };
  let h = {}; // the nodes of the current paint

  // --- leave guard: only while a plan is held --------------------------------
  const holding = () => !st.released && (st.stage === 'loading' || st.stage === 'checklist');
  const onUnload = (ev) => {
    if (!holding()) return;
    if (ev && typeof ev.preventDefault === 'function') ev.preventDefault();
    if (ev) ev.returnValue = ''; // the browser shows its own fixed warning
  };
  let armed = false;
  const win = () => (typeof globalThis.addEventListener === 'function' ? globalThis : null);
  function syncGuard() {
    const want = holding();
    if (want && !armed && win()) { win().addEventListener('beforeunload', onUnload); armed = true; }
    if (!want && armed) { win().removeEventListener('beforeunload', onUnload); armed = false; }
  }
  if (ctx.setLeaveGuard) ctx.setLeaveGuard(holding);
  ctx.onLeave(() => {
    st.released = true;
    syncGuard();
    if (ctx.setLeaveGuard) ctx.setLeaveGuard(null);
  });

  const alive = () => !(ctx.signal && ctx.signal.aborted);
  const repoByPath = (p) => repos.find((r) => r.path === p) || null;

  // --- step 1: input ---------------------------------------------------------
  function buildInput() {
    h = {};
    h.repo = el('select', { id: 'p-repo', 'data-focus-key': 'p-repo', on: { change: () => { st.repo = h.repo.value; st.submit.base = headOf(repoByPath(st.repo)); } } },
      repos.map((r) => el('option', { value: r.path }, repoLabel(r.path))));
    h.repo.value = st.repo;
    h.text = el('input', {
      type: 'text', id: 'p-text', autocomplete: 'off', spellcheck: 'false', maxlength: String(LIMITS.requirement),
      'data-focus-key': 'p-text', 'aria-describedby': 'p-text-hint',
      on: { keydown: (ev) => { if (ev && ev.key === 'Enter') { if (typeof ev.preventDefault === 'function') ev.preventDefault(); onPreview(); } } },
    });
    h.text.value = st.text;
    h.problem = el('p', { class: ['note', 'err'], id: 'p-problem', role: 'alert' }, st.problem ? st.problem.text : '');
    h.problem.hidden = !st.problem;
    const busy = st.busy;
    const go = el('button', {
      type: 'button', class: ['btn', 'primary'], id: 'p-preview', 'data-focus-key': 'p-preview',
      'aria-disabled': busy ? 'true' : 'false', 'aria-busy': busy ? 'true' : 'false', on: { click: onPreview },
    }, busy ? '미리보기 만드는 중…' : '미리보기');
    return el('div', { class: 'ws' }, [
      el('div', { class: ['panel', 'canvas', 'form'] }, [
        el('h1', { id: 'h-plan' }, 'AI로 계획서 만들기'),
        el('p', { class: 'lead' }, '한 줄로 요구사항을 적으면 AI가 확인할 수 있는 계획서(체크리스트)를 만듭니다. AI로 보내기 전에 어떤 파일이 나가는지 먼저 보여 드립니다.'),
        el('div', { class: 'sec' }, [
          el('div', { class: 'field' }, [el('label', { for: 'p-repo' }, '저장소'), h.repo]),
          el('div', { class: 'field' }, [
            el('label', { for: 'p-text' }, '요구사항(한 줄)'),
            h.text,
            el('small', { class: 'hint', id: 'p-text-hint' }, '최대 2000자. 글 그대로 AI로 전송됩니다. 비밀번호·키 같은 값은 적지 마세요.'),
          ]),
          h.problem,
          el('div', { class: 'row' }, [go, el('a', { class: 'btn', href: '#/new' }, '← 일반 작성으로')]),
        ]),
      ]),
      aside(),
    ]);
  }
  const aside = () => el('aside', { class: ['panel', 'insp'], 'aria-label': '안내' }, [
    el('h2', {}, '안내'),
    el('ul', { class: 'rows' }, [
      el('li', {}, [el('span', {}, '1단계'), el('span', {}, '미리보기')]),
      el('li', {}, [el('span', {}, '2단계'), el('span', {}, '계획서 확인')]),
      el('li', {}, [el('span', {}, '3단계'), el('span', {}, '확정·제출')]),
    ]),
    el('p', { class: 'note' }, '계획서는 이 화면에만 있습니다. 새로고침하거나 나가면 사라집니다. 제출하면 보통 작업 하나가 만들어집니다.'),
  ]);

  // checkInput: the same shape the server enforces (2000 characters, no
  // control/bidi characters, not empty), checked before anything is sent.
  function checkInput(t) {
    if (t.trim() === '') return '요구사항을 입력하세요.';
    if (Array.from(t).length > LIMITS.requirement) return '요구사항은 ' + LIMITS.requirement + '자까지 쓸 수 있습니다.';
    const bad = findHostile(t, { allowNewline: true, allowTab: true });
    if (bad) return '요구사항에 보이지 않는 문자나 제어 문자가 있습니다 (' + bad + '). 지우고 다시 입력하세요.';
    return '';
  }

  async function onPreview() {
    if (st.busy || st.stage !== 'input') return;
    const text = h.text ? h.text.value : '';
    st.repo = h.repo ? h.repo.value : st.repo;
    st.text = typeof text === 'string' ? text : '';
    if (!repoByPath(st.repo)) { setProblem('저장소 목록에 없는 값입니다. 목록에서 고르세요.'); return; }
    const bad = checkInput(st.text);
    if (bad) { setProblem(bad); return; }
    st.problem = null;
    st.busy = true;
    paint();
    let pv;
    try {
      pv = await ctx.post('/api/v1/refine/preview', { repo: st.repo, text: st.text }, { timeoutMs: PREVIEW_TIMEOUT_MS });
    } catch (err) {
      if (!alive()) return;
      st.busy = false;
      st.problem = { text: refineErrorText(err) };
      paint();
      return;
    }
    if (!alive()) return;
    st.busy = false;
    st.preview = usablePreview(pv);
    st.err = null;
    st.stage = 'preview';
    paint();
  }
  function setProblem(text) {
    st.problem = { text };
    if (h.problem) {
      clear(h.problem);
      h.problem.appendChild(globalThis.document.createTextNode(text));
      h.problem.hidden = false;
    }
  }

  // --- step 2: preview -------------------------------------------------------
  function buildPreview() {
    const pv = st.preview;
    const back = el('button', { type: 'button', class: 'btn', id: 'p-back', 'data-focus-key': 'p-back', on: { click: () => { st.stage = 'input'; st.err = null; st.problem = null; paint(); } } }, '← 다시 입력');
    const summary = el('div', { class: 'sec' }, [
      el('h2', {}, '요구사항'),
      el('p', { class: 'lead' }, inline(st.text, 300)),
      note('저장소: ' + repoLabel(st.repo)),
    ]);
    if (!pv.allowed) {
      return el('div', { class: 'ws' }, [
        el('div', { class: ['panel', 'canvas', 'form'] }, [
          el('h1', { id: 'h-plan' }, 'AI 전송 미리보기'),
          summary,
          el('p', { class: 'todo', role: 'status' }, '이 저장소는 AI 전송이 허용되어 있지 않습니다. 서버 설정에서 허용한 저장소만 AI 계획서를 만들 수 있습니다. 아무것도 전송되지 않았습니다.'),
          el('div', { class: 'row' }, [back, el('a', { class: 'btn', href: '#/new' }, '일반 작성으로')]),
        ]),
        aside(),
      ]);
    }
    const files = el('ul', { class: ['rows', 'plan-rows'], 'aria-label': 'AI로 보낼 파일' },
      pv.files.map((f) => el('li', {}, [
        el('span', { class: 'mono ell' }, inline(f.path, 200)),
        el('span', { class: 'note' }, (has(WHY, f.why) ? WHY[f.why] + ' · ' : '') + (Number.isSafeInteger(f.bytes) && f.bytes >= 0 ? f.bytes + ' 바이트' : '')),
      ])));
    const excluded = el('ul', { class: ['rows', 'plan-rows'], 'aria-label': '제외된 파일' },
      pv.excluded.map((f) => el('li', {}, [
        el('span', { class: 'mono ell' }, inline(f.path, 200)),
        el('span', { class: 'note' }, has(EXCLUDE_REASONS, f.reason) ? EXCLUDE_REASONS[f.reason] : '제외됨'),
      ])));
    const stale = !!(st.err && st.err.stale);
    const action = stale
      ? el('button', { type: 'button', class: ['btn', 'primary'], id: 'p-repreview', 'data-focus-key': 'p-repreview', on: { click: onRepreview } }, '미리보기 다시 하기')
      : el('button', {
        type: 'button', class: ['btn', 'primary'], id: 'p-confirm', 'data-focus-key': 'p-confirm',
        'aria-disabled': st.busy ? 'true' : 'false', on: { click: onConfirm },
      }, CONFIRM_TEXT);
    return el('div', { class: 'ws' }, [
      el('div', { class: ['panel', 'canvas', 'form'] }, [
        el('h1', { id: 'h-plan' }, 'AI 전송 미리보기'),
        summary,
        el('div', { class: 'sec' }, [
          el('h2', {}, 'AI로 보낼 파일 ' + pv.files.length + '개 · 총 ' + pv.total + ' 바이트'),
          pv.files.length ? files : note('보낼 파일이 없습니다. 요구사항 글만 전송됩니다.'),
        ]),
        pv.excluded.length ? el('div', { class: 'sec' }, [el('h2', {}, '제외된 파일 ' + pv.excluded.length + '개'), excluded]) : null,
        el('p', { class: ['note', 'plan-warn'] }, '위 파일의 내용과 요구사항 글이 AI 서비스로 전송됩니다. 확인 단추를 누르기 전에는 아무것도 전송되지 않습니다.'),
        st.err ? el('p', { class: ['note', 'err'], role: 'alert', 'data-focus-key': 'p-err' }, st.err.text) : null,
        el('div', { class: 'row' }, [action, back]),
      ]),
      aside(),
    ]);
  }

  async function onRepreview() {
    if (st.busy) return;
    st.busy = true;
    st.err = null;
    let pv;
    try {
      pv = await ctx.post('/api/v1/refine/preview', { repo: st.repo, text: st.text }, { timeoutMs: PREVIEW_TIMEOUT_MS });
    } catch (err) {
      if (!alive()) return;
      st.busy = false;
      st.err = { text: refineErrorText(err), stale: STALE_CODES.has(err && err.code) };
      paint();
      return;
    }
    if (!alive()) return;
    st.busy = false;
    st.preview = usablePreview(pv);
    paint();
  }

  // onConfirm sends the PREVIEWED text and selection id. The busy flag is
  // set before the first await, so a double click sends once.
  async function onConfirm() {
    if (st.busy || st.stage !== 'preview' || !st.preview || !st.preview.allowed) return;
    st.busy = true;
    st.err = null;
    st.stage = 'loading';
    paint();
    let plan;
    try {
      plan = await ctx.post('/api/v1/refine', { repo: st.repo, text: st.text, selection_id: st.preview.selectionId }, { timeoutMs: REFINE_TIMEOUT_MS });
    } catch (err) {
      if (!alive()) return;
      st.busy = false;
      st.stage = 'preview';
      st.err = { text: refineErrorText(err), stale: STALE_CODES.has(err && err.code) };
      paint();
      return;
    }
    if (!alive()) return;
    st.busy = false;
    const state = newPlanState(plan);
    if (state.items.length === 0) {
      st.stage = 'preview';
      st.err = { text: REFINE_ERRORS.bad_plan, stale: false };
      paint();
      return;
    }
    st.plan = state;
    st.stage = 'checklist';
    paint();
  }

  // --- loading ---------------------------------------------------------------
  function buildLoading() {
    return el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-plan' }, 'AI 계획서 만드는 중'),
      el('p', { class: 'lead', role: 'status' }, 'AI가 계획서를 만들고 있습니다. 최대 약 1분 걸립니다. 이 화면을 닫지 마세요.'),
    ]);
  }

  // --- step 3: checklist -----------------------------------------------------
  const labeled = (id, label, control, hint) => el('div', { class: 'field' }, [
    el('label', { for: id }, label),
    control,
    hint ? el('small', { class: 'hint' }, hint) : null,
  ]);
  const textInput = (id, value, max) => {
    const n = el('input', { type: 'text', id, autocomplete: 'off', spellcheck: 'false', maxlength: String(max), 'data-focus-key': id, on: { input: onEdit, change: onEdit } });
    n.value = value;
    return n;
  };
  const textArea = (id, value, max, rows) => {
    const n = el('textarea', { id, autocomplete: 'off', spellcheck: 'false', maxlength: String(max), 'data-focus-key': id, on: { input: onEdit, change: onEdit } });
    n.value = value;
    if (rows) n.setAttribute('rows', String(rows));
    return n;
  };

  function buildChecklist() {
    const pl = st.plan;
    h = { items: [], extras: [], answers: [] };
    const itemNodes = pl.items.map((it, i) => {
      const k = 'p-' + it.id;
      const n = i + 1;
      const row = { it };
      row.check = el('input', { type: 'checkbox', id: k + '-check', 'data-focus-key': k + '-check', on: { change: onEdit } });
      row.check.checked = it.checked;
      row.title = textInput(k + '-title', it.title, LIMITS.title);
      row.desc = textArea(k + '-desc', it.description, LIMITS.description, 3);
      row.comment = textInput(k + '-comment', it.comment, LIMITS.comment);
      h.items.push(row);
      return el('div', { class: 'plan-item', 'data-item': it.id }, [
        el('div', { class: 'plan-check' }, [row.check, el('label', { for: k + '-check' }, '항목 ' + n + ' 포함 (체크를 풀면 "하지 말 것"으로 전달)')]),
        labeled(k + '-title', '항목 ' + n + ' 제목', row.title),
        labeled(k + '-desc', '항목 ' + n + ' 설명', row.desc),
        it.files.length
          ? el('div', { class: 'plan-files', 'data-files': it.id }, [el('small', { class: 'hint' }, '관련 파일(참고용, 글자로만 표시)'), el('div', { class: 'files-list' }, it.files.map((f) => el('code', { class: 'mono' }, f)))])
          : null,
        labeled(k + '-comment', '항목 ' + n + ' 의견', row.comment),
      ]);
    });
    const extraNodes = pl.extraItems.map((x, i) => {
      const k = 'p-' + x.id;
      const row = { it: x };
      row.title = textInput(k + '-title', x.title, LIMITS.title);
      row.desc = textArea(k + '-desc', x.description, LIMITS.description, 3);
      h.extras.push(row);
      return el('div', { class: 'plan-item added', 'data-item': x.id }, [
        labeled(k + '-title', '추가 항목 ' + (i + 1) + ' 제목', row.title),
        labeled(k + '-desc', '추가 항목 ' + (i + 1) + ' 설명', row.desc),
        el('button', { type: 'button', class: 'btn', id: k + '-del', 'data-focus-key': k + '-del', on: { click: () => removeExtra(x.id) } }, '추가 항목 ' + (i + 1) + ' 삭제'),
      ]);
    });
    const canAdd = pl.extraItems.length < LIMITS.extraItems;
    const add = el('button', {
      type: 'button', class: 'btn', id: 'p-add', 'data-focus-key': 'p-add', 'aria-disabled': canAdd ? 'false' : 'true', on: { click: addExtra },
    }, '항목 추가');
    const questions = pl.questions.map((q, i) => {
      const k = 'p-' + q.id + '-answer';
      const inp = textInput(k, q.answer, LIMITS.answer);
      h.answers.push({ q, inp });
      return labeled(k, '질문 ' + (i + 1) + ': ' + q.text, inp);
    });
    h.notes = textArea('p-notes', pl.notes, LIMITS.notes, 3);

    // final part: hidden until the person confirms
    h.final = el('pre', { id: 'p-final', class: 'block-pre', 'aria-label': '전송될 요청 문장(읽기 전용)' });
    h.final.hidden = !st.finalOpen;
    h.count = el('p', { class: 'note', id: 'p-count' });
    h.problem = el('p', { class: ['note', 'err'], 'aria-live': 'polite', id: 'p-problem' });
    h.result = el('div', { class: 'form-result', role: 'status' });
    h.base = textInput('p-base', st.submit.base, 200);
    h.base.setAttribute('class', 'mono');
    h.cls = classes.length > 0
      ? el('select', { id: 'p-class', 'data-focus-key': 'p-class', on: { change: onEdit } }, classes.map((c) => el('option', { value: c }, c)))
      : el('input', { type: 'text', id: 'p-class', autocomplete: 'off', spellcheck: 'false', maxlength: '32', placeholder: '예: docs', 'data-focus-key': 'p-class', on: { input: onEdit, change: onEdit } });
    h.cls.value = st.submit.cls;
    h.agent = el('select', { id: 'p-agent', 'data-focus-key': 'p-agent', on: { change: onEdit } }, AGENTS.map((a) => el('option', { value: a }, a)));
    h.agent.value = st.submit.agent;
    h.submit = el('button', { type: 'button', class: ['btn', 'primary'], id: 'p-submit', 'data-focus-key': 'p-submit', on: { click: onSubmit } }, '작업으로 제출');
    h.finalSec = el('div', { class: 'sec' }, [
      el('h2', {}, '전송될 요청 문장'),
      el('p', { class: 'note' }, '아래 문장이 그대로 작업 하나로 제출됩니다. 고치려면 위의 항목을 바꾸세요.'),
      h.final,
      h.count,
      el('div', { class: 'row fields2' }, [
        labeled('p-base', '기준 커밋(base)', h.base),
        labeled('p-class', '종류', h.cls),
        labeled('p-agent', '에이전트', h.agent),
      ]),
      h.problem,
      el('div', { class: 'row' }, [h.submit]),
      h.result,
    ]);

    h.finalSec.hidden = !st.finalOpen;
    const discard = st.discard
      ? el('div', { class: 'confirm', role: 'group', 'aria-label': '계획서 버리기', on: { keydown: (ev) => { if (ev && ev.key === 'Escape') { if (typeof ev.preventDefault === 'function') ev.preventDefault(); closeDiscard(); } } } }, [
        el('p', { class: 'q' }, '계획서를 버릴까요?'),
        el('p', { class: 'meaning' }, '지금까지 고친 내용과 AI가 만든 계획서가 사라지고, 새로 만들려면 AI를 다시 호출해야 합니다.'),
        el('div', { class: 'row' }, [
          el('button', { type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'p-discard-yes', on: { click: onDiscardYes } }, '버리기'),
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'p-discard', on: { click: closeDiscard } }, '계속 편집'),
        ]),
      ])
      : null;

    const main = el('div', { class: ['panel', 'canvas', 'form'] }, [
      el('h1', { id: 'h-plan' }, '계획서 확인'),
      el('p', { class: 'lead' }, 'AI가 만든 계획서입니다. 필요 없는 항목은 체크를 풀고, 제목·설명은 고칠 수 있습니다. 체크를 푼 항목은 에이전트에게 "하지 말 것"으로 전달됩니다.'),
      el('div', { class: 'sec' }, [el('h2', {}, '목표 요약'), el('p', { class: 'plan-summary' }, pl.summary || '(요약 없음)')]),
      el('div', { class: 'sec' }, [el('h2', {}, '항목 ' + pl.items.length + '개'), ...itemNodes]),
      el('div', { class: 'sec' }, [el('h2', {}, '추가한 항목'), ...extraNodes, add]),
      pl.questions.length ? el('div', { class: 'sec' }, [el('h2', {}, 'AI의 질문'), ...questions]) : null,
      pl.outOfScope.length
        ? el('div', { class: 'sec' }, [el('h2', {}, '범위 밖(하지 않을 일)'), el('ul', { class: 'plan-oos', 'aria-label': '범위 밖 목록' }, pl.outOfScope.map((o) => el('li', {}, o)))])
        : null,
      el('div', { class: 'sec' }, [labeled('p-notes', '전체 추가 의견', h.notes, '에이전트에게 따로 전할 말이 있으면 적으세요.')]),
      el('div', { class: 'row' }, [
        el('button', { type: 'button', class: ['btn', 'primary'], id: 'p-finalize', 'data-focus-key': 'p-finalize', on: { click: onFinalize } }, '확정'),
        el('button', { type: 'button', class: 'btn', id: 'p-discard', 'data-focus-key': 'p-discard', on: { click: openDiscard } }, '계획서 버리기'),
      ]),
      discard,
      h.finalSec,
    ]);
    const info = el('aside', { class: ['panel', 'insp'], 'aria-label': '안내' }, [
      el('h2', {}, '안내'),
      el('ul', { class: 'rows' }, [
        el('li', {}, [el('span', {}, '요청 문장 상한'), el('span', { class: 'mono' }, '64 KiB')]),
        el('li', {}, [el('span', {}, '제출'), el('span', {}, '작업 하나')]),
      ]),
      el('p', { class: 'note' }, '이 계획서는 브라우저 메모리에만 있습니다. 새로고침하거나 나가면 사라집니다.'),
    ]);
    return el('div', { class: 'ws' }, [main, info]);
  }

  // syncFromDom copies what the person typed into the plan state. It runs
  // before every assembly, so the text shown and sent never depends on an
  // event having fired.
  function syncFromDom() {
    if (!st.plan || !h.items) return;
    for (const row of h.items) {
      row.it.checked = row.check.checked === true;
      row.it.title = str(row.title.value);
      row.it.description = str(row.desc.value);
      row.it.comment = str(row.comment.value);
      row.it.edited = row.it.title !== row.it.origTitle || row.it.description !== row.it.origDescription;
    }
    for (const row of h.extras) {
      row.it.title = str(row.title.value);
      row.it.description = str(row.desc.value);
    }
    for (const a of h.answers) a.q.answer = str(a.inp.value);
    st.plan.notes = str(h.notes.value);
    st.submit.base = str(h.base.value);
    st.submit.cls = str(h.cls.value);
    st.submit.agent = str(h.agent.value);
  }

  function onEdit() {
    syncFromDom();
    if (st.finalOpen) refreshFinal();
  }
  function addExtra() {
    if (st.plan.extraItems.length >= LIMITS.extraItems) return;
    syncFromDom();
    st.xn++;
    const id = 'x' + st.xn;
    st.plan.extraItems.push({ id, title: '', description: '' });
    paint();
    const t = findById('p-' + id + '-title');
    if (t) t.focus();
  }
  function removeExtra(id) {
    syncFromDom();
    st.plan.extraItems = st.plan.extraItems.filter((x) => x.id !== id);
    paint();
    const a = findById('p-add');
    if (a) a.focus();
  }
  function findById(id) {
    const walk = (n) => {
      if (n.nodeType !== 1) return null;
      if (n.getAttribute('id') === id) return n;
      for (const c of n.childNodes) { const f = walk(c); if (f) return f; }
      return null;
    };
    return walk(ctx.root);
  }
  function openDiscard() { syncFromDom(); st.discard = true; paint(); }
  function closeDiscard() { st.discard = false; paint(); }
  function onDiscardYes() {
    st.released = true;
    syncGuard();
    ctx.navigate('#/new');
  }
  function onFinalize() {
    syncFromDom();
    st.finalOpen = true;
    paint();
    const f = findById('p-final');
    if (f && typeof f.focus === 'function') { f.setAttribute('tabindex', '-1'); f.focus(); }
  }

  // current reads the screen into a task body, or the reason it can not be sent.
  function current() {
    syncFromDom();
    const repo = repoByPath(st.repo);
    if (!repo) return { problem: '저장소 목록에 없는 값입니다.' };
    const base = st.submit.base.trim();
    if (base === '') return { problem: '이 저장소에는 아직 커밋이 없어 기준 커밋을 정할 수 없습니다.', needBase: true };
    if (!BASE_RE.test(base)) return { problem: '기준 커밋 형식이 올바르지 않습니다. 영문자·숫자와 . _ / ~ ^ @ { } + - 만 쓸 수 있습니다.' };
    const cls = st.submit.cls;
    if (classes.length > 0 ? !classes.includes(cls) : !CLASS_RE.test(cls)) {
      return { problem: classes.length > 0 ? '정책에 없는 작업 종류입니다. 목록에서 고르세요.' : '종류는 소문자·숫자·밑줄·하이픈·한글 1~32자로 적으세요.' };
    }
    if (!AGENTS.includes(st.submit.agent)) return { problem: '에이전트는 claude 또는 codex 중에서 고르세요.' };
    let prompt;
    try {
      prompt = assemblePlanPrompt(st.plan);
    } catch (err) {
      if (err instanceof PromptError) return { problem: err.message };
      throw err;
    }
    return { body: { repo: repo.path, base, prompt, class: cls, agent: st.submit.agent }, prompt };
  }

  const blocked = () => st.busy || st.done || !!st.submitProblem;
  function syncButton() {
    if (!h.submit) return;
    h.submit.setAttribute('aria-disabled', blocked() ? 'true' : 'false');
    h.submit.setAttribute('aria-busy', st.busy ? 'true' : 'false');
  }
  // refreshFinal rewrites the displayed text from the SAME string a submit
  // would send. The problem region is rewritten only when its text changes
  // (a polite live region, so a screen reader hears each problem once).
  function refreshFinal() {
    const cur = current();
    st.submitProblem = cur.problem || null;
    st.shown = cur.prompt === undefined ? null : cur.prompt;
    clear(h.final);
    if (st.shown !== null) h.final.appendChild(globalThis.document.createTextNode(st.shown));
    clear(h.count);
    if (st.shown !== null) h.count.appendChild(globalThis.document.createTextNode(utf8Bytes(st.shown) + ' / ' + PROMPT_MAX_BYTES + ' 바이트'));
    const want = st.submitProblem || '';
    if (want !== h.problem.textContent) {
      clear(h.problem);
      if (want) h.problem.appendChild(globalThis.document.createTextNode(want));
    }
    h.problem.hidden = !want;
    syncButton();
    return cur;
  }
  function showResult(text, kind, extra) {
    st.result = text ? { text, kind } : null;
    clear(h.result);
    if (!text) return;
    h.result.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    h.result.appendChild(el('p', { class: ['note', kind === 'error' ? 'err' : 'ok'], 'data-focus-key': 'p-result' }, text));
    if (extra) h.result.appendChild(extra);
  }

  const fingerprint = (b) => JSON.stringify([b.repo, b.base, b.prompt, b.class, b.agent]);

  async function onSubmit() {
    if (st.busy || st.done || !st.finalOpen) return;
    const before = st.shown;
    const cur = refreshFinal();
    if (cur.problem) return;
    if (cur.prompt !== before) return; // the person has not seen this text yet: show it first
    const body = cur.body;
    const fp = fingerprint(body);
    let key = st.keys.get(fp);
    if (!key) {
      key = newIdempotencyKey();
      st.keys.set(fp, key);
    }
    st.busy = true;
    syncButton();
    showResult('제출하는 중…', 'info');
    try {
      const task = await ctx.post('/api/v1/tasks', body, { headers: { 'Idempotency-Key': key } });
      if (!alive()) return;
      const id = idOf(task && typeof task === 'object' ? task.id : null);
      st.busy = false;
      st.keys.clear();
      st.done = true; // a task may exist: never offer a second submit
      st.released = true;
      syncGuard();
      syncButton();
      if (!id) {
        showResult('작업이 접수됐지만 작업 번호를 확인할 수 없습니다. 작업 목록에서 확인하세요.', 'error', el('a', { href: '#/tasks' }, '작업 목록으로 →'));
        return;
      }
      showResult('작업 #' + id + '을(를) 접수했습니다. 상세 화면으로 이동합니다.', 'ok');
      ctx.navigate('#/tasks/' + id);
    } catch (err) {
      if (!alive()) return;
      st.busy = false;
      refreshFinal();
      showResult(submitErrorText(err), 'error');
    }
  }

  // --- paint -----------------------------------------------------------------
  function paint() {
    const node = st.stage === 'input' ? buildInput()
      : st.stage === 'preview' ? buildPreview()
        : st.stage === 'loading' ? buildLoading()
          : buildChecklist();
    replaceContent(ctx.root, node);
    if (st.stage === 'checklist') {
      if (st.finalOpen) refreshFinal();
      if (st.result) showResult(st.result.text, st.result.kind);
    }
    syncGuard();
  }

  paint();
}

export async function planView(ctx) {
  ctx.setCrumb('AI 계획서');
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
          el('h1', { id: 'h-plan' }, 'AI로 계획서 만들기'),
          el('p', { class: 'lead', role: 'status' }, '계획서를 만들 수 있는 저장소가 없습니다. 서버의 허용 경로 아래에 git 저장소가 있는지 확인하세요.'),
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'retry', on: { click: load } }, '다시 불러오기'),
        ]));
        return;
      }
      renderPlan({ ctx, repos, policy });
    } catch (err) {
      if (ctx.signal && ctx.signal.aborted) return;
      const pane = errorPane(err, '#/new', '← 새 작업');
      pane.appendChild(el('button', { type: 'button', class: 'btn', 'data-focus-key': 'retry', on: { click: load } }, '다시 불러오기'));
      replaceContent(ctx.root, pane);
    } finally {
      busy = false;
    }
  }
  await load();
}

registerView('plan', planView);
