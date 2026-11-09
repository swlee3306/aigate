import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import {
  webcrypto, createPrivateKey, createPublicKey, sign as nodeSign, verify as nodeVerify, createHash, generateKeyPairSync,
  randomBytes,
} from 'node:crypto';
import { installDocument, findAll, allElements } from './dom-stub.js';

installDocument();
const fxURL = (n) => new URL('./fixtures/' + n, import.meta.url);
const fx = (n) => JSON.parse(readFileSync(fxURL(n), 'utf8'));
const wa = await import('../static/webauthn.js');
const api = await import('../static/api.js');
const R = await import('../static/render.js');
const A = await import('../static/views/approvals.js');
const L = await import('../static/views/login.js');
const { getView, parseRoute, navItem } = await import('../static/router.js');

// ---- stubs ---------------------------------------------------------------------

const sha = (b) => new Uint8Array(createHash('sha256').update(b).digest());
const u8 = (b) => new Uint8Array(b);
const toAB = (b) => { const x = u8(b); return x.buffer.slice(x.byteOffset, x.byteOffset + x.byteLength); };
const cat = (...parts) => u8(Buffer.concat(parts.map((p) => Buffer.from(p))));
const utf8 = (s) => new TextEncoder().encode(s);
const B64U_STRICT = /^[A-Za-z0-9_-]+$/;

// recordingSubtle is node's WebCrypto with every digest input recorded.
function recordingSubtle() {
  const inputs = [];
  return {
    inputs,
    digest: async (alg, data) => { inputs.push(u8(data)); return webcrypto.subtle.digest(alg, data); },
  };
}

// FakeAuthenticator answers navigator.credentials.get/create like a browser
// with a platform passkey: authenticatorData = sha256(rpId) | UP+UV | counter,
// clientDataJSON as the browser builds it, and a DER ECDSA signature over
// authenticatorData | sha256(clientDataJSON) (the same synthetic format as
// internal/orch/api/webauthn_login_test.go).
class FakeAuthenticator {
  constructor(o = {}) {
    this.key = o.pkcs8
      ? createPrivateKey({ key: Buffer.from(wa.b64uDecode(o.pkcs8)), format: 'der', type: 'pkcs8' })
      : generateKeyPairSync('ec', { namedCurve: 'P-256' }).privateKey;
    this.pub = createPublicKey(this.key);
    this.credId = o.credId || u8(randomBytes(32));
    this.rpId = o.rpId || 'orch.example.test';
    this.origin = o.origin || 'https://orch.example.test';
    this.mode = o.mode || 'ok';
    this.gets = [];
    this.creates = [];
    this.events = o.events || [];
    this.gate = null;
  }
  async get(opts) {
    this.gets.push(opts);
    this.events.push('get');
    if (this.gate) await this.gate;
    if (this.mode === 'deny') throw new DOMException('The operation either timed out or was not allowed.', 'NotAllowedError');
    const pk = opts.publicKey;
    const authData = cat(sha(Buffer.from(pk.rpId || this.rpId)), [0x05, 0, 0, 0, 7]);
    const cdj = Buffer.from(JSON.stringify({ type: 'webauthn.get', challenge: wa.b64uEncode(pk.challenge), origin: this.origin, crossOrigin: false }));
    const sig = nodeSign('sha256', Buffer.concat([authData, sha(cdj)]), { key: this.key, dsaEncoding: 'der' });
    this.events.push('signed');
    return {
      type: 'public-key',
      rawId: toAB(this.mode === 'other-id' ? randomBytes(32) : this.credId),
      response: { authenticatorData: toAB(authData), clientDataJSON: toAB(cdj), signature: toAB(sig), userHandle: null },
    };
  }
  async create(opts) {
    this.creates.push(opts);
    if (this.mode === 'deny') throw new DOMException('denied', 'NotAllowedError');
    const cdj = Buffer.from(JSON.stringify({ type: 'webauthn.create', challenge: wa.b64uEncode(opts.publicKey.challenge), origin: this.origin }));
    return { type: 'public-key', rawId: toAB(this.credId), response: { clientDataJSON: toAB(cdj), attestationObject: toAB(Buffer.from('fake-attestation-object')) } };
  }
  // verify checks a grant body's signature with this passkey's public key.
  verify(body) {
    const ad = wa.b64uDecode(body.webauthn.authenticator_data);
    const cd = wa.b64uDecode(body.webauthn.client_data_json);
    return nodeVerify('sha256', Buffer.concat([ad, sha(cd)]), { key: this.pub, dsaEncoding: 'der' }, wa.b64uDecode(body.sig));
  }
}

function apiError(status, code, message) {
  const e = new Error(message || 'server text <img src=x onerror=alert(1)>');
  e.name = 'ApiError';
  e.status = status;
  e.code = code;
  return e;
}

// makeCtx: the view context of app.js with recorded get/post and a manual poll.
function makeCtx(routes, extra = {}) {
  const ctl = new AbortController();
  const ctx = {
    root: R.el('main', { tabindex: '-1' }),
    params: extra.params || [],
    session: extra.session === undefined ? { principal: 'phone', kind: 'session' } : extra.session,
    calls: [],
    posts: [],
    polls: [],
    events: extra.events || [],
    signal: ctl.signal,
    setCrumb() {},
    onLeave() {},
    get: async (path, opts) => {
      ctx.calls.push({ path, opts });
      const r = routes[path];
      if (r === undefined) throw apiError(404, 'not_found');
      return typeof r === 'function' ? r(opts) : structuredClone(r);
    },
    post: async (path, body, opts) => {
      ctx.events.push('post');
      JSON.stringify(body); // must be plain JSON
      ctx.posts.push({ path, body: structuredClone(body), opts });
      const r = (extra.post || {})[path];
      if (r === undefined) throw apiError(404, 'not_found');
      return typeof r === 'function' ? r(body, opts) : structuredClone(r);
    },
    poll: (fn, ms) => {
      const h = { fn, ms, paused: false, pause() { this.paused = true; }, resume() { this.paused = false; }, stop() {} };
      ctx.polls.push(h);
      return h;
    },
    onAuthenticated: async (s) => { ctx.authed = s; },
    leave() { ctl.abort(); },
  };
  return ctx;
}

const tick = () => new Promise((r) => setTimeout(r, 2));
// Every async wait is bounded: a regression must fail a test, never hang it.
const WAIT_MS = 3000;
async function until(pred, what) {
  const end = Date.now() + WAIT_MS;
  while (Date.now() < end) {
    if (pred()) return;
    await tick();
  }
  assert.fail('timed out waiting for ' + what);
}
function withTimeout(p, what) {
  let timer;
  const t = new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('timed out: ' + what)), WAIT_MS); });
  return Promise.race([p, t]).finally(() => clearTimeout(timer));
}
const settle = async () => { for (let i = 0; i < 15; i++) await tick(); };
const byKey = (root, k) => findAll(root, (e) => e.getAttribute('data-focus-key') === k)[0] || null;
const text = (n) => n.textContent;
const fire = (node, ev) => Promise.all((node.listeners[ev] || []).map((f) => f({ target: node, preventDefault() {} })));
const isDisabled = (b) => b.hasAttribute('disabled');
const noHostileElements = (root) => {
  for (const e of allElements(root)) assert.ok(!['IMG', 'SCRIPT', 'SVG', 'IFRAME', 'STYLE'].includes(e.tagName), e.tagName);
};
async function tickConfirm(root) {
  const cb = byKey(root, 'read');
  cb.checked = true;
  await fire(cb, 'change');
}

let auth;
let subtle;
beforeEach(() => {
  auth = new FakeAuthenticator();
  subtle = recordingSubtle();
  wa.configure({ credentials: auth, subtle });
  api.setCsrf(null);
});
const signerOf = (a) => ({ id: 'phone', credentialId: wa.b64uEncode(a.credId), rpId: a.rpId });
const signersRoute = () => ({ signers: [{ id: 'phone', credential_id: wa.b64uEncode(auth.credId), label: '내 폰' }] });

// detailWith re-derives message and digest consistently (so only the check
// under test can catch the change).
function detailWith(over, msgEdit) {
  const d = { ...fx('approval.json'), ...over };
  if (msgEdit) {
    d.message = msgEdit(d.message);
    d.digest = Buffer.from(sha(Buffer.from(d.message, 'utf8'))).toString('hex');
  }
  return d;
}

// ---- signApproval ------------------------------------------------------------------

test('TestSignApprovalUsesDigestAsChallenge', async () => {
  const detail = fx('approval.json');
  const body = await wa.signApproval(detail, signerOf(auth));
  assert.equal(auth.gets.length, 1);
  const pk = auth.gets[0].publicKey;
  const digest = Buffer.from(detail.digest, 'hex');
  assert.equal(digest.length, 32);
  assert.deepEqual(u8(pk.challenge), u8(digest), 'challenge bytes are exactly the server digest');
  assert.deepEqual(u8(pk.challenge), sha(Buffer.from(detail.message, 'utf8')), 'and equal sha256(utf8(message))');
  assert.equal(pk.userVerification, 'required');
  assert.equal(pk.rpId, auth.rpId);
  assert.deepEqual(pk.allowCredentials.map((c) => [c.type, Buffer.from(c.id).toString('hex')]), [['public-key', Buffer.from(auth.credId).toString('hex')]]);
  // the body: exact fields, base64url without padding, a DER signature that verifies
  assert.deepEqual(Object.keys(body), ['signer', 'alg', 'sig', 'issued', 'webauthn']);
  assert.deepEqual(Object.keys(body.webauthn), ['authenticator_data', 'client_data_json']);
  assert.equal(body.signer, 'phone');
  assert.equal(body.alg, 'webauthn-es256');
  assert.equal(body.issued, detail.issued);
  for (const v of [body.sig, body.webauthn.authenticator_data, body.webauthn.client_data_json]) assert.match(v, B64U_STRICT);
  assert.equal(wa.b64uDecode(body.sig)[0], 0x30, 'DER SEQUENCE');
  assert.ok(auth.verify(body));
  const cd = JSON.parse(Buffer.from(wa.b64uDecode(body.webauthn.client_data_json)).toString());
  assert.equal(cd.challenge, wa.b64uEncode(digest));
  // only the repo path (repo id) and the message were hashed, never hashed_text
  assert.deepEqual(subtle.inputs.map((b) => Buffer.from(b).toString()).sort(), [detail.message, detail.repo].sort());

  // message/digest disagreement: refused BEFORE the authenticator
  const other = { ...detail, digest: 'ab'.repeat(32) };
  await assert.rejects(wa.signApproval(other, signerOf(auth)), (e) => e.code === 'digest_mismatch');
  const flipped = { ...detail, message: detail.message.replace('task: 38', 'task: 39') };
  await assert.rejects(wa.signApproval(flipped, signerOf(auth)), (e) => e.code === 'message_mismatch');
  await assert.rejects(wa.signApproval({ ...detail, digest: detail.digest.toUpperCase() }, signerOf(auth)), (e) => e.code === 'bad_digest');
  await assert.rejects(wa.signApproval({ ...detail, digest: detail.digest.slice(2) }, signerOf(auth)), (e) => e.code === 'bad_digest');
  assert.equal(auth.gets.length, 1, 'no authenticator call for a refused detail');
});

test('message fields must equal the detail fields (kind, task, base, hash, issued)', async () => {
  const cases = [
    detailWith({}, (m) => m.replace('task: 38', 'task: 39')),
    detailWith({}, (m) => m.replace(/hash: [0-9a-f]+/, 'hash: ' + 'f'.repeat(64))),
    detailWith({}, (m) => m.replace(/base: [0-9a-f]+/, 'base: ' + '2'.repeat(40))),
    detailWith({}, (m) => m.replace('protected_modified', 'publish_outside_allowlist')),
    detailWith({}, (m) => m.replace('10:04:05Z', '10:04:06Z')),
    detailWith({ issued: '2026-11-09T10:04:06Z' }),
  ];
  for (const d of cases) await assert.rejects(wa.signApproval(d, signerOf(auth)), (e) => e.code === 'message_mismatch');
  for (const d of [
    detailWith({}, (m) => m + 'extra: 1\n'),
    detailWith({}, (m) => m.replace('aigate-approval-v1', 'aigate-approval-v2')),
    detailWith({}, (m) => m.slice(0, -1)),
    detailWith({}, (m) => m.replace('\nrepo:', '\n repo:')),
    // a 16-hex hash (the pre-2026-11-09 diff hash) is not canonical, even when
    // the detail carries the same short hash
    detailWith({ hash: 'f'.repeat(16) }, (m) => m.replace(/hash: [0-9a-f]+/, 'hash: ' + 'f'.repeat(16))),
    detailWith({ hash: 'f'.repeat(63) }, (m) => m.replace(/hash: [0-9a-f]+/, 'hash: ' + 'f'.repeat(63))),
    detailWith({ hash: 'F'.repeat(64) }, (m) => m.replace(/hash: [0-9a-f]+/, 'hash: ' + 'F'.repeat(64))),
  ]) {
    await assert.rejects(wa.signApproval(d, signerOf(auth)), (e) => e.code === 'bad_message');
  }
  assert.equal(auth.gets.length, 0);
});

test('a message with control, bidi or invisible characters is refused before signing', async () => {
  for (const bad of ['\u202e', '\u0007', '\u001b[31m', '\u200b', '\r', '\t', '\u2066', '\u{e0041}', '\u0085']) {
    const d = detailWith({}, (m) => m.replace('repo: ', 'repo: ' + bad));
    await assert.rejects(wa.signApproval(d, signerOf(auth)), (e) => e.code === 'hostile_message', JSON.stringify(bad));
  }
  assert.equal(auth.gets.length, 0);
});

test('signApproval: not signed mode, truncation, signer and response checks', async () => {
  const d = fx('approval.json');
  await assert.rejects(wa.signApproval({ ...d, requires_signature: false }, signerOf(auth)), (e) => e.code === 'not_signed_mode');
  await assert.rejects(wa.signApproval({ ...d, message: '', digest: '' }, signerOf(auth)), (e) => e.code === 'no_message');
  const t = fx('approval-truncated.json');
  await assert.rejects(wa.signApproval(t, signerOf(auth)), (e) => e.code === 'truncated');
  await assert.rejects(wa.signApproval(t, signerOf(auth), { acknowledgeTruncation: true }), (e) => e.code === 'truncated', 'no bypass');
  await assert.rejects(wa.signApproval(d, { ...signerOf(auth), id: 'Bad Id' }), (e) => e.code === 'bad_signer');
  await assert.rejects(wa.signApproval(d, { ...signerOf(auth), credentialId: 'abc=' }), (e) => e.code === 'bad_signer');
  assert.equal(auth.gets.length, 0);
  auth.mode = 'other-id';
  await assert.rejects(wa.signApproval(d, signerOf(auth)), (e) => e.code === 'bad_response');
  auth.mode = 'deny';
  await assert.rejects(wa.signApproval(d, signerOf(auth)), (e) => e.code === 'cancelled');
});

test('issued is the server value, never derived from the clock', async () => {
  const d = fx('approval.json'); // issued 2026-11-09T10:04:05Z, a fixed past time
  const RealDate = globalThis.Date;
  class NoClock extends RealDate {
    constructor(...a) { if (a.length === 0) throw new Error('clock read'); super(...a); }
    static now() { throw new Error('clock read'); }
  }
  globalThis.Date = NoClock;
  let body;
  try {
    body = await wa.signApproval(d, signerOf(auth));
  } finally {
    globalThis.Date = RealDate;
  }
  assert.equal(body.issued, '2026-11-09T10:04:05Z');
  assert.equal(body.issued, d.issued);
});

test('base64url is strict: no padding, no standard alphabet, canonical trailing bits, round trip', () => {
  for (const bad of ['ab=c', 'abc=', 'a+b/', 'a', 'ab\n', ' ab', 'AB', 'AAB', 'AAAAB', 'ab cd', 'é', null, 12]) {
    assert.throws(() => wa.b64uDecode(bad), JSON.stringify(bad));
  }
  assert.deepEqual(wa.b64uDecode('AA'), u8([0]));
  for (let n = 0; n < 70; n++) {
    const b = u8(randomBytes(n));
    const s = wa.b64uEncode(b);
    assert.match(s, /^[A-Za-z0-9_-]*$/);
    assert.equal(s, Buffer.from(b).toString('base64url'));
    assert.deepEqual(wa.b64uDecode(s), b);
  }
  assert.equal(wa.b64uEncode(toAB([251, 255])), '-_8');
});

// ---- approvals list ----------------------------------------------------------------

test('TestApprovalsShowMeaningAndMessage', async () => {
  const detail = fx('approval.json');
  // list
  const lctx = makeCtx({ '/api/v1/approvals': fx('approvals.json'), '/api/v1/approvals/9': detail, '/api/v1/approvals/11': fx('approval-publish.json') });
  A.approvalsView(lctx);
  assert.equal(lctx.polls.length, 1);
  await lctx.polls[0].fn();
  const list = text(lctx.root);
  assert.match(list, /무엇을 허용하나요\?/);
  assert.ok(list.includes(detail.meaning), 'server meaning shown whole');
  assert.ok(list.includes('/srv/repos/proj'));
  assert.ok(list.includes('pytest.ini') && list.includes('tests/spec.txt'), 'protected files');
  assert.ok(list.includes('diff --git a/pytest.ini b/pytest.ini'), 'diff snippet');
  assert.ok(list.includes('승인 #9 · 작업 #38'));
  assert.ok(findAll(lctx.root, (e) => e.tagName === 'A').some((a) => a.getAttribute('href') === '#/approvals/9'));
  assert.match(list, /처음 12줄만 보입니다/);
  noHostileElements(lctx.root);
  // detail
  const ctx = makeCtx({ '/api/v1/approvals/9': detail, '/api/v1/auth/signers': signersRoute() }, { params: ['9'] });
  await A.approvalView(ctx);
  await until(() => byKey(ctx.root, 'sign'), 'sign button');
  const pre = findAll(ctx.root, (e) => e.tagName === 'PRE');
  assert.equal(pre.length, 1);
  assert.equal(text(pre[0]), detail.message, 'the canonical message verbatim');
  assert.equal(pre[0].getAttribute('class'), 'msg');
  const all = text(ctx.root);
  assert.ok(all.includes(detail.digest), 'full digest');
  assert.ok(all.includes(detail.digest.slice(0, 16) + '…'), 'short digest');
  assert.ok(all.includes(detail.meaning), 'meaning whole (with the full hash)');
  assert.ok(all.includes(detail.hash));
  assert.ok(all.includes(A.NOT_RECOMPUTED));
  assert.ok(all.includes('위 내용을 읽었습니다'));
  assert.ok(all.includes('+<script>alert(1)</script>'), 'whole hashed text, markup literal');
  assert.ok(all.includes('␛[31mred') && all.includes('⟨U+202E⟩'), 'diff text neutralised');
  const btn = byKey(ctx.root, 'sign');
  assert.equal(btn.tagName, 'BUTTON');
  assert.equal(btn.getAttribute('type'), 'button');
  assert.ok(isDisabled(btn), 'disabled until confirmed');
  noHostileElements(ctx.root);
});

test('TestHashedTextHashRecomputeIsNotClaimed', async () => {
  const CLAIM = /검증됨|검증 완료|검증했|verified|해시 일치|해시가 일치|일치합니다|확인됨|확인 완료|확인했습니다/i;
  for (const name of ['approval.json', 'approval-truncated.json']) {
    const detail = fx(name);
    subtle.inputs.length = 0;
    const ctx = makeCtx({ '/api/v1/approvals/9': detail, '/api/v1/auth/signers': signersRoute() }, { params: ['9'] });
    await withTimeout(A.approvalView(ctx), 'view');
    if (!detail.truncated) {
      await until(() => byKey(ctx.root, 'sign'), 'sign button');
      await tickConfirm(ctx.root);
      await withTimeout(fire(byKey(ctx.root, 'sign'), 'click'), 'sign');
    }
    await settle();
    for (const e of allElements(ctx.root)) {
      for (const [, v] of e.attrs) assert.doesNotMatch(v, CLAIM);
    }
    const all = text(ctx.root);
    assert.doesNotMatch(all, CLAIM, name);
    assert.ok(all.includes(A.NOT_RECOMPUTED), name);
    assert.ok(all.includes('확인되지 않음'));
    // nothing about hashed_text was hashed: only the message and the repo path
    for (const inp of subtle.inputs) assert.ok([detail.message, detail.repo].includes(Buffer.from(inp).toString()), name);
    if (!detail.truncated) assert.ok(subtle.inputs.some((b) => Buffer.from(b).toString() === detail.message));
  }
  const lctx = makeCtx({ '/api/v1/approvals': fx('approvals.json'), '/api/v1/approvals/9': fx('approval.json'), '/api/v1/approvals/11': fx('approval-publish.json') });
  A.approvalsView(lctx);
  await lctx.polls[0].fn();
  assert.doesNotMatch(text(lctx.root), CLAIM);
});

// ---- the signing screen ---------------------------------------------------------------

function detailCtx(detail, extra = {}) {
  const routes = { '/api/v1/approvals/9': typeof detail === 'function' ? detail : () => structuredClone(detail), '/api/v1/auth/signers': signersRoute() };
  return makeCtx(routes, { params: ['9'], ...extra });
}
const grantPosts = (ctx) => ctx.posts.filter((p) => p.path === '/api/v1/approvals/9/grant');

test('TestGrantSentOnlyAfterConfirm', async () => {
  const detail = fx('approval.json');
  const events = [];
  auth.events = events;
  const ctx = detailCtx(detail, { events, post: { '/api/v1/approvals/9/grant': { id: 38, state: 'verifying' } } });
  await A.approvalView(ctx);
  await until(() => byKey(ctx.root, 'sign'), 'sign button');
  // no activation: clicking the (disabled) button does nothing
  assert.ok(isDisabled(byKey(ctx.root, 'sign')));
  await fire(byKey(ctx.root, 'sign'), 'click');
  await settle();
  assert.equal(auth.gets.length, 0);
  assert.equal(ctx.posts.length, 0);
  // the authenticator is denied/cancelled: nothing is sent, confirmation reset
  await tickConfirm(ctx.root);
  assert.ok(!isDisabled(byKey(ctx.root, 'sign')));
  auth.mode = 'deny';
  await fire(byKey(ctx.root, 'sign'), 'click');
  await settle();
  assert.equal(auth.gets.length, 1);
  assert.equal(ctx.posts.length, 0, 'cancel sends nothing');
  assert.ok(text(ctx.root).includes(wa.WA_TEXT.cancelled));
  assert.ok(isDisabled(byKey(ctx.root, 'sign')), 'a new attempt needs a new confirmation');
  assert.equal(byKey(ctx.root, 'read').checked, false);
  // confirmed, double click: one ceremony, one request, sent after the signature
  auth.mode = 'ok';
  let release;
  auth.gate = new Promise((r) => { release = r; });
  await tickConfirm(ctx.root);
  const btn = byKey(ctx.root, 'sign');
  const c1 = fire(btn, 'click');
  const c2 = fire(btn, 'click');
  const c3 = fire(byKey(ctx.root, 'sign'), 'click');
  await settle();
  assert.equal(auth.gets.length, 2, 'one ceremony for three clicks');
  assert.equal(ctx.posts.length, 0, 'no request while the authenticator has not answered');
  release();
  await withTimeout(Promise.all([c1, c2, c3]), 'clicks settle');
  await until(() => ctx.posts.length === 1, 'grant post');
  await settle();
  assert.equal(grantPosts(ctx).length, 1, 'double click sends one request');
  assert.deepEqual(events.slice(-3), ['get', 'signed', 'post']);
  const body = grantPosts(ctx)[0].body;
  assert.equal(body.issued, detail.issued);
  assert.ok(auth.verify(body));
  assert.deepEqual(u8(auth.gets[1].publicKey.challenge), u8(Buffer.from(detail.digest, 'hex')));
  assert.ok(text(ctx.root).includes('승인했습니다'));
  assert.equal(byKey(ctx.root, 'sign'), null, 'no sign button after success');
});

test('grant errors: fixed Korean text per code, server text never shown', async () => {
  for (const [status, code] of [[409, 'not_pending'], [403, 'bad_signature'], [403, 'unknown_signer'], [400, 'not_signed_mode'],
    [503, 'signers_unavailable'], [403, 'forbidden'], [400, 'invalid_message'], [429, 'rate_limited'], [500, 'internal'], [0, 'network']]) {
    auth = new FakeAuthenticator();
    wa.configure({ credentials: auth, subtle: recordingSubtle() });
    const ctx = detailCtx(fx('approval.json'), { post: { '/api/v1/approvals/9/grant': () => { throw apiError(status, code, 'RAW <img src=x onerror=alert(1)> ' + code); } } });
    await A.approvalView(ctx);
    await until(() => byKey(ctx.root, 'sign'), 'sign');
    await tickConfirm(ctx.root);
    await fire(byKey(ctx.root, 'sign'), 'click');
    await until(() => ctx.posts.length === 1, 'post');
    await settle();
    const all = text(ctx.root);
    assert.doesNotMatch(all, /RAW|onerror/, code);
    const want = A.GRANT_ERRORS[code] || (status === 0 ? '서버에 연결하지 못했습니다' : '서버에서 오류가 발생했습니다');
    assert.ok(all.includes(want), code + ': ' + want);
    assert.ok(isDisabled(byKey(ctx.root, 'sign')), code + ': confirmation reset');
    noHostileElements(ctx.root);
  }
});

test('hash_changed and stale_signature reload the detail and require a fresh confirmation', async () => {
  for (const code of ['hash_changed', 'stale_signature']) {
    auth = new FakeAuthenticator();
    wa.configure({ credentials: auth, subtle: recordingSubtle() });
    const first = fx('approval.json');
    const second = detailWith({ issued: '2026-11-09T10:09:00Z' }, (m) => m.replace('10:04:05Z', '10:09:00Z'));
    let n = 0;
    const ctx = detailCtx(() => structuredClone(n++ === 0 ? first : second), {
      post: { '/api/v1/approvals/9/grant': () => { throw apiError(code === 'hash_changed' ? 409 : 403, code); } },
    });
    await A.approvalView(ctx);
    await until(() => byKey(ctx.root, 'sign'), 'sign');
    await tickConfirm(ctx.root);
    await fire(byKey(ctx.root, 'sign'), 'click');
    await until(() => n === 2, 'detail reloaded');
    await until(() => byKey(ctx.root, 'sign') && text(ctx.root).includes('10:09:00Z'), 'new detail shown');
    await settle();
    assert.ok(text(ctx.root).includes(A.GRANT_ERRORS[code]));
    assert.equal(text(findAll(ctx.root, (e) => e.tagName === 'PRE')[0]), second.message, 'the new message is what is shown');
    assert.equal(byKey(ctx.root, 'read').checked, false);
    assert.ok(isDisabled(byKey(ctx.root, 'sign')));
    await fire(byKey(ctx.root, 'sign'), 'click');
    await settle();
    assert.equal(grantPosts(ctx).length, 1, 'nothing more without a new confirmation');
    await tickConfirm(ctx.root);
    await fire(byKey(ctx.root, 'sign'), 'click');
    await until(() => grantPosts(ctx).length === 2, 'second grant');
    assert.equal(grantPosts(ctx)[1].body.issued, '2026-11-09T10:09:00Z', 'signed the reloaded detail');
  }
});

// Cut or not fully visible details are refused outright, like `orch approve`
// (R22): no confirmation controls, no authenticator, no /auth/signers, no POST.
function cutVariants() {
  const d = fx('approval.json');
  return {
    'server truncated': fx('approval-truncated.json'),
    'client clipped (long line)': { ...d, hashed_text: d.hashed_text.replace('+[pytest]', '+' + 'x'.repeat(2000)) },
    'client clipped (many lines)': { ...d, hashed_text: d.hashed_text + '+x\n'.repeat(A.MAX_BLOCK_LINES + 10) },
    'unseen protected file': { ...d, protected_files: [...d.protected_files, 'secret/hidden.yml'] },
    'protected file names omitted': { ...d, protected_files: Array.from({ length: 51 }, (_, i) => 'p' + i + '.txt') },
    'meaning cut': { ...d, meaning: d.meaning + ' ' + 'x'.repeat(4000) },
  };
}

test('truncated, clipped or unseen content: refusal with orch approve guidance, authenticator and POST never called', async () => {
  for (const [name, detail] of Object.entries(cutVariants())) {
    auth = new FakeAuthenticator();
    wa.configure({ credentials: auth, subtle: recordingSubtle() });
    assert.equal(A.analyze(detail).cut, true, name);
    const ctx = detailCtx(detail, { post: { '/api/v1/approvals/9/grant': { id: 38 } } });
    await withTimeout(A.approvalView(ctx), name);
    await settle();
    const all = text(ctx.root);
    assert.ok(all.includes(A.CUT_REFUSAL), name);
    assert.match(all, /orch approve/, name);
    assert.match(all, /git diff/, name);
    assert.ok(findAll(ctx.root, (e) => e.getAttribute('role') === 'alert').some((e) => text(e).includes(A.CUT_REFUSAL)), name);
    assert.equal(byKey(ctx.root, 'read'), null, name + ': no checkbox');
    assert.equal(byKey(ctx.root, 'sign'), null, name + ': no sign button');
    assert.equal(findAll(ctx.root, (e) => e.tagName === 'INPUT').length, 0, name + ': no input');
    assert.equal(auth.gets.length, 0, name);
    assert.equal(ctx.posts.length, 0, name);
    assert.equal(ctx.calls.filter((c) => c.path.startsWith('/api/v1/auth/')).length, 0, name);
  }
  const lctx = makeCtx({ '/api/v1/approvals': [{ id: 9, task_id: 38, kind: 'protected_modified', hash: 'h', head: 'x', status: 'pending' }], '/api/v1/approvals/9': fx('approval-truncated.json') });
  A.approvalsView(lctx);
  await lctx.polls[0].fn();
  assert.ok(text(lctx.root).includes(A.CUT_REFUSAL));
  const plain = detailCtx(fx('approval.json'));
  await withTimeout(A.approvalView(plain), 'plain');
  await until(() => byKey(plain.root, 'sign'), 'sign');
  assert.ok(!text(plain.root).includes(A.CUT_REFUSAL));
  assert.equal(A.analyze(fx('approval.json')).cut, false);
});

test('leaving the view while the passkey prompt is open sends nothing', async () => {
  const ctx = detailCtx(fx('approval.json'), { post: { '/api/v1/approvals/9/grant': { id: 38 } } });
  await withTimeout(A.approvalView(ctx), 'view');
  await until(() => byKey(ctx.root, 'sign'), 'sign');
  let release;
  auth.gate = new Promise((r) => { release = r; });
  await tickConfirm(ctx.root);
  const c = fire(byKey(ctx.root, 'sign'), 'click');
  await until(() => auth.gets.length === 1, 'prompt open');
  ctx.leave();
  release();
  await withTimeout(c, 'click');
  await settle();
  assert.deepEqual(auth.events.slice(-2), ['get', 'signed'], 'the authenticator answered');
  assert.equal(ctx.posts.length, 0, 'but nothing was sent');
});

test('repo id: the message repo must be sha256(repo path)[:12] (Go vectors)', async () => {
  for (const name of ['approval.json', 'approval-truncated.json', 'approval-publish.json']) {
    const d = fx(name);
    assert.equal(await wa.repoIdOf(d.repo), /\nrepo: ([0-9a-f]{12})\n/.exec(d.message)[1], name);
  }
  const d = fx('approval.json');
  await assert.rejects(wa.signApproval({ ...d, repo: '/srv/repos/other' }, signerOf(auth)), (e) => e.code === 'repo_mismatch');
  await assert.rejects(wa.signApproval(detailWith({}, (m) => m.replace(/repo: [0-9a-f]{12}/, 'repo: 0123456789ab')), signerOf(auth)), (e) => e.code === 'repo_mismatch');
  assert.equal(auth.gets.length, 0);
  const ctx = detailCtx(d);
  await withTimeout(A.approvalView(ctx), 'view');
  await until(() => byKey(ctx.root, 'sign'), 'sign');
  assert.ok(text(ctx.root).includes(await wa.repoIdOf(d.repo)), 'message repo id shown');
  const bad = detailCtx({ ...d, repo: '/srv/repos/other' });
  await withTimeout(A.approvalView(bad), 'view');
  await until(() => text(bad.root).includes(wa.WA_TEXT.repo_mismatch), 'mismatch');
  assert.equal(byKey(bad.root, 'sign'), null);
});

test('publish actions: JS publishHash reproduces approvals.PublishHash (Go vectors); derived action shown and required', async () => {
  for (const v of fx('publish_vectors.json')) {
    assert.equal(await wa.publishHash(v.task, v.head, v.action), v.hash, v.action + ' ' + v.task);
  }
  const p = fx('approval-publish.json');
  assert.equal(await wa.deriveAction(p), 'push_branch');
  assert.ok(auth.verify(await wa.signApproval(p, signerOf(auth))));
  await assert.rejects(wa.signApproval({ ...p, action: 'draft_pr' }, signerOf(auth)), (e) => e.code === 'publish_action_mismatch');
  await assert.rejects(wa.signApproval({ ...p, action: '' }, signerOf(auth)), (e) => e.code === 'publish_action_mismatch');
  const h = 'e'.repeat(64);
  const none = detailWith({ ...p, hash: h }, (m) => m.replace(p.hash, h));
  await assert.rejects(wa.signApproval(none, signerOf(auth)), (e) => e.code === 'publish_action_unknown');
  const pv = fx('publish_vectors.json').find((v) => v.task === 38 && v.action.startsWith('policy_changed:'));
  const pol = detailWith({ ...p, hash: pv.hash, action: pv.action }, (m) => m.replace(p.hash, pv.hash));
  assert.equal(await wa.deriveAction(pol), pv.action);
  assert.ok(auth.verify(await wa.signApproval(pol, signerOf(auth))));
  await assert.rejects(wa.signApproval({ ...pol, policy_hash: '' }, signerOf(auth)), (e) => e.code === 'publish_action_unknown');
  // the screen shows the DERIVED action label
  const ctx = makeCtx({ '/api/v1/approvals/11': p, '/api/v1/auth/signers': signersRoute() }, { params: ['11'] });
  await withTimeout(A.approvalView(ctx), 'view');
  await until(() => byKey(ctx.root, 'sign'), 'sign');
  const dd = findAll(ctx.root, (e) => e.tagName === 'DD').map(text);
  assert.ok(dd.includes(A.actionLabel('push_branch')), dd.join('|'));
  const mm = makeCtx({ '/api/v1/approvals/11': { ...p, action: 'draft_pr' }, '/api/v1/auth/signers': signersRoute() }, { params: ['11'] });
  await withTimeout(A.approvalView(mm), 'view');
  await until(() => text(mm.root).includes(wa.WA_TEXT.publish_action_mismatch), 'mismatch');
  assert.equal(byKey(mm.root, 'sign'), null);
});

test('not signed mode: says so and never asks the authenticator or the signers list', async () => {
  const ctx = detailCtx({ ...fx('approval.json'), requires_signature: false });
  await A.approvalView(ctx);
  await settle();
  assert.match(text(ctx.root), /서명 필수 모드가 아닙니다/);
  assert.equal(byKey(ctx.root, 'sign'), null);
  assert.equal(auth.gets.length, 0);
  assert.equal(ctx.calls.filter((c) => c.path.startsWith('/api/v1/auth/')).length, 0);
  // nothing to sign right now (already granted, changed, destructive)
  const none = detailCtx({ ...fx('approval.json'), message: '', digest: '' });
  await A.approvalView(none);
  await settle();
  assert.match(text(none.root), /지금은 서명할 수 있는 상태가 아닙니다/);
  assert.equal(byKey(none.root, 'sign'), null);
});

test('hostile meaning, paths, repo and message are shown neutralised; a hostile message is not signable', async () => {
  const H = '<img src=x onerror=alert(1)> \u001b[31m\u202eevil\u0000 \u200b';
  const d = detailWith({ meaning: H, repo: H, protected_files: [H, 'a\u202eb'], action: H, kind: 'protected_modified' },
    (m) => m.replace('repo: ', 'repo: \u202e'));
  const ctx = detailCtx(d, { post: { '/api/v1/approvals/9/grant': {} } });
  await A.approvalView(ctx);
  await settle();
  noHostileElements(ctx.root);
  const all = text(ctx.root);
  assert.ok(all.includes('<img src=x onerror=alert(1)>'), 'markup literal');
  assert.ok(all.includes('⟨U+202E⟩') && all.includes('␛[31m') && all.includes('␀') && all.includes('⟨U+200B⟩'));
  assert.doesNotMatch(all, /[\u0000-\u0009\u000b-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069\u200b]/);
  assert.ok(all.includes(A.CUT_REFUSAL), 'hostile names are not in the diff: refused as unseen');
  // the same hostile message with the real file list: refused as hostile
  const hm = detailCtx({ ...d, protected_files: fx('approval.json').protected_files });
  await withTimeout(A.approvalView(hm), 'view');
  await settle();
  assert.ok(text(hm.root).includes(wa.WA_TEXT.hostile_message));
  assert.equal(byKey(hm.root, 'sign'), null);
  assert.equal(byKey(ctx.root, 'sign'), null, 'no sign control for a hostile message');
  assert.equal(auth.gets.length, 0);
  assert.equal(ctx.posts.length, 0);
  // the list neutralises the same text
  const lctx = makeCtx({ '/api/v1/approvals': [{ id: 9, task_id: 38, kind: H, hash: 'x', head: 'y', status: 'pending' }], '/api/v1/approvals/9': d });
  A.approvalsView(lctx);
  await lctx.polls[0].fn();
  noHostileElements(lctx.root);
  assert.doesNotMatch(text(lctx.root), /[\u0000-\u0009\u000b-\u001f\u202a-\u202e]/);
  for (const e of allElements(lctx.root)) assert.doesNotMatch(e.className, /onerror|img/);
});

test('message/digest mismatch in the detail: error shown, no sign control', async () => {
  const ctx = detailCtx({ ...fx('approval.json'), digest: 'cd'.repeat(32) });
  await A.approvalView(ctx);
  await until(() => text(ctx.root).includes(wa.WA_TEXT.digest_mismatch), 'mismatch text');
  assert.equal(byKey(ctx.root, 'sign'), null);
  assert.equal(auth.gets.length, 0);
});

test('signer: the logged-in passkey; /auth/signers fetched at most once per page load', async () => {
  wa.configure({ credentials: auth, subtle });
  const a = detailCtx(fx('approval.json'));
  await A.approvalView(a);
  await until(() => byKey(a.root, 'sign'), 'sign');
  const b = detailCtx(fx('approval.json'));
  await A.approvalView(b);
  await until(() => byKey(b.root, 'sign'), 'sign');
  const authCalls = [...a.calls, ...b.calls].filter((c) => c.path === '/api/v1/auth/signers');
  assert.equal(authCalls.length, 1, 'cached for the page');
  assert.equal(authCalls[0].opts && authCalls[0].opts.quiet401, true);
  assert.ok(!(authCalls[0].opts && authCalls[0].opts.polling));
  // a token or socket session cannot sign on the web
  const c = detailCtx(fx('approval.json'), { session: { principal: 'x', kind: 'token' } });
  await A.approvalView(c);
  await settle();
  assert.match(text(c.root), /패스키로 로그인한 세션에서만/);
  assert.ok(isDisabled(byKey(c.root, 'sign')));
  // a principal without an active passkey
  const d = detailCtx(fx('approval.json'), { session: { principal: 'nobody', kind: 'session' } });
  await A.approvalView(d);
  await settle();
  assert.match(text(d.root), /활성 패스키를 찾지 못했습니다/);
  assert.ok(isDisabled(byKey(d.root, 'sign')));
});

test('pending approvals: polled through ctx.poll, details once per item, never auth endpoints', async () => {
  const ctx = makeCtx({ '/api/v1/approvals': fx('approvals.json'), '/api/v1/approvals/9': fx('approval.json') });
  A.approvalsView(ctx);
  assert.equal(ctx.polls.length, 1);
  assert.equal(ctx.polls[0].ms, 10000);
  for (let i = 0; i < 3; i++) await ctx.polls[0].fn();
  assert.equal(ctx.calls.filter((c) => c.path === '/api/v1/approvals').length, 3);
  assert.equal(ctx.calls.filter((c) => c.path === '/api/v1/approvals/9').length, 1, 'detail (git on the server) once');
  assert.ok(ctx.calls.every((c) => c.opts && c.opts.polling === true));
  assert.equal(ctx.calls.filter((c) => c.path.startsWith('/api/v1/auth/')).length, 0);
  // the poll handle is the shell's (pause button): pausing it is the view's pause
  ctx.polls[0].pause();
  assert.equal(ctx.polls[0].paused, true);
  // the detail screen is not polled
  const d = detailCtx(fx('approval.json'));
  await A.approvalView(d);
  assert.equal(d.polls.length, 0);
  // empty list
  const e = makeCtx({ '/api/v1/approvals': [] });
  A.approvalsView(e);
  await e.polls[0].fn();
  assert.match(text(e.root), /대기 중인 승인이 없습니다/);
});

test('routes and registration', () => {
  assert.deepEqual(parseRoute('#/approvals'), { name: 'approvals', params: [] });
  assert.deepEqual(parseRoute('#/approvals/9'), { name: 'approval', params: ['9'] });
  for (const bad of ['#/approvals/x', '#/approvals/9/1', '#/approvals/-1', '#/approvals/' + '9'.repeat(30)]) {
    assert.deepEqual(parseRoute(bad), { name: 'notfound', params: [] }, bad);
  }
  assert.equal(navItem('approval'), 'approvals');
  assert.equal(getView('approvals'), A.approvalsView);
  assert.equal(getView('approval'), A.approvalView);
  assert.equal(getView('login'), L.loginView);
});

// ---- login and enrollment ------------------------------------------------------------

const CRED = () => wa.b64uEncode(auth.credId);
function loginCtx(over = {}) {
  const loginChal = wa.b64uEncode(randomBytes(32));
  const regChal = wa.b64uEncode(randomBytes(32));
  const userId = wa.b64uEncode(randomBytes(16));
  const secrets = {
    csrf: 'CSRFSECRETTOKEN0123456789abcdefABCDEF', loginChal, regChal, userId,
    loginCid: 'a1'.repeat(32), regCid: 'b2'.repeat(32), cred: CRED(),
  };
  const routes = { '/api/v1/auth/signers': over.signers || { signers: [{ id: 'phone', credential_id: CRED(), label: '내 폰 <b>x</b>' }] } };
  const post = {
    '/api/v1/auth/webauthn/login/begin': over.loginBegin || (() => ({ challenge_id: secrets.loginCid, expires: 'x',
      options: { challenge: loginChal, rpId: 'orch.example.test', allowCredentials: [{ type: 'public-key', id: CRED() }], userVerification: 'required', timeout: 120000 } })),
    '/api/v1/auth/webauthn/login/finish': over.loginFinish || (() => ({ csrf: secrets.csrf, expires: '2026-11-09T22:00:00Z', signer: 'phone' })),
    '/api/v1/auth/webauthn/register/begin': over.regBegin || (() => ({ challenge_id: secrets.regCid, expires: 'x',
      options: { rp: { id: 'orch.example.test', name: 'aigate' }, user: { id: userId, name: 'aigate:laptop', displayName: '노트북' }, challenge: regChal,
        pubKeyCredParams: [{ type: 'public-key', alg: -257 }], timeout: 300000, attestation: 'direct',
        authenticatorSelection: { residentKey: 'required', userVerification: 'discouraged' } } })),
    '/api/v1/auth/webauthn/register/finish': over.regFinish || (() => ({ id: 'laptop', label: '노트북 <i>', fingerprint: 'SHA256:' + 'A'.repeat(42) + 'E', rp_id: 'orch.example.test', origin: 'https://orch.example.test' })),
  };
  const ctx = makeCtx(routes, { post, session: null });
  return { ctx, secrets };
}

function assertNoSecrets(root, secrets) {
  const vals = Object.values(secrets).filter((s) => typeof s === 'string' && s.length >= 6);
  for (const e of allElements(root)) {
    const blobs = [...e.attrs.values(), typeof e.value === 'string' ? e.value : ''];
    for (const b of blobs) for (const s of vals) assert.ok(!b.includes(s), 'secret in attribute/value of ' + e.tagName);
  }
  const all = text(root);
  for (const s of vals) assert.ok(!all.includes(s), 'secret in text: ' + s.slice(0, 8));
}

test('TestLoginViewNoSecretsInDOM', async () => {
  const { ctx, secrets } = loginCtx();
  await L.loginView(ctx);
  await settle();
  assert.equal(ctx.calls.filter((c) => c.path === '/api/v1/auth/signers').length, 1);
  assert.ok(text(ctx.root).includes('내 폰 <b>x</b>'), 'label as text');
  assertNoSecrets(ctx.root, secrets);
  // login (the only passkey is preselected)
  const btn = byKey(ctx.root, 'login');
  assert.ok(!isDisabled(btn));
  await fire(btn, 'click');
  await until(() => ctx.authed, 'authenticated');
  assert.deepEqual(ctx.authed, { principal: 'phone', kind: 'session', expires: '2026-11-09T22:00:00Z' }, 'no csrf in the session object');
  assert.ok(api.hasCsrf(), 'csrf handed to api.js memory');
  assert.deepEqual(u8(auth.gets[0].publicKey.challenge), wa.b64uDecode(secrets.loginChal));
  assert.equal(auth.gets[0].publicKey.userVerification, 'required');
  const fin = ctx.posts.find((p) => p.path.endsWith('/login/finish')).body;
  assert.deepEqual(Object.keys(fin), ['challenge_id', 'credential']);
  assert.deepEqual(Object.keys(fin.credential), ['id', 'rawId', 'type', 'response']);
  assert.deepEqual(Object.keys(fin.credential.response), ['clientDataJSON', 'authenticatorData', 'signature']);
  secrets.clientData = fin.credential.response.clientDataJSON;
  secrets.signature = fin.credential.response.signature;
  assertNoSecrets(ctx.root, secrets);
  assert.equal(wa.rememberedPasskey().signer, 'phone');
  // enrollment
  const inp = byKey(ctx.root, 'enroll') && findAll(ctx.root, (e) => e.tagName === 'INPUT')[0];
  assert.equal(inp.getAttribute('maxlength'), '20');
  assert.equal(inp.getAttribute('autocomplete'), 'off');
  inp.value = 'abcd-efgh';
  await fire(inp, 'input');
  await fire(byKey(ctx.root, 'enroll'), 'click');
  await until(() => text(ctx.root).includes('대기 중'), 'pending text');
  const regBegin = ctx.posts.find((p) => p.path.endsWith('/register/begin')).body;
  assert.deepEqual(regBegin, { code: 'ABCDEFGH' });
  const regFin = ctx.posts.find((p) => p.path.endsWith('/register/finish')).body;
  secrets.attestation = regFin.credential.response.attestationObject;
  secrets.regClientData = regFin.credential.response.clientDataJSON;
  secrets.code = 'ABCDEFGH';
  secrets.typed = 'abcd-efgh';
  assertNoSecrets(ctx.root, secrets);
  assert.equal(findAll(ctx.root, (e) => e.tagName === 'INPUT')[0].value, '', 'code cleared');
});

test('register: only the expected fields, attestation none and UV required enforced, pending text', async () => {
  const { ctx } = loginCtx();
  await L.loginView(ctx);
  await settle();
  const inp = findAll(ctx.root, (e) => e.tagName === 'INPUT')[0];
  inp.value = 'ABCD EFGH';
  await fire(inp, 'input');
  await fire(byKey(ctx.root, 'enroll'), 'click');
  await until(() => text(ctx.root).includes('대기 중'), 'pending');
  const pk = auth.creates[0].publicKey;
  assert.equal(pk.attestation, 'none');
  assert.deepEqual(pk.authenticatorSelection, { residentKey: 'preferred', userVerification: 'required' });
  assert.deepEqual(pk.pubKeyCredParams, [{ type: 'public-key', alg: -7 }]);
  assert.equal(pk.rp.id, 'orch.example.test');
  assert.equal(pk.challenge.length, 32);
  assert.equal(pk.user.id.length, 16);
  const fin = ctx.posts.find((p) => p.path.endsWith('/register/finish')).body;
  assert.deepEqual(Object.keys(fin), ['challenge_id', 'credential']);
  assert.deepEqual(Object.keys(fin.credential), ['id', 'rawId', 'type', 'response']);
  assert.deepEqual(Object.keys(fin.credential.response), ['clientDataJSON', 'attestationObject']);
  assert.equal(fin.credential.id, CRED());
  assert.equal(fin.credential.rawId, CRED());
  assert.equal(fin.credential.type, 'public-key');
  for (const v of Object.values(fin.credential.response)) assert.match(v, B64U_STRICT);
  for (const p of ctx.posts) assert.equal(p.opts && p.opts.quiet401, true, p.path + ' must not log out on 401');
  const all = text(ctx.root);
  assert.ok(all.includes('sudo orch admin signers add --pending laptop --fingerprint SHA256:' + 'A'.repeat(42) + 'E'));
  assert.ok(all.includes('노트북 <i>'));
  assert.match(all, /아직 로그인이나 서명에 쓸 수 없습니다/);
  assert.doesNotMatch(all, /활성화되었습니다|활성화했습니다|등록 완료|이제 쓸 수 있습니다/);
});

test('enrollment code format is checked before anything is sent', async () => {
  for (const [s, want] of [['ABCD-EFGH', 'ABCDEFGH'], ['abcd efgh', 'ABCDEFGH'], ['oILo-1234', '0110' + '1234'], ['ABCDEFG', null],
    ['ABCDEFGHJ', null], ['ABCD-EFGU', null], ['ABCD_EFGH', null], ['x'.repeat(21), null], ['', null], [null, null], ['ＡBCDEFGH', null]]) {
    assert.equal(wa.normalizeEnrollCode(s), want, String(s));
  }
  const { ctx } = loginCtx();
  await L.loginView(ctx);
  await settle();
  const inp = findAll(ctx.root, (e) => e.tagName === 'INPUT')[0];
  inp.value = 'ABCD-EFGU';
  await fire(inp, 'input');
  await fire(byKey(ctx.root, 'enroll'), 'click');
  await settle();
  assert.equal(ctx.posts.length, 0);
  assert.ok(text(ctx.root).includes(wa.WA_TEXT.bad_code_format));
});

test('login errors: fixed Korean text per server code, never the server text, never a logout', async () => {
  for (const [status, code, path] of [[401, 'bad_assertion', 'finish'], [401, 'bad_challenge', 'finish'], [503, 'signers_unavailable', 'begin'],
    [503, 'webauthn_unconfigured', 'begin'], [503, 'busy', 'begin'], [429, 'rate_limited', 'begin'], [500, 'internal', 'begin']]) {
    auth = new FakeAuthenticator();
    wa.configure({ credentials: auth, subtle });
    const thrower = () => { throw apiError(status, code, 'RAW <img src=x onerror=1> ' + code); };
    const { ctx } = loginCtx(path === 'finish' ? { loginFinish: thrower } : { loginBegin: thrower });
    await L.loginView(ctx);
    await settle();
    await fire(byKey(ctx.root, 'login'), 'click');
    await settle();
    const all = text(ctx.root);
    assert.doesNotMatch(all, /RAW|onerror/);
    const want = L.LOGIN_ERRORS[code] || '요청을 처리하지 못했습니다';
    assert.ok(all.includes(want), code);
    assert.equal(ctx.authed, undefined);
    noHostileElements(ctx.root);
  }
  for (const [code, want] of [['bad_enroll_code', L.LOGIN_ERRORS.bad_enroll_code], ['bad_attestation', L.LOGIN_ERRORS.bad_attestation],
    ['bad_origin', L.LOGIN_ERRORS.bad_origin], ['already_pending', L.LOGIN_ERRORS.already_pending]]) {
    const thrower = () => { throw apiError(code === 'bad_enroll_code' ? 401 : 400, code, 'RAW ' + code); };
    const { ctx } = loginCtx(code === 'bad_enroll_code' ? { regBegin: thrower } : { regFinish: thrower });
    await L.loginView(ctx);
    await settle();
    const inp = findAll(ctx.root, (e) => e.tagName === 'INPUT')[0];
    inp.value = 'ABCDEFGH';
    await fire(inp, 'input');
    await fire(byKey(ctx.root, 'enroll'), 'click');
    await settle();
    assert.ok(text(ctx.root).includes(want), code);
    assert.doesNotMatch(text(ctx.root), /RAW/);
    assert.doesNotMatch(text(ctx.root), /대기 중:/);
  }
});

test('login: signers 429 shows fixed text (api.js already waited once), loaded once, never polled', async () => {
  const { ctx } = loginCtx({ signers: () => { throw apiError(429, 'rate_limited', 'RAW'); } });
  await L.loginView(ctx);
  await settle();
  assert.ok(text(ctx.root).includes(L.LOGIN_ERRORS.rate_limited));
  assert.equal(ctx.calls.length, 1);
  assert.equal(ctx.calls[0].opts.quiet401, true);
  assert.equal(ctx.polls.length, 0);
  assert.ok(isDisabled(byKey(ctx.root, 'login')));
});

test('login: unsupported browser and cancelled ceremony', async () => {
  wa.configure({ credentials: {}, subtle });
  const { ctx } = loginCtx();
  await L.loginView(ctx);
  await settle();
  assert.ok(text(ctx.root).includes(wa.WA_TEXT.unsupported));
  assert.ok(isDisabled(byKey(ctx.root, 'login')) && isDisabled(byKey(ctx.root, 'enroll')));
  assert.equal(ctx.calls.length, 0);
  // cancelled: no finish request
  auth = new FakeAuthenticator({ mode: 'deny' });
  wa.configure({ credentials: auth, subtle });
  const c2 = loginCtx();
  await L.loginView(c2.ctx);
  await settle();
  await fire(byKey(c2.ctx.root, 'login'), 'click');
  await settle();
  assert.ok(text(c2.ctx.root).includes(wa.WA_TEXT.cancelled));
  assert.equal(c2.ctx.posts.filter((p) => p.path.endsWith('/finish')).length, 0);
  assert.equal(c2.ctx.authed, undefined);
  // several passkeys: nothing preselected, picking one enables the button
  wa.configure({ credentials: new FakeAuthenticator(), subtle });
  const c3 = loginCtx({ signers: { signers: [{ id: 'a', credential_id: 'AAAA', label: 'A' }, { id: 'b.c', credential_id: 'BBBB', label: 'B' }] } });
  await L.loginView(c3.ctx);
  await settle();
  assert.ok(isDisabled(byKey(c3.ctx.root, 'login')));
  await fire(byKey(c3.ctx.root, 'pk-1'), 'click');
  assert.ok(!isDisabled(byKey(c3.ctx.root, 'login')));
  assertNoSecrets(c3.ctx.root, { a: 'AAAA', b: 'BBBB' + 'xx' });
});

// ---- golden request shared with Go -----------------------------------------------------

// grant_request.json is produced HERE from the JS builder (signApproval) over
// fixtures/approval.json (itself produced by the Go TestWebFixturesGolden) with
// a fixed test-only P-256 key, and consumed by the Go test
// TestWebGrantRequestGolden (internal/orch/api), which decodes it strictly and
// verifies it with the real signing.Verify. ECDSA signatures are randomised,
// so every field but sig must be byte-equal; the committed sig and a fresh one
// must both verify. ORCH_UPDATE_FIXTURES=1 rewrites the request part.
test('grant request golden shared with Go', async () => {
  const path = fxURL('grant_request.json');
  const update = !!process.env.ORCH_UPDATE_FIXTURES;
  let g = existsSync(path) ? JSON.parse(readFileSync(path, 'utf8')) : null;
  if (!g) {
    assert.ok(update, 'fixtures/grant_request.json missing: run with ORCH_UPDATE_FIXTURES=1');
    const { privateKey, publicKey } = generateKeyPairSync('ec', { namedCurve: 'P-256' });
    const jwk = publicKey.export({ format: 'jwk' });
    g = {
      _comment: 'TEST-ONLY key. Produced by internal/orch/web/test/approvals.test.js (JS grant body builder); verified by internal/orch/api TestWebGrantRequestGolden.',
      authenticator: {
        signer: 'phone',
        credential_id: wa.b64uEncode(randomBytes(32)),
        rp_id: 'orch.example.test',
        origin: 'https://orch.example.test',
        public_key: wa.b64uEncode(cat([4], Buffer.from(jwk.x, 'base64url'), Buffer.from(jwk.y, 'base64url'))),
        private_key_pkcs8: wa.b64uEncode(privateKey.export({ format: 'der', type: 'pkcs8' })),
      },
    };
  }
  const a = g.authenticator;
  const fa = new FakeAuthenticator({ pkcs8: a.private_key_pkcs8, credId: wa.b64uDecode(a.credential_id), rpId: a.rp_id, origin: a.origin });
  wa.configure({ credentials: fa, subtle });
  const src = fx('approval.json');
  const detail = { id: src.id, kind: src.kind, task_id: src.task_id, base: src.base, hash: src.hash, message: src.message,
    digest: src.digest, issued: src.issued, requires_signature: src.requires_signature, truncated: src.truncated };
  const body = await wa.signApproval(src, { id: a.signer, credentialId: a.credential_id, rpId: a.rp_id });
  assert.ok(fa.verify(body));
  if (update) {
    g.detail = detail;
    g.request = body;
    writeFileSync(path, JSON.stringify(g, null, 2) + '\n');
    return;
  }
  assert.deepEqual(g.detail, detail, 'approval.json changed: rerun with ORCH_UPDATE_FIXTURES=1');
  const shape = (o) => (o && typeof o === 'object' ? Object.fromEntries(Object.entries(o).map(([k, v]) => [k, shape(v)])) : typeof o);
  assert.deepEqual(shape(body), shape(g.request), 'JS grant body shape drifted from the golden (Go reads it)');
  assert.deepEqual(Object.keys(body), Object.keys(g.request));
  assert.deepEqual({ ...body, sig: '' }, { ...g.request, sig: '' }, 'deterministic fields drifted');
  assert.ok(fa.verify(g.request), 'committed signature verifies');
});
