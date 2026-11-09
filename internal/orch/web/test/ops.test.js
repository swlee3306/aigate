import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { findAll, byClass, allElements } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const O = await import('../static/views/ops.js');
const { renderOps, opsView, QUOTA_WARN_RATIO, quotaLevel } = O;
const { getView, parseRoute, navItem } = await import('../static/router.js');

const text = (n) => n.textContent;
const noHostile = (root) => {
  for (const e of allElements(root)) assert.ok(!['IMG', 'SCRIPT', 'SVG', 'IFRAME', 'STYLE'].includes(e.tagName), 'element ' + e.tagName + ' from data');
};
const quota = (g, v) => ({ ...fx('ops.json'), cloud: { ...fx('ops.json').cloud, quota: { gigabytes: g, volumes: v } } });
const warns = (root) => byClass(root, 'quota-warn');

test('route and view registration', () => {
  assert.deepEqual(parseRoute('#/ops'), { name: 'ops', params: [] });
  assert.deepEqual(parseRoute('#/new'), { name: 'new', params: [] });
  assert.equal(parseRoute('#/ops/1').name, 'notfound');
  assert.equal(navItem('ops'), 'ops');
  assert.equal(navItem('new'), 'new');
  assert.equal(getView('ops'), opsView);
});

test('TestOpsViewQuotaWarningThreshold', () => {
  assert.equal(QUOTA_WARN_RATIO, 0.8);
  // below the line: no warning, and no cleanup text at all
  const calm = renderOps(fx('ops.json'));
  assert.equal(warns(calm).length, 0);
  assert.doesNotMatch(text(calm), /cleanup-restore-volumes/);
  assert.match(text(calm), /210 \/ 300/, 'in_use + reserved of the limit');
  // exactly 80% warns, one below does not (in_use + reserved over limit)
  assert.equal(warns(renderOps(quota({ limit: 300, in_use: 240, reserved: 0 }, { limit: 20, in_use: 1 }))).length, 1);
  assert.equal(warns(renderOps(quota({ limit: 300, in_use: 239, reserved: 0 }, { limit: 20, in_use: 1 }))).length, 0);
  assert.equal(warns(renderOps(quota({ limit: 300, in_use: 200, reserved: 40 }, { limit: 20, in_use: 1 }))).length, 1, 'reserved counts');
  // gigabytes and volumes are judged independently
  const volOnly = renderOps(quota({ limit: 300, in_use: 10 }, { limit: 20, in_use: 16 }));
  assert.equal(warns(volOnly).length, 1);
  assert.match(text(warns(volOnly)[0]), /볼륨 개수/);
  assert.doesNotMatch(text(warns(volOnly)[0]), /용량/);
  const both = renderOps(fx('ops-quota-warn.json'));
  assert.equal(warns(both).length, 2);
  // the warning names the cleanup script, nothing about running it
  const t = text(both);
  assert.match(t, /80%/);
  assert.match(t, /infra\/cleanup-restore-volumes\.sh/);
  assert.doesNotMatch(t, /sudo|bash |sh -c|\.\/infra/);
  // unlimited never warns, however large the usage
  const unl = renderOps(quota({ limit: -1, in_use: 999999, reserved: 5, unlimited: true }, { limit: -1, in_use: 999999, unlimited: true }));
  assert.equal(warns(unl).length, 0);
  assert.match(text(unl), /제한 없음/);
  assert.equal(quotaLevel({ limit: -1, in_use: 9, unlimited: false }).kind, 'unlimited', 'a negative limit is unlimited too');
  // a zero limit with usage is full
  assert.equal(quotaLevel({ limit: 0, in_use: 1 }).warn, true);
  assert.equal(quotaLevel({ limit: 0, in_use: 0 }).warn, false);
  // bars: CSSOM width, no style attribute, decoration only, text alternative
  const bars = findAll(renderOps(fx('ops.json')), (e) => e.className.split(/\s+/).includes('bar-f'));
  assert.ok(bars.length >= 1);
  for (const b of bars) { assert.match(b.style.width, /^\d+(\.\d+)?%$/); assert.equal(b.hasAttribute('style'), false); }
  assert.equal(findAll(calm, (e) => e.className.split(/\s+/).includes('bar-t'))[0].getAttribute('aria-hidden'), 'true');
  assert.equal(bars.length, 1, 'no bar for an unlimited line');
  // over 100% is capped
  const over = findAll(renderOps(quota({ limit: 10, in_use: 50 }, { limit: -1, unlimited: true })), (e) => e.className.split(/\s+/).includes('bar-f'))[0];
  assert.equal(over.style.width, '100%');

  // every cloud state is shown, and none of them hides the rest of /ops
  const states = [
    ['ops-no-cloud.json', /클라우드 상태.*조회하지 않/s],
    ['ops-cloud-down.json', /클라우드 상태를 지금 읽을 수 없습니다/],
    ['ops-cloud-timeout.json', /클라우드 응답이 늦어/],
    ['ops-vm-unknown.json', /알 수 없음/],
    ['ops.json', /ACTIVE/],
  ];
  for (const [f, re] of states) {
    const r = renderOps(fx(f));
    assert.match(text(r), re, f);
    assert.match(text(r), /rev-abc123/, f + ': the rest of /ops stays');
    assert.match(text(r), /0123abcd/, f);
  }
  assert.match(text(renderOps(fx('ops-vm-unknown.json'))), /서버를 읽지 못했/);
  assert.equal(warns(renderOps(fx('ops-vm-unknown.json'))).length, 0, '150/300 and 4/20: no warning');
  // a failed cloud is not a failure of the page
  assert.doesNotMatch(text(renderOps(fx('ops-cloud-down.json'))), /불러오지 못했습니다/);
  // history
  assert.match(text(renderOps(fx('ops.json'))), /create_volume/);
  // an unknown failure code gets the generic fixed text
  const weird = fx('ops-cloud-down.json');
  weird.cloud.code = '<img src=x onerror=1>';
  assert.match(text(renderOps(weird)), /클라우드 상태를 지금 읽을 수 없습니다/);
  assert.doesNotMatch(text(renderOps(weird)), /onerror/);
});

test('ops view polls /ops only, slowly, and keeps the old data when a poll fails', async () => {
  let fail = false;
  const ctx = fakeCtx({ '/api/v1/ops': () => { if (fail) throw apiError(500, 'internal', 'SECRETWORD'); return fx('ops.json'); } });
  opsView(ctx);
  await ctx.settle();
  assert.deepEqual(ctx.calls, ['/api/v1/ops']);
  assert.ok(ctx.pollMs[0] >= 15000, 'never faster than 15 s: ' + ctx.pollMs[0]);
  assert.match(text(ctx.root), /rev-abc123/);
  fail = true;
  ctx.pollers[0].pause(); ctx.pollers[0].resume();
  await ctx.settle();
  assert.match(text(ctx.root), /rev-abc123/, 'stale data stays on screen');
  ctx.leave();
  // first load fails: a fixed-text pane
  const c2 = fakeCtx({ '/api/v1/ops': () => { throw apiError(500, 'internal', 'SECRETWORD'); } });
  opsView(c2);
  await c2.settle();
  assert.match(text(c2.root), /불러오지 못했습니다/);
  assert.doesNotMatch(text(c2.root), /SECRETWORD/);
  c2.leave();
  // an empty answer (no ops supplier)
  const c3 = fakeCtx({ '/api/v1/ops': {} });
  opsView(c3);
  await c3.settle();
  assert.match(text(c3.root), /운영 정보를 제공하지 않습니다/);
  c3.leave();
});

test('TestOpsViewHostileText', () => {
  const H = '<img src=x onerror=alert(1)>‮\u001b[31m\u0000';
  const o = fx('ops.json');
  o.build = H;
  o.policy_hash = H;
  o.last_restore.phases[0].name = H;
  o.cloud.test_vm = { name: H, status: H };
  o.cloud.history[0].phases[0].name = H;
  o.cloud.quota.gigabytes = { limit: H, in_use: H, reserved: H, unlimited: false };
  o.cloud.quota.volumes = { limit: '<b>9</b>', in_use: NaN, reserved: null, unlimited: false };
  o.cloud.restore_volumes = H;
  o.cloud.checked_at = H;
  const root = renderOps(o);
  noHostile(root);
  assert.match(text(root), /<img src=x onerror=alert\(1\)>/, 'shown literally');
  assert.doesNotMatch(text(root), /[‮\u001b\u0000]/);
  assert.doesNotMatch(text(root), /undefined|NaN|\[object/);
  for (const e of allElements(root)) for (const v of e.attrs.values()) assert.doesNotMatch(v, /onerror|<img/);
  assert.equal(warns(root).length, 0, 'garbage numbers never produce a warning or a bar');
  // numbers: infinities, huge, negative, strings -> a dash, no bar
  for (const bad of [Infinity, -5, 1e300, '12', {}, [3]]) {
    const r = renderOps(quota({ limit: bad, in_use: bad, reserved: bad }, { limit: 5, in_use: bad }));
    noHostile(r);
    assert.doesNotMatch(text(r), /Infinity|undefined|NaN|1e\+300/);
  }
  // wrong container types
  for (const junk of [null, undefined, 5, 'x', [], { cloud: 5 }, { cloud: { ok: true, quota: 'x', history: 'y', test_vm: 7 } }, { cloud: { ok: true, history: [null, 3, { phases: 'z' }] } }]) {
    noHostile(renderOps(junk));
  }
});
