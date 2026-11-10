import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { createHash, webcrypto } from 'node:crypto';
import { findAll } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const src = (n) => readFileSync(new URL('../static/' + n, import.meta.url), 'utf8');
const wa = await import('../static/webauthn.js');
const T = await import('../static/views/tokens.js');
const E = await import('../static/expiry.js');
const P = await import('../static/views/projects.js');
const G = await import('../static/guard.js');
const { getView, parseRoute, navItem } = await import('../static/router.js');

const LIST = fx('tokens-list.json');
const LIST_CONFIG = fx('tokens-list-config.json');
const VEC = fx('token-vectors.json');
const APPLIED_SET = fx('tokens-apply-set.json');
const APPLIED_DEL = fx('tokens-apply-delete.json');
const VALUE = 'canary-NOT-A-REAL-TOKEN-0123456789abcdefghij-ZQxw';
const SECRET = '<img src=x onerror=alert(1)> javascript:alert(1) SERVER-WORDS';
const SESSION = { principal: 'phone', kind: 'session', expires: '2026-11-11T00:00:00Z' };
const TODAY = () => new Date(Date.UTC(2026, 10, 10, 3, 0, 0)); // 2026-11-10 UTC

// ---- helpers ------------------------------------------------------------------

const text = (n) => n.textContent;
const byTag = (root, tag) => findAll(root, (e) => e.tagName === tag.toUpperCase());
const byLabel = (root, label) => byTag(root, 'button').filter((b) => text(b).replace(/\s+/g, ' ').trim().startsWith(label));
const byId = (root, id) => findAll(root, (e) => e.getAttribute('id') === id)[0];
const byData = (root, k, v) => findAll(root, (e) => e.getAttribute('data-' + k) === v);
const click = (b) => { for (const f of b.listeners.click || []) f({ preventDefault() {} }); };
const fire = (n, ev) => { for (const f of n.listeners[ev] || []) f({}); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(cond, what) {
  for (let i = 0; i < 300; i++) {
    if (cond()) return;
    await sleep(2);
  }
  assert.fail('timed out waiting for ' + what);
}

const u8 = (b) => new Uint8Array(b);
const toAB = (b) => { const x = u8(b); return x.buffer.slice(x.byteOffset, x.byteOffset + x.byteLength); };
const CRED = new Uint8Array(32).fill(7);
const SIGNERS = { signers: [{ id: 'phone', credential_id: wa.b64uEncode(CRED), label: 'x' }] };

class Auth {
  constructor() { this.gets = []; this.gate = null; this.deny = false; }
  async get(opts) {
    this.gets.push(opts);
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
  T.configure({ now: TODAY });
});

// The JS copy of Go's tokenPreview. TestPreviewHelperMatchesGoFixtures pins it
// to the real sentences, so the fake server below says what the real one says.
function previewFor(b) {
  const stamp = '서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다';
  if (b.action === 'delete_token') {
    const out = [
      "토큰 '" + b.subject + "'을(를) 삭제합니다. 이 이름으로 저장된 값이 지워집니다",
      '삭제 서명에는 토큰의 종류가 들어가지 않습니다. 이름이 맞는지만 확인하세요',
    ];
    if (b.subject === 'openai') out.push('[중요] openai 키를 지우면 설정 파일에 키가 있으면 그 키를 쓰고, 없으면 AI 계획 기능이 멈춥니다');
    return [...out, stamp];
  }
  const kind = b.fields.kind;
  const out = ["토큰 '" + b.subject + "'을(를) 저장합니다(종류: " + kind + '). 같은 이름이 같은 종류로 이미 있으면 새 값으로 바꿉니다'];
  out.push(b.fields.expires === '' ? '만료일: 없음' : '만료일: ' + b.fields.expires + ' (직접 적은 날짜입니다. 서버가 확인하지 않으며 날짜는 UTC로 셉니다)');
  out.push(kind === 'openai'
    ? '[중요] 이 키는 AI 계획 기능이 외부 AI 서비스(OpenAI)를 호출할 때 쓰이며, 저장하면 다음 호출부터 바로 쓰입니다'
    : '이 토큰은 저장만 되고 아직 어떤 기능도 쓰지 않습니다(이후 연결 기능이 이름으로 가져다 씁니다)');
  return [...out, '토큰 값은 이 화면과 서명 메시지에 들어가지 않고, 서명에는 값의 해시만 묶입니다', stamp];
}

let seq = 0;
async function messageFor(body) {
  const change = await T.computeChange(body.action, body.subject, body.fields);
  return {
    message: 'aigate-registry-v1\naction: ' + body.action + '\nsubject: ' + body.subject + '\nchange: ' + change +
      '\nrequest: ' + body.request + '\nissued: ' + body.issued + '\n',
    change, request: body.request, issued: body.issued, preview: previewFor(body),
  };
}

// mount builds the real view on a fake ctx. `wire` records every POST body as
// JSON at the moment of the call (the view may clear its objects afterwards).
async function mount(over, extra) {
  const wire = [];
  const events = [];
  const rec = (name, fn) => (body, opts) => {
    wire.push({ name, json: JSON.stringify(body) });
    events.push(name);
    return fn(body, opts);
  };
  const routes = {
    '/api/v1/settings/tokens': LIST,
    '/api/v1/auth/signers': SIGNERS,
    'POST /api/v1/tokens/begin': rec('begin', () => {
      seq++;
      return { request: seq.toString(16).padStart(32, '0'), issued: '2026-11-10T03:00:00Z' };
    }),
    'POST /api/v1/tokens/message': rec('message', (b) => messageFor(b)),
    'POST /api/v1/tokens/apply': rec('apply', (b) => (b.action === 'set_token' ? { ...APPLIED_SET, ref: b.subject, kind: b.fields.kind } : { ...APPLIED_DEL, ref: b.subject })),
    ...(over || {}),
  };
  const ctx = fakeCtx(routes, { session: SESSION, ...(extra || {}) });
  ctx.wire = wire;
  ctx.events = events;
  T.tokensView(ctx);
  await until(() => byLabel(ctx.root, '토큰 추가').length > 0 || text(ctx.root).includes('불러오지 못했습니다'), 'list');
  return ctx;
}

async function openForm(ctx, over) {
  click(byLabel(ctx.root, '토큰 추가')[0]);
  await until(() => byId(ctx.root, 'x-field-b'), 'form');
  if (over) fillForm(ctx, over);
}
function fillForm(ctx, o) {
  if (o.kind !== undefined) { byId(ctx.root, 't-kind').value = o.kind; fire(byId(ctx.root, 't-kind'), 'change'); }
  if (o.ref !== undefined) byId(ctx.root, 'x-field-a').value = o.ref;
  if (o.expires !== undefined) byId(ctx.root, 't-expires').value = o.expires;
  if (o.value !== undefined) { byId(ctx.root, 'x-field-b').value = o.value; fire(byId(ctx.root, 'x-field-b'), 'input'); }
}
const GOOD = { kind: 'gitlab', ref: 'gitlab-main', expires: '2027-06-30', value: VALUE };
async function toConfirm(ctx, o) {
  await openForm(ctx, { ...GOOD, ...(o || {}) });
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => !!byId(ctx.root, 'h-says') || byData(ctx.root, 'warn', 'mismatch').length > 0, 'confirm');
  await ctx.settle();
}
const signBtn = (ctx) => byLabel(ctx.root, '패스키로 서명')[0];
const waitSignable = (ctx) => until(() => !!signBtn(ctx), 'sign button');
const posts = (ctx, name) => ctx.posts.filter((p) => p.path === '/api/v1/tokens/' + name);
const wireOf = (ctx, name) => ctx.wire.filter((w) => w.name === name).map((w) => w.json);

// scan: every text node, every attribute value and every .value property under root.
function leaks(root, needles) {
  const hits = [];
  const walk = (n) => {
    const check = (s, where) => {
      if (typeof s !== 'string') return;
      for (const nd of needles) if (s.includes(nd)) hits.push(where + ' has ' + nd.slice(0, 8));
    };
    if (n.nodeType === 3) check(n.data, 'text');
    if (n.nodeType === 1) {
      for (const [k, v] of n.attrs) check(v, 'attr ' + k);
      check(n.value, 'value prop of ' + n.tagName);
      n.childNodes.forEach(walk);
    }
  };
  walk(root);
  return hits;
}
const NEEDLES = [VALUE, VALUE.slice(0, 40), VALUE.slice(5, 30)];

// ---- tests (the brief's names) ------------------------------------------------

test('TestValueHashMatchesGoVector', async () => {
  assert.ok(VEC.value_hash.length >= 5 && VEC.value_hash_refused.length >= 4);
  for (const c of VEC.value_hash) {
    assert.equal(await T.computeValueHash(c.request, c.value), c.hash, c.name);
    // the same bytes as SHA-256(request ‖ ":" ‖ value), by an independent implementation
    assert.equal(createHash('sha256').update(c.request + ':' + c.value).digest('hex'), c.hash, c.name);
  }
  for (const c of VEC.value_hash_refused) {
    await assert.rejects(() => T.computeValueHash(c.request, c.value), { name: 'ChangeError' }, c.name);
  }
  // canonical change vectors, as in W3a
  assert.ok(VEC.change.length >= 5 && VEC.refused.length >= 10);
  for (const c of VEC.change) {
    assert.equal(T.canonicalChange(c.action, c.subject, c.fields), c.canonical, c.name);
    assert.equal(await T.computeChange(c.action, c.subject, c.fields), c.change, c.name);
    const rev = {};
    for (const k of Object.keys(c.fields).reverse()) rev[k] = c.fields[k];
    assert.equal(T.canonicalChange(c.action, c.subject, rev), c.canonical, c.name + ' (reversed)');
  }
  for (const r of VEC.refused) {
    await assert.rejects(async () => T.computeChange(r.action, r.subject, r.fields), { name: 'ChangeError' }, r.name);
  }
  const m = VEC.message;
  assert.equal(createHash('sha256').update(m.text).digest('hex'), m.digest);
  assert.deepEqual(wa.checkRegistryMessage(m.text), { action: m.action, subject: m.subject, change: m.change, request: m.request, issued: m.issued });
  // a signing message for each of the four registry actions is accepted, others are not
  for (const a of ['register_project', 'set_flag', 'set_token', 'delete_token']) {
    assert.equal(wa.checkRegistryMessage(m.text.replace('set_token', a)).action, a);
  }
  assert.throws(() => wa.checkRegistryMessage(m.text.replace('set_token', 'purge_token')));
});

test('TestPreviewHelperMatchesGoFixtures', () => {
  const same = (name, body) => assert.deepEqual(previewFor(body), fx(name).preview, name);
  same('tokens-message-set.json', { action: 'set_token', subject: 'gitlab-main', fields: { kind: 'gitlab', expires: '2027-06-30' } });
  same('tokens-message-set-noexp.json', { action: 'set_token', subject: 'gitlab-main', fields: { kind: 'gitlab', expires: '' } });
  same('tokens-message-set-openai.json', { action: 'set_token', subject: 'openai', fields: { kind: 'openai', expires: '' } });
  same('tokens-message-delete.json', { action: 'delete_token', subject: 'gitlab-main', fields: {} });
  same('tokens-message-delete-openai.json', { action: 'delete_token', subject: 'openai', fields: {} });
});

test('TestTokensListRendersAsText', async () => {
  const hostile = { ref: '<img src=x onerror=alert(1)>', kind: 'javascript:alert(1)', last4: '<b>x', expires: '"><script>1', set_at: SECRET, source: 'store', days_left: 3, usable: true };
  const ctx = await mount({ '/api/v1/settings/tokens': { tokens: [...LIST.tokens, hostile], warn_days: 14 } });
  assert.equal(byTag(ctx.root, 'img').length, 0);
  assert.equal(byTag(ctx.root, 'script').length, 0);
  assert.equal(byTag(ctx.root, 'b').length, 0, 'no <b> from data');
  for (const e of findAll(ctx.root, () => true)) {
    for (const [k, v] of e.attrs) {
      assert.ok(!/onerror|javascript:|<img|SERVER-WORDS/.test(v), 'attribute ' + k + ' carries server text: ' + v);
    }
  }
  const t = text(ctx.root);
  assert.ok(t.includes('<img src=x onerror=alert(1)>'), 'shown literally');
  assert.ok(t.includes('javascript:alert(1)'));
  // a name that is not a valid ref gets no action buttons (it could not be signed anyway)
  const rows = findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).includes('onerror'));
  assert.equal(rows.length, 1);
  assert.equal(byTag(rows[0], 'button').length, 0);
  // the real rows: name, kind, last 4, expiry, days left, source
  assert.ok(t.includes('gitlab-main') && t.includes('a1B2') && t.includes('2026-11-13'));
  assert.match(t, /남은 일수/);
  // list failure: fixed text, no server words
  const bad = fakeCtx({ '/api/v1/settings/tokens': () => { throw apiError(503, 'tokens_unavailable', SECRET); } }, { session: SESSION });
  T.tokensView(bad);
  await until(() => text(bad.root).includes(T.ERROR_TEXT.tokens_unavailable), 'list failure');
  assert.ok(!text(bad.root).includes('SERVER-WORDS'));
});

test('TestConfigRowHasNoDeleteAndUnusableIsShown', async () => {
  const ctx = await mount({ '/api/v1/settings/tokens': { tokens: [...LIST_CONFIG.tokens, ...LIST.tokens], warn_days: 14 } });
  const rowOf = (name, src) => findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).includes(name) && (src ? text(e).includes(src) : true))[0];
  const cfg = findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).includes('설정 파일'))[0];
  assert.ok(cfg, 'config row');
  assert.equal(byLabel(cfg, '삭제').length, 0, 'no delete for a config key (the broker would answer token_unknown)');
  assert.equal(byLabel(cfg, '교체').length, 1, 'a config key can be replaced by a stored one');
  const lost = rowOf('github-lost');
  assert.match(text(lost), /사용 불가/);
  assert.equal(byLabel(lost, '삭제').length, 1, 'an unusable stored token can be deleted');
  assert.equal(byLabel(lost, '교체').length, 1);
  assert.equal(byLabel(rowOf('gitlab-main'), '삭제').length, 1);
});

test('TestPasswordInputAttributes', async () => {
  // History: type=password failed (Chrome ignores autocomplete=off); a masked text
  // input with CSS text masking also failed in real Chrome (treated as password-like,
  // the name field looked like a username). Now: a plain text input whose text is
  // transparent, with a bullet overlay drawn by this page; autocomplete=one-time-code.
  const ctx = await mount();
  await openForm(ctx);
  const v = byId(ctx.root, 'x-field-b');
  assert.equal(v.tagName, 'INPUT');
  assert.equal(v.getAttribute('type'), 'text');
  assert.match(v.getAttribute('class'), /\bovlin\b/, 'text made transparent by CSS');
  for (const [k, want] of Object.entries({ autocomplete: 'one-time-code', autocapitalize: 'off', autocorrect: 'off', spellcheck: 'false',
    'data-lpignore': 'true', 'data-1p-ignore': 'true', 'data-bwignore': 'true', 'data-form-type': 'other' })) {
    assert.equal(v.getAttribute(k), want, k);
  }
  assert.equal(v.getAttribute('value'), null, 'no value attribute');
  assert.equal(v.getAttribute('maxlength'), String(T.TOKEN_VALUE_MAX + 1), 'longer input is still seen (and refused with a sentence)');
  assert.equal(v.getAttribute('data-focus-key'), 'value');
  assert.equal(v.getAttribute('name'), 'x-field-b');
  // the label and the length description: never the value
  assert.equal(findAll(ctx.root, (e) => e.getAttribute('for') === 'x-field-b').length, 1);
  const desc = v.getAttribute('aria-describedby');
  assert.match(desc, /^[A-Za-z0-9_-]+$/);
  assert.equal(text(byId(ctx.root, desc)), '입력한 글자 수: 0');
  // neutral names: nothing username/password/token/secret/key-like on ids, names, placeholders, classes
  const r = byId(ctx.root, 'x-field-a');
  const bad = /user|login|pass|pwd|secret|token|email|account|key|name/i;
  for (const inp of [v, r]) {
    for (const k of ['id', 'name', 'placeholder', 'aria-label']) assert.ok(!bad.test(inp.getAttribute(k) || ''), k + ' of ' + inp.getAttribute('id'));
  }
  assert.ok(!/secret|pass|token|key/i.test(v.getAttribute('class')));
  // the label field: not "name", not username-like; off + readonly until focus
  assert.equal(r.getAttribute('autocomplete'), 'off');
  assert.equal(r.getAttribute('autocapitalize'), 'off');
  assert.equal(r.getAttribute('data-lpignore'), 'true');
  assert.equal(r.getAttribute('type'), 'text', 'no email/tel/search tricks');
  assert.equal(r.hasAttribute('readonly'), true, 'readonly at render');
  fire(r, 'focus');
  assert.equal(r.hasAttribute('readonly'), false, 'removed on focus');
  const c2 = await mount();
  await openForm(c2);
  const r2 = byId(c2.root, 'x-field-a');
  assert.equal(r2.hasAttribute('readonly'), true);
  fire(r2, 'pointerdown');
  assert.equal(r2.hasAttribute('readonly'), false, 'removed on pointerdown');
  for (const lb of findAll(ctx.root, (e) => e.tagName === 'LABEL')) {
    assert.ok(!/이름|사용자|비밀번호|password|username|아이디/i.test(text(lb)), 'label wording: ' + text(lb));
  }
  assert.match(text(findAll(ctx.root, (e) => e.tagName === 'LABEL' && e.getAttribute('for') === 'x-field-a')[0]), /식별 라벨/);
  // no <form>, no password-type field, nothing text-masking anywhere
  assert.equal(findAll(ctx.root, (e) => e.tagName === 'FORM').length, 0);
  assert.equal(findAll(ctx.root, (e) => e.getAttribute('type') === 'password').length, 0);
  const code = src('views/tokens.js');
  for (const bad2 of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'caches.', 'navigator.storage', 'history.']) {
    assert.ok(!code.includes(bad2), 'tokens.js uses ' + bad2);
  }
  assert.ok(!/\.paste|onpaste/.test(code), 'pasting stays allowed');
  // the CSS: transparent text, visible caret, an overlay that ignores the pointer
  const css = src('style.css');
  assert.match(css, /input\.ovlin\s*\{[^}]*color:\s*transparent/);
  assert.match(css, /input\.ovlin\s*\{[^}]*caret-color:/);
  assert.match(css, /\.ovlmask\s*\{[^}]*pointer-events:\s*none/);
  // forced colors / high contrast must not override the transparent text and print the characters
  assert.match(css, /^\.form \.ovlbox input\.ovlin\s*\{[^}]*forced-color-adjust:\s*none/m);
  assert.match(css, /@media \(forced-colors: active\)\s*\{[^}]*input\.ovlin\s*\{[^}]*color:\s*transparent/);
  // the expiry field asks for nothing from the browser either
  const ex = byId(ctx.root, 't-expires');
  assert.equal(ex.getAttribute('autocomplete'), 'off');
  assert.equal(ex.getAttribute('name'), 'x-field-c');
  assert.ok(!bad.test(ex.getAttribute('id') + ex.getAttribute('name')));
});

test('TestNoPasswordLikeMarkupShipped', () => {
  // The failed approaches must not come back: no password type, no CSS text masking, anywhere in static/.
  const dir = new URL('../static/', import.meta.url);
  const walk = (u) => readdirSync(u, { withFileTypes: true }).flatMap((d) => (d.isDirectory() ? walk(new URL(d.name + '/', u)) : [new URL(d.name, u)]));
  const files = walk(dir);
  assert.ok(files.length > 15);
  for (const f of files) {
    if (!/\.(js|css|html)$/.test(f.pathname)) continue;
    const t = readFileSync(f, 'utf8');
    assert.ok(!/text-?security/i.test(t), f.pathname + ' has text-security (or webkitTextSecurity)');
    assert.ok(!/type\s*[:=]\s*['"`]?password/i.test(t) && !/type=["']password/i.test(t) && !/\[type=["']?password/i.test(t), f.pathname + ' has a password type');
    assert.ok(!/autocomplete['"]?\s*[:=]\s*['"](?:new-password|current-password|username)/i.test(t), f.pathname + ' uses a credential autocomplete');
  }
});

test('TestMaskOverlayFollowsValue', async () => {
  const mask = (ctx) => byData(ctx.root, 'mask', 'value')[0];
  const lenText = (ctx) => text(byId(ctx.root, byId(ctx.root, 'x-field-b').getAttribute('aria-describedby')));
  const ctx = await mount();
  await openForm(ctx);
  const inp = byId(ctx.root, 'x-field-b');
  const m0 = mask(ctx);
  assert.equal(mask(ctx).getAttribute('aria-hidden'), 'true', 'the bullets are not read out');
  assert.equal(text(mask(ctx)), '');
  const type = (v) => { inp.value = v; fire(inp, 'input'); };
  type('a'); assert.equal(text(mask(ctx)), '•');
  type(VALUE); // paste
  assert.equal(text(mask(ctx)), '•'.repeat(VALUE.length));
  assert.equal(lenText(ctx), '입력한 글자 수: ' + VALUE.length);
  type(VALUE.slice(0, 10)); // partial delete
  assert.equal(text(mask(ctx)), '•'.repeat(10));
  assert.equal(lenText(ctx), '입력한 글자 수: 10');
  type(''); assert.equal(text(mask(ctx)), '');
  assert.equal(lenText(ctx), '입력한 글자 수: 0');
  // never the real characters, never the value, in the overlay, the description or any attribute
  type(VALUE);
  assert.deepEqual(leaks(mask(ctx), NEEDLES), []);
  assert.ok(!/[A-Za-z0-9]/.test(text(mask(ctx))));
  assert.equal(inp.value, VALUE, 'the typed characters are the field value (copy is blocked, see TestCopyCutDragBlockedPasteKept)');
  // cancel empties input and overlay together
  click(byLabel(ctx.root, '취소')[0]);
  assert.equal(inp.value, '');
  assert.equal(text(m0), '', 'overlay cleared on cancel');
  // leave
  const c2 = await mount();
  await openForm(c2, GOOD);
  const i2 = byId(c2.root, 'x-field-b'); const m2 = mask(c2);
  assert.equal(text(m2), '•'.repeat(VALUE.length));
  c2.leave();
  assert.equal(i2.value, ''); assert.equal(text(m2), '', 'overlay cleared on leave');
  // submit while busy: field and overlay already empty; typing while busy then confirmation: both cleared
  let gate;
  const hold = new Promise((r) => { gate = r; });
  const c3 = await mount({ 'POST /api/v1/tokens/begin': async () => { await hold; seq++; return { request: seq.toString(16).padStart(32, '0'), issued: '2026-11-10T03:00:00Z' }; } });
  await openForm(c3, GOOD);
  const i3 = byId(c3.root, 'x-field-b'); const m3 = mask(c3);
  click(byLabel(c3.root, '변경 내용 확인')[0]);
  await until(() => posts(c3, 'begin').length === 1, 'begin');
  assert.equal(i3.value, ''); assert.equal(text(m3), '', 'overlay empty right after submit');
  i3.value = 'late'; fire(i3, 'input');
  assert.equal(text(m3), '••••');
  gate();
  await waitSignable(c3);
  assert.equal(i3.value, ''); assert.equal(text(m3), '', 'overlay cleared when the confirmation is built');
  // an error path (begin fails): overlay empty
  const c4 = await mount({ 'POST /api/v1/tokens/begin': () => { throw apiError(500, 'internal_error', SECRET); } });
  await openForm(c4, GOOD);
  const m4 = mask(c4);
  click(byLabel(c4.root, '변경 내용 확인')[0]);
  await until(() => text(c4.root).includes(T.PREP_TEXT.server), 'error');
  assert.equal(text(m4), '');
  // a refused local validation keeps what was typed (and its bullets)
  const c5 = await mount();
  await openForm(c5, { ...GOOD, value: 'short' });
  click(byLabel(c5.root, '변경 내용 확인')[0]);
  await until(() => text(c5.root).includes(T.ERROR_TEXT.token_too_short), 'too short');
  assert.equal(text(mask(c5)), '•••••');
});

test('TestConfirmShowsKindRefExpiryLast4AndHashPrefixOnly', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  assert.ok(signBtn(ctx));
  const hash = await T.computeValueHash(JSON.parse(wireOf(ctx, 'message')[0]).request, VALUE);
  const field = (k) => text(byData(ctx.root, 'field', k)[0]);
  assert.match(field('kind'), /gitlab/);
  assert.match(field('ref'), /gitlab-main/);
  assert.match(field('expires'), /2027-06-30/);
  assert.match(field('last4'), /ZQxw/);
  assert.ok(field('hash').includes(hash.slice(0, 8)));
  assert.ok(!text(ctx.root).includes(hash), 'the full hash is not shown');
  assert.ok(!text(ctx.root).includes(hash.slice(0, 9)), 'only 8 characters of it');
  assert.deepEqual(leaks(ctx.root, NEEDLES), [], 'the value is nowhere on the confirmation screen');
  assert.ok(!text(ctx.root).includes(VALUE.slice(0, -4) + 'Z'));
  // the server's sentences are shown as text, in their own list
  const items = byData(ctx.root, 'preview', 'item').map(text);
  assert.deepEqual(items, previewFor({ action: 'set_token', subject: 'gitlab-main', fields: { kind: 'gitlab', expires: '2027-06-30' } }));
  assert.equal(byLabel(ctx.root, '취소').length, 1);
  // the confirmation says "no expiry" in words when none was typed
  const c2 = await mount();
  await toConfirm(c2, { expires: '' });
  assert.match(text(byData(c2.root, 'field', 'expires')[0]), /없음/);
});

test('TestSignButtonOnlyWhenChangeMatches', async () => {
  const bad = {
    'wrong change': async (b) => ({ ...(await messageFor(b)), change: '0'.repeat(64) }),
    'message with another change': async (b) => { const r = await messageFor(b); return { ...r, message: r.message.replace(r.change, '1'.repeat(64)) }; },
    'message for another subject': async (b) => { const r = await messageFor(b); return { ...r, message: r.message.replace('subject: gitlab-main', 'subject: gitlab-other') }; },
    'message for another action': async (b) => { const r = await messageFor(b); return { ...r, message: r.message.replace('set_token', 'delete_token') }; },
    'message with another request': async (b) => { const r = await messageFor(b); return { ...r, message: r.message.replace(b.request, 'f'.repeat(32)) }; },
    'response with another request': async (b) => ({ ...(await messageFor(b)), request: 'f'.repeat(32) }),
    'response with another issued': async (b) => ({ ...(await messageFor(b)), issued: '2026-11-10T03:00:01Z' }),
    'message not canonical (extra blank)': async (b) => { const r = await messageFor(b); return { ...r, message: r.message + '\n' }; },
    'message with hidden character': async (b) => { const r = await messageFor(b); return { ...r, message: r.message.replace('subject: ', 'subject: ​') }; },
    'hostile preview': async (b) => ({ ...(await messageFor(b)), preview: ['ok‮evil'] }),
    'empty preview': async (b) => ({ ...(await messageFor(b)), preview: [] }),
    'preview of another token': async (b) => ({ ...(await messageFor(b)), preview: previewFor({ ...b, subject: 'gitlab-other' }) }),
    'preview of another expiry': async (b) => ({ ...(await messageFor(b)), preview: previewFor({ ...b, fields: { ...b.fields, expires: '2030-01-01' } }) }),
    'preview of another kind': async (b) => ({ ...(await messageFor(b)), preview: previewFor({ ...b, fields: { ...b.fields, kind: 'github' } }) }),
    'not an object': async () => 'nope',
  };
  for (const [name, fn] of Object.entries(bad)) {
    const ctx = await mount({ 'POST /api/v1/tokens/message': fn });
    await toConfirm(ctx);
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, name + ': warning shown');
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, name + ': no sign button');
    assert.equal(auth.gets.length, 0, name + ': nothing signed');
    assert.equal(posts(ctx, 'apply').length, 0, name);
    assert.deepEqual(leaks(ctx.root, NEEDLES), [], name + ': value not shown');
    assert.equal(T.heldValueLength(), 0, name + ': nothing held after a refused proposal');
  }
  const ok = await mount();
  await toConfirm(ok);
  await waitSignable(ok);
  assert.equal(byData(ok.root, 'warn', 'mismatch').length, 0);
});

test('TestFlowOrderBeginMessageSignApply', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => /저장했습니다/.test(text(ctx.root)), 'done');
  assert.deepEqual(ctx.events, ['begin', 'message', 'apply']);
  assert.equal(auth.gets.length, 1, 'one passkey signature between message and apply');
  const [begin, message, apply] = ['begin', 'message', 'apply'].map((n) => JSON.parse(wireOf(ctx, n)[0]));
  assert.deepEqual(begin, { action: 'set_token', subject: 'gitlab-main', fields: { kind: 'gitlab', expires: '2027-06-30' } });
  assert.deepEqual(Object.keys(message).sort(), ['action', 'fields', 'issued', 'request', 'subject']);
  const hash = createHash('sha256').update(message.request + ':' + VALUE).digest('hex');
  assert.equal(message.fields.value_hash, hash, 'value_hash is bound to the request id begin returned');
  assert.equal(message.request, '0'.repeat(32 - seq.toString(16).length) + seq.toString(16));
  assert.deepEqual(Object.keys(message.fields).sort(), ['expires', 'kind', 'value_hash']);
  // the value is in the apply body and nowhere else
  for (const n of ['begin', 'message']) assert.ok(!wireOf(ctx, n)[0].includes(VALUE) && !wireOf(ctx, n)[0].includes('ZQxw'), n + ' carries no value');
  assert.equal(apply.value, VALUE);
  assert.deepEqual(Object.keys(apply).sort(), ['action', 'change', 'fields', 'issued', 'request', 'signature', 'subject', 'value']);
  assert.deepEqual(apply.fields, message.fields);
  assert.equal(apply.change, await T.computeChange('set_token', 'gitlab-main', message.fields));
  assert.equal(apply.request, message.request);
  assert.equal(apply.issued, message.issued);
  assert.equal(apply.signature.alg, 'webauthn-es256');
  // the passkey signed the message text of THIS request
  const digest = createHash('sha256').update(
    'aigate-registry-v1\naction: set_token\nsubject: gitlab-main\nchange: ' + apply.change + '\nrequest: ' + apply.request + '\nissued: ' + apply.issued + '\n').digest();
  assert.deepEqual(Buffer.from(auth.gets[0].publicKey.challenge), digest);
  // calls: all three writes and nothing but the list reload besides
  assert.deepEqual(ctx.calls.filter((c) => c !== '/api/v1/auth/signers'), ['/api/v1/settings/tokens', '/api/v1/settings/tokens']);
  assert.equal(T.heldValueLength(), 0);
});

test('TestValueNeverInDOMOrStateAfterSubmit', async () => {
  const noValueAnywhere = (ctx, what) => {
    assert.deepEqual(leaks(ctx.root, NEEDLES), [], what + ': DOM');
    assert.equal(T.heldValueLength(), 0, what + ': variable');
    const input = byId(ctx.root, 'x-field-b');
    if (input) assert.equal(input.value, '', what + ': input');
    assert.ok(!JSON.stringify(ctx.calls).includes(VALUE) && !JSON.stringify(ctx.nav).includes(VALUE), what + ': urls');
    assert.equal(ctx.guard, null, what + ': guard released');
  };
  // 1. while the confirmation is up the value is held but not drawn; the input is empty
  const a = await mount();
  await toConfirm(a);
  await waitSignable(a);
  assert.deepEqual(leaks(a.root, NEEDLES), []);
  assert.equal(byId(a.root, 'x-field-b'), undefined, 'the password field is gone');
  assert.equal(T.heldValueLength(), VALUE.length, 'held only for the apply');
  // 2. after the apply
  click(signBtn(a));
  await until(() => /저장했습니다/.test(text(a.root)), 'done');
  noValueAnywhere(a, 'after apply');
  click(byLabel(a.root, '목록으로')[0]);
  await until(() => byLabel(a.root, '토큰 추가').length === 1, 'list');
  noValueAnywhere(a, 'back on the list');
  // 3. cancel from the confirmation; a second go without typing hits the local rule: the variable really is empty
  const b = await mount();
  await toConfirm(b);
  click(byLabel(b.root, '취소')[0]);
  await until(() => byLabel(b.root, '토큰 추가').length === 1, 'list after cancel');
  noValueAnywhere(b, 'after cancel');
  await openForm(b, { ...GOOD, value: undefined });
  click(byLabel(b.root, '변경 내용 확인')[0]);
  await until(() => text(b.root).includes(T.ERROR_TEXT.token_too_short), 'too short again');
  assert.equal(posts(b, 'begin').length, 1, 'no second begin without a value');
  // 4. leaving the view while the confirmation is up
  const c = await mount();
  await toConfirm(c);
  assert.ok(T.heldValueLength() > 0);
  c.leave();
  assert.equal(T.heldValueLength(), 0, 'after leaving');
  assert.deepEqual(leaks(c.root, NEEDLES), []);
  // 5. an error at begin, at message, at the passkey and at apply
  for (const [name, over, fail] of [
    ['begin error', { 'POST /api/v1/tokens/begin': () => { throw apiError(429, 'token_rate_limited', SECRET); } }, 'form'],
    ['message error', { 'POST /api/v1/tokens/message': () => { throw apiError(400, 'bad_request', SECRET); } }, 'form'],
    ['apply error', { 'POST /api/v1/tokens/apply': () => { throw apiError(403, 'bad_signature', SECRET); } }, 'result'],
  ]) {
    const d = await mount(over);
    if (fail === 'form') {
      await openForm(d, GOOD);
      click(byLabel(d.root, '변경 내용 확인')[0]);
      await until(() => /너무 잦|형식이 올바르지/.test(text(d.root)), name);
      await d.settle();
      assert.equal(T.heldValueLength(), 0, name + ': variable');
      assert.equal(byId(d.root, 'x-field-b').value, '', name + ': input');
      assert.deepEqual(leaks(d.root, NEEDLES), [], name);
      assert.equal(d.guard, null, name + ': no trap after an error');
    } else {
      await toConfirm(d);
      await waitSignable(d);
      click(signBtn(d));
      await until(() => byData(d.root, 'result', 'error').length === 1, name);
      noValueAnywhere(d, name);
    }
  }
  // 6. the passkey was cancelled: the confirmation stays (and the value with it, to try again), then cancel wipes
  const e = await mount();
  await toConfirm(e);
  await waitSignable(e);
  auth.deny = true;
  click(signBtn(e));
  await until(() => text(e.root).includes(wa.WA_TEXT.cancelled), 'cancelled');
  assert.deepEqual(leaks(e.root, NEEDLES), []);
  click(byLabel(e.root, '취소')[0]);
  await until(() => byLabel(e.root, '토큰 추가').length === 1, 'list');
  noValueAnywhere(e, 'after a cancelled passkey and cancel');
  // static: nothing writes the value to a log, a URL, an attribute or an event
  const code = src('views/tokens.js');
  for (const bad of ['console.', 'dispatchEvent', 'postMessage', 'location.', 'JSON.stringify(held', 'setAttribute', 'innerHTML', 'outerHTML', 'insertAdjacent']) {
    assert.ok(!code.includes(bad), 'tokens.js contains ' + bad);
  }
});

test('TestDeleteNeedsNameConfirmAndSignature', async () => {
  const ctx = await mount();
  const row = findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).includes('gitlab-main'))[0];
  click(byLabel(row, '삭제')[0]);
  await until(() => byId(ctx.root, 't-confirm-ref'), 'delete form');
  assert.equal(byId(ctx.root, 'x-field-b'), undefined, 'a delete never asks for a value');
  const go = () => byLabel(ctx.root, '삭제 내용 확인')[0];
  // no name, a wrong name, a name with a space: nothing is requested
  for (const typed of ['', 'gitlab-other', 'gitlab-main ', 'GITLAB-MAIN']) {
    byId(ctx.root, 't-confirm-ref').value = typed;
    click(go());
    await ctx.settle();
    assert.equal(ctx.wire.length, 0, 'nothing sent for ' + JSON.stringify(typed));
    assert.match(text(ctx.root), /이름을 정확히/);
  }
  byId(ctx.root, 't-confirm-ref').value = 'gitlab-main';
  click(go());
  await until(() => !!byId(ctx.root, 'h-says'), 'confirmation');
  await waitSignable(ctx);
  assert.equal(auth.gets.length, 0, 'not signed before the button');
  assert.match(text(byData(ctx.root, 'field', 'ref')[0]), /gitlab-main/);
  assert.equal(byData(ctx.root, 'field', 'last4').length, 0);
  assert.deepEqual(byData(ctx.root, 'preview', 'item').map(text), previewFor({ action: 'delete_token', subject: 'gitlab-main', fields: {} }));
  click(signBtn(ctx));
  await until(() => /삭제했습니다/.test(text(ctx.root)), 'deleted');
  assert.deepEqual(ctx.events, ['begin', 'message', 'apply']);
  const [begin, message, apply] = ['begin', 'message', 'apply'].map((n) => JSON.parse(wireOf(ctx, n)[0]));
  assert.deepEqual(begin, { action: 'delete_token', subject: 'gitlab-main', fields: {} });
  assert.deepEqual(message.fields, {});
  assert.ok(!('value' in apply), 'no value key at all');
  assert.deepEqual(Object.keys(apply).sort(), ['action', 'change', 'fields', 'issued', 'request', 'signature', 'subject']);
  assert.equal(apply.signature.alg, 'webauthn-es256');
  assert.equal(apply.change, await T.computeChange('delete_token', 'gitlab-main', {}));
  assert.equal(auth.gets.length, 1);
  // cancel from the name screen goes back without a request
  const c2 = await mount();
  click(byLabel(findAll(c2.root, (e) => e.tagName === 'TR' && text(e).includes('github-old'))[0], '삭제')[0]);
  await until(() => byId(c2.root, 't-confirm-ref'), 'form');
  click(byLabel(c2.root, '취소')[0]);
  await until(() => byLabel(c2.root, '토큰 추가').length === 1, 'list');
  assert.equal(c2.wire.length, 0);
});

test('TestExpiryBannerWithin14DaysAndPast', async () => {
  const rows = LIST.tokens;
  assert.deepEqual(E.expiringRows(rows, 14).map((r) => r.ref), ['gitlab-main', 'github-old'], 'within 14 days and already past');
  assert.deepEqual(E.expiringRows(rows, 2).map((r) => r.ref), ['github-old'], 'the server\'s warn_days is used, not a literal 14');
  assert.deepEqual(E.expiringRows(rows, 233).map((r) => r.ref), ['openai', 'gitlab-main', 'github-old', 'github-lost']);
  assert.deepEqual(E.expiringRows(rows, 0).map((r) => r.ref), ['github-old'], 'warn_days 0: only what is already past or due today');
  assert.deepEqual(E.expiringRows(rows, -5).map((r) => r.ref), ['gitlab-main', 'github-old'], 'an impossible warn_days falls back to 14');
  assert.deepEqual(E.expiringRows([{ ref: 'a', days_left: null }, { ref: 'b' }, { ref: 'c', days_left: '3' }, { ref: 'd', days_left: 1.5 }], 14), []);
  assert.deepEqual(E.expiringRows(rows, 'x').map((r) => r.ref), ['gitlab-main', 'github-old'], 'a broken warn_days falls back to 14');
  assert.equal(E.expiryBanner([], 14), null);
  assert.equal(E.expiryBanner(rows.filter((r) => r.days_left > 14 || r.days_left === null), 14), null);
  const b = E.expiryBanner(rows, 14);
  const t = text(b);
  assert.match(t, /토큰 2개가 14일 안에 만료됩니다/);
  assert.match(t, /이미 만료일이 지난 토큰이 1개/);
  assert.match(t, /UTC/);
  assert.match(t, /직접 적은 날짜/);
  assert.match(t, /GitLab·GitHub/);
  assert.match(t, /확인하지 않/);
  assert.ok(t.includes('gitlab-main') && t.includes('github-old'));
  assert.equal(b.getAttribute('role'), 'status');
  assert.equal(b.getAttribute('data-banner'), 'expiry');
  // hostile names are text
  const h = E.expiryBanner([{ ref: '<img src=x onerror=alert(1)>', days_left: 1 }], 14);
  assert.equal(byTag(h, 'img').length, 0);
  assert.ok(text(h).includes('<img src=x onerror=alert(1)>'));
  // on the tokens screen
  const ctx = await mount();
  assert.equal(byData(ctx.root, 'banner', 'expiry').length, 1);
  assert.match(text(byData(ctx.root, 'banner', 'expiry')[0]), /토큰 2개가 14일 안에/);
  const calm = await mount({ '/api/v1/settings/tokens': LIST_CONFIG });
  assert.equal(byData(calm.root, 'banner', 'expiry').length, 0);
  // and on the projects screen (a small shared component)
  const pctx = fakeCtx({
    '/api/v1/projects/registry': fx('projects-registry.json'),
    '/api/v1/settings/tokens': LIST,
  }, { session: SESSION });
  P.projectsView(pctx);
  await until(() => byData(pctx.root, 'banner', 'expiry').length === 1, 'banner on the projects screen');
  assert.match(text(byData(pctx.root, 'banner', 'expiry')[0]), /토큰 2개가 14일 안에/);
  // a failing token list never breaks the projects screen
  const pfail = fakeCtx({
    '/api/v1/projects/registry': fx('projects-registry.json'),
    '/api/v1/settings/tokens': () => { throw apiError(503, 'tokens_unavailable', SECRET); },
  }, { session: SESSION });
  P.projectsView(pfail);
  await until(() => byLabel(pfail.root, '프로젝트 추가').length === 1, 'projects list');
  await pfail.settle();
  assert.equal(byData(pfail.root, 'banner', 'expiry').length, 0);
  assert.ok(!text(pfail.root).includes('SERVER-WORDS'));
});

test('TestExpiresRequiredHintForGitHostTokens', async () => {
  const ctx = await mount();
  await openForm(ctx);
  const hint = () => text(byData(ctx.root, 'hint', 'expires')[0]);
  fillForm(ctx, { kind: 'gitlab' });
  assert.match(hint(), /권장/, 'gitlab: recommended');
  assert.match(hint(), /GitLab|GitHub/);
  fillForm(ctx, { kind: 'github' });
  assert.match(hint(), /권장/, 'github: recommended');
  fillForm(ctx, { kind: 'openai' });
  assert.ok(!/권장/.test(hint()), 'openai: no recommendation');
  assert.equal(byId(ctx.root, 'x-field-a').value, 'openai', 'the openai kind has one name');
  // the expiry is typed by the user: the form says so
  assert.match(text(ctx.root), /직접 적은 날짜/);
  // expiry rules, checked by the browser before anything is requested
  const tryExp = async (expires, ok, why) => {
    const c = await mount();
    await openForm(c, { ...GOOD, expires });
    click(byLabel(c.root, '변경 내용 확인')[0]);
    if (ok) {
      await until(() => posts(c, 'begin').length === 1, why);
    } else {
      await until(() => /만료일/.test(text(byData(c.root, 'problem', 'form')[0])), why);
      assert.equal(c.wire.length, 0, why + ': nothing sent');
    }
  };
  await tryExp('', true, 'empty is allowed (recommended, not required)');
  await tryExp('2026-11-10', true, 'today (UTC) is allowed');
  await tryExp('9999-12-31', true, 'the latest year');
  await tryExp('2026-11-09', false, 'yesterday (UTC)');
  await tryExp('1999-12-31', false, 'before 2000');
  await tryExp('2026-02-30', false, 'impossible date');
  await tryExp('2028-02-30', false, 'impossible date in a leap year');
  await tryExp('2026-13-01', false, 'month 13');
  await tryExp('2026-1-1', false, 'format');
  await tryExp('2027-06-30 ', false, 'trailing space is refused, not trimmed');
  await tryExp(' 2027-06-30', false, 'leading space');
  await tryExp('10000-01-01', false, 'five-digit year');
  await tryExp('２０２７-06-30', false, 'full-width digits');
  // the day is the BROWSER's UTC date: 23:30 UTC on the 10th is still the 10th (KST already the 11th)
  T.configure({ now: () => new Date(Date.UTC(2026, 10, 10, 23, 30, 0)) });
  await tryExp('2026-11-10', true, 'still today in UTC');
  await tryExp('2026-11-09', false, 'a day before');
  T.configure({ now: () => new Date(Date.UTC(2026, 10, 11, 0, 0, 1)) });
  await tryExp('2026-11-10', false, 'a second into the 11th UTC');
});

test('TestD3ValuePreValidationBeforeHashAndSign', async () => {
  const cases = {
    'too short (31)': ['a'.repeat(31), 'token_too_short'],
    'empty': ['', 'token_too_short'],
    'too long (513)': ['a'.repeat(513), 'token_bad_format'],
    'space inside': ['a'.repeat(20) + ' ' + 'b'.repeat(20), 'token_bad_format'],
    'leading space': [' ' + 'a'.repeat(40), 'token_bad_format'],
    'trailing space': ['a'.repeat(40) + ' ', 'token_bad_format'],
    'trailing newline': ['a'.repeat(40) + '\n', 'token_bad_format'],
    'tab inside': ['a'.repeat(20) + '\t' + 'b'.repeat(20), 'token_bad_format'],
    'non-ASCII': ['a'.repeat(40) + 'é', 'token_bad_format'],
    'full-width': ['ａ'.repeat(40), 'token_bad_format'],
    'zero-width': ['a'.repeat(20) + '​' + 'b'.repeat(20), 'token_bad_format'],
    'DEL': ['a'.repeat(40) + '\x7f', 'token_bad_format'],
    'NUL': ['a'.repeat(40) + '\0', 'token_bad_format'],
  };
  for (const [name, [value, code]] of Object.entries(cases)) {
    const ctx = await mount();
    await openForm(ctx, { ...GOOD, value });
    click(byLabel(ctx.root, '변경 내용 확인')[0]);
    await until(() => text(byData(ctx.root, 'problem', 'form')[0]).includes(T.ERROR_TEXT[code]), name);
    await ctx.settle();
    assert.equal(ctx.wire.length, 0, name + ': nothing requested, no hash computed');
    assert.equal(auth.gets.length, 0);
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, name + ': no sign button');
    assert.ok(!text(ctx.root).includes(value.trim()) || value.trim() === '', name + ': the sentence does not echo the value');
    assert.equal(T.heldValueLength(), 0, name + ': never held after a refusal');
  }
  // the boundaries are accepted
  for (const value of ['a'.repeat(32), 'b'.repeat(512), '!~'.repeat(20)]) {
    const ctx = await mount();
    await toConfirm(ctx, { value });
    await waitSignable(ctx);
  }
  // refs and kinds
  const refs = { '': 'ref', 'A': 'ref', '1a': 'ref', 'a_b': 'ref', 'a b': 'ref', ['a' + 'b'.repeat(40)]: 'ref', '한글': 'ref', ' gitlab': 'ref' };
  for (const ref of Object.keys(refs)) {
    const ctx = await mount();
    await openForm(ctx, { ...GOOD, ref });
    click(byLabel(ctx.root, '변경 내용 확인')[0]);
    await until(() => /이름/.test(text(byData(ctx.root, 'problem', 'form')[0])), 'ref ' + JSON.stringify(ref));
    assert.equal(ctx.wire.length, 0);
  }
  const k1 = await mount();
  await openForm(k1, { ...GOOD, kind: 'openai', ref: 'gitlab-main' });
  click(byLabel(k1.root, '변경 내용 확인')[0]);
  await until(() => text(byData(k1.root, 'problem', 'form')[0]).includes(T.ERROR_TEXT.openai_singleton), 'openai with another name');
  const k2 = await mount();
  await openForm(k2, { ...GOOD, kind: 'gitlab', ref: 'openai' });
  click(byLabel(k2.root, '변경 내용 확인')[0]);
  await until(() => text(byData(k2.root, 'problem', 'form')[0]).includes(T.ERROR_TEXT.openai_singleton), 'gitlab named openai');
  const k3 = await mount();
  await openForm(k3, { ...GOOD, kind: 'slack' });
  click(byLabel(k3.root, '변경 내용 확인')[0]);
  await until(() => text(byData(k3.root, 'problem', 'form')[0]).includes(T.ERROR_TEXT.bad_kind), 'unknown kind');
  assert.equal(k1.wire.length + k2.wire.length + k3.wire.length, 0);
  // the openai kind with its one name goes through
  const ok = await mount();
  await toConfirm(ok, { kind: 'openai', ref: 'openai', expires: '' });
  await waitSignable(ok);
});

test('TestApplyErrorsFixedTexts', async () => {
  const statuses = { not_signed: 400, bad_signature: 403, unknown_signer: 403, stale_signature: 403, signers_unavailable: 503,
    request_reused: 409, change_mismatch: 400, bad_request: 400, token_unknown: 404, bad_kind: 400, token_too_short: 400,
    token_bad_format: 400, value_mismatch: 400, openai_singleton: 400, token_exists_other_kind: 409, tokens_unavailable: 503,
    outcome_unknown: 504, broker_unavailable: 503, token_rate_limited: 429, deploy_key_unavailable: 503, internal_error: 500,
    broker_error: 502, ai_key_missing: 503, body_too_large: 413 };
  for (const code of Object.keys(statuses)) {
    assert.equal(typeof T.ERROR_TEXT[code], 'string', 'fixed text for ' + code);
    assert.ok(T.ERROR_TEXT[code].length > 10);
  }
  assert.deepEqual(Object.keys(T.ERROR_TEXT).sort(), Object.keys(statuses).sort(), 'no code without a test, no test without a code');
  for (const code of Object.keys(statuses)) {
    auth = new Auth();
    wa.configure({ credentials: auth, subtle: webcrypto.subtle });
    const ctx = await mount({ 'POST /api/v1/tokens/apply': () => { throw apiError(statuses[code], code, SECRET, { reason: SECRET }); } });
    await toConfirm(ctx);
    await waitSignable(ctx);
    click(signBtn(ctx));
    await until(() => byData(ctx.root, 'result', 'error').length === 1, 'error ' + code);
    const shown = text(byData(ctx.root, 'result', 'error')[0]);
    assert.ok(shown.includes(T.ERROR_TEXT[code]), code + ': ' + shown);
    assert.ok(!/SERVER-WORDS|img|javascript/.test(text(ctx.root)), 'server text not shown for ' + code);
    for (const e of findAll(ctx.root, () => true)) for (const [, v] of e.attrs) assert.ok(!/SERVER-WORDS|onerror/.test(v));
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, 'a spent change is not signed again after ' + code);
    assert.equal(posts(ctx, 'apply').length, 1, code + ': one apply');
    assert.equal(T.heldValueLength(), 0, code + ': the value is wiped');
  }
  // begin and message failures use the same fixed texts
  for (const [path, code] of [['begin', 'token_rate_limited'], ['message', 'bad_request'], ['begin', 'openai_singleton'], ['message', 'bad_kind']]) {
    const ctx = await mount({ ['POST /api/v1/tokens/' + path]: () => { throw apiError(statuses[code], code, SECRET); } });
    await openForm(ctx, GOOD);
    click(byLabel(ctx.root, '변경 내용 확인')[0]);
    await until(() => text(byData(ctx.root, 'problem', 'form')[0]).includes(T.ERROR_TEXT[code]), path + ' ' + code);
    assert.ok(!text(ctx.root).includes('SERVER-WORDS'));
    assert.equal(posts(ctx, path).length, 1, 'no retry');
  }
  // an unknown code is told by status, never in the server's words
  const odd = await mount({ 'POST /api/v1/tokens/apply': () => { throw apiError(418, 'tea_pot', SECRET); } });
  await toConfirm(odd);
  await waitSignable(odd);
  click(signBtn(odd));
  await until(() => byData(odd.root, 'result', 'error').length === 1, 'odd');
  assert.ok(!text(odd.root).includes('SERVER-WORDS'));
  // a 5xx without a known code says to check the list
  const five = await mount({ 'POST /api/v1/tokens/apply': () => { throw apiError(500, 'weird', SECRET); } });
  await toConfirm(five);
  await waitSignable(five);
  click(signBtn(five));
  await until(() => byData(five.root, 'result', 'error').length === 1, 'five');
  assert.match(text(five.root), /목록/);
});

test('TestOutcomeUnknownReloadsList', async () => {
  for (const mk of [() => apiError(504, 'outcome_unknown', SECRET), () => apiError(0, 'network', SECRET), () => apiError(0, 'timeout', SECRET),
    () => apiError(502, 'broker_error', SECRET), () => apiError(500, 'internal_error', SECRET)]) {
    const ctx = await mount({ 'POST /api/v1/tokens/apply': () => { throw mk(); } });
    await toConfirm(ctx);
    await waitSignable(ctx);
    click(signBtn(ctx));
    await until(() => byData(ctx.root, 'result', 'error').length === 1, 'unknown outcome');
    assert.match(text(ctx.root), /목록을 다시 읽어/);
    const n = () => ctx.calls.filter((c) => c === '/api/v1/settings/tokens').length;
    await until(() => n() === 2, 'the list is read again');
    await sleep(30);
    assert.equal(n(), 2, 'read once, not polled');
    assert.equal(posts(ctx, 'apply').length, 1, 'never retried');
    assert.equal(posts(ctx, 'begin').length, 1);
    assert.equal(T.heldValueLength(), 0);
    assert.ok(!text(ctx.root).includes('SERVER-WORDS'));
  }
  // a refusal that says "nothing changed" does not pretend otherwise and does not reload
  const ctx = await mount({ 'POST /api/v1/tokens/apply': () => { throw apiError(403, 'bad_signature', SECRET); } });
  await toConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'error').length === 1, 'refused');
  await sleep(20);
  assert.equal(ctx.calls.filter((c) => c === '/api/v1/settings/tokens').length, 1);
  // an apply answer that is not the row asked for is also "unknown"
  const odd = await mount({ 'POST /api/v1/tokens/apply': () => ({ ref: 'someone-else', deleted: true }) });
  await toConfirm(odd);
  await waitSignable(odd);
  click(signBtn(odd));
  await until(() => byData(odd.root, 'result', 'error').length === 1, 'odd answer');
  assert.match(text(odd.root), /목록을 다시 읽어/);
});

test('TestSubmitOnceNoDoubleSign', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  let release;
  auth.gate = new Promise((r) => { release = r; });
  const btn = signBtn(ctx);
  click(btn); click(btn); click(btn);
  await until(() => auth.gets.length >= 1, 'prompt');
  await sleep(20);
  assert.equal(auth.gets.length, 1, 'one passkey request');
  release();
  await until(() => posts(ctx, 'apply').length >= 1, 'apply');
  await sleep(20);
  assert.equal(posts(ctx, 'apply').length, 1, 'one apply');
  // a double click on the form button: one begin
  const c2 = await mount();
  await openForm(c2, GOOD);
  const go = byLabel(c2.root, '변경 내용 확인')[0];
  click(go); click(go); click(go);
  await until(() => !!signBtn(c2), 'sign');
  assert.equal(posts(c2, 'begin').length, 1);
  assert.equal(posts(c2, 'message').length, 1);
  // the same for the delete
  const c3 = await mount();
  click(byLabel(findAll(c3.root, (e) => e.tagName === 'TR' && text(e).includes('gitlab-main'))[0], '삭제')[0]);
  await until(() => byId(c3.root, 't-confirm-ref'), 'delete form');
  byId(c3.root, 't-confirm-ref').value = 'gitlab-main';
  const g3 = byLabel(c3.root, '삭제 내용 확인')[0];
  click(g3); click(g3);
  await until(() => !!signBtn(c3), 'sign (delete)');
  assert.equal(posts(c3, 'begin').length, 1);
});

test('TestLeaveWhileSigningSendsNothing', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  let release;
  auth.gate = new Promise((r) => { release = r; });
  click(signBtn(ctx));
  await until(() => auth.gets.length === 1, 'prompt');
  ctx.leave();
  release();
  await sleep(30);
  assert.equal(posts(ctx, 'apply').length, 0);
  assert.equal(T.heldValueLength(), 0);
});

test('TestLeaveGuardOnlyWhenValueTyped', async () => {
  const ctx = await mount();
  const holds = () => (ctx.guard ? ctx.guard() : false);
  assert.equal(holds(), false, 'the list holds nothing');
  await openForm(ctx);
  assert.equal(holds(), false, 'an empty form holds nothing');
  fillForm(ctx, { ref: 'gitlab-main', expires: '2027-06-30' }); // typing a name does not hold the page
  assert.equal(holds(), false, 'only a typed value does');
  fillForm(ctx, { value: 'x' });
  assert.ok(typeof holds() === 'string' && holds().length > 10, 'one character is enough');
  assert.ok(!/계획서/.test(holds()), 'the notice is about the token, not the plan');
  assert.match(holds(), /취소/);
  // the real guard blocks with those words and restores the hash
  const log = { restored: [], notices: [] };
  const onChange = G.makeHashGuard({ guard: () => ctx.guard, current: () => '#/tokens', restore: (h) => log.restored.push(h), notice: (t) => log.notices.push(t) });
  assert.equal(onChange(), true);
  assert.deepEqual(log.restored, ['#/tokens']);
  assert.equal(log.notices[0], holds());
  // emptying the field releases it
  fillForm(ctx, { value: '' });
  assert.equal(holds(), false);
  fillForm(ctx, { ...GOOD });
  assert.ok(holds());
  // cancel releases it, and the person is never trapped: the cancel button is on every screen that holds a value
  click(byLabel(ctx.root, '취소')[0]);
  await until(() => byLabel(ctx.root, '토큰 추가').length === 1, 'list');
  assert.equal(holds(), false, 'released on cancel');
  // submit: the value moves into the request; confirmation holds it; cancel releases
  await openForm(ctx, GOOD);
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => !!byId(ctx.root, 'h-says'), 'confirm');
  assert.ok(holds(), 'the confirmation holds the typed value until it is sent or cancelled');
  assert.equal(byLabel(ctx.root, '취소').length, 1);
  click(byLabel(ctx.root, '취소')[0]);
  await until(() => byLabel(ctx.root, '토큰 추가').length === 1, 'list');
  assert.equal(holds(), false);
  // sent: released as soon as the apply request is out, whatever the answer
  const sent = await mount({ 'POST /api/v1/tokens/apply': () => new Promise(() => {}) });
  await toConfirm(sent);
  await waitSignable(sent);
  assert.ok(sent.guard && sent.guard());
  click(signBtn(sent));
  await until(() => posts(sent, 'apply').length === 1, 'apply');
  assert.equal(sent.guard, null, 'released once the value left (even while the answer is pending)');
  // an error releases it
  const err = await mount({ 'POST /api/v1/tokens/begin': () => { throw apiError(500, 'internal_error', SECRET); } });
  await openForm(err, GOOD);
  click(byLabel(err.root, '변경 내용 확인')[0]);
  await until(() => text(err.root).includes(T.PREP_TEXT.server), 'error');
  assert.equal(err.guard, null);
  // a refused proposal releases it too
  const mm = await mount({ 'POST /api/v1/tokens/message': async (b) => ({ ...(await messageFor(b)), change: '0'.repeat(64) }) });
  await toConfirm(mm);
  assert.equal(mm.guard, null, 'a mismatched proposal can not be sent, so nothing is held');
  // leaving clears everything
  await openForm(ctx, GOOD);
  ctx.leave();
  assert.equal(ctx.guard, null);
  // the guard answering with a string never blocks when it is empty, and a long string is not trusted
  assert.equal(G.makeHashGuard({ guard: () => () => '', current: () => '', restore() {}, notice() {} })(), false);
  assert.equal(G.makeHashGuard({ guard: () => () => 'x'.repeat(201), current: () => '', restore() {}, notice() {} })(), false);
});

test('TestNoStorageNoNavigateTrap', async () => {
  const code = src('views/tokens.js');
  const banner = src('expiry.js');
  for (const c of [code, banner]) {
    for (const bad of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'caches.', 'history.pushState', 'history.replaceState',
      'navigator.storage', 'ctx.navigate', 'navigate(', 'location.hash', 'confirm(', 'alert(', 'innerHTML', 'Date.now', 'eval(', 'new Function']) {
      assert.ok(!c.includes(bad), 'uses ' + bad);
    }
    assert.ok(!/(?:^|[^.\w])(?:confirm|alert|prompt)\s*\(/.test(c));
    assert.ok(!/\.setAttribute\(/.test(c), 'attributes go through el()');
  }
  assert.ok(!banner.includes('new Date'), 'the banner has no clock: it works from days_left');
  // every ctx member the view uses exists in the REAL app.js makeCtx
  const app = src('app.js');
  const makeCtx = app.slice(app.indexOf('function makeCtx'), app.indexOf('async function show'));
  for (const m of new Set([...code.matchAll(/\bctx\.([A-Za-z]+)/g)].map((x) => x[1]))) {
    assert.ok(new RegExp('\\b' + m + '\\b').test(makeCtx), 'ctx.' + m + ' is not in app.js makeCtx');
  }
  const had = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  try {
    const ctx = await mount();
    assert.equal(ctx.navigate, undefined, 'the real app context has no navigate');
    await toConfirm(ctx);
    await waitSignable(ctx);
    click(signBtn(ctx));
    await until(() => /저장했습니다/.test(text(ctx.root)), 'done');
    click(byLabel(ctx.root, '목록으로')[0]);
    await until(() => byLabel(ctx.root, '토큰 추가').length === 1, 'list again');
    assert.deepEqual(ctx.nav, [], 'the flow never writes location.hash');
    assert.equal(ctx.guard, null);
  } finally {
    if (had) Object.defineProperty(globalThis, 'localStorage', had); else delete globalThis.localStorage;
    delete globalThis.sessionStorage;
  }
  // wiring through the REAL router, the app and the page
  assert.deepEqual(parseRoute('#/tokens'), { name: 'tokens', params: [] });
  assert.deepEqual(parseRoute('#/tokens/x'), { name: 'notfound', params: [] });
  assert.equal(navItem('tokens'), 'tokens');
  assert.equal(typeof getView('tokens'), 'function');
  assert.match(app, /VIEW_MODULES = \[[^\]]*'tokens'/);
  assert.match(src('index.html'), /<a class="nav" href="#\/tokens" data-nav="tokens">[^]*?<span class="lb">토큰<\/span>/);
});

test('TestNoRetry429ForTokenWrites', async () => {
  const ctx = await mount();
  await toConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => posts(ctx, 'apply').length === 1, 'apply');
  for (const n of ['begin', 'message', 'apply']) {
    assert.equal(posts(ctx, n)[0].opts.noRetry429, true, n);
    assert.equal(posts(ctx, n)[0].opts.timeoutMs, 25000, n + ': longer than orchd\'s 15 s broker write');
  }
  // each write that answers 429 is reported once with a fixed text and never repeated
  for (const n of ['begin', 'message', 'apply']) {
    const c = await mount({ ['POST /api/v1/tokens/' + n]: () => { throw apiError(429, 'token_rate_limited', SECRET); } });
    if (n === 'apply') {
      await toConfirm(c);
      await waitSignable(c);
      click(signBtn(c));
      await until(() => byData(c.root, 'result', 'error').length === 1, 'apply 429');
    } else {
      await openForm(c, GOOD);
      click(byLabel(c.root, '변경 내용 확인')[0]);
      await until(() => text(byData(c.root, 'problem', 'form')[0]).includes(T.ERROR_TEXT.token_rate_limited), n + ' 429');
    }
    await sleep(20);
    assert.equal(posts(c, n).length, 1, n + ': one request');
  }
  // and the real api layer honours it: one request, no sleep
  const api = await import('../static/api.js');
  let n = 0;
  let slept = 0;
  api.configure({
    fetch: async () => { n++; return new Response('{"error":{"code":"token_rate_limited","message":"x"}}', { status: 429, headers: { 'Retry-After': '3' } }); },
    sleep: async () => { slept++; },
  });
  try {
    await assert.rejects(() => api.post('/api/v1/tokens/apply', {}, { noRetry429: true }), (e) => e.status === 429);
    assert.equal(n, 1);
    assert.equal(slept, 0);
  } finally {
    api.configure();
  }
});

test('TestTokensConstantsAndIds', () => {
  assert.equal(T.TOKEN_VALUE_MIN, 32);
  assert.equal(T.TOKEN_VALUE_MAX, 512);
  assert.equal(T.TOKEN_APPLY_MAX_BYTES, 4096);
  assert.deepEqual(T.TOKEN_KINDS, ['openai', 'gitlab', 'github']);
  assert.ok(T.TOKEN_REF_RE.test('a') && !T.TOKEN_REF_RE.test('A'));
  // ids and names that reach attributes pass SAFE_TOKEN
  const code = src('views/tokens.js');
  for (const m of code.matchAll(/\b(?:id|for|name):\s*'([^']*)'/g)) assert.match(m[1], /^[A-Za-z0-9_-]{1,64}$/);
  // the value is never a property of the view state
  assert.ok(!/\bst\.(value|secret|token)\b/.test(code));
});

test('TestPreviewSentencesAreTextNodes', async () => {
  const markup = ' <img src=x onerror=alert(1)> javascript:alert(1) <b>굵게</b>';
  const ctx = await mount({
    'POST /api/v1/tokens/message': async (b) => {
      const r = await messageFor(b);
      return { ...r, preview: r.preview.map((x, i) => (i === 0 ? x + markup : x)) };
    },
  });
  await toConfirm(ctx);
  await waitSignable(ctx);
  const items = byData(ctx.root, 'preview', 'item');
  assert.ok(text(items[0]).includes('<img src=x onerror=alert(1)>'), 'shown literally');
  assert.equal(byTag(ctx.root, 'img').length, 0);
  for (const li of items) assert.equal(li.children.length, 0, 'no element inside a sentence');
  for (const e of findAll(ctx.root, () => true)) {
    for (const [k, v] of e.attrs) assert.ok(!/onerror|javascript:|<img|굵게/.test(v), 'attribute ' + k + ' carries a server sentence');
  }
  for (const li of items) assert.deepEqual([...li.attrs.keys()], ['data-preview'], 'the sentence is only a text node');
});

// ---- fix round 1 ----

test('TestFailedReloadNeverShowsOldList', async () => {
  let n = 0;
  const ctx = await mount({
    '/api/v1/settings/tokens': () => { n++; if (n === 1) return LIST; throw apiError(503, 'tokens_unavailable', SECRET); },
    'POST /api/v1/tokens/apply': () => { throw apiError(504, 'outcome_unknown', SECRET); },
  });
  assert.ok(text(ctx.root).includes('gitlab-main'));
  await toConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'error').length === 1, 'unknown outcome');
  await until(() => n >= 2, 'reload attempt');
  await ctx.settle();
  click(byLabel(ctx.root, '목록으로')[0]);
  await until(() => text(ctx.root).includes('불러오지 못했습니다'), 'failure pane');
  assert.ok(!text(ctx.root).includes('gitlab-main'), 'the pre-change rows are not shown as current');
  assert.equal(byData(ctx.root, 'banner', 'expiry').length, 0, 'no banner from old data');
  assert.ok(text(ctx.root).includes(T.ERROR_TEXT.tokens_unavailable));
  assert.equal(byLabel(ctx.root, '토큰 추가').length, 0, 'no change offered on a list that could not be read');
  // a retry that works brings the list back
  n = 0;
  click(byLabel(ctx.root, '다시 시도')[0]);
  await until(() => byLabel(ctx.root, '토큰 추가').length === 1, 'list again');
});

test('TestInputClearedWhileBusyGuardReleasedAndBodyCleared', async () => {
  let gate; let bodies = [];
  const hold = new Promise((r) => { gate = r; });
  const ctx = await mount({
    'POST /api/v1/tokens/begin': async () => { await hold; seq++; return { request: seq.toString(16).padStart(32, '0'), issued: '2026-11-10T03:00:00Z' }; },
    'POST /api/v1/tokens/apply': (b) => { bodies.push(b); return { ...APPLIED_SET, ref: b.subject, kind: b.fields.kind }; },
  });
  await openForm(ctx, GOOD);
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => posts(ctx, 'begin').length === 1, 'begin pending');
  // pending begin: the field is empty and disabled, the holder has the value, the guard says so with a button that exists
  const inp = byId(ctx.root, 'x-field-b');
  assert.equal(inp.value, '', 'input empty right after submit, while busy');
  assert.equal(inp.disabled, true, 'input disabled while busy');
  assert.equal(T.heldValueLength(), VALUE.length);
  const g = ctx.guard();
  assert.equal(typeof g, 'string');
  assert.ok(byLabel(ctx.root, '취소').length === 1 && g.includes('취소'), 'the guard names a button that is on this screen');
  gate();
  await until(() => !!byId(ctx.root, 'h-says'), 'confirm');
  await waitSignable(ctx);
  assert.ok(typeof ctx.guard() === 'string' && g.includes('취소') && byLabel(ctx.root, '취소').length === 1);
  click(signBtn(ctx));
  await until(() => /저장했습니다/.test(text(ctx.root)), 'done');
  assert.equal(T.heldValueLength(), 0);
  assert.equal(ctx.guard, null, 'released after apply');
  assert.equal(bodies.length, 1);
  assert.equal(bodies[0].value, '', 'the body object no longer carries the value after the post');
  // the same on a failing apply
  bodies = [];
  const bad = await mount({ 'POST /api/v1/tokens/apply': (b) => { bodies.push(b); throw apiError(403, 'bad_signature', SECRET); } });
  await toConfirm(bad);
  await waitSignable(bad);
  click(signBtn(bad));
  await until(() => byData(bad.root, 'result', 'error').length === 1, 'error');
  assert.equal(bodies[0].value, '', 'cleared after a failed post too');
  assert.equal(bad.guard, null);
  assert.equal(T.heldValueLength(), 0);
});

test('TestRealGoMessageFixturesAreAccepted', async () => {
  // The browser rebuilds the canonical message and change itself; the answers
  // below are Go's own bytes (value hash, change, message, sentences).
  const begin = fx('tokens-begin.json');
  const cases = [
    ['tokens-message-set.json', { kind: 'gitlab', ref: 'gitlab-main', expires: '2027-06-30', value: VALUE }],
    ['tokens-message-set-noexp.json', { kind: 'gitlab', ref: 'gitlab-main', expires: '', value: VALUE }],
    ['tokens-message-set-openai.json', { kind: 'openai', ref: 'openai', expires: '', value: VALUE }],
  ];
  for (const [file, form] of cases) {
    const ctx = await mount({
      'POST /api/v1/tokens/begin': () => begin,
      'POST /api/v1/tokens/message': () => fx(file),
    });
    await openForm(ctx, form);
    click(byLabel(ctx.root, '변경 내용 확인')[0]);
    await until(() => !!byId(ctx.root, 'h-says') || byData(ctx.root, 'warn', 'mismatch').length > 0, file);
    await waitSignable(ctx);
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 0, file + ': Go\'s message equals the browser\'s rebuild');
    assert.deepEqual(byData(ctx.root, 'preview', 'item').map(text), fx(file).preview, file);
    click(byLabel(ctx.root, '취소')[0]);
  }
  for (const [file, ref] of [['tokens-message-delete.json', 'gitlab-main'], ['tokens-message-delete-openai.json', 'openai']]) {
    const ctx = await mount({
      'POST /api/v1/tokens/begin': () => begin,
      'POST /api/v1/tokens/message': () => fx(file),
    });
    const row = findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).startsWith(ref) && byLabel(e, '삭제').length === 1)[0];
    click(byLabel(row, '삭제')[0]);
    await until(() => byId(ctx.root, 't-confirm-ref'), 'delete form');
    byId(ctx.root, 't-confirm-ref').value = ref;
    click(byLabel(ctx.root, '삭제 내용 확인')[0]);
    await waitSignable(ctx);
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 0, file);
  }
  // a Go answer for ANOTHER value (changed by one character) is refused by the browser
  const ctx = await mount({
    'POST /api/v1/tokens/begin': () => begin,
    'POST /api/v1/tokens/message': () => fx('tokens-message-set.json'),
  });
  await openForm(ctx, { ...GOOD, value: VALUE.slice(0, -1) + 'X' });
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => byData(ctx.root, 'warn', 'mismatch').length === 1, 'mismatch for another value');
});

test('TestPrepFailuresSayNothingWasApplied', async () => {
  assert.ok(T.PREP_TEXT.server.includes('아무것도'));
  assert.ok(!/알 수 없|목록을 다시 읽어/.test(T.PREP_TEXT.server));
  for (const [path, mk] of [['begin', () => apiError(504, 'outcome_unknown', SECRET)], ['message', () => apiError(502, 'broker_error', SECRET)],
    ['begin', () => apiError(500, 'internal_error', SECRET)], ['message', () => apiError(503, 'tokens_unavailable', SECRET)],
    ['begin', () => apiError(500, 'weird', SECRET)], ['message', () => apiError(504, 'timeout', SECRET)]]) {
    const ctx = await mount({ ['POST /api/v1/tokens/' + path]: () => { throw mk(); } });
    await openForm(ctx, GOOD);
    click(byLabel(ctx.root, '변경 내용 확인')[0]);
    await until(() => byData(ctx.root, 'problem', 'form')[0].hidden === false, path);
    const shown = text(byData(ctx.root, 'problem', 'form')[0]);
    assert.match(shown, /아무것도|적용되지 않았/, shown);
    assert.ok(!/알 수 없|목록을 다시 읽어/.test(shown), 'a step that stores nothing never says "unknown": ' + shown);
    assert.ok(!shown.includes('SERVER-WORDS'));
    await ctx.settle();
    assert.equal(ctx.calls.filter((c) => c === '/api/v1/settings/tokens').length, 1, 'no list reload after a prep failure');
  }
  // apply keeps its "not known" wording
  const ap = await mount({ 'POST /api/v1/tokens/apply': () => { throw apiError(504, 'outcome_unknown', SECRET); } });
  await toConfirm(ap);
  await waitSignable(ap);
  click(signBtn(ap));
  await until(() => byData(ap.root, 'result', 'error').length === 1, 'apply unknown');
  assert.match(text(ap.root), /알 수 없/);
});

test('TestTypingWhileBusyAndLeavingClearTheField', async () => {
  // something typed while the begin is out (a real browser disables the field, but events can still arrive)
  let gate;
  const hold = new Promise((r) => { gate = r; });
  const ctx = await mount({
    'POST /api/v1/tokens/begin': async () => { await hold; seq++; return { request: seq.toString(16).padStart(32, '0'), issued: '2026-11-10T03:00:00Z' }; },
  });
  await openForm(ctx, GOOD);
  const inp = byId(ctx.root, 'x-field-b');
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => posts(ctx, 'begin').length === 1, 'begin pending');
  inp.value = 'late typing';
  fire(inp, 'input');
  gate();
  await waitSignable(ctx);
  assert.equal(inp.value, '', 'cleared when the confirmation is built');
  click(signBtn(ctx));
  await until(() => /저장했습니다/.test(text(ctx.root)), 'done');
  assert.equal(ctx.guard, null, 'the guard is released: typed was reset');
  // leaving while a value is typed in the field empties the field itself
  const c2 = await mount();
  await openForm(c2, GOOD);
  const i2 = byId(c2.root, 'x-field-b');
  assert.equal(i2.value, VALUE);
  c2.leave();
  assert.equal(i2.value, '', 'the input is emptied on leave');
  assert.equal(T.heldValueLength(), 0);
  // cancelling the form with a value typed empties the field
  const c3 = await mount();
  await openForm(c3, GOOD);
  const i3 = byId(c3.root, 'x-field-b');
  click(byLabel(c3.root, '취소')[0]);
  assert.equal(i3.value, '');
});

test('TestFarFutureAndFarPastDaysLeftRenderSanely', async () => {
  const far = { ref: 'gitlab-far', kind: 'gitlab', last4: 'abcd', expires: '9999-12-31', set_at: '2026-11-10T01:02:03Z', source: 'store', days_left: 2912129, usable: true };
  const old = { ref: 'gitlab-old', kind: 'gitlab', last4: 'efgh', expires: '2000-01-01', set_at: '2026-11-10T01:02:03Z', source: 'store', days_left: -2900000, usable: true };
  assert.deepEqual(E.expiringRows([far, old], 14).map((r) => r.ref), ['gitlab-old'], 'far future never warns, far past always does');
  const b = E.expiryBanner([far, old], 14);
  assert.match(text(b), /토큰 1개가 14일 안에 만료됩니다/);
  assert.ok(!/NaN|Infinity|undefined/.test(text(b)));
  const ctx = await mount({ '/api/v1/settings/tokens': { tokens: [far, old], warn_days: 14 } });
  const t = text(ctx.root);
  assert.ok(t.includes('2912129일'), 'exact day count shown');
  assert.ok(t.includes('2900000일 지남'));
  assert.ok(!/NaN|Infinity|undefined|e\+/.test(t));
});

test('TestCopyCutDragBlockedPasteKept', async () => {
  const ctx = await mount();
  await openForm(ctx, GOOD);
  const inp = byId(ctx.root, 'x-field-b');
  for (const ev of ['copy', 'cut', 'dragstart']) {
    assert.ok((inp.listeners[ev] || []).length > 0, ev + ' is handled');
    let prevented = false;
    for (const f of inp.listeners[ev]) f({ preventDefault() { prevented = true; } });
    assert.equal(prevented, true, ev + ' is cancelled');
  }
  assert.equal(inp.listeners.paste, undefined, 'paste stays allowed');
  assert.equal(inp.value, VALUE, 'cutting did not remove anything');
  // the other fields are not affected
  assert.equal(byId(ctx.root, 'x-field-a').listeners.copy, undefined);
  assert.equal(byId(ctx.root, 'x-field-a').listeners.cut, undefined);
});

test('TestMaskScrollSyncIsWired', async () => {
  const ctx = await mount();
  await openForm(ctx);
  const inp = byId(ctx.root, 'x-field-b');
  const m = byData(ctx.root, 'mask', 'value')[0];
  for (const ev of ['input', 'scroll', 'keyup', 'select']) assert.ok((inp.listeners[ev] || []).length > 0, ev + ' keeps the overlay in step');
  inp.value = 'a'.repeat(200);
  inp.scrollLeft = 123;
  fire(inp, 'scroll');
  assert.equal(m.scrollLeft, 123, 'overlay follows the field when it scrolls');
  inp.scrollLeft = 400;
  fire(inp, 'input');
  assert.equal(m.scrollLeft, 400, 'and after typing');
  inp.scrollLeft = 0;
  fire(inp, 'keyup');
  assert.equal(m.scrollLeft, 0);
  inp.value = '';
  fire(inp, 'input');
  assert.equal(m.scrollLeft, 0);
  const css = src('style.css');
  assert.match(css, /\.ovlmask\s*\{[^}]*overflow:\s*hidden/);
});
