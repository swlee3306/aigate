import { test, mock } from 'node:test';
import assert from 'node:assert/strict';
import { installDocument, byClass, findAll, allElements } from './dom-stub.js';

const doc = installDocument();
const { el, renderStageMenu, renderOverview, poll, formatSeconds, formatLive } = await import('../static/render.js');

const HOSTILE = [
  '<img src=x onerror=alert(1)>',
  '</script><script>alert(1)</script>',
  '"><svg onload=alert(1)>',
  'a\u0000b\u0007c\u001b[31m‮',
  '&lt;b&gt;',
];

test('TestElNeverCreatesHTML', () => {
  for (const s of HOSTILE) {
    const n = el('div', {}, s);
    assert.equal(n.children.length, 0, 'no child element for ' + JSON.stringify(s));
    assert.equal(n.childNodes.length, 1);
    assert.equal(n.childNodes[0].nodeType, 3);
    assert.equal(n.textContent, s, 'text kept literally');
    // numbers and arrays of strings are text too
    const m = el('p', null, [s, 7]);
    assert.equal(m.children.length, 0);
    assert.equal(m.textContent, s + '7');
  }
  // hostile text in attribute values stays an attribute value
  const t = el('span', { title: HOSTILE[0], 'data-x': HOSTILE[1] }, 'x');
  assert.equal(t.getAttribute('title'), HOSTILE[0]);
  assert.equal(t.children.length, 0);

  // attribute allowlist
  assert.throws(() => el('a', { onclick: 'alert(1)' }), /not allowed/);
  assert.throws(() => el('a', { onmouseover: () => {} }), /not allowed/);
  assert.throws(() => el('div', { style: 'color:red' }), /not allowed/);
  assert.throws(() => el('div', { srcdoc: '<p>' }), /not allowed/);
  assert.throws(() => el('a', { href: 'javascript:alert(1)' }), /href/);
  assert.throws(() => el('a', { href: ' javascript:alert(1)' }), /href/);
  assert.throws(() => el('a', { href: 'https://evil.example/' }), /href/);
  assert.throws(() => el('a', { href: '//evil.example/' }), /href/);
  assert.throws(() => el('a', { href: '/api/v1/tasks' }), /href/);
  assert.throws(() => el('span', { href: '#/tasks' }), /href/);
  assert.equal(el('a', { href: '#/tasks/38' }, 'x').getAttribute('href'), '#/tasks/38');
  assert.throws(() => el('script', {}, 'alert(1)'), /tag not allowed/);
  assert.throws(() => el('iframe'), /tag not allowed/);
  assert.throws(() => el('img', { src: 'x' }), /tag not allowed/);
  assert.throws(() => el('svg'), /tag not allowed/);
  // listeners: functions only
  assert.throws(() => el('button', { on: { click: 'alert(1)' } }), /listener/);
  let hit = 0;
  const b = el('button', { on: { click: () => hit++ } }, 'ok');
  b.listeners.click[0]();
  assert.equal(hit, 1);
  // booleans and arrays
  const d = el('button', { disabled: true, hidden: false, class: ['a', null, 'b'] }, 'x');
  assert.equal(d.getAttribute('disabled'), '');
  assert.equal(d.hasAttribute('hidden'), false);
  assert.equal(d.getAttribute('class'), 'a b');
});

const STAGES = [
  { id: 'design', name: '설계', status: '⬜', components: [], next: '아직 세션에서 작성', extra: 'ignored' },
  { id: 'implement', name: '구현', status: '🔄', components: ['x'], next: '작업을 제출합니다' },
  { id: 'verify', name: '검증', status: '✅', components: [], next: '' },
  { id: 'approve', name: '승인', status: '🔄', components: [], next: '승인 대기 2건' },
  { id: 'evil', name: '<img src=x onerror=alert(1)>', status: '??', next: '</script>' },
  { id: 5, name: 'bad id' },
  null,
];

test('TestStageMenuFromStages', () => {
  const ul = renderStageMenu(STAGES, { pending: 2 });
  const lis = findAll(ul, (e) => e.tagName === 'LI');
  assert.deepEqual(lis.map((l) => l.getAttribute('data-stage')), ['design', 'implement', 'verify', 'approve', 'evil'],
    'server order kept, malformed entries skipped');
  const names = byClass(ul, 'stg-name').map((e) => e.textContent);
  assert.deepEqual(names, ['설계', '구현', '검증', '승인', '<img src=x onerror=alert(1)>']);
  // hostile data stayed text: only the elements el() made exist
  assert.ok(allElements(ul).every((e) => ['UL', 'LI', 'A', 'SPAN', 'SMALL'].includes(e.tagName)));
  // links only for stages with a route
  const hrefs = findAll(ul, (e) => e.tagName === 'A').map((a) => a.getAttribute('href'));
  assert.deepEqual(hrefs, ['#/tasks', '#/tasks', '#/approvals']);
  // status emoji from the server becomes a word, unknown status shown as is
  const chips = byClass(ul, 'chip').map((e) => e.textContent);
  assert.deepEqual(chips, ['시작 전', '진행 중', '완료', '진행 중', '??']);
  // the pending badge comes from opts, only on the approve stage
  const badges = byClass(ul, 'badge');
  assert.equal(badges.length, 1);
  assert.equal(badges[0].textContent, '2');
  assert.equal(findAll(ul, (e) => e.getAttribute('data-stage') === 'approve')[0].textContent.includes('2'), true);
  // next text shown, empty one omitted
  assert.equal(byClass(ul, 'stg-next').length, 4);
  // no pending: no badge; non-array input: empty list
  assert.equal(byClass(renderStageMenu(STAGES, { pending: 0 }), 'badge').length, 0);
  assert.equal(renderStageMenu(null).children.length, 0);
  assert.equal(renderStageMenu({ id: 'x' }).children.length, 0);
});

const OV = {
  total: 38,
  by_state: { done: 36, failed: 2, needs_approval: 1, queued: 0 },
  pending_approvals: 1,
  stage_avg: { preparing: 2, agent_running: 25.4, verifying: 134, queued: 0.2 },
  todo: [{ kind: 'approval', task: 5, approval: 9, text: '작업 #5 승인 대기: <img src=x onerror=alert(1)>' }],
};

test('TestOverviewShowsTodo', () => {
  const root = renderOverview(OV);
  const bars = byClass(root, 'todo');
  assert.equal(bars.length, 1, 'exactly one emphasised todo bar');
  assert.match(bars[0].textContent, /사람이 볼 것 1건/);
  assert.ok(bars[0].textContent.includes('<img src=x onerror=alert(1)>'), 'agent text literal');
  assert.equal(allElements(root).filter((e) => e.tagName === 'IMG').length, 0);
  assert.equal(findAll(bars[0], (e) => e.tagName === 'A')[0].getAttribute('href'), '#/approvals');
  // exactly one big number, and it is the pending count
  const ones = byClass(root, 'one');
  assert.equal(ones.length, 1);
  assert.equal(ones[0].textContent, '1건');
  assert.match(byClass(root, 'lead')[0].textContent, /작업 38건 중 36건 완료, 2건 실패\./);
  // pipeline nodes carry the average times, quiet metrics
  const metric = (state) => byClass(findAll(root, (e) => e.getAttribute('data-node') === state)[0], 'm')[0].textContent;
  assert.equal(metric('agent_running'), '평균 25초');
  assert.equal(metric('preparing'), '평균 2.0초');
  assert.equal(metric('done'), '36건');

  // nothing waiting: no bar, no big number
  const calm = renderOverview({ ...OV, pending_approvals: 0, todo: [], by_state: { done: 3 }, total: 3 });
  assert.equal(byClass(calm, 'todo').length, 0);
  assert.equal(byClass(calm, 'one').length, 0);
  assert.match(byClass(calm, 'lead')[0].textContent, /작업 3건 중 3건 완료\./);
  // todo without pending_approvals still counts
  assert.equal(byClass(renderOverview({ total: 1, todo: [{ text: 'x' }] }), 'todo').length, 1);
  // empty and malformed input does not throw
  assert.match(byClass(renderOverview({}), 'lead')[0].textContent, /아직 작업이 없습니다/);
  renderOverview(null);
  renderOverview({ by_state: 'x', stage_avg: [], todo: 'y', total: 'z' });
  assert.equal(formatSeconds(NaN), '-');
  assert.equal(formatSeconds(700), '11분 40초');
});

// ---- poll --------------------------------------------------------------

function pollEnv() {
  mock.timers.enable({ apis: ['setTimeout'] });
  doc.visibilityState = 'visible';
  let calls = 0;
  let running = 0;
  let maxRunning = 0;
  const gates = [];
  const fn = () => {
    calls++;
    running++;
    maxRunning = Math.max(maxRunning, running);
    return new Promise((resolve, reject) => {
      gates.push({ ok: () => { running--; resolve(); }, fail: () => { running--; reject(new Error('x')); } });
    });
  };
  return { fn, get calls() { return calls; }, get maxRunning() { return maxRunning; }, gates };
}
const flush = async () => { for (let i = 0; i < 5; i++) await Promise.resolve(); };

test('TestPollPauseStopsFetch', async (t) => {
  t.after(() => mock.timers.reset());
  const e = pollEnv();
  const seen = [];
  const p = poll(e.fn, 1000, { onUpdate: (u) => seen.push(u.state), now: () => new Date(2026, 10, 9, 12, 31, 8) });
  mock.timers.tick(0);
  assert.equal(e.calls, 1, 'first fetch immediately');
  // a slow request: ticking past the interval never starts a second one
  mock.timers.tick(5000);
  assert.equal(e.calls, 1, 'never overlaps');
  e.gates[0].ok();
  await flush();
  assert.ok(p.lastUpdated instanceof Date);
  assert.match(formatLive('running', p.lastUpdated, 0), /12:31:08/);
  mock.timers.tick(999);
  assert.equal(e.calls, 1);
  mock.timers.tick(1);
  assert.equal(e.calls, 2, 'chained, not an interval');
  e.gates[1].ok();
  await flush();

  // pause: nothing fetches however long we wait
  p.pause();
  assert.equal(p.state, 'paused');
  mock.timers.tick(60000);
  assert.equal(e.calls, 2, 'paused: no fetch');
  // resume: refreshes at once
  p.resume();
  mock.timers.tick(0);
  assert.equal(e.calls, 3, 'resume restarts');
  e.gates[2].ok();
  await flush();

  // hidden: stops; visible again: refreshes
  doc.setVisibility('hidden');
  assert.equal(p.state, 'hidden');
  mock.timers.tick(60000);
  assert.equal(e.calls, 3, 'hidden: no fetch');
  doc.setVisibility('visible');
  mock.timers.tick(0);
  assert.equal(e.calls, 4, 'visible again refreshes');
  assert.equal(e.maxRunning, 1, 'never overlapped');

  // pause while a request is in flight: it settles but nothing follows
  p.pause();
  e.gates[3].ok();
  await flush();
  mock.timers.tick(60000);
  assert.equal(e.calls, 4);
  // resume while paused-then-nothing in flight is fine; stop is final
  p.stop();
  assert.equal(p.state, 'stopped');
  p.resume();
  mock.timers.tick(60000);
  assert.equal(e.calls, 4, 'stopped: no fetch');
  assert.equal(seen.includes('paused'), true);
});

test('poll backs off after errors and recovers', async (t) => {
  t.after(() => mock.timers.reset());
  const e = pollEnv();
  const p = poll(e.fn, 1000);
  mock.timers.tick(0);
  e.gates[0].fail();
  await flush();
  assert.equal(p.failures, 1);
  mock.timers.tick(1999);
  assert.equal(e.calls, 1, 'first retry waits 2x');
  mock.timers.tick(1);
  assert.equal(e.calls, 2);
  e.gates[1].fail();
  await flush();
  mock.timers.tick(3999);
  assert.equal(e.calls, 2, 'then 4x');
  mock.timers.tick(1);
  assert.equal(e.calls, 3);
  e.gates[2].ok();
  await flush();
  assert.equal(p.failures, 0);
  mock.timers.tick(1000);
  assert.equal(e.calls, 4, 'back to the normal interval');
  p.stop();
});
