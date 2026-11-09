import { test, after } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { findAll, allElements } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const R = await import('../static/render.js');
const { el, safeExternalHref } = R;
const { renderTask, taskView } = await import('../static/views/task.js');

const text = (n) => n.textContent;
const buttons = (root) => findAll(root, (e) => e.tagName === 'BUTTON');
const btn = (root, label) => buttons(root).find((b) => text(b) === label);
const links = (root) => findAll(root, (e) => e.tagName === 'A');
const click = (node) => { for (const fn of node.listeners.click || []) fn({ target: node, preventDefault() {} }); };
const noHostile = (root) => {
  for (const e of allElements(root)) assert.ok(!['IMG', 'SCRIPT', 'SVG', 'IFRAME', 'STYLE'].includes(e.tagName), 'element ' + e.tagName + ' from data');
};
const gate = () => { let go; const p = new Promise((r) => { go = r; }); return { p, go }; };

test('TestMRLinkOnlyForSafeUrls', () => {
  const good = [
    'http://100.64.0.7:18082/lab/toy/-/merge_requests/7',
    'http://100.100.1.2:18082/x',
    'https://gitlab.example.com/group/proj/-/merge_requests/12',
    'HTTPS://gitlab.example.com/a',
  ];
  for (const u of good) assert.equal(safeExternalHref(u), u, u);
  const bad = [
    'javascript:alert(1)', 'JaVaScRiPt:alert(1)', ' javascript:alert(1)', 'data:text/html,<script>alert(1)</script>',
    'vbscript:x', 'file:///etc/passwd', 'ftp://example.com/x', '//example.com/x', '/relative/path', '#/tasks/1', 'example.com/x',
    'http://user:pw@100.64.0.7:18082/x', 'http://user@100.64.0.7/x', 'https://:pw@example.com/x',
    'http://example.com/a b', 'http://example.com/a\nb', 'http://example.com/a\tb', 'http://example.com/\u0000', 'http://example.com/\u001b[31m',
    'http://example.com/\u007f', 'http://example.com/\u0085', 'http://example.com/‮evil', 'http://exa​mple.com/', 'http://example.com/한글',
    'http://example.com\\@evil.com/', 'http:///nohost', 'http://', 'https:example.com',
    'http://example.com/' + 'a'.repeat(500), '', null, undefined, 42, {}, [], ['http://example.com/'],
  ];
  for (const u of bad) assert.equal(safeExternalHref(u), null, JSON.stringify(u));
  // the length limit is on the whole string: 500 is fine, 501 is not
  const base = 'http://example.com/';
  assert.equal(safeExternalHref(base + 'a'.repeat(500 - base.length)), base + 'a'.repeat(500 - base.length));
  assert.equal(safeExternalHref(base + 'a'.repeat(501 - base.length)), null);
  // el() builds an external link only through that gate
  const a = el('a', { href: good[0], target: '_blank', rel: 'noopener noreferrer' }, 'x');
  assert.equal(a.getAttribute('href'), good[0]);
  assert.throws(() => el('a', { href: 'javascript:alert(1)' }), /href/);
  assert.throws(() => el('a', { href: 'http://u:p@example.com/' }), /href/);
  assert.throws(() => el('a', { href: 'http://example.com/a b' }), /href/);
  assert.throws(() => el('span', { href: good[0] }), /href/);
  assert.throws(() => el('a', { href: good[0] }), /href/, 'an external address is never linked without the fixed target and rel');
  assert.throws(() => el('a', { href: good[0], rel: 'noopener noreferrer' }), /href/);
});

test('TestExternalLinkAttributesFixed', () => {
  const href = 'https://gitlab.example.com/g/p/-/merge_requests/1';
  const ok = el('a', { href, target: '_blank', rel: 'noopener noreferrer' }, 'MR');
  assert.equal(ok.getAttribute('target'), '_blank');
  assert.equal(ok.getAttribute('rel'), 'noopener noreferrer');
  for (const t of ['_self', '_top', '_parent', 'evil', '', '_BLANK', '_blank ', true, 1]) {
    assert.throws(() => el('a', { href, target: t, rel: 'noopener noreferrer' }), /target/, 'target ' + JSON.stringify(t));
  }
  for (const r of ['noopener', 'noreferrer', 'opener', 'noopener  noreferrer', 'noreferrer noopener', 'nofollow', '', true]) {
    assert.throws(() => el('a', { href, target: '_blank', rel: r }), /rel/, 'rel ' + JSON.stringify(r));
  }
  // only on <a>, and a _blank target without the fixed rel is refused
  assert.throws(() => el('span', { target: '_blank' }), /target/);
  assert.throws(() => el('span', { rel: 'noopener noreferrer' }), /rel/);
  assert.throws(() => el('button', { target: '_blank', rel: 'noopener noreferrer' }), /target|rel/);
  assert.throws(() => el('a', { href, target: '_blank' }), /rel/, 'target needs the fixed rel');
  // other link-ish attributes stay refused
  for (const name of ['download', 'ping', 'referrerpolicy', 'src', 'srcdoc', 'formaction', 'onclick', 'style']) {
    assert.throws(() => el('a', { href, [name]: 'x' }), /not allowed/, name);
  }
});

// ---- the task page ----------------------------------------------------------

const created = fx('task-mr-created.json');
const failed = fx('task-mr-failed.json');
const skipped = fx('task-mr-skipped.json');
const none = fx('task-mr-none.json');
const route = (d) => ({ [`/api/v1/tasks/${d.task.id}/events`]: () => structuredClone(d), '/api/v1/approvals': [] });
const mounted = [];
after(() => mounted.forEach((c) => c.leave())); // stop the task pollers so the process can exit
async function mount(data, extra) {
  const ctx = fakeCtx({ ...route(data), ...(extra || {}) }, { params: [String(data.task.id)] });
  mounted.push(ctx);
  taskView(ctx);
  await ctx.settle();
  return ctx;
}
const mrSection = (root) => findAll(root, (e) => e.getAttribute('data-section') === 'mr')[0];

test('TestMRStatesRendered', async () => {
  // created: a link to the safe URL with fixed attributes
  const r = renderTask(created, {});
  const sec = mrSection(r);
  assert.ok(sec, 'MR section');
  const a = links(sec);
  assert.equal(a.length, 1);
  assert.equal(a[0].getAttribute('href'), created.task.mr_url);
  assert.equal(a[0].getAttribute('target'), '_blank');
  assert.equal(a[0].getAttribute('rel'), 'noopener noreferrer');
  assert.match(text(a[0]), /병합 요청/);
  assert.match(text(a[0]), /7/);
  assert.doesNotMatch(text(a[0]), /100\.64/, 'link text is fixed text, not the server URL');
  assert.match(text(sec), /초안|Draft/);
  assert.equal(btn(sec, '다시 시도'), undefined);

  // created with an unsafe URL: no link at all, a fixed sentence
  for (const u of ['javascript:alert(1)', 'data:text/html,x', 'http://u:p@h/x', 'http://h/a b', '']) {
    const c = renderTask({ ...created, task: { ...created.task, mr_url: u } }, {});
    assert.equal(links(mrSection(c)).length, 0, JSON.stringify(u));
    assert.match(text(mrSection(c)), /GitLab에서 확인/);
    assert.doesNotMatch(text(c), /javascript|data:text/);
  }

  // skipped: a short note, no button, no link
  const s = mrSection(renderTask(skipped, {}));
  if (s) {
    assert.ok(text(s).length < 120);
    assert.equal(buttons(s).length, 0);
    assert.equal(links(s).length, 0);
  }
  // nothing at all when there is no merge request state
  assert.equal(mrSection(renderTask(none, {})), undefined);
  assert.equal(mrSection(renderTask({ task: { ...none.task, mr_state: 'weird' } }, {})), undefined);
  assert.equal(mrSection(renderTask({ task: { ...created.task, state: 'running', mr_state: undefined } }, {})), undefined);

  // failed: fixed sentence (never the server's code text) and a retry button
  const f = mrSection(renderTask({ ...failed, task: { ...failed.task, mr_code: '<img src=x onerror=alert(1)>' } }, {}));
  assert.match(text(f), /병합 요청을 만들지 못했습니다/);
  assert.doesNotMatch(text(f), /onerror|<img/, 'the code is not echoed unless it is a known one');
  assert.ok(btn(f, '다시 시도'));
  noHostile(f);
  const known = mrSection(renderTask(failed, {}));
  assert.match(text(known), /병합 요청을 만들지 못했습니다/);

  // retry: ONE POST on a double click, then the page is refreshed with the new state
  const g = gate();
  let ctx2done = false;
  const ctx = await mount(failed, {
    'POST /api/v1/tasks/52/mr': async () => { await g.p; return created.task; },
    '/api/v1/tasks/52/events': () => (ctx2done ? { ...created, task: { ...created.task, id: 52 } } : structuredClone(failed)),
  });
  const b = btn(ctx.root, '다시 시도');
  assert.ok(b);
  click(b); click(b); click(b);
  assert.equal(ctx.posts.length, 1, 'one request');
  assert.equal(ctx.posts[0].path, '/api/v1/tasks/52/mr');
  assert.deepEqual(ctx.posts[0].body, {});
  const b2 = btn(ctx.root, '다시 시도') || btn(ctx.root, '처리 중…');
  assert.equal(b2.getAttribute('aria-disabled'), 'true');
  assert.equal(b2.hasAttribute('disabled'), false);
  ctx2done = true;
  g.go();
  await ctx.settle();
  assert.equal(ctx.posts.length, 1);
  const after = mrSection(ctx.root);
  assert.equal(links(after).length, 1, 'the link appears once the retry created it');
  assert.equal(btn(after, '다시 시도'), undefined);
});

test('TestMRRetryErrorsFixedTexts', async () => {
  const SECRET = '<img src=x onerror=alert(1)> SECRETWORD /srv/private';
  const cases = [
    [409, 'mr_not_applicable', /만들 수 없습니다/],
    [409, 'mr_already_created', /이미 만들어/],
    [502, 'upstream_error', /만들지 못했습니다/],
    [404, 'not_found', /찾을 수 없습니다/],
    [429, 'rate_limited', /요청이 많아/],
    [0, 'network', /서버에 연결하지 못했습니다/],
    [500, 'internal', /서버에서 오류가 발생했습니다/],
  ];
  for (const [status, code, re] of cases) {
    const ctx = await mount(failed, { 'POST /api/v1/tasks/52/mr': () => { throw apiError(status, code, SECRET, { reason: SECRET }); } });
    click(btn(ctx.root, '다시 시도'));
    await ctx.settle();
    const s = text(mrSection(ctx.root));
    assert.match(s, re, code);
    assert.doesNotMatch(s, /SECRETWORD|\/srv\/private|onerror/, code + ': server text never shown');
    noHostile(ctx.root);
    // usable again (a real failure may be retried), never a real disabled attribute
    const again = btn(ctx.root, '다시 시도');
    assert.ok(again, code + ': button present');
    assert.equal(again.getAttribute('aria-disabled'), 'false');
    assert.equal(ctx.posts.length, 1);
    assert.equal(ctx.posts[0].opts && ctx.posts[0].opts.noRetry429, true, 'no automatic 429 retry');
  }
});
