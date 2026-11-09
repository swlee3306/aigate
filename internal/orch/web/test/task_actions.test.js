import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { findAll, byClass, allElements } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const T = await import('../static/views/task.js');
const { renderTask, taskView, currentProgress } = T;

const text = (n) => n.textContent;
const buttons = (root) => findAll(root, (e) => e.tagName === 'BUTTON');
const btn = (root, label) => buttons(root).find((b) => text(b) === label);
const click = (node) => { for (const fn of node.listeners.click || []) fn({ target: node, preventDefault() {} }); };
const noHostile = (root) => {
  for (const e of allElements(root)) assert.ok(!['IMG', 'SCRIPT', 'SVG', 'IFRAME', 'STYLE'].includes(e.tagName), 'element ' + e.tagName + ' from data');
};

const running = fx('events-running.json'); // task 41: agent_running, cancellable, actor session:mac-passkey
const [, retryableTask, queuedTask] = fx('tasks-actions.json');
const at0 = Date.parse(running.steps[2].at);
const failedData = () => ({
  task: retryableTask,
  steps: [...running.steps.slice(0, 3).map((s) => ({ ...s })), { state: 'failed', seconds: 0, note: 'x', at: '2026-11-09T13:03:00Z' }],
  verify: null,
});
const route = (data) => ({ [`/api/v1/tasks/${data.task.id}/events`]: () => structuredClone(data) });

async function mount(data, extraRoutes) {
  const ctx = fakeCtx({ ...route(data), '/api/v1/approvals': [], ...(extraRoutes || {}) }, { params: [String(data.task.id)] });
  taskView(ctx);
  await ctx.settle();
  return ctx;
}
const evCalls = (ctx, id) => ctx.calls.filter((c) => c === `/api/v1/tasks/${id}/events`).length;

test('TestTaskCancelRetryButtons', async () => {
  // buttons follow the server flags
  const r1 = renderTask(running, {});
  assert.ok(btn(r1, '작업 취소'));
  assert.equal(btn(r1, '다시 시도'), undefined);
  const r2 = renderTask(failedData(), {});
  assert.ok(btn(r2, '다시 시도'));
  assert.equal(btn(r2, '작업 취소'), undefined);
  const done = { task: { ...running.task, state: 'done', cancellable: false }, steps: running.steps };
  const r3 = renderTask(done, {});
  assert.equal(btn(r3, '작업 취소'), undefined);
  assert.equal(btn(r3, '다시 시도'), undefined);
  // a flag is only honoured when it is exactly true
  assert.equal(btn(renderTask({ task: { ...running.task, cancellable: 'yes' }, steps: [] }, {}), '작업 취소'), undefined);

  // cancel: first click asks, nothing is sent
  const ctx = await mount(running, { 'POST /api/v1/tasks/41/cancel': () => ({ ...running.task, state: 'cancelled' }) });
  assert.ok(btn(ctx.root, '작업 취소'));
  click(btn(ctx.root, '작업 취소'));
  await ctx.settle();
  assert.equal(ctx.posts.length, 0, 'no request before the confirmation');
  assert.match(text(ctx.root), /진행 중인 단계는 끝난 뒤 결과가 버려집니다/);
  assert.ok(btn(ctx.root, '취소 실행'));
  // going back sends nothing
  click(btn(ctx.root, '돌아가기'));
  assert.equal(btn(ctx.root, '취소 실행'), undefined);
  assert.equal(ctx.posts.length, 0);
  // confirm: one request, then the page is refreshed
  click(btn(ctx.root, '작업 취소'));
  const before = evCalls(ctx, 41);
  click(btn(ctx.root, '취소 실행'));
  await ctx.settle();
  assert.deepEqual(ctx.posts.map((p) => p.path), ['/api/v1/tasks/41/cancel']);
  assert.ok(evCalls(ctx, 41) > before, 'reloaded after the action');
  assert.match(text(ctx.root), /취소를 요청했습니다|취소했습니다/);
  ctx.leave();

  // 409 state_changed: fixed text and a refresh
  const c2 = await mount(running, { 'POST /api/v1/tasks/41/cancel': () => { throw apiError(409, 'state_changed', '<img src=x onerror=1> SECRETWORD'); } });
  click(btn(c2.root, '작업 취소'));
  const b2 = evCalls(c2, 41);
  click(btn(c2.root, '취소 실행'));
  await c2.settle();
  assert.match(text(c2.root), /그 사이 작업 상태가 바뀌었습니다/);
  assert.doesNotMatch(text(c2.root), /SECRETWORD/);
  assert.ok(evCalls(c2, 41) > b2, 'refreshed after state_changed');
  noHostile(c2.root);
  c2.leave();

  // already_final and 404
  for (const [st, code, re] of [[409, 'already_final', /이미 끝난 작업/], [404, 'not_found', /찾을 수 없습니다/], [500, 'internal', /서버에서 오류/]]) {
    const c = await mount(running, { 'POST /api/v1/tasks/41/cancel': () => { throw apiError(st, code, 'SECRETWORD'); } });
    click(btn(c.root, '작업 취소'));
    const bc = evCalls(c, 41);
    click(btn(c.root, '취소 실행'));
    await c.settle();
    assert.match(text(c.root), re, code);
    if (code === 'already_final') assert.ok(evCalls(c, 41) > bc, 'already_final re-reads the task');
    assert.doesNotMatch(text(c.root), /SECRETWORD/);
    c.leave();
  }

  // retry: confirmation, request, the server's reason is shown as TEXT
  const rd = failedData();
  const c3 = await mount(rd, { 'POST /api/v1/tasks/42/retry': () => { throw apiError(409, 'not_retryable', 'm', { reason: '<img src=x onerror=1> 재시도 한도(3회)를 모두 썼습니다‮' }); } });
  click(btn(c3.root, '다시 시도'));
  assert.equal(c3.posts.length, 0);
  assert.match(text(c3.root), /실패한 단계부터 다시 실행/);
  const b3 = evCalls(c3, 42);
  click(btn(c3.root, '재시도 실행'));
  await c3.settle();
  assert.ok(evCalls(c3, 42) > b3, 'not_retryable re-reads the task');
  assert.deepEqual(c3.posts.map((p) => p.path), ['/api/v1/tasks/42/retry']);
  assert.match(text(c3.root), /재시도 한도\(3회\)를 모두 썼습니다/);
  assert.match(text(c3.root), /<img src=x onerror=1>/, 'markup in the reason stays text');
  assert.doesNotMatch(text(c3.root), /‮/);
  noHostile(c3.root);
  c3.leave();
  // not_retryable without a reason: fixed text
  const c4 = await mount(rd, { 'POST /api/v1/tasks/42/retry': () => { throw apiError(409, 'not_retryable', 'SECRETWORD'); } });
  click(btn(c4.root, '다시 시도'));
  const b4 = evCalls(c4, 42);
  click(btn(c4.root, '재시도 실행'));
  await c4.settle();
  assert.ok(evCalls(c4, 42) > b4, 'not_retryable without a reason re-reads the task too');
  assert.match(text(c4.root), /다시 시도할 수 없는 작업입니다/);
  assert.doesNotMatch(text(c4.root), /SECRETWORD/);
  c4.leave();
  // retry success refreshes
  const c5 = await mount(rd, { 'POST /api/v1/tasks/42/retry': () => ({ ...retryableTask, state: 'verifying' }) });
  const b5 = evCalls(c5, 42);
  click(btn(c5.root, '다시 시도'));
  click(btn(c5.root, '재시도 실행'));
  await c5.settle();
  assert.ok(evCalls(c5, 42) > b5);
  assert.match(text(c5.root), /다시 시도를 요청했습니다/);
  c5.leave();
});

test('TestTaskActionsNoDoubleSend', async () => {
  let go;
  const held = new Promise((r) => { go = r; });
  const ctx = await mount(running, { 'POST /api/v1/tasks/41/cancel': async () => { await held; return running.task; } });
  click(btn(ctx.root, '작업 취소'));
  const confirmBtn = btn(ctx.root, '취소 실행');
  click(confirmBtn); click(confirmBtn); click(confirmBtn);
  assert.equal(ctx.posts.length, 1, 'double click: one request');
  // the buttons stay marked busy meanwhile (a repaint already happened)
  const again = btn(ctx.root, '취소 실행');
  if (again) { assert.equal(again.getAttribute('aria-disabled'), 'true'); click(again); }
  const open = btn(ctx.root, '작업 취소');
  if (open) click(open);
  assert.equal(ctx.posts.length, 1);
  go();
  await ctx.settle();
  assert.equal(ctx.posts.length, 1);
  ctx.leave();
  // retry too
  let go2;
  const held2 = new Promise((r) => { go2 = r; });
  const c2 = await mount(failedData(), { 'POST /api/v1/tasks/42/retry': async () => { await held2; return retryableTask; } });
  click(btn(c2.root, '다시 시도'));
  const rb = btn(c2.root, '재시도 실행');
  click(rb); click(rb);
  assert.equal(c2.posts.length, 1);
  go2();
  await c2.settle();
  c2.leave();
});

test('TestActorShownAsText', async () => {
  const shown = (data) => text(renderTask(data, {}));
  assert.match(shown(running), /session:mac-passkey/);
  assert.match(shown({ task: retryableTask, steps: [] }), /token:mac-ssh/);
  const hostile = '<img src=x onerror=alert(1)>‮\u001b[31m';
  const root = renderTask({ task: { ...running.task, actor: hostile }, steps: [] }, {});
  noHostile(root);
  assert.match(text(root), /<img src=x onerror=alert\(1\)>/);
  assert.doesNotMatch(text(root), /[‮\u001b]/);
  // absent actor: a dash, never "undefined"
  const none = text(renderTask({ task: { ...running.task, actor: undefined }, steps: [] }, {}));
  assert.doesNotMatch(none, /undefined|null/);
  assert.match(none, /마지막 변경/);
  // retries count is shown as a number only
  assert.match(shown({ task: retryableTask, steps: [] }), /재시도1/);
  assert.doesNotMatch(shown({ task: { ...retryableTask, retries: '<b>9</b>' }, steps: [] }), /<b>/);
});

test('TestTaskProgressShowsCurrentStepElapsed', () => {
  const now = (s) => new Date(at0 + s * 1000);
  const prog = (data, s) => byClass(renderTask(data, { now: now(s) }), 'progress')[0];
  // running: the current step and the elapsed time since its start event
  const p = prog(running, 151);
  assert.ok(p, 'progress shown');
  assert.match(text(p), /진행 중/);
  assert.match(text(p), /에이전트/);
  assert.match(text(p), /151초/);
  assert.match(text(prog(running, 700)), /11분 40초/);
  // clock skew never shows a negative time
  assert.doesNotMatch(text(prog(running, -30)), /-/);
  // other running steps
  for (const st of ['preparing', 'collecting', 'verifying', 'publishing']) {
    const d = { task: { ...running.task, state: st }, steps: running.steps.map((s, i) => (i === 2 ? { ...s, state: st } : s)) };
    assert.match(text(prog(d, 20)), /진행 중/, st);
    assert.match(text(prog(d, 20)), /20초/, st);
  }
  // waiting states: "대기 중", no running timer
  for (const st of ['queued', 'needs_approval', 'error', 'passed']) {
    const d = { task: { ...running.task, state: st, cancellable: false }, steps: running.steps.map((s, i) => (i === 2 ? { ...s, state: st } : s)) };
    const pr = prog(d, 151);
    assert.ok(pr, st);
    assert.match(text(pr), /대기 중/, st);
    assert.doesNotMatch(text(pr), /진행 중|151초|경과/, st);
  }
  // final states: nothing
  for (const st of ['done', 'failed', 'cancelled']) {
    const d = { task: { ...running.task, state: st, cancellable: false }, steps: running.steps.map((s, i) => (i === 2 ? { ...s, state: st } : s)) };
    assert.equal(prog(d, 151), undefined, st);
  }
  // no steps / no timestamp: the step is named, no invented time
  assert.deepEqual(currentProgress({ task: { state: 'agent_running' }, steps: [] }, now(5)), { kind: 'running', state: 'agent_running', elapsed: null });
  assert.deepEqual(currentProgress({ task: { state: 'done' }, steps: [] }, now(5)), { kind: 'none' });
  assert.equal(currentProgress({ task: { state: 'agent_running' }, steps: [{ state: 'agent_running', at: 'garbage' }] }, now(5)).elapsed, null);
  // the timeline agrees: a queued last step is "대기 중", not a running timer
  const q = renderTask({ task: { ...running.task, state: 'queued' }, steps: [{ state: 'queued', seconds: 0, at: running.steps[0].at }] }, { now: now(600) });
  const row = text(byClass(q, 'bar-r').pop());
  assert.match(row, /대기 중/);
  assert.doesNotMatch(row, /진행 중/);
});

test('progress and actions survive a poll re-render; the page polls only the task endpoints', async () => {
  const ctx = await mount(running);
  click(btn(ctx.root, '작업 취소'));
  ctx.pollers[0].pause(); ctx.pollers[0].resume();
  await ctx.settle();
  assert.ok(btn(ctx.root, '취소 실행'), 'an open confirmation is kept when the page refreshes');
  assert.equal(ctx.calls.every((c) => c.startsWith('/api/v1/tasks/41') || c === '/api/v1/approvals'), true);
  ctx.leave();
});

const keydown = (node, key) => {
  let prevented = false;
  for (let n = node; n; n = n.parentNode) {
    for (const fn of (n.listeners && n.listeners.keydown) || []) fn({ key, target: node, preventDefault() { prevented = true; } });
  }
  return prevented;
};

test('TestTaskConfirmEscapeCloses', async () => {
  const ctx = await mount(running, { 'POST /api/v1/tasks/41/cancel': () => running.task });
  click(btn(ctx.root, '작업 취소'));
  const run = btn(ctx.root, '취소 실행');
  assert.ok(run);
  // other keys do nothing
  assert.equal(keydown(run, 'Enter'), false);
  assert.ok(btn(ctx.root, '취소 실행'));
  // Escape from a button inside the confirmation closes it, sends nothing
  assert.equal(keydown(btn(ctx.root, '돌아가기'), 'Escape'), true);
  assert.equal(btn(ctx.root, '취소 실행'), undefined);
  assert.ok(btn(ctx.root, '작업 취소'), 'the opener is back (same focus key)');
  assert.equal(ctx.posts.length, 0);
  // the same for retry
  const c2 = await mount(failedData());
  click(btn(c2.root, '다시 시도'));
  keydown(btn(c2.root, '재시도 실행'), 'Escape');
  assert.equal(btn(c2.root, '재시도 실행'), undefined);
  assert.equal(c2.posts.length, 0);
  ctx.leave();
  c2.leave();
});

test('TestTaskConfirmResetWhenFlagFlips', async () => {
  let flag = true;
  const data = () => ({ ...structuredClone(running), task: { ...structuredClone(running.task), cancellable: flag } });
  const ctx = fakeCtx({ '/api/v1/tasks/41/events': () => data(), '/api/v1/approvals': [] }, { params: ['41'] });
  taskView(ctx);
  await ctx.settle();
  const repoll = async () => { ctx.pollers[0].pause(); ctx.pollers[0].resume(); await ctx.settle(); };
  click(btn(ctx.root, '작업 취소'));
  assert.ok(btn(ctx.root, '취소 실행'));
  // the task moved on: the flag is gone, so is the confirmation
  flag = false;
  await repoll();
  assert.equal(btn(ctx.root, '취소 실행'), undefined);
  assert.equal(btn(ctx.root, '작업 취소'), undefined);
  // the flag comes back: the plain button, NOT the stale confirmation
  flag = true;
  await repoll();
  assert.equal(btn(ctx.root, '취소 실행'), undefined, 'a forgotten confirmation must not pop up again');
  assert.ok(btn(ctx.root, '작업 취소'));
  assert.equal(ctx.posts.length, 0);
  ctx.leave();
});
