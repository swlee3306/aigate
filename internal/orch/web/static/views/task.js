// task.js: task detail (#/tasks/ID): header, timeline bars from
// GET /api/v1/tasks/{id}/events, verify timing, links to the diff and the
// approval screen, and the saved report as TEXT (loaded on demand).

import {
  el, inline, stateChip, idOf, formatTime, formatSeconds, errorPane, errorText, textBlock, chunked,
  STATE_LABELS, stateLabel,
} from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';

const POLL_MS = 10000;
export const MIN_BAR_PCT = 2; // every stage stays visible
const FINAL = new Set(['done', 'failed', 'cancelled']);
// RUNNING states have a live timer; needs_approval, passed and error are
// waiting for a person or a retry, so no growing timer is shown for them.
const RUNNING = new Set(['queued', 'preparing', 'agent_running', 'collecting', 'verifying', 'publishing']);
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

// renderTimeline: one row per step (label, bar, duration text). The bars are
// decoration (aria-hidden); the label and the duration text carry the data.
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
    return el('li', { class: 'bar-r', 'data-step': known ? s.state : '?' }, [
      el('span', {}, label),
      el('div', { class: 'bar-t', 'aria-hidden': 'true' }, fill),
      el('span', { class: 'mono' }, live ? '진행 중 · ' + formatSeconds(secs[i]) : waiting ? '대기 중' : formatSeconds(secs[i])),
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

// renderTask: data is the events response {task, steps, verify}. extras:
// {report, approval, now, onLoadReport}.
export function renderTask(data, extras) {
  extras = extras || {};
  const t = data && typeof data.task === 'object' && data.task ? data.task : {};
  const id = idOf(t.id);
  const head = typeof t.head_sha === 'string' && HEX_RE.test(t.head_sha) ? t.head_sha.slice(0, 12) : '';
  const waiting = t.state === 'needs_approval';
  const ap = extras.approval && typeof extras.approval === 'object' ? extras.approval : null;

  const actions = el('div', { class: 'filters' }, [
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
      el('a', { href: '#/approvals', 'data-focus-key': 'to-approvals' }, '승인 화면으로 →'),
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
    actions,
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
  const st = { data: null, approval: null, report: { status: 'idle' } };
  const paint = () => replaceContent(ctx.root, renderTask(st.data, {
    report: st.report, approval: st.approval, now: new Date(), onLoadReport: loadReport,
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
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  let last = '';
  ctx.poll(async () => {
    try {
      const data = await ctx.get('/api/v1/tasks/' + id + '/events', { polling: true });
      let approval = null;
      if (data && data.task && data.task.state === 'needs_approval') {
        try {
          const list = await ctx.get('/api/v1/approvals', { polling: true });
          approval = (Array.isArray(list) ? list : []).find((a) => a && String(a.task_id) === id) || null;
        } catch { /* the banner works without the reason */ }
      }
      if (ctx.signal && ctx.signal.aborted) return;
      const live = data && Array.isArray(data.steps) && data.steps.length && RUNNING.has(data.steps[data.steps.length - 1].state);
      const key = JSON.stringify([data, approval, live ? Math.floor(Date.now() / POLL_MS) : 0]);
      if (st.data && key === last) return;
      last = key;
      st.data = data;
      st.approval = approval;
      paint();
    } catch (err) {
      if (!(ctx.signal && ctx.signal.aborted) && !st.data) replaceContent(ctx.root, errorPane(err, '#/tasks', '← 작업 목록'));
      throw err;
    }
  }, POLL_MS);
}

registerView('task', taskView);
