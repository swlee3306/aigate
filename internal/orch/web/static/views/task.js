// task.js: task detail (#/tasks/ID): header, progress line, cancel/retry
// buttons (with an in-page confirmation step), timeline bars from
// GET /api/v1/tasks/{id}/events, verify timing, links to the diff and the
// approval screen, and the saved report as TEXT (loaded on demand).

import {
  el, inline, stateChip, idOf, formatTime, formatSeconds, errorPane, errorText, textBlock, chunked,
  STATE_LABELS, stateLabel, safeExternalHref,
} from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';

const POLL_MS = 10000;
export const MIN_BAR_PCT = 2; // every stage stays visible
const FINAL = new Set(['done', 'failed', 'cancelled']);
// RUNNING states have a live timer; queued, needs_approval, passed and error
// are waiting (for a worker, a person or a retry), so no growing timer is
// shown for them.
const RUNNING = new Set(['preparing', 'agent_running', 'collecting', 'verifying', 'publishing']);
const HEX_RE = /^[0-9a-f]{7,64}$/;

const num = (x) => (typeof x === 'number' && Number.isFinite(x) && x >= 0 ? x : 0);

// effectiveSeconds: the last step has no end yet; for a task that is not
// finished it is still running, so its elapsed time is shown (opts.now).
function effectiveSeconds(steps, opts) {
  const now = opts && opts.now instanceof Date ? opts.now.getTime() : null;
  return steps.map((s, i) => {
    let sec = num(s && s.seconds);
    if (i === steps.length - 1 && sec === 0 && now !== null && s && RUNNING.has(s.state)) {
      const at = Date.parse(s.at);
      if (Number.isFinite(at) && now > at) sec = (now - at) / 1000;
    }
    return sec;
  });
}

// barWidths: percentage of the TOTAL time per step, never below MIN_BAR_PCT.
export function barWidths(seconds) {
  const total = seconds.reduce((a, b) => a + b, 0);
  return seconds.map((s) => {
    if (total <= 0) return MIN_BAR_PCT;
    return Math.min(100, Math.max(MIN_BAR_PCT, Math.round((100 * s) / total)));
  });
}

export const ACTOR_MAX = 80;

// actorText: who moved the task into a step, as TEXT. "system" (the worker)
// reads "자동", "invalid" (the server could not record a clean actor) reads
// "알 수 없음", an empty or missing value shows nothing, and anything else
// ("kind:id") goes through the sanitizer and the 80-character cap.
export function actorText(a) {
  if (typeof a !== 'string' || a === '') return '';
  if (a === 'system') return '자동';
  if (a === 'invalid') return '알 수 없음';
  const t = inline(a, ACTOR_MAX);
  return t.trim() === '' ? '' : t;
}

// renderTimeline: one row per step (label, bar, duration text). The bars are
// decoration (aria-hidden); the label, the duration text and the actor ("처리:
// who moved it into the step") carry the data.
export function renderTimeline(steps, opts) {
  const list = Array.isArray(steps) ? steps.filter((s) => s && typeof s === 'object') : [];
  if (list.length === 0) return el('p', { class: 'note' }, '기록된 단계가 없습니다.');
  const secs = effectiveSeconds(list, opts);
  const widths = barWidths(secs);
  const lastState = list[list.length - 1].state;
  const lastLive = RUNNING.has(lastState);
  const lastWait = !lastLive && !FINAL.has(lastState);
  const rows = list.map((s, i) => {
    const known = typeof s.state === 'string' && Object.prototype.hasOwnProperty.call(STATE_LABELS, s.state);
    const label = stateLabel(s.state);
    const last = i === list.length - 1;
    const live = last && lastLive;
    const waiting = last && lastWait;
    const fill = el('div', { class: 'bar-f' });
    fill.style.width = widths[i] + '%'; // CSSOM: CSP forbids style attributes
    const who = actorText(s.actor);
    return el('li', { class: 'bar-r', 'data-step': known ? s.state : '?' }, [
      el('span', {}, label),
      el('div', { class: 'bar-t', 'aria-hidden': 'true' }, fill),
      el('span', { class: 'mono' }, live ? '진행 중 · ' + formatSeconds(secs[i]) : waiting ? '대기 중' : formatSeconds(secs[i])),
      who ? el('span', { class: 'bar-who' }, '처리: ' + who) : null,
    ]);
  });
  const total = secs.reduce((a, b) => a + b, 0);
  return el('div', {}, [
    el('ul', { class: 'bars', 'aria-label': '단계별 소요 시간' }, rows),
    el('p', { class: 'note' }, '합계 ' + formatSeconds(total) + ' · 막대 길이는 전체 시간에 대한 비율입니다.'),
  ]);
}

function renderSteps(steps) {
  const list = Array.isArray(steps) ? steps.filter((s) => s && typeof s === 'object') : [];
  const ol = el('ol', { class: 'rows', 'aria-label': '단계 기록' });
  chunked(ol, list, 30, (s) => el('li', {}, [
    el('span', {}, [stateLabel(s.state) + ' ', el('small', { class: 'ell' }, inline(s.note, 160))]),
    el('span', { class: 'mono' }, formatTime(s.at)),
  ]), '기록');
  return ol;
}

function verifyBlock(v) {
  if (!v || typeof v !== 'object') return el('p', { class: 'note' }, '검증 시간 기록이 없습니다.');
  const rows = [
    ['복원', formatSeconds(v.restore_sec)],
    ['검사 합계', formatSeconds(v.gate_sec)],
    ['기준(base)', formatSeconds(v.base_sec)],
    ['변경(head)', formatSeconds(v.head_sec)],
  ];
  if (num(v.base_cache_runs) > 0) rows.push(['base 캐시', num(v.base_cache_hits) + ' / ' + num(v.base_cache_runs) + ' 재사용']);
  return el('dl', {}, rows.flatMap(([k, val]) => [el('dt', {}, k), el('dd', { class: 'mono' }, val)]));
}

function reportBlock(rep, onLoadArg) {
  const onLoad = typeof onLoadArg === 'function' ? onLoadArg : () => {};
  if (!rep || rep.status === 'idle') {
    return el('button', { type: 'button', class: 'btn', 'data-focus-key': 'report', on: { click: onLoad } }, '보고서 불러오기');
  }
  if (rep.status === 'loading') return el('p', { class: 'note', role: 'status' }, '보고서를 불러오는 중…');
  if (rep.status === 'error') {
    return el('div', {}, [
      el('p', { class: 'note', role: 'alert' }, rep.notFound ? '보고서가 아직 없습니다.' : errorText(rep.err)),
      el('button', { type: 'button', class: 'btn', 'data-focus-key': 'report', on: { click: onLoad } }, '다시 불러오기'),
    ]);
  }
  return textBlock(rep.text, '검증 보고서 (읽기 전용 텍스트)', { chunk: 200 });
}

// --- merge request (Draft MR on GitLab, made by the broker after publish) ----

// Why a merge request failed, by the server's fixed code. Unknown or hostile
// codes get no reason text; the code itself is never shown.
const MR_FAIL_REASONS = {
  upstream_error: 'GitLab 호출에 실패했습니다.',
  timeout: '시간이 초과되었습니다.',
  broker_unavailable: '병합 요청 서비스에 연결할 수 없었습니다.',
  bad_request: '요청 값이 맞지 않았습니다.',
  bad_url: 'GitLab이 돌려준 주소를 확인할 수 없어 저장하지 않았습니다. GitLab에서 직접 확인하세요.',
  internal_error: '내부 오류가 있었습니다.',
};
const MR_NO_RETRY = new Set(['bad_url']); // a retry reaches the same answer

// mrRetryResult maps a failed POST /tasks/{id}/mr to a FIXED text; the server's
// message and reason are never shown. reload: refetch the task.
export function mrRetryResult(err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (code === 'mr_not_applicable') return { text: '이 작업에는 병합 요청을 다시 만들 수 없습니다. 저장소가 GitLab에 연결되어 있는지 확인하세요.', reload: true };
  if (code === 'mr_already_created') return { text: '병합 요청이 이미 만들어져 있습니다. 화면을 새로 고쳤습니다.', reload: true };
  if (code === 'upstream_error') return { text: '병합 요청을 만들지 못했습니다. 작업 결과는 그대로입니다. 잠시 후 다시 시도하세요.', reload: true };
  return { text: errorText(err), reload: false };
}

// mrBlock: the merge request area of a task. Only an exact mr_state of
// created / failed / skipped shows anything. The link text is fixed (the
// number is checked), the URL only becomes an href through safeExternalHref.
function mrBlock(t, extras) {
  const state = t && typeof t.mr_state === 'string' ? t.mr_state : '';
  if (state !== 'created' && state !== 'failed' && state !== 'skipped') return null;
  const mr = extras.mr && typeof extras.mr === 'object' ? extras.mr : {};
  const msg = mr.msg ? el('p', {
    class: ['note', mr.msg.kind === 'ok' ? 'ok' : 'err'], role: mr.msg.kind === 'ok' ? 'status' : 'alert', 'data-focus-key': 'mr-msg',
  }, mr.msg.text) : null;
  let body;
  if (state === 'created') {
    const href = safeExternalHref(t.mr_url);
    const n = Number.isSafeInteger(t.mr_iid) && t.mr_iid > 0 ? ' !' + t.mr_iid : '';
    body = href
      ? [
        el('a', { class: 'btn', href, target: '_blank', rel: 'noopener noreferrer', 'data-focus-key': 'mr-link' }, 'GitLab에서 병합 요청' + n + ' 보기 ↗'),
        el('p', { class: 'note' }, '초안(Draft)입니다. 검토와 병합은 GitLab에서 사람이 합니다.'),
      ]
      : el('p', { class: 'note' }, '병합 요청이 만들어졌지만 주소를 안전하게 표시할 수 없습니다. GitLab에서 확인하세요.');
  } else if (state === 'failed') {
    const code = typeof t.mr_code === 'string' ? t.mr_code : '';
    const why = Object.prototype.hasOwnProperty.call(MR_FAIL_REASONS, code) ? ' ' + MR_FAIL_REASONS[code] : '';
    body = [
      el('p', { class: 'note' }, '병합 요청을 만들지 못했습니다.' + why + ' 작업 결과(브랜치)는 그대로입니다.'),
      MR_NO_RETRY.has(code) ? null : el('button', {
        type: 'button', class: 'btn', 'data-focus-key': 'mr-retry', 'aria-disabled': mr.busy ? 'true' : 'false',
        'aria-busy': mr.busy ? 'true' : 'false', on: { click: () => extras.onRetryMR && extras.onRetryMR() },
      }, mr.busy ? '처리 중…' : '다시 시도'),
    ];
  } else {
    body = el('p', { class: 'note' }, '이 저장소는 병합 요청을 만들지 않습니다.');
  }
  return el('div', { class: 'sec', 'data-section': 'mr' }, [el('h2', {}, '병합 요청'), body, msg]);
}

const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);

// currentProgress: what the task is doing now, from the task state and the
// latest event (the last step's start time): {kind:'running', state, elapsed}
// (elapsed in seconds, null when the start time is unknown), {kind:'waiting',
// state} (queued, needs_approval, passed, error: no timer) or {kind:'none'}
// (final or unknown state).
export function currentProgress(data, now) {
  const t = data && isObj(data.task) ? data.task : {};
  const steps = data && Array.isArray(data.steps) ? data.steps.filter(isObj) : [];
  const lastStep = steps.length ? steps[steps.length - 1] : null;
  const state = typeof t.state === 'string' ? t.state : lastStep && typeof lastStep.state === 'string' ? lastStep.state : '';
  if (!Object.prototype.hasOwnProperty.call(STATE_LABELS, state) || FINAL.has(state)) return { kind: 'none' };
  if (!RUNNING.has(state)) return { kind: 'waiting', state };
  let elapsed = null;
  const at = lastStep && lastStep.state === state ? Date.parse(lastStep.at) : NaN;
  if (now instanceof Date && Number.isFinite(at) && Number.isFinite(now.getTime())) elapsed = Math.max(0, (now.getTime() - at) / 1000);
  return { kind: 'running', state, elapsed };
}

function renderProgress(p) {
  if (p.kind === 'none') return null;
  if (p.kind === 'waiting') {
    return el('p', { class: ['progress', 'wait'] }, [el('b', {}, '대기 중'), ' · ' + stateLabel(p.state)]);
  }
  return el('p', { class: ['progress', 'run'] }, [
    el('b', {}, '진행 중'),
    ' · ' + stateLabel(p.state) + (p.elapsed === null ? '' : ' · ' + formatSeconds(p.elapsed) + ' 경과'),
  ]);
}

// Fixed confirmation texts (the impact of each action).
const CONFIRM = {
  cancel: {
    title: '이 작업을 취소할까요?',
    impact: (state) => (state === 'queued'
      ? '대기 중인 작업이 취소됩니다. 취소는 되돌릴 수 없습니다.'
      : '진행 중인 단계는 끝난 뒤 결과가 버려집니다. 취소는 되돌릴 수 없습니다.'),
    run: '취소 실행',
    open: '작업 취소',
    key: 'act-cancel',
  },
  retry: {
    title: '이 작업을 다시 시도할까요?',
    impact: () => '실패한 단계부터 다시 실행합니다. 시도 횟수와 오류 문구는 초기화되고 기준·head 커밋은 그대로입니다. 인프라 오류로 실패한 작업만 가능하며 재시도 횟수에는 한도가 있습니다.',
    run: '재시도 실행',
    open: '다시 시도',
    key: 'act-retry',
  },
};

// actionResult maps a failed cancel/retry to a FIXED text. The one server
// sentence shown is the retry `reason` (built by the server from fixed
// templates), as sanitized text. reload: the page should refetch the task.
export function actionResult(kind, err) {
  const code = err && typeof err.code === 'string' ? err.code : '';
  const status = err && typeof err.status === 'number' ? err.status : 0;
  if (code === 'state_changed') {
    return { text: '그 사이 작업 상태가 바뀌었습니다. 화면을 새로 고쳤습니다. 현재 상태를 확인하고 다시 시도하세요.', reload: true };
  }
  if (kind === 'cancel' && code === 'already_final') return { text: '이미 끝난 작업이라 취소할 수 없습니다. 화면을 새로 고쳤습니다.', reload: true };
  if (kind === 'retry' && code === 'not_retryable') {
    const reason = err && typeof err.reason === 'string' ? inline(err.reason, 300) : '';
    return { text: reason ? '다시 시도할 수 없습니다: ' + reason : '다시 시도할 수 없는 작업입니다.', reload: true };
  }
  if (status === 404) return { text: '작업을 찾을 수 없습니다.', reload: false };
  return { text: errorText(err), reload: false };
}

// actionAllowed: the server's flag for kind, honoured only when exactly true.
export function actionAllowed(t, kind) {
  if (!t || typeof t !== 'object') return false;
  return kind === 'cancel' ? t.cancellable === true : kind === 'retry' ? t.retryable === true : false;
}

// actionBlock: the cancel/retry buttons (by the server's exact-true flags)
// and, instead of a button, its in-page confirmation. act: {open, busy, msg}.
// The "돌아가기" button shares the opener's focus key so focus lands on it
// when the confirmation opens (replaceContent restores focus by key).
function actionBlock(t, extras) {
  const act = extras.act && typeof extras.act === 'object' ? extras.act : {};
  const kinds = ['cancel', 'retry'].filter((k) => actionAllowed(t, k));
  const out = [];
  for (const kind of kinds) {
    const c = CONFIRM[kind];
    if (act.open === kind) {
      // Escape anywhere in the confirmation is "돌아가기" (nothing is sent).
      const onKey = (ev) => {
        if (!ev || ev.key !== 'Escape') return;
        if (typeof ev.preventDefault === 'function') ev.preventDefault();
        if (extras.onClose) extras.onClose();
      };
      out.push(el('div', { class: 'confirm', role: 'group', 'aria-label': c.title, on: { keydown: onKey } }, [
        el('p', { class: 'q' }, c.title),
        el('p', { class: 'meaning' }, c.impact(t.state)),
        el('div', { class: 'row' }, [
          el('button', {
            type: 'button', class: ['btn', 'primary'], 'data-focus-key': 'confirm-' + kind,
            'aria-disabled': act.busy ? 'true' : 'false', on: { click: () => extras.onRun && extras.onRun(kind) },
          }, act.busy ? '처리 중…' : c.run),
          el('button', {
            type: 'button', class: 'btn', 'data-focus-key': c.key, 'aria-disabled': act.busy ? 'true' : 'false',
            on: { click: () => extras.onClose && extras.onClose() },
          }, '돌아가기'),
        ]),
      ]));
    } else {
      out.push(el('button', {
        type: 'button', class: 'btn', 'data-focus-key': c.key, 'aria-disabled': act.busy ? 'true' : 'false',
        on: { click: () => extras.onOpen && extras.onOpen(kind) },
      }, c.open));
    }
  }
  return out;
}

// renderTask: data is the events response {task, steps, verify}. extras:
// {report, approval, now, onLoadReport}.
export function renderTask(data, extras) {
  extras = extras || {};
  const t = data && typeof data.task === 'object' && data.task ? data.task : {};
  const id = idOf(t.id);
  const head = typeof t.head_sha === 'string' && HEX_RE.test(t.head_sha) ? t.head_sha.slice(0, 12) : '';
  const waiting = t.state === 'needs_approval';
  const ap = extras.approval && typeof extras.approval === 'object' ? extras.approval : null;

  const act = extras.act && typeof extras.act === 'object' ? extras.act : {};
  const actionNodes = actionBlock(t, extras);
  const actions = el('div', { class: 'filters' }, [
    ...actionNodes.filter((n) => n.tagName === 'BUTTON'),
    id && head ? el('a', { class: 'btn', href: '#/diff/' + id, 'data-focus-key': 'to-diff' }, '변경 내용(diff) 보기') : null,
    !head ? el('span', { class: 'note' }, 'head 커밋이 아직 없어 diff를 볼 수 없습니다.') : null,
    el('a', { class: 'btn', href: '#/tasks' }, '← 목록'),
  ]);

  const approval = waiting
    ? el('div', { class: 'todo', role: 'status' }, [
      el('span', {}, [
        el('b', {}, '사람의 승인을 기다리는 중'),
        ap && typeof ap.reason === 'string' ? ' · ' + inline(ap.reason, 160) : null,
      ]),
      el('a', { href: ap && idOf(ap.id) ? '#/approvals/' + idOf(ap.id) : '#/approvals', 'data-focus-key': 'to-approvals' }, '승인 화면으로 →'),
    ])
    : null;

  const reason = typeof t.error === 'string' && t.error
    ? el('div', { class: 'sec' }, [
      el('h2', {}, t.state === 'failed' ? '실패 이유' : '오류'),
      textBlock(t.error, '실패 또는 오류 이유', { chunk: 40 }),
    ])
    : null;

  const main = el('div', { class: ['panel', 'canvas'] }, [
    el('h1', { id: 'h-task' }, [id ? '작업 #' + id : '작업', ' ', stateChip(t.state)]),
    el('p', { class: 'lead' }, [inline(t.repo, 80), ' · ', inline(t.base, 60)]),
    approval,
    renderProgress(currentProgress(data, extras.now)),
    actions,
    ...actionNodes.filter((n) => n.tagName !== 'BUTTON'),
    act.msg ? el('p', { class: ['note', act.msg.kind === 'ok' ? 'ok' : 'err'], role: act.msg.kind === 'ok' ? 'status' : 'alert', 'data-focus-key': 'act-msg' }, act.msg.text) : null,
    mrBlock(t, extras),
    el('div', { class: 'sec' }, [el('h2', {}, '시간 분포'), renderTimeline(data && data.steps, { now: extras.now })]),
    reason,
    el('div', { class: 'sec' }, [el('h2', {}, '요청 내용'), textBlock(t.prompt, '요청 내용', { chunk: 40 })]),
    el('div', { class: 'sec' }, [el('h2', {}, '검증 보고서'), reportBlock(extras.report, extras.onLoadReport)]),
  ]);

  const info = el('aside', { class: ['panel', 'insp'], 'aria-label': '작업 정보' }, [
    el('h2', {}, '정보'),
    el('dl', {}, [
      el('dt', {}, '종류'), el('dd', {}, inline(t.class, 40)),
      el('dt', {}, '에이전트'), el('dd', {}, inline(t.agent, 40)),
      el('dt', {}, '시도'), el('dd', {}, Number.isFinite(t.attempts) ? String(t.attempts) : '-'),
      el('dt', {}, '재시도'), el('dd', {}, t.retries === undefined ? '0' : Number.isSafeInteger(t.retries) && t.retries >= 0 ? String(t.retries) : '-'),
      el('dt', {}, '마지막 변경'), el('dd', { class: 'mono' }, typeof t.actor === 'string' && t.actor ? inline(t.actor, 80) : '-'),
      el('dt', {}, 'head'), el('dd', { class: 'mono' }, head || '-'),
      el('dt', {}, '판정'), el('dd', {}, t.verdict ? inline(t.verdict, 40) : '-'),
      el('dt', {}, '만든 때'), el('dd', { class: 'mono' }, formatTime(t.created_at)),
      el('dt', {}, '갱신'), el('dd', { class: 'mono' }, formatTime(t.updated_at)),
    ]),
    el('div', { class: 'sec' }, [el('h2', {}, '검증 시간 요약'), verifyBlock(data && data.verify)]),
    el('div', { class: 'sec' }, [el('h2', {}, '단계 기록'), renderSteps(data && data.steps)]),
  ]);
  return el('div', { class: 'ws' }, [main, info]);
}

export function taskView(ctx) {
  const id = idOf(ctx.params && ctx.params[0]);
  ctx.setCrumb(id ? '작업 #' + id : '작업');
  if (!id) {
    ctx.root.append(errorPane({ status: 404 }, '#/tasks', '← 작업 목록'));
    return;
  }
  const st = { data: null, approval: null, report: { status: 'idle' }, act: { open: null, busy: false, msg: null }, mr: { busy: false, msg: null } };
  const paint = () => replaceContent(ctx.root, renderTask(st.data, {
    report: st.report, approval: st.approval, now: new Date(), onLoadReport: loadReport,
    act: st.act, onOpen: openConfirm, onClose: closeConfirm, onRun: runAction,
    mr: st.mr, onRetryMR: retryMR,
  }));
  async function loadReport() {
    st.report = { status: 'loading' };
    paint();
    try {
      const rep = await ctx.get('/api/v1/tasks/' + id + '/report');
      if (ctx.signal && ctx.signal.aborted) return;
      st.report = { status: 'ok', text: JSON.stringify(rep, null, 2) };
    } catch (err) {
      if (ctx.signal && ctx.signal.aborted) return;
      st.report = { status: 'error', err, notFound: err && err.status === 404 };
    }
    paint();
  }

  // fetchData reads the task page data (events and, while waiting, the reason).
  async function fetchData() {
    const data = await ctx.get('/api/v1/tasks/' + id + '/events', { polling: true });
    let approval = null;
    if (data && data.task && data.task.state === 'needs_approval') {
      try {
        const list = await ctx.get('/api/v1/approvals', { polling: true });
        approval = (Array.isArray(list) ? list : []).find((a) => a && String(a.task_id) === id) || null;
      } catch { /* the banner works without the reason */ }
    }
    return { data, approval };
  }
  let last = '';
  const keyOf = ({ data, approval }) => {
    const live = data && Array.isArray(data.steps) && data.steps.length && RUNNING.has(data.steps[data.steps.length - 1].state);
    return JSON.stringify([data, approval, live ? Math.floor(Date.now() / POLL_MS) : 0]);
  };

  // cancel / retry: ask first (nothing is sent), then ONE request; the busy
  // flag is set before the first await so a double click sends once.
  function openConfirm(kind) {
    if (st.act.busy) return;
    st.act = { open: kind, busy: false, msg: null };
    paint();
  }
  function closeConfirm() {
    if (st.act.busy) return;
    st.act = { open: null, busy: false, msg: null };
    paint();
  }
  async function runAction(kind) {
    if (st.act.busy || st.act.open !== kind) return;
    st.act = { open: kind, busy: true, msg: null };
    paint();
    let msg;
    let reload = true;
    try {
      await ctx.post('/api/v1/tasks/' + id + '/' + kind, {});
      msg = { kind: 'ok', text: kind === 'cancel' ? '취소를 요청했습니다. 화면을 새로 고쳤습니다.' : '다시 시도를 요청했습니다. 실패한 단계부터 다시 진행됩니다.' };
    } catch (err) {
      const r = actionResult(kind, err);
      msg = { kind: 'error', text: r.text };
      reload = r.reload;
    }
    if (ctx.signal && ctx.signal.aborted) return;
    st.act = { open: null, busy: false, msg };
    if (reload) {
      try {
        const r = await fetchData();
        if (ctx.signal && ctx.signal.aborted) return;
        st.data = r.data;
        st.approval = r.approval;
        last = keyOf(r);
      } catch { /* the next poll shows the failure */ }
    }
    paint();
  }

  // retryMR: ONE request (the busy flag is set before the first await), then
  // the task is refetched so the new merge request state shows.
  async function retryMR() {
    if (st.mr.busy) return;
    st.mr = { busy: true, msg: null };
    paint();
    let msg;
    let reload = true;
    try {
      await ctx.post('/api/v1/tasks/' + id + '/mr', {}, { noRetry429: true });
      msg = { kind: 'ok', text: '병합 요청을 만들었습니다.' };
    } catch (err) {
      const r = mrRetryResult(err);
      msg = { kind: 'error', text: r.text };
      reload = r.reload;
    }
    if (ctx.signal && ctx.signal.aborted) return;
    st.mr = { busy: false, msg };
    if (reload) {
      try {
        const r = await fetchData();
        if (ctx.signal && ctx.signal.aborted) return;
        st.data = r.data;
        st.approval = r.approval;
        last = keyOf(r);
      } catch { /* the next poll shows the failure */ }
    }
    paint();
  }

  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  ctx.poll(async () => {
    try {
      const r = await fetchData();
      if (ctx.signal && ctx.signal.aborted) return;
      const key = keyOf(r);
      if (st.data && key === last) return;
      last = key;
      st.data = r.data;
      st.approval = r.approval;
      // The flag went away while the confirmation was open (the task moved
      // on): the confirmation disappears, so forget it too; otherwise it would
      // pop up again if the flag came back later.
      if (st.act.open && !st.act.busy && !actionAllowed(r.data && r.data.task, st.act.open)) {
        st.act = { open: null, busy: false, msg: st.act.msg };
      }
      paint();
    } catch (err) {
      if (!(ctx.signal && ctx.signal.aborted) && !st.data) replaceContent(ctx.root, errorPane(err, '#/tasks', '← 작업 목록'));
      throw err;
    }
  }, POLL_MS);
}

registerView('task', taskView);
