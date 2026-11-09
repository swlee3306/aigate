import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { findAll, allElements } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const N = await import('../static/views/new.js');
const { assemblePrompt, newIdempotencyKey, PromptError, PROMPT_MAX_BYTES, newView } = N;
const { getView } = await import('../static/router.js');

const text = (n) => n.textContent;
const byId = (root, id) => findAll(root, (e) => e.getAttribute('id') === id)[0];
const fire = (node, ev) => { for (const fn of node.listeners[ev] || []) fn({ target: node, preventDefault() {} }); };
const setField = (root, id, v) => { const n = byId(root, id); n.value = v; fire(n, 'input'); fire(n, 'change'); return n; };
const click = (node) => { for (const fn of node.listeners.click || []) fn({ target: node, preventDefault() {} }); };
const noHostile = (root) => {
  for (const e of allElements(root)) assert.ok(!['IMG', 'SCRIPT', 'SVG', 'IFRAME', 'STYLE'].includes(e.tagName), 'element ' + e.tagName + ' from data');
};
const gate = () => { let go; const p = new Promise((r) => { go = r; }); return { p, go }; };

// ---- assemblePrompt -------------------------------------------------------

test('TestAssemblePromptTemplateAndLimits', () => {
  assert.equal(
    assemblePrompt({ goal: 'A', scope: 'B', done: 'C', avoid: 'D' }),
    'Goal: A\nScope: B\nDone when: C\nDo not: D',
  );
  // empty and whitespace-only fields are omitted; the others are trimmed
  assert.equal(assemblePrompt({ goal: '  A  ', scope: '', done: '   ', avoid: 'D' }), 'Goal: A\nDo not: D');
  assert.equal(assemblePrompt({ goal: 'x', scope: 'l1\nl2\n\tindented' }), 'Goal: x\nScope: l1\nl2\n\tindented');
  assert.equal(assemblePrompt({ goal: '한글 😀' }), 'Goal: 한글 😀');
  assert.throws(() => assemblePrompt({ goal: '', scope: ' ' }), (e) => e instanceof PromptError && e.code === 'empty');
  assert.throws(() => assemblePrompt(), (e) => e instanceof PromptError && e.code === 'empty');

  // the limit counts UTF-8 BYTES of the whole prompt (headers included): 65536 is fine, 65537 is not
  assert.equal(PROMPT_MAX_BYTES, 65536);
  const head = 'Goal: '.length;
  assert.equal(new TextEncoder().encode(assemblePrompt({ goal: 'a'.repeat(PROMPT_MAX_BYTES - head) })).length, PROMPT_MAX_BYTES);
  assert.throws(() => assemblePrompt({ goal: 'a'.repeat(PROMPT_MAX_BYTES - head + 1) }),
    (e) => e instanceof PromptError && e.code === 'too_long' && e.bytes === PROMPT_MAX_BYTES + 1 && /64 KiB/.test(e.message));
  // 21846 Hangul syllables are 21846 characters but 65538 bytes
  assert.throws(() => assemblePrompt({ goal: '가'.repeat(21846) }), (e) => e.code === 'too_long' && e.bytes === 6 + 65538);
  // a multi-byte character is never counted as one byte: 21842 syllables = 65526 + 6 = 65532 bytes: ok
  assert.equal(new TextEncoder().encode(assemblePrompt({ goal: '가'.repeat(21842) })).length, 65532);
  // 4-byte emoji
  assert.throws(() => assemblePrompt({ goal: '😀'.repeat(16384) }), (e) => e.code === 'too_long');
});

test('TestAssemblePromptRejectsHostileChars', () => {
  const bad = {
    nul: '\u0000', esc: '\u001b[31m', del: '\u007f', c1: '\u0085', cr: 'a\rb', bell: '\u0007',
    rlo: '‮', lri: '⁦', zwsp: '​', bom: '﻿', shy: '­', tag: '\u{e0041}', ls: ' ',
    vs: '️', lone: '\ud800', lone2: 'x\udc00',
  };
  for (const [name, s] of Object.entries(bad)) {
    for (const field of ['goal', 'scope', 'done', 'avoid']) {
      assert.throws(() => assemblePrompt({ goal: 'ok', [field]: 'a' + s + 'b' }),
        (e) => e instanceof PromptError && e.code === 'hostile' && e.field === field && /U\+[0-9A-F]{4,6}/.test(e.message),
        name + ' in ' + field);
    }
  }
  // rejected, never silently stripped: there is no return value to inspect
  let out = null;
  try { out = assemblePrompt({ goal: 'a‮b' }); } catch { /* expected */ }
  assert.equal(out, null);
  // newline, tab, Korean, emoji and a real surrogate pair are fine
  assert.equal(assemblePrompt({ goal: 'a\nb\tc 한 😀' }), 'Goal: a\nb\tc 한 😀');
});

test('newIdempotencyKey: 32 alphanumerics from crypto, no modulo bias, never Math.random', () => {
  const k = newIdempotencyKey();
  assert.match(k, /^[A-Za-z0-9]{32}$/);
  assert.equal(new Set(Array.from({ length: 300 }, newIdempotencyKey)).size, 300);
  const real = Object.getOwnPropertyDescriptor(globalThis, 'crypto');
  const alpha = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  let calls = 0;
  const realRandom = Math.random;
  Math.random = () => { throw new Error('Math.random is not allowed'); };
  Object.defineProperty(globalThis, 'crypto', {
    configurable: true,
    value: { getRandomValues: (a) => { calls++; for (let i = 0; i < a.length; i++) a[i] = calls === 1 ? 255 : (i * 7) % 248; return a; } },
  });
  try {
    const key = newIdempotencyKey();
    assert.ok(calls >= 2, 'bytes >= 248 are rejected and drawn again');
    assert.equal(key, Array.from({ length: 32 }, (_, i) => alpha[((i * 7) % 248) % 62]).join(''));
  } finally {
    Math.random = realRandom;
    Object.defineProperty(globalThis, 'crypto', real);
  }
});

// ---- the view --------------------------------------------------------------

const created = { id: 77, repo: '/srv/repos/calc', base: 'x', prompt: 'p', class: 'docs', agent: 'claude', state: 'queued', attempts: 0, created_at: '2026-11-09T10:00:00Z', updated_at: '2026-11-09T10:00:00Z' };

async function mount(over) {
  over = over || {};
  const routes = {
    '/api/v1/repos': over.repos || fx('repos.json'),
    '/api/v1/policy': over.policy || fx('policy.json'),
    'POST /api/v1/tasks': over.post || (() => created),
  };
  const ctx = fakeCtx(routes);
  await newView(ctx);
  await ctx.settle();
  return ctx;
}
const fill = (root, o) => {
  const f = Object.assign({ goal: 'README 오타를 고친다', scope: 'README.md', done: '', avoid: '' }, o || {});
  for (const [k, id] of [['goal', 'f-goal'], ['scope', 'f-scope'], ['done', 'f-done'], ['avoid', 'f-avoid']]) setField(root, id, f[k]);
};
const submitBtn = (root) => byId(root, 'f-submit');

test('new view is registered under #/new', () => {
  assert.equal(getView('new'), newView);
});

test('TestNewViewSubmitsOnceWithIdempotencyKey', async () => {
  const g = gate();
  const ctx = await mount({ post: async () => { await g.p; return created; } });
  fill(ctx.root);
  const btn = submitBtn(ctx.root);
  assert.equal(btn.tagName, 'BUTTON', 'a native button');
  assert.equal(btn.getAttribute('type'), 'button');
  click(btn); click(btn); click(btn); // double click and Enter spam
  assert.equal(ctx.posts.length, 1, 'exactly one request');
  assert.equal(btn.getAttribute('aria-disabled'), 'true', 'marked disabled while pending');
  assert.equal(btn.hasAttribute('disabled'), false, 'never a real disabled (keeps focus)');
  const p = ctx.posts[0];
  assert.equal(p.path, '/api/v1/tasks');
  assert.deepEqual(Object.keys(p.body).sort(), ['agent', 'base', 'class', 'prompt', 'repo']);
  assert.match(p.opts.headers['Idempotency-Key'], /^[A-Za-z0-9]{32}$/);
  assert.deepEqual(Object.keys(p.opts.headers), ['Idempotency-Key'], 'no other header');
  assert.deepEqual(ctx.nav, []);
  g.go();
  await ctx.settle();
  click(btn); // even after it finished, nothing is sent while we are leaving
  assert.deepEqual(ctx.nav, ['#/tasks/77']);
  assert.equal(ctx.posts.length, 1, 'no second request after success');
});

test('same content keeps the key across queue_full/network errors; changed content gets a new key', async () => {
  let n = 0;
  const seen = new Map(); // a tiny idempotent server: key -> task
  const ctx = await mount({
    post: (body, opts) => {
      n++;
      if (n === 1) throw apiError(429, 'queue_full', '내 대기 작업이 가득 찼습니다');
      if (n === 2) throw apiError(0, 'network', 'x');
      const k = opts.headers['Idempotency-Key'];
      if (!seen.has(k)) seen.set(k, { ...created, id: 100 + seen.size });
      return seen.get(k);
    },
  });
  fill(ctx.root);
  const btn = submitBtn(ctx.root);
  click(btn); await ctx.settle();
  assert.match(text(ctx.root), /가득/);
  assert.equal(btn.getAttribute('aria-disabled'), 'false', 'retry is possible');
  click(btn); await ctx.settle();
  click(btn); await ctx.settle();
  const keys = ctx.posts.map((p) => p.opts.headers['Idempotency-Key']);
  assert.equal(keys.length, 3);
  assert.equal(new Set(keys).size, 1, 'same content: same key');
  assert.deepEqual(ctx.nav, ['#/tasks/100']);

  // a fresh form: content change -> new key; change back -> the earlier key again
  const c2 = await mount({ post: (body, opts) => { throw apiError(0, 'timeout', 'x'); } });
  fill(c2.root, { goal: 'one' });
  click(submitBtn(c2.root)); await c2.settle();
  fill(c2.root, { goal: 'two' });
  click(submitBtn(c2.root)); await c2.settle();
  fill(c2.root, { goal: 'one' });
  click(submitBtn(c2.root)); await c2.settle();
  const k2 = c2.posts.map((p) => p.opts.headers['Idempotency-Key']);
  assert.notEqual(k2[0], k2[1]);
  assert.equal(k2[0], k2[2]);
  // other fields count as content too
  const c3 = await mount({ post: () => { throw apiError(0, 'network', 'x'); } });
  fill(c3.root);
  click(submitBtn(c3.root)); await c3.settle();
  setField(c3.root, 'f-agent', 'codex');
  click(submitBtn(c3.root)); await c3.settle();
  assert.notEqual(c3.posts[0].opts.headers['Idempotency-Key'], c3.posts[1].opts.headers['Idempotency-Key']);
  assert.equal(c3.posts[1].body.agent, 'codex');
});

test('navigates only after ID_RE validation of the response id', async () => {
  for (const id of ['../x', '0', 'abc', '1e3', '', null, -4, 1.5, '12345678901234567890', { a: 1 }]) {
    const ctx = await mount({ post: () => ({ ...created, id }) });
    fill(ctx.root);
    click(submitBtn(ctx.root)); await ctx.settle();
    assert.deepEqual(ctx.nav, [], 'no navigation for id ' + JSON.stringify(id));
    assert.match(text(ctx.root), /번호를 확인할 수 없습니다/);
  }
  const ok = await mount({ post: () => ({ ...created, id: '321' }) });
  fill(ok.root);
  click(submitBtn(ok.root)); await ok.settle();
  assert.deepEqual(ok.nav, ['#/tasks/321']);
});

test('TestNewViewShowsFinalPromptBeforeSubmit', async () => {
  const ctx = await mount();
  const preview = byId(ctx.root, 'f-preview');
  assert.ok(preview);
  fill(ctx.root, { goal: 'G <b>1</b>', scope: 'S', done: 'D', avoid: 'A' });
  const want = 'Goal: G <b>1</b>\nScope: S\nDone when: D\nDo not: A';
  assert.equal(text(preview), want, 'the preview is the exact prompt');
  noHostile(ctx.root);
  assert.match(text(ctx.root), /\d+ \/ 65536 바이트/);
  click(submitBtn(ctx.root)); await ctx.settle();
  assert.equal(ctx.posts[0].body.prompt, want, 'what is sent equals what was shown');
  assert.equal(ctx.posts[0].body.prompt, text(preview));

  // a value changed without an input event: the click only refreshes the preview
  const c2 = await mount();
  fill(c2.root);
  byId(c2.root, 'f-goal').value = 'silently changed';
  click(submitBtn(c2.root));
  await c2.settle();
  assert.equal(c2.posts.length, 0, 'nothing sent that the user has not seen');
  assert.match(text(byId(c2.root, 'f-preview')), /Goal: silently changed/);
  click(submitBtn(c2.root)); await c2.settle();
  assert.equal(c2.posts[0].body.prompt, text(byId(c2.root, 'f-preview')));

  // hostile input: error shown, no submit
  const c3 = await mount();
  fill(c3.root, { goal: 'ok', scope: 'a‮b' });
  assert.match(text(c3.root), /보이지 않는 문자|제어 문자/);
  assert.match(text(c3.root), /U\+202E/);
  assert.equal(submitBtn(c3.root).getAttribute('aria-disabled'), 'true');
  click(submitBtn(c3.root)); await c3.settle();
  assert.equal(c3.posts.length, 0);
  // too long: guidance, no submit
  const c4 = await mount();
  fill(c4.root, { goal: 'a'.repeat(65537) });
  assert.match(text(c4.root), /64 KiB/);
  click(submitBtn(c4.root)); await c4.settle();
  assert.equal(c4.posts.length, 0);
  // empty
  const c5 = await mount();
  click(submitBtn(c5.root)); await c5.settle();
  assert.equal(c5.posts.length, 0);
});

test('TestNewViewErrorsFixedTexts', async () => {
  const SECRET = '<img src=x onerror=alert(1)> SECRETWORD /srv/private';
  const cases = [
    [400, 'unknown_class', /정책에 없는 작업 종류/],
    [429, 'queue_full', /가득/],
    [400, 'bad_request', /요청 형식이 올바르지 않습니다/],
    [429, 'rate_limited', /요청이 많아/],
    [409, 'idempotency_conflict', /제출 키/],
    [413, 'body_too_large', /너무 큽니다/],
    [0, 'network', /서버에 연결하지 못했습니다/],
    [0, 'timeout', /서버에 연결하지 못했습니다/],
    [500, 'internal', /서버에서 오류가 발생했습니다/],
    [403, 'csrf_failed', /다시 로그인|권한/],
  ];
  for (const [status, code, re] of cases) {
    const ctx = await mount({ post: () => { throw apiError(status, code, code === 'queue_full' ? '내 대기 작업이 가득 찼습니다' : SECRET); } });
    fill(ctx.root);
    click(submitBtn(ctx.root)); await ctx.settle();
    assert.match(text(ctx.root), re, code);
    assert.doesNotMatch(text(ctx.root), /SECRETWORD|\/srv\/private|onerror/, code + ': server text never shown');
    noHostile(ctx.root);
    assert.equal(submitBtn(ctx.root).getAttribute('aria-disabled'), 'false', code + ': usable again');
    assert.deepEqual(ctx.nav, []);
  }
  // the queue_full text says the queue is full, not "rate limited"
  const q = await mount({ post: () => { throw apiError(429, 'queue_full', '내 대기 작업이 가득 찼습니다'); } });
  fill(q.root);
  click(submitBtn(q.root)); await q.settle();
  assert.doesNotMatch(text(q.root), /요청이 많아/);
});

test('TestNewViewQueueFullShowsServerSentenceAsText', async () => {
  // the two server sentences are shown (as text), each followed by the fixed advice
  for (const msg of ['내 대기 작업이 가득 찼습니다', '서버 대기열이 가득 찼습니다']) {
    const ctx = await mount({ post: () => { throw apiError(429, 'queue_full', msg); } });
    fill(ctx.root);
    click(submitBtn(ctx.root)); await ctx.settle();
    assert.match(text(ctx.root), new RegExp(msg + '\\. 앞의 작업이 진행된 뒤 다시 제출하세요'));
    assert.match(text(ctx.root), /중복되지 않습니다/);
    noHostile(ctx.root);
  }
  // hostile or oversized server text stays text, bounded; other codes still never show it
  const evil = '<img src=x onerror=alert(1)>' + 'A'.repeat(500);
  const h = await mount({ post: () => { throw apiError(429, 'queue_full', evil); } });
  fill(h.root);
  click(submitBtn(h.root)); await h.settle();
  assert.doesNotMatch(text(h.root), /A{81}/, 'bounded');
  noHostile(h.root);
  assert.equal(findAll(h.root, (e) => e.tagName === 'IMG').length, 0);
  // no usable message: the fixed fallback sentence
  const f = await mount({ post: () => { throw apiError(429, 'queue_full', '   '); } });
  fill(f.root);
  click(submitBtn(f.root)); await f.settle();
  assert.match(text(f.root), /대기 중인 작업이 가득 찼습니다\(queue full\)/);
});

test('TestNewViewReposAndClassesFromApi', async () => {
  const repos = fx('repos.json');
  const policy = fx('policy.json');
  const ctx = await mount();
  assert.deepEqual(ctx.calls.sort(), ['/api/v1/policy', '/api/v1/repos']);
  const opts = (id) => findAll(byId(ctx.root, id), (e) => e.tagName === 'OPTION');
  assert.deepEqual(opts('f-repo').map((o) => o.getAttribute('value')), repos.map((r) => r.path));
  assert.deepEqual(opts('f-class').map((o) => o.getAttribute('value')), Object.keys(policy.classes).sort());
  assert.deepEqual(opts('f-agent').map((o) => o.getAttribute('value')), ['claude', 'codex']);
  // HEAD is the default base and follows the selected repository
  assert.equal(byId(ctx.root, 'f-base').value, repos[0].head);
  setField(ctx.root, 'f-repo', repos[1].path);
  assert.equal(byId(ctx.root, 'f-base').value, repos[1].head);
  // an unknown repository or class can not be submitted
  fill(ctx.root);
  setField(ctx.root, 'f-repo', '/etc');
  setField(ctx.root, 'f-base', repos[0].head); // a valid base must not rescue an unknown repository
  click(submitBtn(ctx.root)); await ctx.settle();
  assert.equal(ctx.posts.length, 0);
  setField(ctx.root, 'f-repo', repos[0].path);
  setField(ctx.root, 'f-class', 'hacker');
  click(submitBtn(ctx.root)); await ctx.settle();
  assert.equal(ctx.posts.length, 0);
  setField(ctx.root, 'f-class', '기능');
  setField(ctx.root, 'f-agent', 'rm -rf');
  click(submitBtn(ctx.root)); await ctx.settle();
  assert.equal(ctx.posts.length, 0, 'unknown agent');
  setField(ctx.root, 'f-agent', 'codex');
  click(submitBtn(ctx.root)); await ctx.settle();
  assert.equal(ctx.posts.length, 1);
  assert.equal(ctx.posts[0].body.repo, repos[0].path);
  assert.equal(ctx.posts[0].body.class, '기능');
  assert.equal(ctx.posts[0].body.base, repos[0].head);

  // base: a quick pick from the branches fills the field; an invalid base blocks the submit
  const c2 = await mount();
  fill(c2.root);
  const pick = findAll(c2.root, (e) => e.tagName === 'BUTTON' && e.getAttribute('data-base') === 'fix-1')[0];
  assert.ok(pick, 'branch button');
  click(pick);
  assert.equal(byId(c2.root, 'f-base').value, 'fix-1');
  assert.equal(findAll(c2.root, (e) => e.getAttribute('data-base') === '기능/새것').length, 0, 'a branch the server would refuse as base is not offered');
  setField(c2.root, 'f-base', 'bad base; rm');
  click(submitBtn(c2.root)); await c2.settle();
  assert.equal(c2.posts.length, 0);
  assert.match(text(c2.root), /기준 커밋/);

  // an unborn repository (no HEAD) can not be submitted
  const c3 = await mount();
  setField(c3.root, 'f-repo', repos[2].path);
  assert.equal(byId(c3.root, 'f-base').value, '');
  fill(c3.root);
  click(submitBtn(c3.root)); await c3.settle();
  assert.equal(c3.posts.length, 0);

  // no class list (empty or no policy): a free text field limited to the class regex
  for (const pol of [fx('policy-empty.json'), {}]) {
    const c4 = await mount({ policy: pol });
    assert.equal(byId(c4.root, 'f-class').tagName, 'INPUT');
    fill(c4.root);
    setField(c4.root, 'f-class', 'Bad Class');
    click(submitBtn(c4.root)); await c4.settle();
    assert.equal(c4.posts.length, 0);
    assert.match(text(c4.root), /종류/);
    setField(c4.root, 'f-class', 'docs-2_기능');
    click(submitBtn(c4.root)); await c4.settle();
    assert.equal(c4.posts.length, 1);
    assert.equal(c4.posts[0].body.class, 'docs-2_기능');
  }

  // hostile names stay text; a hostile class key is not offered
  const hostile = '<img src=x onerror=alert(1)>‮';
  const c5 = await mount({ repos: [{ path: '/srv/' + hostile, head: 'a'.repeat(40), branch: hostile, branches: [hostile] }],
    policy: { ...policy, classes: { docs: 'auto', [hostile]: 'auto' } } });
  noHostile(c5.root);
  assert.doesNotMatch(text(c5.root), /‮/);
  assert.deepEqual(findAll(byId(c5.root, 'f-class'), (e) => e.tagName === 'OPTION').map((o) => o.getAttribute('value')), ['docs']);

  // a failing list is an error pane with a retry, not a form
  let fail = true;
  const c6 = fakeCtx({ '/api/v1/repos': () => { if (fail) throw apiError(503, 'repos_unavailable', 'SECRETWORD'); return repos; }, '/api/v1/policy': policy });
  await newView(c6); await c6.settle();
  assert.doesNotMatch(text(c6.root), /SECRETWORD/);
  assert.equal(byId(c6.root, 'f-submit'), undefined);
  fail = false;
  const retry = findAll(c6.root, (e) => e.tagName === 'BUTTON')[0];
  click(retry); await c6.settle();
  assert.ok(byId(c6.root, 'f-submit'));
  // no repository at all
  const c7 = await mount({ repos: [] });
  assert.match(text(c7.root), /저장소가 없습니다/);
  assert.equal(byId(c7.root, 'f-submit'), undefined);
});

test('TestNewViewSubmitAriaDisabledKeepsFocus', async () => {
  const g = gate();
  const ctx = await mount({ post: async () => { await g.p; return created; } });
  const btn = submitBtn(ctx.root);
  // empty form: blocked, but the button keeps focusability (no disabled attribute)
  assert.equal(btn.getAttribute('aria-disabled'), 'true');
  btn.focus();
  click(btn); await ctx.settle();
  assert.equal(ctx.posts.length, 0, 'a blocked button sends nothing');
  fill(ctx.root);
  assert.equal(btn.getAttribute('aria-disabled'), 'false');
  btn.focus();
  click(btn);
  assert.equal(btn.getAttribute('aria-disabled'), 'true');
  assert.equal(btn.hasAttribute('disabled'), false);
  assert.equal(globalThis.document.activeElement, btn, 'focus stays on the pressed button');
  click(btn);
  assert.equal(ctx.posts.length, 1);
  g.go();
  await ctx.settle();
  for (const b of findAll(ctx.root, (e) => e.tagName === 'BUTTON')) assert.equal(b.hasAttribute('disabled'), false);
});

test('TestNewViewProblemIsPoliteAndStable', async () => {
  const ctx = await mount();
  const pr = byId(ctx.root, 'f-problem');
  assert.equal(pr.getAttribute('aria-live'), 'polite');
  assert.equal(pr.getAttribute('role'), null, 'not an alert');
  fill(ctx.root, { goal: 'ok', scope: 'a‮b' });
  const node = pr.firstChild;
  assert.ok(node);
  assert.match(text(pr), /U\+202E/);
  // more typing that leaves the same problem does not rewrite the region
  setField(ctx.root, 'f-goal', 'ok, still typing');
  setField(ctx.root, 'f-done', 'more');
  assert.equal(pr.firstChild, node, 'same text node: nothing re-announced');
  // a different problem replaces it once
  setField(ctx.root, 'f-scope', 'a​b');
  assert.notEqual(pr.firstChild, node);
  assert.match(text(pr), /U\+200B/);
  // fixed: the region empties and hides
  setField(ctx.root, 'f-scope', 'README.md');
  assert.equal(text(pr), '');
  assert.equal(pr.hidden, true);
});

test('TestNewViewRepoLabelsKeepTheTail', async () => {
  const { repoLabel, REPO_LABEL_MAX } = N;
  assert.equal(REPO_LABEL_MAX, 120);
  const prefix = '/srv/repos/' + 'very-long-team-name/'.repeat(8);
  const a = prefix + 'service-alpha', b = prefix + 'service-beta';
  assert.ok(a.length > 120);
  const la = repoLabel(a), lb = repoLabel(b);
  assert.notEqual(la, lb, 'paths that differ at the end stay distinguishable');
  assert.ok(la.endsWith('service-alpha') && lb.endsWith('service-beta'));
  assert.ok(la.startsWith('/srv/repos/'));
  assert.equal(Array.from(la).length, 120);
  assert.match(la, /…/);
  assert.equal(repoLabel('/srv/repos/calc'), '/srv/repos/calc', 'short paths are unchanged');
  // never splits a surrogate pair; control characters are still neutralised
  const emoji = '/srv/' + '😀'.repeat(200);
  assert.ok(!/[\ud800-\udfff]/.test(repoLabel(emoji).replace(/[\ud800-\udbff][\udc00-\udfff]/g, '')));
  assert.doesNotMatch(repoLabel('/srv/a\u001bb'), /\u001b/);
  // in the view
  const ctx = await mount({ repos: [{ path: a, head: 'a'.repeat(40), branch: 'main', branches: ['main'] }, { path: b, head: 'b'.repeat(40), branch: 'main', branches: ['main'] }] });
  const opts = findAll(ctx.root, (e) => e.tagName === 'OPTION' && e.getAttribute('value') && e.getAttribute('value').startsWith('/srv/'));
  assert.equal(opts.length, 2);
  assert.notEqual(text(opts[0]), text(opts[1]));
});
