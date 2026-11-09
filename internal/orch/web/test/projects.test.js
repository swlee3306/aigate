import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createHash, webcrypto } from 'node:crypto';
import { findAll } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fxURL = (n) => new URL('./fixtures/' + n, import.meta.url);
const fx = (n) => JSON.parse(readFileSync(fxURL(n), 'utf8'));
const src = (n) => readFileSync(new URL('../static/' + n, import.meta.url), 'utf8');
const wa = await import('../static/webauthn.js');
const P = await import('../static/views/projects.js');
const { getView, parseRoute, navItem } = await import('../static/router.js');

const REGISTRY = fx('projects-registry.json');
const CANDS = fx('projects-candidates.json');
const PROP_REG = fx('projects-propose-register.json');
const PROP_AI = fx('projects-propose-register-ai.json');
const PROP_FLAG = fx('projects-propose-setflag.json');
const APPLIED = fx('projects-apply-ok.json');
const SECRET = '<img src=x onerror=alert(1)> javascript:alert(1) ‮evil';

// ---- helpers ------------------------------------------------------------------

const text = (n) => n.textContent;
const byTag = (root, tag) => findAll(root, (e) => e.tagName === tag.toUpperCase());
const buttons = (root) => byTag(root, 'button');
const byLabel = (root, label) => buttons(root).filter((b) => text(b).replace(/\s+/g, ' ').trim().startsWith(label));
const byId = (root, id) => findAll(root, (e) => e.getAttribute('id') === id)[0];
const click = (b) => { for (const f of b.listeners.click || []) f({ preventDefault() {} }); };
const fire = (n, ev) => { for (const f of n.listeners[ev] || []) f({}); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(cond, what) {
  for (let i = 0; i < 200; i++) {
    if (cond()) return;
    await sleep(2);
  }
  assert.fail('timed out waiting for ' + what);
}

const sha = (b) => new Uint8Array(createHash('sha256').update(b).digest());
const u8 = (b) => new Uint8Array(b);
const toAB = (b) => { const x = u8(b); return x.buffer.slice(x.byteOffset, x.byteOffset + x.byteLength); };
const CRED = new Uint8Array(32).fill(7);
const SIGNERS = { signers: [{ id: 'phone', credential_id: wa.b64uEncode(CRED), label: 'x' }] };

// A passkey stand-in that records the challenge it was asked to sign.
class Auth {
  constructor() { this.gets = []; this.events = []; this.gate = null; this.deny = false; }
  async get(opts) {
    this.gets.push(opts);
    this.events.push('get');
    if (this.gate) await this.gate;
    if (this.deny) throw new DOMException('no', 'NotAllowedError');
    return {
      type: 'public-key',
      rawId: toAB(CRED),
      response: { authenticatorData: toAB([1, 2, 3]), clientDataJSON: toAB([4, 5, 6]), signature: toAB([7, 8, 9]) },
    };
  }
  async create() { throw new Error('not used'); }
}

let auth;
beforeEach(() => {
  auth = new Auth();
  wa.configure({ credentials: auth, subtle: webcrypto.subtle });
});

// proposalFor answers a propose body like the server: it echoes what was asked
// and builds the change hash and message from it (the golden vector test pins
// computeChange to Go; the preview sentences are the golden ones).
async function proposalFor(body) {
  const base = body.action === 'set_flag' ? PROP_FLAG : (body.fields.ai_context ? PROP_AI : PROP_REG);
  const change = await P.computeChange(body.action, body.subject, body.fields);
  return {
    action: body.action, subject: body.subject, fields: structuredClone(body.fields), change, request: base.request, issued: base.issued,
    message: 'aigate-registry-v1\naction: ' + body.action + '\nsubject: ' + body.subject + '\nchange: ' + change +
      '\nrequest: ' + base.request + '\nissued: ' + base.issued + '\n',
    preview: base.preview.slice(),
  };
}

const SESSION = { principal: 'phone', kind: 'session', expires: '2026-11-10T00:00:00Z' };

// mount builds the real view on a fake ctx. `over` replaces routes. A route
// function gets (body, opts); every call is recorded on ctx.calls / ctx.posts.
async function mount(over, extra) {
  const events = (auth && auth.events) || [];
  const routes = {
    '/api/v1/projects/registry': REGISTRY,
    '/api/v1/projects/candidates': CANDS,
    '/api/v1/auth/signers': SIGNERS,
    'POST /api/v1/projects/propose': (body) => {
      events.push('propose');
      return proposalFor(body);
    },
    'POST /api/v1/projects/apply': () => { events.push('apply'); return APPLIED; },
    ...(over || {}),
  };
  const ctx = fakeCtx(routes, { session: SESSION, ...(extra || {}) });
  P.projectsView(ctx);
  await until(() => byLabel(ctx.root, '프로젝트 추가').length > 0 || text(ctx.root).includes('불러오지 못했습니다'), 'list');
  return ctx;
}

async function openForm(ctx) {
  click(byLabel(ctx.root, '프로젝트 추가')[0]);
  await until(() => byId(ctx.root, 'p-cand'), 'form');
}
async function toConfirm(ctx, o) {
  o = o || {};
  await openForm(ctx);
  if (o.ai) { const b = byId(ctx.root, 'p-ai'); b.checked = true; fire(b, 'change'); }
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => !!byId(ctx.root, 'h-says') || findAll(ctx.root, (e) => e.getAttribute('data-warn') === 'mismatch').length > 0, 'confirm');
  await ctx.settle();
}
const signBtn = (ctx) => byLabel(ctx.root, '패스키로 서명')[0];
async function waitSignable(ctx) {
  await until(() => !!signBtn(ctx), 'sign button');
}
const posts = (ctx, path) => ctx.posts.filter((p) => p.path === path);

// ---- Step 1 tests -----------------------------------------------------------------

test('TestComputeChangeMatchesGoVector', async () => {
  const v = fx('registry-vectors.json');
  assert.ok(v.change.length >= 5 && v.refused.length >= 10);
  for (const c of v.change) {
    assert.equal(P.canonicalChange(c.action, c.subject, c.fields), c.canonical, c.name);
    assert.equal(await P.computeChange(c.action, c.subject, c.fields), c.change, c.name);
    // the same fields in another insertion order give the same bytes
    const rev = {};
    for (const k of Object.keys(c.fields).reverse()) rev[k] = c.fields[k];
    assert.equal(P.canonicalChange(c.action, c.subject, rev), c.canonical, c.name + ' (reversed)');
    assert.equal(createHash('sha256').update(c.canonical).digest('hex'), c.change);
  }
  // anything Go refuses, the browser refuses too
  for (const r of v.refused) {
    await assert.rejects(async () => P.computeChange(r.action, r.subject, r.fields), (e) => e.name === 'ChangeError', r.name);
  }
  // cases JSON cannot carry
  const ok = { path: '/a', kind: 'docs', ai_context: false };
  assert.throws(() => P.canonicalChange('register_project', 'a', { ...ok, path: '/a\ud800b' }), { name: 'ChangeError' }, 'lone surrogate');
  assert.throws(() => P.canonicalChange('register_project', 'a', { ...ok, path: '/a\udc00b' }), { name: 'ChangeError' });
  assert.throws(() => P.canonicalChange('register_project', 'a', { path: '/a', kind: 'docs', ai_context: 1 }), { name: 'ChangeError' });
  assert.throws(() => P.canonicalChange('register_project', 'a', { path: '/a', kind: 'docs', ai_context: null }), { name: 'ChangeError' });
  assert.throws(() => P.canonicalChange('register_project', 'a', Object.create({ path: '/a', kind: 'docs', ai_context: false })), { name: 'ChangeError' }, 'inherited fields');
  // the golden signing message and its digest
  const m = v.message;
  assert.equal(createHash('sha256').update(m.text).digest('hex'), m.digest);
  const parsed = wa.checkRegistryMessage(m.text);
  assert.deepEqual(parsed, { action: m.action, subject: m.subject, change: m.change, request: m.request, issued: m.issued });
});

test('TestProjectsListRendersAsText', async () => {
  const hostile = { id: SECRET, path: 'javascript:alert(1)', kind: '<b>x</b>', ai_context: true, gitlab: { project: SECRET, project_id: null }, auto_merge: { enabled: false } };
  const reg = { ...REGISTRY, projects: [...REGISTRY.projects, hostile] };
  const ctx = await mount({ '/api/v1/projects/registry': reg });
  const all = findAll(ctx.root, () => true);
  assert.equal(findAll(ctx.root, (e) => ['IMG', 'SCRIPT', 'IFRAME', 'A'].includes(e.tagName)).length, 0);
  assert.equal(findAll(ctx.root, (e) => e.hasAttribute('href') || e.hasAttribute('src')).length, 0, 'no href/src at all');
  for (const e of all) {
    for (const [k, v] of e.attrs) {
      assert.ok(!/onerror|javascript:|<img|evil/i.test(k + '=' + v), 'attribute ' + k + '=' + v);
      assert.ok(!k.startsWith('on'), k);
    }
  }
  const t = text(ctx.root);
  assert.match(t, /\/home\/orch\/repos\/docs/);
  assert.match(t, /docs/);
  assert.match(t, /javascript:alert\(1\)/, 'shown as plain text');
  assert.match(t, /<img src=x onerror=alert\(1\)>/, 'markup stays literal text');
  // ai_context shown for every row
  assert.match(t, /AI 전송 허용/);
  // a hostile id can not be proposed: no toggle for that row
  const toggles = buttons(ctx.root).filter((b) => /AI 전송 (켜기|끄기)/.test(text(b)));
  assert.equal(toggles.length, 2);
  // read-only GitLab / auto merge placeholders
  assert.match(t, /team\/docs/);
  assert.match(t, /자동 병합/);
});

test('TestRegisterNeedsConfirmationBeforeSigning', async () => {
  const ctx = await mount();
  await openForm(ctx);
  assert.equal(auth.gets.length, 0);
  assert.equal(posts(ctx, '/api/v1/projects/propose').length, 0, 'nothing proposed by merely opening the form');
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => !!signBtn(ctx), 'sign button');
  // the confirmation screen is up: the proposal exists, nothing was signed or applied
  assert.equal(posts(ctx, '/api/v1/projects/propose').length, 1);
  assert.equal(auth.gets.length, 0, 'no signature request before the confirmation');
  assert.equal(posts(ctx, '/api/v1/projects/apply').length, 0, 'no apply before signing');
  click(signBtn(ctx));
  await until(() => posts(ctx, '/api/v1/projects/apply').length === 1, 'apply');
  assert.deepEqual(auth.events, ['propose', 'get', 'apply']);
  // the passkey signed the SHA-256 of the server's message
  const prop = await proposalFor(posts(ctx, '/api/v1/projects/propose')[0].body);
  const want = sha(Buffer.from(prop.message));
  assert.deepEqual([...u8(auth.gets[0].publicKey.challenge)], [...want]);
  assert.equal(auth.gets[0].publicKey.userVerification, 'required');
  const body = posts(ctx, '/api/v1/projects/apply')[0].body;
  assert.deepEqual(Object.keys(body).sort(), ['action', 'change', 'fields', 'issued', 'request', 'signature', 'subject']);
  assert.equal(body.change, prop.change);
  assert.equal(body.request, prop.request);
  assert.equal(body.issued, prop.issued);
  assert.deepEqual(Object.keys(body.signature).sort(), ['alg', 'sig', 'signer', 'webauthn']);
  assert.equal(body.signature.alg, 'webauthn-es256');
  assert.equal(body.signature.signer, 'phone');
  await until(() => /등록했습니다/.test(text(ctx.root)), 'result');
  // the list was read again
  assert.equal(ctx.calls.filter((c) => c === '/api/v1/projects/registry').length, 2);
});

test('TestSignButtonDisabledUntilChangeMatches', async () => {
  const bad = (mut) => async () => {
    const ctx = await mount({ 'POST /api/v1/projects/propose': async (body) => mut(await proposalFor(body)) });
    await toConfirm(ctx);
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, 'no sign button');
    assert.equal(buttons(ctx.root).filter((b) => /서명/.test(text(b))).length, 0);
    assert.equal(findAll(ctx.root, (e) => e.getAttribute('data-warn') === 'mismatch').length, 1);
    assert.match(text(ctx.root), /서버가 보낸 변경 내용이 화면의 내용과 맞지 않아 서명할 수 없습니다/);
    assert.equal(auth.gets.length, 0);
    assert.equal(posts(ctx, '/api/v1/projects/apply').length, 0);
  };
  await bad((p) => { p.change = '0'.repeat(64); return p; })(); // the server's hash is not of the fields shown
  await bad((p) => { p.fields.ai_context = true; return p; })(); // echoed fields differ from what was asked
  await bad((p) => { p.fields.path = '/home/orch/repos/other'; return p; })();
  await bad((p) => { p.subject = 'other'; return p; })();
  await bad((p) => { p.message = p.message.replace(/change: [0-9a-f]{64}/, 'change: ' + '1'.repeat(64)); return p; })(); // message differs
  await bad((p) => { p.message = p.message.replace('action: register_project', 'action: set_flag'); return p; })();
  await bad((p) => { p.message = 'aigate-approval-v1\n' + p.message; return p; })();
  await bad((p) => { p.request = 'f'.repeat(32); return p; })(); // request differs from the message
  await bad((p) => { p.preview = ['안전한 문장', 'x‮y']; return p; })(); // hostile sentence
  await bad((p) => { delete p.preview; return p; })();
  // the honest proposal gets the button
  const ok = await mount();
  await toConfirm(ok);
  await waitSignable(ok);
  assert.equal(byLabel(ok.root, '패스키로 서명').length, 1);
  assert.equal(findAll(ok.root, (e) => e.getAttribute('data-warn') === 'mismatch').length, 0);
});

test('TestConfirmShowsEveryFieldBeingChanged', async () => {
  const off = await mount();
  await toConfirm(off);
  const t = text(off.root);
  assert.match(t, /프로젝트 ID: notes/);
  assert.match(t, /경로: \/home\/orch\/repos\/notes/);
  assert.match(t, /종류: code/);
  assert.match(t, /AI 전송 허용: 아니오/);
  assert.equal(findAll(off.root, (e) => e.getAttribute('data-emph') === 'ai-on').length, 0);
  for (const s of PROP_REG.preview) assert.ok(t.includes(s), s);
  assert.equal(PROP_REG.message.split('\n').length, 7, 'golden message shape');
  // the form's own choices went into the request
  assert.deepEqual(posts(off, '/api/v1/projects/propose')[0].body,
    { action: 'register_project', subject: 'notes', fields: { path: '/home/orch/repos/notes', kind: 'code', ai_context: false } });
  const on = await mount();
  await toConfirm(on, { ai: true });
  const t2 = text(on.root);
  assert.match(t2, /AI 전송 허용: 예/);
  const emph = findAll(on.root, (e) => e.getAttribute('data-emph') === 'ai-on');
  assert.equal(emph.length, 1);
  assert.match(text(emph[0]), /OpenAI/);
  assert.match(t2, /\[중요\]/);
  // a toggle shows the project, the flag and the new value
  const tg = await mount();
  click(byLabel(tg.root, 'AI 전송 켜기')[0]);
  await until(() => /AI 전송 허용: 예/.test(text(tg.root)), 'flag confirm');
  assert.match(text(tg.root), /프로젝트 ID: docs/);
  assert.match(text(tg.root), /바꿀 설정: AI 전송 허용/);
});

test('TestAIContextDefaultsOff', async () => {
  const ctx = await mount();
  await openForm(ctx);
  const box = byId(ctx.root, 'p-ai');
  assert.equal(box.tagName, 'INPUT');
  assert.equal(box.getAttribute('type'), 'checkbox');
  assert.equal(box.checked, false);
  assert.equal(box.hasAttribute('checked'), false);
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => posts(ctx, '/api/v1/projects/propose').length === 1, 'propose');
  assert.equal(posts(ctx, '/api/v1/projects/propose')[0].body.fields.ai_context, false);
  assert.equal(posts(ctx, '/api/v1/projects/propose')[0].body.fields.kind, 'code');
});

test('TestToggleAIContextGoesThroughSignature', async () => {
  for (const [row, label, value] of [['docs', 'AI 전송 켜기', true], ['calc', 'AI 전송 끄기', false]]) {
    auth = new Auth();
    wa.configure({ credentials: auth, subtle: webcrypto.subtle });
    const prop = structuredClone(PROP_FLAG);
    prop.subject = row;
    prop.fields.value = value;
    prop.change = await P.computeChange('set_flag', row, prop.fields);
    prop.message = prop.message.replace(/subject: \S+/, 'subject: ' + row).replace(/change: [0-9a-f]{64}/, 'change: ' + prop.change);
    const ctx = await mount({ 'POST /api/v1/projects/propose': () => { auth.events.push('propose'); return prop; } });
    const btns = byLabel(ctx.root, label);
    assert.equal(btns.length, 1, label);
    click(btns[0]);
    await until(() => !!signBtn(ctx), 'sign');
    assert.equal(posts(ctx, '/api/v1/projects/apply').length, 0, 'not applied before the signature');
    assert.deepEqual(posts(ctx, '/api/v1/projects/propose')[0].body, { action: 'set_flag', subject: row, fields: { flag: 'ai_context', value } });
    click(signBtn(ctx));
    await until(() => posts(ctx, '/api/v1/projects/apply').length === 1, 'apply');
    assert.deepEqual(auth.events, ['propose', 'get', 'apply']);
    const b = posts(ctx, '/api/v1/projects/apply')[0].body;
    assert.equal(b.action, 'set_flag');
    assert.equal(b.subject, row);
    assert.deepEqual(b.fields, { flag: 'ai_context', value });
    assert.equal(b.change, prop.change);
    assert.ok(b.signature && b.signature.sig);
    await until(() => /변경했습니다/.test(text(ctx.root)), 'result');
  }
});

const D5_CODES = ['not_signed', 'bad_signature', 'unknown_signer', 'stale_signature', 'signers_unavailable', 'request_reused',
  'change_mismatch', 'project_exists', 'project_unknown', 'path_not_allowed', 'registry_unavailable', 'not_supported'];
const MORE_CODES = ['outcome_unknown', 'project_rate_limited', 'bad_request', 'broker_unavailable', 'broker_error'];

test('TestApplyErrorsFixedTexts', async () => {
  const statuses = { not_signed: 400, bad_signature: 403, unknown_signer: 403, stale_signature: 403, signers_unavailable: 503,
    request_reused: 409, change_mismatch: 400, project_exists: 409, project_unknown: 404, path_not_allowed: 400, registry_unavailable: 503,
    not_supported: 400, outcome_unknown: 504, project_rate_limited: 429, bad_request: 400, broker_unavailable: 503, broker_error: 502 };
  const seen = new Set();
  for (const code of [...D5_CODES, ...MORE_CODES]) {
    assert.equal(typeof P.ERROR_TEXT[code], 'string', 'fixed text for ' + code);
    assert.ok(P.ERROR_TEXT[code].length > 10);
    assert.ok(!seen.has(P.ERROR_TEXT[code]) || code === 'broker_unavailable', 'distinct text for ' + code);
    seen.add(P.ERROR_TEXT[code]);
    auth = new Auth();
    wa.configure({ credentials: auth, subtle: webcrypto.subtle });
    const ctx = await mount({ 'POST /api/v1/projects/apply': () => { throw apiError(statuses[code], code, SECRET, { reason: SECRET }); } });
    await toConfirm(ctx);
    await waitSignable(ctx);
    click(signBtn(ctx));
    await until(() => findAll(ctx.root, (e) => e.getAttribute('data-result') === 'error').length === 1, 'error ' + code);
    const shown = text(findAll(ctx.root, (e) => e.getAttribute('data-result') === 'error')[0]);
    assert.ok(shown.includes(P.ERROR_TEXT[code]), code + ': ' + shown);
    assert.ok(!shown.includes('img') && !shown.includes('javascript') && !shown.includes('evil'), 'server text not shown for ' + code);
    assert.ok(!text(ctx.root).includes('evil'));
    // a spent proposal can not be signed again
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, 'no second sign button after ' + code);
    assert.equal(posts(ctx, '/api/v1/projects/apply').length, 1);
    if (code === 'outcome_unknown') {
      assert.match(shown, /목록을 다시 읽어/);
      await until(() => ctx.calls.filter((c) => c === '/api/v1/projects/registry').length === 2, 'reload after unknown outcome');
    }
  }
  // a lost connection during apply is an unknown outcome too
  const ctx = await mount({ 'POST /api/v1/projects/apply': () => { throw apiError(0, 'network', SECRET); } });
  await toConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => findAll(ctx.root, (e) => e.getAttribute('data-result') === 'error').length === 1, 'network error');
  assert.match(text(ctx.root), /목록을 다시 읽어/);
  await until(() => ctx.calls.filter((c) => c === '/api/v1/projects/registry').length === 2, 'reload');
  // an unknown code falls back to a fixed text chosen by status, never the server's words
  const odd = await mount({ 'POST /api/v1/projects/apply': () => { throw apiError(418, 'tea_pot', SECRET); } });
  await toConfirm(odd);
  await waitSignable(odd);
  click(signBtn(odd));
  await until(() => findAll(odd.root, (e) => e.getAttribute('data-result') === 'error').length === 1, 'odd');
  assert.ok(!text(odd.root).includes('evil'));
  // propose failures use the same fixed texts
  const pf = await mount({ 'POST /api/v1/projects/propose': () => { throw apiError(429, 'project_rate_limited', SECRET); } });
  await openForm(pf);
  click(byLabel(pf.root, '변경 내용 확인')[0]);
  await until(() => text(pf.root).includes(P.ERROR_TEXT.project_rate_limited), 'propose 429');
  assert.ok(!text(pf.root).includes('evil'));
  assert.equal(posts(pf, '/api/v1/projects/propose').length, 1, 'no retry');
});

test('TestSubmitOnceNoDoubleSign', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  let release;
  auth.gate = new Promise((r) => { release = r; });
  const btn = signBtn(ctx);
  click(btn);
  click(btn); // a double click, before the first prompt finishes
  click(btn);
  await until(() => auth.gets.length >= 1, 'prompt');
  await sleep(20);
  assert.equal(auth.gets.length, 1, 'one passkey request');
  release();
  await until(() => posts(ctx, '/api/v1/projects/apply').length >= 1, 'apply');
  await sleep(20);
  assert.equal(posts(ctx, '/api/v1/projects/apply').length, 1, 'one apply');
  // double click on propose: one proposal
  const c2 = await mount();
  await openForm(c2);
  const go = byLabel(c2.root, '변경 내용 확인')[0];
  click(go); click(go);
  await until(() => !!signBtn(c2), 'sign');
  assert.equal(posts(c2, '/api/v1/projects/propose').length, 1);
});

test('TestLeaveWhileSigningSendsNothing', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  let release;
  auth.gate = new Promise((r) => { release = r; });
  click(signBtn(ctx));
  await until(() => auth.gets.length === 1, 'prompt');
  ctx.leave(); // the user went to another page
  release();
  await sleep(30);
  assert.equal(posts(ctx, '/api/v1/projects/apply').length, 0);
});

test('TestPasskeyCancelledSendsNothing', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  auth.deny = true;
  click(signBtn(ctx));
  await until(() => text(ctx.root).includes(wa.WA_TEXT.cancelled), 'cancel text');
  assert.equal(posts(ctx, '/api/v1/projects/apply').length, 0);
  assert.ok(signBtn(ctx), 'the same confirmation can be tried again');
});

test('TestNoStorageAndNoNavigateTrap', async () => {
  const code = src('views/projects.js');
  for (const bad of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'caches.', 'history.pushState', 'history.replaceState',
    'navigator.storage', 'ctx.navigate', 'navigate(', 'location.hash', 'setLeaveGuard', 'confirm(', 'alert(', 'innerHTML', 'Date.now', 'new Date']) {
    assert.ok(!code.includes(bad), 'projects.js uses ' + bad);
  }
  assert.ok(!/(?:^|[^.\w])(?:confirm|alert|prompt)\s*\(/.test(code));
  assert.ok(!/\.setAttribute\(/.test(code), 'attributes go through el()');
  const had = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  try {
    const ctx = await mount();
    assert.equal(ctx.navigate, undefined, 'the real app context has no navigate');
    await toConfirm(ctx);
    await waitSignable(ctx);
    click(signBtn(ctx));
    await until(() => /등록했습니다/.test(text(ctx.root)), 'done');
    // back to the list from the result: still no navigate, no hash written
    click(byLabel(ctx.root, '목록으로')[0]);
    await until(() => byLabel(ctx.root, '프로젝트 추가').length === 1, 'list again');
    assert.deepEqual(ctx.nav, [], 'the flow never writes location.hash');
    assert.equal(ctx.guard, null, 'no leave guard: there is no unsent input');
  } finally {
    if (had) Object.defineProperty(globalThis, 'localStorage', had); else delete globalThis.localStorage;
    delete globalThis.sessionStorage;
  }
  // wiring through the REAL router / shell
  assert.deepEqual(parseRoute('#/projects'), { name: 'projects', params: [] });
  assert.deepEqual(parseRoute('#/projects/x'), { name: 'notfound', params: [] });
  assert.equal(navItem('projects'), 'projects');
  assert.equal(typeof getView('projects'), 'function');
  assert.match(src('app.js'), /VIEW_MODULES = \[[^\]]*'projects'/);
  assert.match(src('index.html'), /<a class="nav" href="#\/projects" data-nav="projects">/);
});

test('TestNoRetry429ForProjectWrites', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => posts(ctx, '/api/v1/projects/apply').length === 1, 'apply');
  assert.equal(posts(ctx, '/api/v1/projects/propose')[0].opts.noRetry429, true);
  assert.equal(posts(ctx, '/api/v1/projects/apply')[0].opts.noRetry429, true);
  // and the real api layer honours it: one request, no sleep
  const api = await import('../static/api.js');
  let n = 0;
  let slept = 0;
  api.configure({
    fetch: async () => { n++; return new Response('{"error":{"code":"project_rate_limited","message":"x"}}', { status: 429, headers: { 'Retry-After': '3' } }); },
    sleep: async () => { slept++; },
  });
  try {
    await assert.rejects(() => api.post('/api/v1/projects/propose', {}, { noRetry429: true }), (e) => e.status === 429);
    assert.equal(n, 1);
    assert.equal(slept, 0);
  } finally {
    api.configure();
  }
});

test('TestPreviewSentencesAreTextNodes', async () => {
  const preview = ['<img src=x onerror=alert(1)> 보통 문장', 'javascript:alert(1)', '<b>굵게</b>'];
  const prop = { preview };
  const ctx = await mount({ 'POST /api/v1/projects/propose': async (body) => ({ ...(await proposalFor(body)), preview }) });
  await toConfirm(ctx);
  await waitSignable(ctx);
  const items = findAll(ctx.root, (e) => e.getAttribute('data-preview') === 'item');
  assert.equal(items.length, 3);
  assert.equal(items[0].textContent, prop.preview[0]);
  for (const it of items) {
    assert.ok(it.childNodes.every((c) => c.nodeType === 3), 'only text children');
  }
  assert.equal(findAll(ctx.root, (e) => ['IMG', 'SCRIPT'].includes(e.tagName)).length, 0);
  assert.equal(findAll(ctx.root, (e) => e.tagName === 'B' && /굵게/.test(text(e))).length, 0, 'the sentence did not become markup');
  for (const e of findAll(ctx.root, () => true)) {
    for (const [k, v] of e.attrs) assert.ok(!/onerror|javascript:|<img|<b>/.test(v), k + '=' + v);
  }
});

test('TestCandidatesAndFormValidation', async () => {
  const cands = {
    candidates: [...CANDS.candidates, { path: '/home/orch/repos/ev‮il', head: 'x' }, { path: SECRET, head: 'x' }],
    kinds: ['docs', 'Bad Kind', '버그수정', 'code'],
  };
  const ctx = await mount({ '/api/v1/projects/candidates': cands });
  await openForm(ctx);
  const sel = byId(ctx.root, 'p-cand');
  const opts = byTag(sel, 'option');
  assert.equal(opts.length, 2, 'hostile and non-text paths are not offered');
  for (const o of opts) assert.ok(!o.hasAttribute('value') || /^\d+$/.test(o.getAttribute('value')), 'option values are indexes');
  const kinds = byTag(byId(ctx.root, 'p-kind'), 'option').map(text);
  assert.deepEqual(kinds, ['docs', 'code'], 'only well-formed kinds');
  // choosing the second candidate fills a default id from its folder name
  sel.value = '1';
  fire(sel, 'change');
  assert.equal(byId(ctx.root, 'p-id').value, 'website');
  // a bad id is refused locally with a fixed text, nothing is proposed
  byId(ctx.root, 'p-id').value = 'Bad Id';
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await ctx.settle();
  assert.equal(posts(ctx, '/api/v1/projects/propose').length, 0);
  assert.match(text(ctx.root), /프로젝트 ID는 소문자로 시작/);
  byId(ctx.root, 'p-id').value = 'web-site';
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => posts(ctx, '/api/v1/projects/propose').length === 1, 'propose');
  assert.deepEqual(posts(ctx, '/api/v1/projects/propose')[0].body.fields,
    { path: '/home/orch/repos/website', kind: 'docs', ai_context: false });
  assert.equal(posts(ctx, '/api/v1/projects/propose')[0].body.subject, 'web-site');
  // nothing to register: fixed note, no form submit button
  const none = await mount({ '/api/v1/projects/candidates': { candidates: [], kinds: ['docs'] } });
  click(byLabel(none.root, '프로젝트 추가')[0]);
  await until(() => /등록할 수 있는 저장소가 없습니다/.test(text(none.root)), 'empty');
  assert.equal(byLabel(none.root, '변경 내용 확인').length, 0);
});

test('TestRegistryListFailureIsFixedText', async () => {
  const ctx = fakeCtx({ '/api/v1/projects/registry': () => { throw apiError(502, 'broker_error', SECRET); } }, { session: SESSION });
  P.projectsView(ctx);
  await until(() => text(ctx.root).includes('불러오지 못했습니다'), 'error pane');
  assert.ok(!text(ctx.root).includes('evil'));
});

test('TestNeedsPasskeySession', async () => {
  const tokenCtx = await mount(null, { session: { principal: 'mac-ssh', kind: 'token', expires: '' } });
  await toConfirm(tokenCtx);
  await sleep(20);
  assert.equal(byLabel(tokenCtx.root, '패스키로 서명').length, 0);
  assert.match(text(tokenCtx.root), /패스키로 로그인한 세션에서만/);
});

// ---- signRegistryChange / constants ---------------------------------------------------

test('TestSignRegistryChangeChecksTheMessage', async () => {
  const signer = { id: 'phone', credentialId: wa.b64uEncode(CRED) };
  const body = await wa.signRegistryChange(PROP_REG.message, signer);
  assert.deepEqual(Object.keys(body).sort(), ['alg', 'sig', 'signer', 'webauthn']);
  assert.deepEqual([...u8(auth.gets[0].publicKey.challenge)], [...sha(Buffer.from(PROP_REG.message))]);
  const n = auth.gets.length;
  for (const m of [
    '', 'aigate-approval-v1\n', PROP_REG.message + 'x', PROP_REG.message.slice(0, -1), PROP_REG.message.replace('\n', '\r\n'),
    PROP_REG.message.replace('subject: notes', 'subject: Notes'), PROP_REG.message.replace('request: 00', 'request: 0'),
    PROP_REG.message.replace('notes', 'no‮es'), PROP_REG.message + '\n',
  ]) {
    await assert.rejects(() => wa.signRegistryChange(m, signer), (e) => e.name === 'WebAuthnError', JSON.stringify(m));
  }
  await assert.rejects(() => wa.signRegistryChange(PROP_REG.message, { id: 'Bad Id', credentialId: signer.credentialId }), { code: 'bad_signer' });
  assert.equal(auth.gets.length, n, 'the authenticator was never asked for a bad message');
});

test('TestProjectsConstantsAndPathRule', () => {
  assert.equal(P.SUBJECT_RE.source, '^[a-z][a-z0-9-]{0,39}$');
  assert.equal(P.KIND_RE.source, '^[a-z][a-z0-9_-]{0,31}$');
  assert.equal(P.MAX_PATH_BYTES, 4096);
  assert.equal(P.SUBJECT_MAX, 40);
  assert.equal(P.KIND_MAX, 32);
  assert.equal(P.ACTION_REGISTER, 'register_project');
  assert.equal(P.ACTION_SET_FLAG, 'set_flag');
  assert.equal(P.FLAG_AI_CONTEXT, 'ai_context');
  const ok = (s) => P.isSafeProjectPath(s);
  assert.ok(ok('/home/orch/repos/notes'));
  assert.ok(ok('/home/orch/repos/프로젝트'));
  assert.ok(ok('/a b'));
  for (const s of ['', '/a​x', '/a　x', '/a x', '/áx', '/a️x', '/a‮x', '/a\tx', '/a\nx', '/a\x7fx', '/😀', '/꯿x', '/힤x']) assert.ok(!ok(s), JSON.stringify(s));
  assert.ok(ok('/가') && ok('/힣'));
  assert.ok(ok('/' + 'a'.repeat(4095)) && !ok('/' + 'a'.repeat(4096)));
  assert.ok(!ok('/' + '가'.repeat(1366)), 'length counts UTF-8 bytes');
});
