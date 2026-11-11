import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { createHash, webcrypto } from 'node:crypto';
import { findAll } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const src = (n) => readFileSync(new URL('../static/' + n, import.meta.url), 'utf8');
const wa = await import('../static/webauthn.js');
const P = await import('../static/views/providers.js');
const PR = await import('../static/views/projects.js');
const { getView, parseRoute, navItem } = await import('../static/router.js');

const VEC = fx('w3c-vectors.json');
const CODES = fx('w3c-error-codes.json').codes;
const PROVIDERS = fx('providers.json');
const GROUPS = fx('provider-groups.json');
const REG = fx('providers-registry-w3c.json');
const CREATE_OK = fx('project-create-ok.json');
const CREATE_CLONE_FAILED = fx('project-create-clone-failed.json');
const CREATE_PARTIAL = fx('project-create-partial.json');
const CLONE_OK = fx('project-clone-ok.json');
const SET_OK = fx('provider-set-ok.json');
const DEL_OK = fx('provider-delete-ok.json');
const GROUP_OK = fx('group-create-ok.json');
const TOKEN_LIST = fx('tokens-list.json');
const KEY = VEC.deploy_keys[0];
const SECRET = '<img src=x onerror=alert(1)> javascript:alert(1) SERVER-WORDS';
const SESSION = { principal: 'phone', kind: 'session', expires: '2026-11-11T00:00:00Z' };

// ---- helpers ------------------------------------------------------------------

const text = (n) => n.textContent;
const byTag = (root, tag) => findAll(root, (e) => e.tagName === tag.toUpperCase());
const byLabel = (root, label) => byTag(root, 'button').filter((b) => text(b).replace(/\s+/g, ' ').trim().startsWith(label));
const byId = (root, id) => findAll(root, (e) => e.getAttribute('id') === id)[0];
const byData = (root, k, v) => findAll(root, (e) => (v === undefined ? e.hasAttribute('data-' + k) : e.getAttribute('data-' + k) === v));
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
  constructor() { this.gets = []; this.gate = null; }
  async get(opts) {
    this.gets.push(opts);
    if (this.gate) await this.gate;
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

// A token list the way GET /settings/tokens answers: rows of the W3b fixture plus the
// admin tokens the providers point at, one with an extra field that must never be read.
const TOKENS = {
  warn_days: 14,
  tokens: [
    ...TOKEN_LIST.tokens,
    { ref: 'gl-admin', kind: 'gitlab', last4: 'Ab12', expires: '2027-03-01', set_at: '2026-10-01T09:00:00Z', source: 'store', days_left: 111, usable: true },
    { ref: 'gl2-admin', kind: 'gitlab', last4: 'Cd34', expires: '', set_at: '2026-10-01T09:00:00Z', source: 'store', days_left: null, usable: true },
  ],
};

// The JS copy of the server's fixed sentences (providerPreview, createPreview). The test
// TestPreviewHelperMatchesGoFixtures pins it to the real ones.
const STAMP = '서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다';
function previewFor(action, subject, f, fingerprint) {
  if (action === 'set_provider') {
    return ["제공자 '" + subject + "'를 저장합니다(종류: " + f.kind + '). 같은 id가 있으면 이 값으로 바뀝니다', 'GitLab 주소: ' + f.base_url, 'ssh 주소: ' + f.ssh_url,
      '사용할 토큰 이름: ' + f.token_ref + ' (토큰 값은 이 화면에 나오지 않습니다)', '[중요] 이 토큰의 권한으로 위 주소의 GitLab에 그룹과 프로젝트를 만듭니다',
      '이 제공자를 쓰는 프로젝트가 있으면 주소는 바꿀 수 없습니다', STAMP];
  }
  if (action === 'delete_provider') {
    return ["제공자 '" + subject + "'를 삭제합니다", '이 제공자를 쓰는 프로젝트가 있으면 삭제되지 않습니다', 'GitLab의 그룹과 프로젝트는 지워지지 않습니다', STAMP];
  }
  if (action === 'create_group') {
    return ["제공자 '" + subject + "'의 GitLab에 새 그룹을 만듭니다", '그룹 이름: ' + f.name, '그룹 경로: ' + f.path, '상위 그룹: ' + (f.parent_path === '' ? '(최상위)' : f.parent_path),
      '비공개 그룹으로 만들어집니다. 이 도구는 그룹을 삭제하지 않습니다', STAMP];
  }
  const aiOn = '[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다';
  const aiOff = '이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)';
  const pub = f.publish === 'read' ? '읽기: 배포 키는 읽기 전용으로 등록합니다(이 서버는 브랜치를 올리지 못합니다)' : '쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다';
  return ["GitLab 제공자 '" + f.provider + "'의 그룹 '" + f.group_path + "'에 새 프로젝트 '" + subject + "'를 만듭니다",
    '템플릿: ' + f.template + ' (코드에 고정된 파일들이 첫 커밋으로 들어갑니다)', '설명: ' + (f.description === '' ? '(없음)' : f.description),
    f.ai_context ? aiOn : aiOff, '게시 권한 — ' + pub, '배포 키 지문(SHA-256): ' + fingerprint + ' (이 서버의 배포 키 공개 키 파일 기준)',
    '등록 후 정책 종류(kind)는 registered 입니다', 'GitLab 프로젝트는 비공개로 만들어지고, 만든 뒤 이 도구는 삭제하지 않습니다', '만든 뒤 이 서버에 자동으로 한 번 복제합니다', STAMP];
}

let seq = 0;
// proposeFor is the fake server's propose: the fields as the real one answers them.
async function proposeFor(body, over) {
  over = over || {};
  let fields = body.fields;
  let extra = {};
  if (body.action === 'create_project') {
    fields = { ...body.fields, deploy_key_sha256: KEY.line_sha256 };
    extra = { deploy_key: { public_key: KEY.line, fingerprint: KEY.fingerprint } };
  }
  const change = await P.computeChange(body.action, body.subject, fields);
  seq++;
  const request = seq.toString(16).padStart(32, '0');
  const issued = '2026-11-10T03:00:00Z';
  return {
    action: body.action, subject: body.subject, fields,
    message: 'aigate-registry-v1\naction: ' + body.action + '\nsubject: ' + body.subject + '\nchange: ' + change + '\nrequest: ' + request + '\nissued: ' + issued + '\n',
    change, request, issued, preview: previewFor(body.action, body.subject, fields, KEY.fingerprint), ...extra, ...over,
  };
}

// mount builds the real view on a fake ctx. `wire` records every POST body as JSON at the call.
function routesFor(over) {
  const wire = [];
  const events = [];
  const rec = (name, fn) => (body, opts) => {
    wire.push({ name, json: JSON.stringify(body) });
    events.push(name);
    return fn(body, opts);
  };
  const routes = {
    '/api/v1/providers': PROVIDERS,
    '/api/v1/providers/gl/groups': GROUPS,
    '/api/v1/providers/gl2/groups': { groups: [], truncated: false },
    '/api/v1/settings/tokens': TOKENS,
    '/api/v1/auth/signers': SIGNERS,
    '/api/v1/projects/registry': REG,
    'POST /api/v1/providers/propose': rec('pp', (b) => proposeFor(b)),
    'POST /api/v1/providers/apply': rec('pa', (b) => {
      if (b.action === 'set_provider') return { provider: { id: b.subject, ...b.fields } };
      if (b.action === 'delete_provider') return { ...DEL_OK, id: b.subject };
      return { ...GROUP_OK, provider: b.subject, group: { path: (b.fields.parent_path ? b.fields.parent_path + '/' : '') + b.fields.path, name: b.fields.name } };
    }),
    'POST /api/v1/projects/create/propose': rec('cp', (b) => proposeFor(b)),
    'POST /api/v1/projects/create/apply': rec('ca', (b) => ({ ...CREATE_OK, project: { ...CREATE_OK.project, id: b.subject } })),
    'POST /api/v1/projects/app-one/clone': rec('clone', () => CLONE_OK),
    ...(over || {}),
  };
  return { routes, wire, events };
}
function ctxFor(over, extra) {
  const { routes, wire, events } = routesFor(over);
  const ctx = fakeCtx(routes, { session: SESSION, ...(extra || {}) });
  ctx.wire = wire;
  ctx.events = events;
  return ctx;
}
const posts = (ctx, path) => ctx.posts.filter((p) => p.path === path);
const signBtn = (ctx) => byLabel(ctx.root, '패스키로 서명')[0];
const waitSignable = (ctx) => until(() => !!signBtn(ctx), 'sign button');
const facts = (ctx) => Object.fromEntries(byData(ctx.root, 'field').map((e) => [e.getAttribute('data-field'), text(e)]));

async function mountProviders(over) {
  const ctx = ctxFor(over);
  P.providersView(ctx);
  await until(() => byLabel(ctx.root, '제공자 추가').length > 0 || text(ctx.root).includes('불러오지 못했습니다'), 'providers list');
  return ctx;
}
async function mountProjects(over) {
  const ctx = ctxFor(over);
  PR.projectsView(ctx);
  await until(() => byLabel(ctx.root, '새 프로젝트 만들기').length > 0 || text(ctx.root).includes('불러오지 못했습니다'), 'projects list');
  return ctx;
}
const rowOf = (ctx, needle) => findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).includes(needle))[0];

// the provider form
async function openProviderForm(ctx, vals) {
  click(byLabel(ctx.root, '제공자 추가')[0]);
  await until(() => byId(ctx.root, 'pv-id'), 'provider form');
  if (vals) fillProvider(ctx, vals);
}
function fillProvider(ctx, o) {
  if (o.id !== undefined) byId(ctx.root, 'pv-id').value = o.id;
  if (o.base !== undefined) byId(ctx.root, 'pv-base').value = o.base;
  if (o.ssh !== undefined) byId(ctx.root, 'pv-ssh').value = o.ssh;
  if (o.token !== undefined) byId(ctx.root, 'pv-token').value = o.token;
}
const GOOD_PROVIDER = { id: 'gl3', base: 'http://gitlab.example:18082', ssh: 'ssh://git@gitlab.example', token: 'gitlab-main' };

// the project creation form
async function openCreateForm(ctx, vals) {
  click(byLabel(ctx.root, '새 프로젝트 만들기')[0]);
  await until(() => byId(ctx.root, 'cp-name'), 'create form');
  await until(() => byTag(byId(ctx.root, 'cp-group'), 'option').length > 1, 'groups');
  if (vals) fillCreate(ctx, vals);
}
function fillCreate(ctx, o) {
  const set = (id, v) => { byId(ctx.root, id).value = v; fire(byId(ctx.root, id), 'change'); };
  if (o.group !== undefined) set('cp-group', o.group);
  if (o.name !== undefined) byId(ctx.root, 'cp-name').value = o.name;
  if (o.desc !== undefined) byId(ctx.root, 'cp-desc').value = o.desc;
  if (o.template !== undefined) set('cp-template', o.template);
  if (o.publish !== undefined) set('cp-publish', o.publish);
  if (o.ai !== undefined) byId(ctx.root, 'cp-ai').checked = o.ai;
}
const GOOD_CREATE = { group: 'team/sub', name: 'app-two', desc: '두 번째 프로젝트', template: 'go', publish: 'write', ai: false };
async function toCreateConfirm(ctx, vals) {
  await openCreateForm(ctx, vals || GOOD_CREATE);
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await until(() => byData(ctx.root, 'warn', 'mismatch').length > 0 || !!signBtn(ctx) || byData(ctx.root, 'problem', 'form').some((e) => text(e) !== ''), 'confirm');
}

// ---- Step 1 tests (the names of the brief) -------------------------------------

test('TestChangeVectorsMatchGoForNewActions', async () => {
  assert.ok(VEC.change.length >= 8 && VEC.refused.length >= 30);
  for (const c of VEC.change) {
    assert.equal(P.canonicalChange(c.action, c.subject, c.fields), c.canonical, c.name);
    assert.equal(await P.computeChange(c.action, c.subject, c.fields), c.change, c.name);
    const rev = {};
    for (const k of Object.keys(c.fields).reverse()) rev[k] = c.fields[k];
    assert.equal(P.canonicalChange(c.action, c.subject, rev), c.canonical, c.name + ' (reversed)');
    // an independent SHA-256 over the same bytes
    assert.equal(createHash('sha256').update(c.canonical).digest('hex'), c.change, c.name);
  }
  // everything Go refuses, the browser refuses too (a one-letter project name included)
  for (const r of VEC.refused) {
    await assert.rejects(async () => P.computeChange(r.action, r.subject, r.fields), { name: 'ChangeError' }, r.name);
  }
  // cases JSON cannot carry
  const ok = VEC.change.find((c) => c.name === 'create_project plain').fields;
  assert.throws(() => P.canonicalChange('create_project', 'ab', { ...ok, description: 'a\ud800b' }), { name: 'ChangeError' }, 'lone surrogate');
  assert.throws(() => P.canonicalChange('create_project', 'ab', { ...ok, ai_context: 1 }), { name: 'ChangeError' });
  assert.throws(() => P.canonicalChange('create_project', 'ab', Object.create(ok)), { name: 'ChangeError' }, 'inherited fields');
  // the golden messages: the browser's message check accepts exactly Go's text for each new action
  assert.equal(VEC.messages.length, 4);
  for (const m of VEC.messages) {
    assert.equal(createHash('sha256').update(m.text).digest('hex'), m.digest);
    assert.deepEqual(wa.checkRegistryMessage(m.text), { action: m.action, subject: m.subject, change: m.change, request: m.request, issued: m.issued });
    assert.throws(() => wa.checkRegistryMessage(m.text.replace('action: ' + m.action, 'action: purge_provider')));
  }
  // the shape validators agree with registrymsg on every vector (IPv6 literals: the browser is stricter)
  const agree = (fn, set, what) => {
    for (const v of set) assert.equal(fn(v.value), v.ok, what + ': ' + JSON.stringify(v.value));
  };
  agree(P.validName, VEC.names, 'name');
  agree(P.validGroupPath, VEC.group_paths, 'group path');
  // descriptions: Go's rule AND the page's own text rule (findHostile): the browser may be stricter than Go, never looser
  const { findHostile } = await import('../static/render.js');
  for (const v of VEC.descriptions) {
    assert.equal(P.validDescription(v.value), v.ok && findHostile(v.value) === null, 'description: ' + JSON.stringify(v.value));
  }
  assert.ok(VEC.descriptions.some((v) => v.ok && findHostile(v.value) !== null), 'vectors where Go accepts and the page refuses exist');
  agree(P.validBaseUrl, VEC.base_urls.filter((v) => !v.value.includes('[')), 'base_url');
  agree(P.validSshUrl, VEC.ssh_urls.filter((v) => !v.value.includes('[')), 'ssh_url');
  for (const v of [...VEC.base_urls.filter((x) => x.value.includes('[')), ...VEC.ssh_urls.filter((x) => x.value.includes('['))]) {
    assert.equal(P.validBaseUrl(v.value) || P.validSshUrl(v.value), false, 'IPv6 literal refused by the browser: ' + v.value);
  }
  // the deploy key: the signed hash is over the line's text, the fingerprint over its blob
  assert.ok(VEC.deploy_keys.length >= 3);
  for (const k of VEC.deploy_keys) {
    assert.equal(await P.deployKeyHash(k.line), k.line_sha256, k.name);
    assert.equal(createHash('sha256').update(k.line).digest('hex'), k.line_sha256, k.name);
    assert.equal(await P.sshFingerprint(k.line), k.fingerprint, k.name);
    assert.match(k.line, P.DEPLOY_KEY_RE);
  }
  assert.equal(await P.sshFingerprint('ssh-ed25519 !!!notbase64'), null);
  assert.equal(await P.sshFingerprint('nonsense'), null);
});

test('TestPreviewHelperMatchesGoFixtures', () => {
  for (const c of VEC.change) {
    assert.deepEqual(previewFor(c.action, c.subject, c.fields, KEY.fingerprint), VEC.previews[c.name], c.name);
  }
});

test('TestProvidersListRendersAsText', async () => {
  const hostile = { id: '<img src=x onerror=alert(1)>', kind: 'javascript:alert(1)', base_url: '"><script>1', ssh_url: SECRET, token_ref: '<b>x</b>', in_use: false };
  const ctx = await mountProviders({ '/api/v1/providers': { providers: [...PROVIDERS.providers, hostile] } });
  assert.equal(findAll(ctx.root, (e) => ['IMG', 'SCRIPT', 'IFRAME', 'B'].includes(e.tagName)).length, 0);
  // the only link is the fixed one to the token screen
  assert.deepEqual(findAll(ctx.root, (e) => e.tagName === 'A').map((a) => a.getAttribute('href')), ['#/tokens']);
  for (const e of findAll(ctx.root, () => true)) {
    for (const [k, v] of e.attrs) {
      assert.ok(!/onerror|javascript:|<img|SERVER-WORDS|<script/.test(k + '=' + v), 'attribute ' + k + ' carries server text: ' + v);
      assert.ok(!k.startsWith('on'), k);
    }
  }
  const t = text(ctx.root);
  assert.ok(t.includes('<img src=x onerror=alert(1)>'), 'shown literally');
  assert.ok(t.includes('http://gitlab.example:18082') && t.includes('ssh://git@gitlab.example'));
  // an id that could not be signed gets no action buttons
  const bad = rowOf(ctx, 'onerror');
  assert.equal(byTag(bad, 'button').length, 0);
  // the token columns come from the token list (name, last four characters, expiry), not from the provider row
  const gl = rowOf(ctx, 'gl-admin');
  assert.ok(text(gl).includes('Ab12') && text(gl).includes('2027-03-01'), text(gl));
  // a failed read: a fixed text, never the server's words
  const failing = fakeCtx({ '/api/v1/providers': () => { throw apiError(503, 'broker_unavailable', SECRET); }, '/api/v1/settings/tokens': TOKENS }, { session: SESSION });
  P.providersView(failing);
  await until(() => text(failing.root).includes(P.ERROR_TEXT.broker_unavailable), 'list failure');
  assert.ok(!text(failing.root).includes('SERVER-WORDS'));
  // no token list: the provider list still works, the token columns say so
  const noTok = await mountProviders({ '/api/v1/settings/tokens': () => { throw apiError(503, 'tokens_unavailable', SECRET); } });
  assert.ok(text(rowOf(noTok, 'gl-admin')).includes('gl-admin'));
});

test('TestAddProviderFlow', async () => {
  const ctx = await mountProviders();
  await openProviderForm(ctx);
  // only usable gitlab tokens can be picked
  const opts = byTag(byId(ctx.root, 'pv-token'), 'option').map((o) => o.getAttribute('value'));
  assert.deepEqual(opts.sort(), ['gitlab-docs', 'gitlab-main', 'gl-admin', 'gl2-admin']);
  for (const never of ['openai', 'github-old', 'github-lost']) assert.ok(!opts.includes(never), never);
  // every address and id is checked before anything is sent
  const go = () => click(byLabel(ctx.root, '변경 내용 확인')[0]);
  const problem = () => text(byData(ctx.root, 'problem', 'form')[0]);
  for (const [bad, expect] of [
    [{ ...GOOD_PROVIDER, id: 'GL' }, /id/],
    [{ ...GOOD_PROVIDER, base: 'ftp://gitlab.example' }, /GitLab 주소/],
    [{ ...GOOD_PROVIDER, base: 'http://gitlab.example/api/v4' }, /GitLab 주소/],
    [{ ...GOOD_PROVIDER, base: 'http://u:p@gitlab.example' }, /GitLab 주소/],
    [{ ...GOOD_PROVIDER, ssh: 'ssh://root@gitlab.example' }, /ssh 주소/],
    [{ ...GOOD_PROVIDER, ssh: 'git@gitlab.example:x.git' }, /ssh 주소/],
    [{ ...GOOD_PROVIDER, token: '' }, /토큰/],
    [{ ...GOOD_PROVIDER, token: 'openai' }, /토큰/],
  ]) {
    fillProvider(ctx, bad);
    go();
    await sleep(5);
    assert.match(problem(), expect, JSON.stringify(bad));
  }
  assert.equal(posts(ctx, '/api/v1/providers/propose').length, 0, 'nothing sent for bad input');
  // a good one: propose, confirm screen with every field, sign, apply, list re-read
  fillProvider(ctx, GOOD_PROVIDER);
  go();
  await waitSignable(ctx);
  const f = facts(ctx);
  assert.ok(f.id.includes('gl3') && f.base_url.includes('http://gitlab.example:18082') && f.ssh_url.includes('ssh://git@gitlab.example') && f.token_ref.includes('gitlab-main'), JSON.stringify(f));
  assert.equal(byData(ctx.root, 'warn', 'edit').length, 0, 'a new provider is not an edit');
  await sleep(20);
  assert.equal(auth.gets.length, 0, 'nothing is signed before the button is pressed');
  assert.equal(posts(ctx, '/api/v1/providers/apply').length, 0, 'nothing is applied before the button is pressed');
  const body = JSON.parse(ctx.wire.find((w) => w.name === 'pp').json);
  assert.deepEqual(body, { action: 'set_provider', subject: 'gl3', fields: { kind: 'gitlab', base_url: GOOD_PROVIDER.base, ssh_url: GOOD_PROVIDER.ssh, token_ref: 'gitlab-main' } });
  const before = ctx.calls.filter((c) => c === '/api/v1/providers').length;
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  const applied = JSON.parse(ctx.wire.find((w) => w.name === 'pa').json);
  assert.equal(applied.action, 'set_provider');
  assert.ok(applied.signature && applied.signature.signer === 'phone');
  assert.equal(applied.change, await P.computeChange('set_provider', 'gl3', body.fields));
  assert.equal(auth.gets.length, 1);
  assert.ok(ctx.calls.filter((c) => c === '/api/v1/providers').length > before, 'the list is read again');
  // the apply carries the long timeout and never waits out a 429
  const ap = posts(ctx, '/api/v1/providers/apply')[0];
  assert.equal(ap.opts.noRetry429, true);
  assert.ok(ap.opts.timeoutMs >= 30000);
});

test('TestProviderEditWarnsAndBlocksInUseAddressChange', async () => {
  const ctx = await mountProviders();
  // an edit of an existing provider: the warning is on the form and on the confirmation
  click(byLabel(rowOf(ctx, 'gl2-admin'), '수정')[0]);
  await until(() => byId(ctx.root, 'pv-id'), 'edit form');
  assert.equal(byId(ctx.root, 'pv-id').value, 'gl2');
  assert.equal(byData(ctx.root, 'warn', 'edit').length, 1, 'warning before the form is submitted');
  assert.ok(byId(ctx.root, 'pv-id').hasAttribute('readonly') || byId(ctx.root, 'pv-id').hasAttribute('disabled'), 'the id of an existing provider is fixed');
  byId(ctx.root, 'pv-token').value = 'gl-admin';
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await waitSignable(ctx);
  assert.equal(byData(ctx.root, 'warn', 'edit').length, 1, 'warning on the confirmation too');
  assert.match(text(byData(ctx.root, 'warn', 'edit')[0]), /토큰|주소/);
  const f = facts(ctx);
  assert.ok(f.token_ref.includes('gl2-admin') && f.token_ref.includes('gl-admin'), 'old and new token shown: ' + f.token_ref);
  // a provider in use: changing the GitLab address or the ssh address is refused locally (the server would burn the signature)
  const ctx2 = await mountProviders();
  click(byLabel(rowOf(ctx2, 'gl-admin'), '수정')[0]);
  await until(() => byId(ctx2.root, 'pv-id'), 'edit form 2');
  byId(ctx2.root, 'pv-base').value = 'http://other.example';
  click(byLabel(ctx2.root, '변경 내용 확인')[0]);
  await sleep(10);
  assert.match(text(byData(ctx2.root, 'problem', 'form')[0]), /사용 중/);
  byId(ctx2.root, 'pv-base').value = 'http://gitlab.example:18082';
  byId(ctx2.root, 'pv-ssh').value = 'ssh://git@other.example';
  click(byLabel(ctx2.root, '변경 내용 확인')[0]);
  await sleep(10);
  assert.match(text(byData(ctx2.root, 'problem', 'form')[0]), /사용 중/);
  assert.equal(posts(ctx2, '/api/v1/providers/propose').length, 0);
  // only the token changed: allowed
  byId(ctx2.root, 'pv-ssh').value = 'ssh://git@gitlab.example';
  byId(ctx2.root, 'pv-token').value = 'gitlab-main';
  click(byLabel(ctx2.root, '변경 내용 확인')[0]);
  await waitSignable(ctx2);
});

test('TestDeleteProviderDisabledWhenInUse', async () => {
  const ctx = await mountProviders();
  const used = rowOf(ctx, 'gl-admin');
  const del = byLabel(used, '삭제')[0];
  assert.ok(del.hasAttribute('disabled'), 'in-use provider: delete is disabled');
  click(del);
  await sleep(10);
  assert.equal(posts(ctx, '/api/v1/providers/propose').length, 0);
  assert.match(text(used), /사용 중/);
  // an unused one can be deleted: confirmation (id and addresses), signature, apply
  const free = rowOf(ctx, 'gl2-admin');
  const del2 = byLabel(free, '삭제')[0];
  assert.ok(!del2.hasAttribute('disabled'));
  click(del2);
  await waitSignable(ctx);
  const f = facts(ctx);
  assert.ok(f.id.includes('gl2') && f.base_url.includes('https://git.example'), JSON.stringify(f));
  assert.equal(JSON.parse(ctx.wire.find((w) => w.name === 'pp').json).action, 'delete_provider');
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'deleted');
  // the server refusing with provider_in_use anyway (a project appeared meanwhile): fixed text, list re-read
  const c3 = await mountProviders({ 'POST /api/v1/providers/apply': () => { throw apiError(409, 'provider_in_use', SECRET); } });
  click(byLabel(rowOf(c3, 'gl2-admin'), '삭제')[0]);
  await waitSignable(c3);
  const n = c3.calls.filter((c) => c === '/api/v1/providers').length;
  click(signBtn(c3));
  await until(() => byData(c3.root, 'result', 'error').length === 1, 'refused');
  assert.ok(text(c3.root).includes(P.ERROR_TEXT.provider_in_use));
  assert.ok(!text(c3.root).includes('SERVER-WORDS'));
  assert.ok(c3.calls.filter((c) => c === '/api/v1/providers').length > n);
});

test('TestTokenSelectDoesNotShowValues', async () => {
  const VALUE = 'canary-NOT-A-REAL-TOKEN-0123456789abcdefghij-ZQxw';
  const rows = TOKENS.tokens.map((t) => ({ ...t, value: VALUE, secret: VALUE, token: VALUE }));
  const ctx = await mountProviders({ '/api/v1/settings/tokens': { tokens: rows, warn_days: 14 } });
  await openProviderForm(ctx);
  const sel = byId(ctx.root, 'pv-token');
  for (const o of byTag(sel, 'option')) {
    const label = text(o);
    assert.ok(label.includes(o.getAttribute('value')), 'the option names the token');
    assert.ok(!label.includes(VALUE));
  }
  // nowhere in the page, and the form has no field that could take a token value
  for (const e of findAll(ctx.root, () => true)) {
    assert.ok(!text(e).includes(VALUE), 'value in text');
    for (const [k, v] of e.attrs) assert.ok(!String(v).includes(VALUE), 'value in attribute ' + k);
  }
  const inputs = byTag(ctx.root, 'input').concat(byTag(ctx.root, 'textarea'));
  for (const i of inputs) {
    assert.ok(!/token|secret|value|key/i.test(i.getAttribute('id') || ''), 'input id ' + i.getAttribute('id'));
    assert.notEqual(i.getAttribute('type'), 'password');
  }
});

test('TestCreateGroupConfirmationShowsEveryField', async () => {
  const ctx = await mountProjects();
  await openCreateForm(ctx);
  click(byLabel(ctx.root, '새 그룹 만들기')[0]);
  await until(() => byId(ctx.root, 'cg-name'), 'group form');
  // the parent list is the provider's groups plus the top level
  const parents = byTag(byId(ctx.root, 'cg-parent'), 'option').map((o) => o.getAttribute('value'));
  assert.deepEqual(parents, ['', 'team', 'team/sub']);
  byId(ctx.root, 'cg-parent').value = 'team';
  byId(ctx.root, 'cg-name').value = 'newgroup';
  fire(byId(ctx.root, 'cg-name'), 'input');
  assert.equal(byId(ctx.root, 'cg-path').value, 'newgroup', 'the path follows the name');
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await waitSignable(ctx);
  const f = facts(ctx);
  assert.ok(f.provider.includes('gl') && f.name.includes('newgroup') && f.path.includes('newgroup') && f.parent_path.includes('team'), JSON.stringify(f));
  assert.deepEqual(JSON.parse(ctx.wire.find((w) => w.name === 'pp').json),
    { action: 'create_group', subject: 'gl', fields: { name: 'newgroup', path: 'newgroup', parent_path: 'team' } });
  // a top-level group says so
  const made = [...GROUPS.groups];
  const c2 = await mountProjects({
    '/api/v1/providers/gl/groups': () => ({ groups: [...made], truncated: false }),
    'POST /api/v1/providers/apply': (b) => { made.push({ path: b.fields.path, name: b.fields.name }); return { ...GROUP_OK, provider: b.subject, group: { path: b.fields.path, name: b.fields.name } }; },
  });
  await openCreateForm(c2);
  click(byLabel(c2.root, '새 그룹 만들기')[0]);
  await until(() => byId(c2.root, 'cg-name'), 'group form 2');
  byId(c2.root, 'cg-parent').value = '';
  byId(c2.root, 'cg-name').value = 'topgroup';
  fire(byId(c2.root, 'cg-name'), 'input');
  click(byLabel(c2.root, '변경 내용 확인')[0]);
  await waitSignable(c2);
  assert.ok(facts(c2).parent_path.includes('최상위'));
  // sign: the group list is read again and the new group is selected in the project form
  click(signBtn(c2));
  await until(() => byId(c2.root, 'cp-name') && byId(c2.root, 'cp-group').value === 'topgroup', 'back in the project form with the new group');
  assert.equal(c2.calls.filter((c) => c === '/api/v1/providers/gl/groups').length, 2);
  // a depth-4 parent can not get a child
  const c3 = await mountProjects({ '/api/v1/providers/gl/groups': { groups: [{ path: 'aa/bb/cc/dd', name: 'dd' }, { path: 'aa', name: 'aa' }], truncated: false } });
  await openCreateForm(c3);
  click(byLabel(c3.root, '새 그룹 만들기')[0]);
  await until(() => byId(c3.root, 'cg-name'), 'group form 3');
  const deep = byTag(byId(c3.root, 'cg-parent'), 'option').map((o) => o.getAttribute('value'));
  assert.ok(!deep.includes('aa/bb/cc/dd'), 'a group at the depth limit is not offered as a parent');
});

test('TestCreateProjectConfirmationShowsEveryField', async () => {
  const ctx = await mountProjects();
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  const f = facts(ctx);
  assert.ok(f.provider.includes('gl'), 'provider');
  assert.ok(f.group_path.includes('team/sub'), 'group path');
  assert.ok(f.name.includes('app-two'), 'name');
  assert.ok(f.template.includes('go'), 'template');
  assert.ok(f.description.includes('두 번째 프로젝트'), 'description');
  assert.match(f.ai_context, /아니오/, 'AI transfer, off by default');
  assert.match(f.publish, /쓰기/, 'publish right');
  assert.ok(f.fingerprint.includes(KEY.fingerprint), 'fingerprint in ssh-keygen -l form');
  assert.ok(f.key_prefix.includes(KEY.line.slice(0, 40)) && !f.key_prefix.includes(KEY.line), 'the front of the .pub line, not the whole key');
  await sleep(20);
  assert.equal(auth.gets.length, 0, 'nothing is signed before the button is pressed');
  assert.equal(posts(ctx, '/api/v1/projects/create/apply').length, 0, 'nothing is applied before the button is pressed');
  assert.deepEqual(JSON.parse(ctx.wire.find((w) => w.name === 'cp').json),
    { action: 'create_project', subject: 'app-two', fields: { provider: 'gl', group_path: 'team/sub', template: 'go', description: '두 번째 프로젝트', ai_context: false, publish: 'write' } },
    'the browser never sends deploy_key_sha256');
  // AI on and read-only publish are shown as such
  const c2 = await mountProjects();
  await toCreateConfirm(c2, { ...GOOD_CREATE, ai: true, publish: 'read', template: 'basic', desc: '' });
  await waitSignable(c2);
  const g = facts(c2);
  assert.match(g.ai_context, /예/);
  assert.ok(byData(c2.root, 'emph', 'ai-on').length === 1);
  assert.match(g.publish, /읽기/);
  assert.match(g.description, /없음/);
});

test('TestSignOnlyWhenChangeMatches', async () => {
  const flip = (s) => (s[0] === 'a' ? 'b' : 'a') + s.slice(1);
  const bad = {
    'wrong change': async (b) => ({ ...(await proposeFor(b)), change: '0'.repeat(64) }),
    'message with another change': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace(r.change, '1'.repeat(64)) }; },
    'message for another subject': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace('subject: ' + b.subject, 'subject: other-one') }; },
    'message for another action': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace('create_project', 'create_group') }; },
    'message with another request': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace(r.request, 'f'.repeat(32)) }; },
    'message not canonical': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message + '\n' }; },
    'response with another issued': async (b) => ({ ...(await proposeFor(b)), issued: '2026-11-10T03:00:01Z' }),
    'a field the screen did not send': async (b) => { const r = await proposeFor(b); const f = { ...r.fields, template: 'basic' }; return { ...r, fields: f, change: await P.computeChange(b.action, b.subject, f) }; },
    'ai switched on by the server': async (b) => { const r = await proposeFor(b); const f = { ...r.fields, ai_context: true }; return { ...r, fields: f, change: await P.computeChange(b.action, b.subject, f) }; },
    'deploy key hash of another key (change recomputed to match)': async (b) => {
      const r = await proposeFor(b);
      const f = { ...r.fields, deploy_key_sha256: flip(r.fields.deploy_key_sha256) };
      const change = await P.computeChange(b.action, b.subject, f);
      return { ...r, fields: f, change, message: r.message.replace(r.change, change) };
    },
    'deploy key line of another key': async (b) => { const r = await proposeFor(b); return { ...r, deploy_key: { ...r.deploy_key, public_key: VEC.deploy_keys[1].line } }; },
    'fingerprint of another key': async (b) => { const r = await proposeFor(b); return { ...r, deploy_key: { ...r.deploy_key, fingerprint: VEC.deploy_keys[1].fingerprint } }; },
    'deploy key missing': async (b) => { const r = await proposeFor(b); delete r.deploy_key; return r; },
    'deploy key with a hostile comment': async (b) => { const r = await proposeFor(b); return { ...r, deploy_key: { ...r.deploy_key, public_key: r.deploy_key.public_key + ' x<img>' } }; },
    'preview of another group': async (b) => { const r = await proposeFor(b); return { ...r, preview: previewFor(b.action, b.subject, { ...r.fields, group_path: 'other' }, KEY.fingerprint) }; },
    'preview of another fingerprint': async (b) => { const r = await proposeFor(b); return { ...r, preview: previewFor(b.action, b.subject, r.fields, VEC.deploy_keys[1].fingerprint) }; },
    'preview says ai is on': async (b) => { const r = await proposeFor(b); return { ...r, preview: previewFor(b.action, b.subject, { ...r.fields, ai_context: true }, KEY.fingerprint) }; },
    'hostile preview': async (b) => ({ ...(await proposeFor(b)), preview: ['ok‮evil'] }),
    'empty preview': async (b) => ({ ...(await proposeFor(b)), preview: [] }),
    'not an object': async () => 'nope',
  };
  for (const [name, fn] of Object.entries(bad)) {
    const ctx = await mountProjects({ 'POST /api/v1/projects/create/propose': fn });
    await toCreateConfirm(ctx);
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, name + ': warning shown');
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, name + ': no sign button');
    assert.equal(auth.gets.length, 0, name + ': nothing signed');
    assert.equal(posts(ctx, '/api/v1/projects/create/apply').length, 0, name);
  }
  // the same for the provider and group proposals
  for (const [name, fn] of Object.entries({
    'provider: wrong change': async (b) => ({ ...(await proposeFor(b)), change: '0'.repeat(64) }),
    'provider: another address in the fields': async (b) => { const r = await proposeFor(b); const f = { ...r.fields, base_url: 'http://evil.example' }; return { ...r, fields: f, change: await P.computeChange(b.action, b.subject, f) }; },
    'provider: preview of another address': async (b) => { const r = await proposeFor(b); return { ...r, preview: previewFor(b.action, b.subject, { ...r.fields, base_url: 'http://evil.example' }) }; },
    'provider: another token': async (b) => { const r = await proposeFor(b); const f = { ...r.fields, token_ref: 'gl-admin' }; return { ...r, fields: f, change: await P.computeChange(b.action, b.subject, f) }; },
  })) {
    const ctx = await mountProviders({ 'POST /api/v1/providers/propose': fn });
    await openProviderForm(ctx, GOOD_PROVIDER);
    click(byLabel(ctx.root, '변경 내용 확인')[0]);
    await until(() => byData(ctx.root, 'warn', 'mismatch').length === 1, name);
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, name);
    assert.equal(auth.gets.length, 0, name);
  }
  const ok = await mountProjects();
  await toCreateConfirm(ok);
  await waitSignable(ok);
  assert.equal(byData(ok.root, 'warn', 'mismatch').length, 0);
  // the real Go previews and messages are accepted (a fake server of its own cannot disagree with Go)
  const real = await mountProjects({
    'POST /api/v1/projects/create/propose': async (b) => {
      const r = await proposeFor(b);
      return { ...r, preview: VEC.previews['create_project plain'].map((s) => s) };
    },
  });
  await toCreateConfirm(real, { group: 'team/sub', name: 'app-one', desc: '첫 프로젝트', template: 'go', publish: 'write', ai: false });
  await waitSignable(real);
});

test('TestNamePrevalidation', async () => {
  const ctx = await mountProjects();
  await openCreateForm(ctx, GOOD_CREATE);
  const go = () => click(byLabel(ctx.root, '변경 내용 확인')[0]);
  const problem = () => text(byData(ctx.root, 'problem', 'form')[0]);
  const bads = [
    [{ name: 'a' }, /이름/], [{ name: 'App' }, /이름/], [{ name: '1app' }, /이름/], [{ name: 'a_b' }, /이름/], [{ name: 'a'.repeat(41) }, /이름/], [{ name: '' }, /이름/],
    [{ name: 'app-two', desc: 'a\nb' }, /설명/], [{ name: 'app-two', desc: 'a​b' }, /설명/], [{ name: 'app-two', desc: 'x'.repeat(201) }, /설명/],
    [{ name: 'app-two', desc: '😀'.repeat(201) }, /설명/], [{ name: 'app-two', desc: 'a‮b' }, /설명/],
    [{ name: 'app-two', desc: 'ok', group: '' }, /그룹/],
  ];
  for (const [o, expect] of bads) {
    fillCreate(ctx, { ...GOOD_CREATE, ...o });
    go();
    await sleep(5);
    assert.match(problem(), expect, JSON.stringify(o).slice(0, 80));
  }
  assert.equal(posts(ctx, '/api/v1/projects/create/propose').length, 0, 'not one request for any bad input');
  assert.equal(auth.gets.length, 0);
  // code points, not UTF-16 units: 200 emoji are fine
  fillCreate(ctx, { ...GOOD_CREATE, desc: '😀'.repeat(200) });
  go();
  await waitSignable(ctx);
  // the group form checks name, path and depth the same way
  const c2 = await mountProjects();
  await openCreateForm(c2);
  click(byLabel(c2.root, '새 그룹 만들기')[0]);
  await until(() => byId(c2.root, 'cg-name'), 'group form');
  for (const [name, path] of [['a', 'a'], ['Grp', 'grp'], ['grp', 'a/b'], ['grp', ''], ['gr p', 'grp']]) {
    byId(c2.root, 'cg-name').value = name;
    byId(c2.root, 'cg-path').value = path;
    click(byLabel(c2.root, '변경 내용 확인')[0]);
    await sleep(5);
    assert.notEqual(text(byData(c2.root, 'problem', 'group')[0]), '', name + '/' + path);
  }
  assert.equal(posts(c2, '/api/v1/providers/propose').length, 0);
});

test('TestProgressStepsRenderedFromFixedStatuses', async () => {
  const stepsOf = (ctx) => Object.fromEntries(byData(ctx.root, 'step').map((e) => [e.getAttribute('data-step'), e.getAttribute('data-status')]));
  // success with a clone: seven steps, all done; while waiting only a fixed sentence
  let release;
  const gate = new Promise((r) => { release = r; });
  const ctx = await mountProjects({ 'POST /api/v1/projects/create/apply': async (b) => { await gate; return { ...CREATE_OK, project: { ...CREATE_OK.project, id: b.subject } }; } });
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'pending').length === 1, 'pending');
  assert.equal(byData(ctx.root, 'step').length, 0, 'no step claims while the server works');
  assert.match(text(byData(ctx.root, 'result', 'pending')[0]), /GitLab/);
  release();
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'done');
  assert.deepEqual(stepsOf(ctx), { group: 'done', project: 'done', first_commit: 'done', protect: 'done', deploy_key: 'done', register: 'done', clone: 'done' });
  assert.ok(byData(ctx.root, 'step').every((e) => text(e).trim().length > 0 && text(e).length < 80), 'fixed short step texts');
  // the step labels are the browser's own: the fixed list
  assert.deepEqual(byData(ctx.root, 'step').map((e) => e.getAttribute('data-step')), ['group', 'project', 'first_commit', 'protect', 'deploy_key', 'register', 'clone']);
  // a partial failure at each broker step: earlier done, that one failed, later not run; server words never shown
  const expectAt = {
    project: { group: 'done', project: 'failed', first_commit: 'skipped', protect: 'skipped', deploy_key: 'skipped', register: 'skipped', clone: 'skipped' },
    first_commit: { group: 'done', project: 'done', first_commit: 'failed', protect: 'skipped', deploy_key: 'skipped', register: 'skipped', clone: 'skipped' },
    default_branch: { group: 'done', project: 'done', first_commit: 'failed', protect: 'skipped', deploy_key: 'skipped', register: 'skipped', clone: 'skipped' },
    protect: { group: 'done', project: 'done', first_commit: 'done', protect: 'failed', deploy_key: 'skipped', register: 'skipped', clone: 'skipped' },
    deploy_key: { group: 'done', project: 'done', first_commit: 'done', protect: 'done', deploy_key: 'failed', register: 'skipped', clone: 'skipped' },
    register: { group: 'done', project: 'done', first_commit: 'done', protect: 'done', deploy_key: 'done', register: 'failed', clone: 'skipped' },
  };
  for (const step of P.PARTIAL_STEPS) {
    const err = apiError(502, 'create_partial', SECRET, { detail: { ...CREATE_PARTIAL.error.detail, step } });
    const c = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw err; } });
    await toCreateConfirm(c);
    await waitSignable(c);
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'error').length === 1, step);
    assert.deepEqual(stepsOf(c), expectAt[step], step);
    assert.ok(!text(c.root).includes('SERVER-WORDS'), step);
  }
  // the automatic clone failed (HTTP 200): everything but the clone is done
  const cf = await mountProjects({ 'POST /api/v1/projects/create/apply': () => CREATE_CLONE_FAILED });
  await toCreateConfirm(cf);
  await waitSignable(cf);
  click(signBtn(cf));
  await until(() => byData(cf.root, 'step').length === 7, 'steps after clone failure');
  assert.deepEqual(stepsOf(cf), { group: 'done', project: 'done', first_commit: 'done', protect: 'done', deploy_key: 'done', register: 'done', clone: 'failed' });
});

test('TestCreatePartialShowsOrphanWithManualCleanupNotice', async () => {
  const detail = { ...CREATE_PARTIAL.error.detail, project: 'team/<img src=x onerror=alert(1)>', provider: 'gl' };
  // a detail with a bad value is not shown; the page falls back to the values the browser itself sent
  const hostileCtx = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail }); } });
  await toCreateConfirm(hostileCtx);
  await waitSignable(hostileCtx);
  click(signBtn(hostileCtx));
  await until(() => byData(hostileCtx.root, 'result', 'error').length === 1, 'partial');
  assert.ok(!text(hostileCtx.root).includes('<img'), 'invalid detail is not displayed');
  assert.ok(text(hostileCtx.root).includes('team/sub/app-two'), 'fallback to what this page sent');
  // the real detail: path and id as text, the fixed notice
  const ctx = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail: CREATE_PARTIAL.error.detail }); } });
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'error').length === 1, 'partial');
  const pr = byData(ctx.root, 'orphan', 'project')[0];
  assert.equal(text(pr).trim(), 'team/app-one');
  assert.equal(pr.tagName, 'SPAN');
  assert.equal(text(byData(ctx.root, 'orphan', 'project_id')[0]).trim(), '99');
  assert.ok(text(ctx.root).includes('이 도구는 삭제하지 않습니다'));
  assert.ok(text(ctx.root).includes('GitLab에서 직접'));
  assert.ok(!text(ctx.root).includes('SERVER-WORDS'));
  // the id is unknown
  const signed0 = auth.gets.length;
  const c2 = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail: { ...CREATE_PARTIAL.error.detail, project_id: null, step: 'project' } }); } });
  await toCreateConfirm(c2);
  await waitSignable(c2);
  click(signBtn(c2));
  await until(() => byData(c2.root, 'result', 'error').length === 1, 'partial 2');
  assert.match(text(byData(c2.root, 'orphan', 'project_id')[0]), /알 수 없음|확인되지/);
  // never a retry, never a "delete" action, no repeat of the apply
  assert.equal(byLabel(c2.root, '삭제').length, 0);
  assert.equal(posts(c2, '/api/v1/projects/create/apply').length, 1);
  assert.equal(auth.gets.length, signed0 + 1, 'one signature for this change');
  // the list afterwards (a re-read) shows the orphan badge from the registry
  click(byLabel(c2.root, '목록으로')[0]);
  await until(() => byData(c2.root, 'orphans').length === 1, 'orphan badge');
  const badge = text(byData(c2.root, 'orphans')[0]);
  assert.ok(badge.includes('team/lost-app') && badge.includes('team/lost-two') && badge.includes('삭제하지 않습니다'));
});

test('TestCloneRetryButtonNoSignature', async () => {
  const ctx = await mountProjects({ 'POST /api/v1/projects/create/apply': () => CREATE_CLONE_FAILED });
  await toCreateConfirm(ctx, { ...GOOD_CREATE, name: 'app-one' });
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => byLabel(ctx.root, '다시 복제').length === 1, 'retry button');
  assert.ok(text(ctx.root).includes(P.CLONE_TEXT.host_not_pinned), 'the fixed sentence of the code, not the server\'s');
  const gets = auth.gets.length;
  const btn = byLabel(ctx.root, '다시 복제')[0];
  click(btn); click(btn); click(btn);
  await until(() => posts(ctx, '/api/v1/projects/app-one/clone').length >= 1, 'clone call');
  await sleep(20);
  assert.equal(posts(ctx, '/api/v1/projects/app-one/clone').length, 1, 'double click: one call');
  assert.equal(auth.gets.length, gets, 'no signature for a clone');
  const call = posts(ctx, '/api/v1/projects/app-one/clone')[0];
  assert.deepEqual(call.body === undefined ? {} : call.body, {}, 'no body, no signature');
  assert.ok(call.opts.timeoutMs >= 130000);
  assert.equal(call.opts.noRetry429, true);
  await until(() => /복제했습니다/.test(text(ctx.root)), 'cloned');
  assert.equal(byLabel(ctx.root, '다시 복제').length, 0);
  // a failure of the retry: a fixed sentence per code, the button stays
  for (const [status, code] of [[400, 'host_not_pinned'], [502, 'clone_failed'], [504, 'clone_timeout'], [409, 'clone_in_progress'], [503, 'git_missing']]) {
    const c = await mountProjects({
      'POST /api/v1/projects/create/apply': () => CREATE_CLONE_FAILED,
      'POST /api/v1/projects/app-one/clone': () => { throw apiError(status, code, SECRET); },
    });
    await toCreateConfirm(c, { ...GOOD_CREATE, name: 'app-one' });
    await waitSignable(c);
    click(signBtn(c));
    await until(() => byLabel(c.root, '다시 복제').length === 1, 'retry button ' + code);
    click(byLabel(c.root, '다시 복제')[0]);
    await until(() => text(c.root).includes(P.CLONE_TEXT[code]), 'text of ' + code);
    assert.ok(!text(c.root).includes('SERVER-WORDS'));
    assert.equal(byLabel(c.root, '다시 복제').length, 1, code + ': the button stays');
    assert.equal(posts(c, '/api/v1/projects/app-one/clone').length, 1);
  }
  // already cloned (a folder appeared): shown as such, the list is re-read
  const c2 = await mountProjects({
    'POST /api/v1/projects/create/apply': () => CREATE_CLONE_FAILED,
    'POST /api/v1/projects/app-one/clone': () => { throw apiError(409, 'already_cloned', SECRET); },
  });
  await toCreateConfirm(c2, { ...GOOD_CREATE, name: 'app-one' });
  await waitSignable(c2);
  click(signBtn(c2));
  await until(() => byLabel(c2.root, '다시 복제').length === 1, 'retry');
  const reads = c2.calls.filter((c) => c === '/api/v1/projects/registry').length;
  click(byLabel(c2.root, '다시 복제')[0]);
  await until(() => c2.calls.filter((c) => c === '/api/v1/projects/registry').length > reads, 're-read');
  // the list row of a project that is not cloned has the same button (no signature)
  const regUncloned = { ...REG, projects: [{ ...REG.projects[0], cloned: false }] };
  const c3 = await mountProjects({ '/api/v1/projects/registry': regUncloned });
  const row = rowOf(c3, 'app-one');
  assert.equal(byLabel(row, '다시 복제').length, 1);
  const signedBefore = auth.gets.length;
  click(byLabel(row, '다시 복제')[0]);
  await until(() => posts(c3, '/api/v1/projects/app-one/clone').length === 1, 'row retry');
  assert.equal(auth.gets.length, signedBefore, 'no signature for the row button either');
});

test('TestAllNewErrorCodesFixedTexts', async () => {
  assert.ok(CODES.length >= 35);
  for (const code of CODES) {
    assert.ok(Object.prototype.hasOwnProperty.call(P.ERROR_TEXT, code), 'no fixed text for ' + code);
    const t = P.ERROR_TEXT[code];
    assert.ok(typeof t === 'string' && t.length > 8 && !t.includes('SERVER'), code);
    assert.equal(P.failureText(apiError(400, code, SECRET)), t, code + ': the text comes from the code, never from the server');
  }
  const texts = CODES.filter((c) => !['broker_error'].includes(c)).map((c) => P.ERROR_TEXT[c]);
  assert.equal(new Set(texts).size, texts.length, 'every code has its own sentence');
  // every clone code also has the clone sentence
  for (const code of ['host_not_pinned', 'clone_failed', 'clone_timeout', 'clone_unavailable', 'not_clonable', 'path_not_allowed', 'clone_in_progress', 'registry_unavailable',
    'broker_unavailable', 'git_missing', 'ssh_missing', 'already_cloned']) {
    assert.ok(typeof P.CLONE_TEXT[code] === 'string' && P.CLONE_TEXT[code].length > 8, 'clone text for ' + code);
  }
  assert.ok(typeof P.CLONE_TEXT.other === 'string');
  // an unknown code falls back to the status, still never to the server's words
  assert.ok(!P.failureText(apiError(418, 'something_new', SECRET)).includes('SERVER'));
  assert.ok(!P.failureText(apiError(502, 'something_new', SECRET)).includes('SERVER'));
  // each of the codes, shown by the real screens (provider apply and create apply)
  for (const code of ['provider_in_use', 'bad_url', 'token_ref_unknown', 'provider_unreachable', 'gitlab_refused', 'provider_exists', 'provider_unknown', 'broker_rate_limited', 'project_rate_limited']) {
    const c = await mountProviders({ 'POST /api/v1/providers/apply': () => { throw apiError(409, code, SECRET); } });
    await openProviderForm(c, GOOD_PROVIDER);
    click(byLabel(c.root, '변경 내용 확인')[0]);
    await waitSignable(c);
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'error').length === 1, code);
    assert.ok(text(c.root).includes(P.ERROR_TEXT[code]), code);
    assert.ok(!text(c.root).includes('SERVER-WORDS'), code);
  }
  for (const code of ['group_unknown', 'project_name_taken', 'deploy_key_mismatch', 'template_unknown', 'policy_class_missing', 'deploy_key_unavailable', 'gitlab_refused', 'provider_unreachable']) {
    const c = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(409, code, SECRET); } });
    await toCreateConfirm(c);
    await waitSignable(c);
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'error').length === 1, code);
    assert.ok(text(c.root).includes(P.ERROR_TEXT[code]), code);
    assert.ok(!text(c.root).includes('SERVER-WORDS'), code);
  }
  // propose-time refusals (policy class missing, deploy key gone) shown on the form
  for (const code of ['policy_class_missing', 'deploy_key_unavailable', 'bad_request']) {
    const c = await mountProjects({ 'POST /api/v1/projects/create/propose': () => { throw apiError(400, code, SECRET); } });
    await openCreateForm(c, GOOD_CREATE);
    click(byLabel(c.root, '변경 내용 확인')[0]);
    await until(() => text(byData(c.root, 'problem', 'form')[0]).includes(P.ERROR_TEXT[code]), code);
  }
});

test('TestOutcomeUnknownReloadsListsNoAutoRetry', async () => {
  const cases = {
    'create outcome_unknown': () => { throw apiError(504, 'outcome_unknown', SECRET); },
    'create network failure': () => { throw apiError(0, 'network', SECRET); },
    'create timeout': () => { throw apiError(0, 'timeout', SECRET); },
  };
  for (const [name, fn] of Object.entries(cases)) {
    const ctx = await mountProjects({ 'POST /api/v1/projects/create/apply': fn });
    await toCreateConfirm(ctx);
    await waitSignable(ctx);
    const signedBefore = auth.gets.length;
    const regBefore = ctx.calls.filter((c) => c === '/api/v1/projects/registry').length;
    const provBefore = ctx.calls.filter((c) => c === '/api/v1/providers').length;
    click(signBtn(ctx));
    await until(() => byData(ctx.root, 'result', 'error').length === 1, name);
    assert.ok(text(ctx.root).includes(P.ERROR_TEXT.outcome_unknown), name);
    assert.ok(!text(ctx.root).includes('SERVER-WORDS'), name);
    await until(() => ctx.calls.filter((c) => c === '/api/v1/projects/registry').length > regBefore, name + ': registry re-read');
    await until(() => ctx.calls.filter((c) => c === '/api/v1/providers').length > provBefore, name + ': providers re-read');
    await sleep(40);
    assert.equal(posts(ctx, '/api/v1/projects/create/apply').length, 1, name + ': never retried');
    assert.equal(auth.gets.length, signedBefore + 1, name + ': never signed again');
    assert.equal(posts(ctx, '/api/v1/projects/create/propose').length, 1, name + ': never proposed again');
    assert.equal(byLabel(ctx.root, '다시 복제').length, 0);
  }
  // a provider write
  const pc = await mountProviders({ 'POST /api/v1/providers/apply': () => { throw apiError(504, 'outcome_unknown', SECRET); } });
  await openProviderForm(pc, GOOD_PROVIDER);
  click(byLabel(pc.root, '변경 내용 확인')[0]);
  await waitSignable(pc);
  const before = pc.calls.filter((c) => c === '/api/v1/providers').length;
  const signedPc = auth.gets.length;
  click(signBtn(pc));
  await until(() => byData(pc.root, 'result', 'error').length === 1, 'provider unknown');
  assert.ok(text(pc.root).includes(P.ERROR_TEXT.outcome_unknown));
  await until(() => pc.calls.filter((c) => c === '/api/v1/providers').length > before, 'provider list re-read');
  await sleep(30);
  assert.equal(posts(pc, '/api/v1/providers/apply').length, 1);
  assert.equal(auth.gets.length, signedPc + 1);
  // a clone retry that times out: the list is read again, no second call
  const cc = await mountProjects({
    'POST /api/v1/projects/create/apply': () => CREATE_CLONE_FAILED,
    'POST /api/v1/projects/app-one/clone': () => { throw apiError(0, 'timeout', SECRET); },
  });
  await toCreateConfirm(cc, { ...GOOD_CREATE, name: 'app-one' });
  await waitSignable(cc);
  click(signBtn(cc));
  await until(() => byLabel(cc.root, '다시 복제').length === 1, 'retry');
  const regs = cc.calls.filter((c) => c === '/api/v1/projects/registry').length;
  click(byLabel(cc.root, '다시 복제')[0]);
  await until(() => cc.calls.filter((c) => c === '/api/v1/projects/registry').length > regs, 'registry re-read after the clone timeout');
  await sleep(30);
  assert.equal(posts(cc, '/api/v1/projects/app-one/clone').length, 1);
});

test('TestSubmitOnceNoDoubleSign', async () => {
  const ctx = await mountProjects();
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  let release;
  auth.gate = new Promise((r) => { release = r; });
  const btn = signBtn(ctx);
  click(btn); click(btn); click(btn);
  await until(() => auth.gets.length >= 1, 'prompt');
  await sleep(20);
  assert.equal(auth.gets.length, 1, 'one passkey request');
  release();
  await until(() => posts(ctx, '/api/v1/projects/create/apply').length >= 1, 'apply');
  await sleep(20);
  assert.equal(posts(ctx, '/api/v1/projects/create/apply').length, 1, 'one apply');
  // a double click on "변경 내용 확인": one proposal
  const c2 = await mountProjects();
  await openCreateForm(c2, GOOD_CREATE);
  const go = byLabel(c2.root, '변경 내용 확인')[0];
  click(go); click(go); click(go);
  await waitSignable(c2);
  assert.equal(posts(c2, '/api/v1/projects/create/propose').length, 1);
  // provider form and delete the same
  const c3 = await mountProviders();
  await openProviderForm(c3, GOOD_PROVIDER);
  const g3 = byLabel(c3.root, '변경 내용 확인')[0];
  click(g3); click(g3);
  await waitSignable(c3);
  assert.equal(posts(c3, '/api/v1/providers/propose').length, 1);
  const c4 = await mountProviders();
  const del = byLabel(rowOf(c4, 'gl2-admin'), '삭제')[0];
  click(del); click(del);
  await waitSignable(c4);
  assert.equal(posts(c4, '/api/v1/providers/propose').length, 1);
  // leaving while the passkey prompt is open sends nothing
  const c5 = await mountProjects();
  await toCreateConfirm(c5);
  await waitSignable(c5);
  let rel2;
  auth.gate = new Promise((r) => { rel2 = r; });
  const before = auth.gets.length;
  click(signBtn(c5));
  await until(() => auth.gets.length === before + 1, 'prompt 2');
  c5.leave();
  rel2();
  await sleep(30);
  assert.equal(posts(c5, '/api/v1/projects/create/apply').length, 0);
});

test('TestNoStorageNoNavigateTrap', async () => {
  const code = src('views/providers.js');
  const proj = src('views/projects.js');
  for (const c of [code, proj]) {
    for (const bad of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'caches.', 'history.pushState', 'history.replaceState',
      'navigator.storage', 'ctx.navigate', 'navigate(', 'location.hash', 'confirm(', 'alert(', 'innerHTML', 'Date.now', 'new Date', 'eval(', 'new Function']) {
      assert.ok(!c.includes(bad), 'uses ' + bad);
    }
    assert.ok(!/(?:^|[^.\w])(?:confirm|alert|prompt)\s*\(/.test(c));
    assert.ok(!/\.setAttribute\(/.test(c), 'attributes go through el()');
  }
  // every ctx member the views use exists in the REAL app.js makeCtx
  const app = src('app.js');
  const makeCtx = app.slice(app.indexOf('function makeCtx'), app.indexOf('async function show'));
  for (const f of [code, proj]) {
    for (const m of new Set([...f.matchAll(/\bctx\.([A-Za-z]+)/g)].map((x) => x[1]))) {
      assert.ok(new RegExp('\\b' + m + '\\b').test(makeCtx), 'ctx.' + m + ' is not in app.js makeCtx');
    }
  }
  const had = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  try {
    const ctx = await mountProjects();
    assert.equal(ctx.navigate, undefined, 'the real app context has no navigate');
    await toCreateConfirm(ctx);
    await waitSignable(ctx);
    click(signBtn(ctx));
    await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'done');
    click(byLabel(ctx.root, '목록으로')[0]);
    await until(() => byLabel(ctx.root, '새 프로젝트 만들기').length === 1, 'list again');
    assert.deepEqual(ctx.nav, [], 'the flow never writes location.hash');
    const pc = await mountProviders();
    assert.deepEqual(pc.nav, []);
  } finally {
    if (had) Object.defineProperty(globalThis, 'localStorage', had); else delete globalThis.localStorage;
    delete globalThis.sessionStorage;
  }
  // wiring through the REAL router, the app, the page and the message check
  assert.deepEqual(parseRoute('#/providers'), { name: 'providers', params: [] });
  assert.deepEqual(parseRoute('#/providers/x'), { name: 'notfound', params: [] });
  assert.equal(navItem('providers'), 'providers');
  assert.equal(typeof getView('providers'), 'function');
  assert.match(app, /VIEW_MODULES = \[[^\]]*'providers'/);
  assert.match(src('index.html'), /<a class="nav" href="#\/providers" data-nav="providers">[^]*?<span class="lb">제공자<\/span>/);
});

test('TestNoPasswordLikeMarkup', () => {
  // The W3b rule covers the new files: no password type, no CSS text masking, no credential autocomplete anywhere in static/.
  const dir = new URL('../static/', import.meta.url);
  const walk = (u) => readdirSync(u, { withFileTypes: true }).flatMap((d) => (d.isDirectory() ? walk(new URL(d.name + '/', u)) : [new URL(d.name, u)]));
  const files = walk(dir);
  assert.ok(files.some((f) => f.pathname.endsWith('views/providers.js')), 'providers.js is scanned');
  for (const f of files) {
    if (!/\.(js|css|html)$/.test(f.pathname)) continue;
    const t = readFileSync(f, 'utf8');
    assert.ok(!/text-?security/i.test(t), f.pathname + ' has text-security');
    assert.ok(!/type\s*[:=]\s*['"`]?password/i.test(t) && !/type=["']password/i.test(t) && !/\[type=["']?password/i.test(t), f.pathname + ' has a password type');
    assert.ok(!/autocomplete['"]?\s*[:=]\s*['"](?:new-password|current-password|username)/i.test(t), f.pathname + ' uses a credential autocomplete');
  }
  // the screens have no secret input at all: the text fields are plain, named by what they hold
  for (const f of ['views/providers.js']) {
    assert.ok(!/type:\s*'password'/.test(src(f)));
  }
});

test('TestPreviewSentencesAreTextNodes', async () => {
  // The server's sentences are only ever text nodes: a sentence with markup stays literal and is in no attribute.
  const evil = 'x<img src=x onerror=alert(1)> SERVER-WORDS';
  const ctx = await mountProjects({
    'POST /api/v1/projects/create/propose': async (b) => {
      const r = await proposeFor(b);
      return { ...r, preview: [...r.preview.slice(0, -1), evil + '', r.preview[r.preview.length - 1]] };
    },
  });
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  assert.ok(text(ctx.root).includes('<img src=x onerror=alert(1)>'), 'shown literally');
  assert.equal(findAll(ctx.root, (e) => e.tagName === 'IMG').length, 0);
  for (const e of findAll(ctx.root, () => true)) {
    for (const [k, v] of e.attrs) assert.ok(!/SERVER-WORDS|onerror/.test(v), 'attribute ' + k + ' carries a server sentence');
  }
  const ctx2 = await mountProviders({
    'POST /api/v1/providers/propose': async (b) => { const r = await proposeFor(b); return { ...r, preview: [...r.preview.slice(0, -1), evil, r.preview[r.preview.length - 1]] }; },
  });
  await openProviderForm(ctx2, GOOD_PROVIDER);
  click(byLabel(ctx2.root, '변경 내용 확인')[0]);
  await waitSignable(ctx2);
  for (const e of findAll(ctx2.root, () => true)) {
    for (const [k, v] of e.attrs) assert.ok(!/SERVER-WORDS|onerror/.test(v), 'attribute ' + k + ' carries a server sentence');
  }
  // the result of a failed apply: the server's message appears nowhere
  const ctx3 = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(409, 'project_name_taken', evil); } });
  await toCreateConfirm(ctx3);
  await waitSignable(ctx3);
  click(signBtn(ctx3));
  await until(() => byData(ctx3.root, 'result', 'error').length === 1, 'error');
  assert.ok(!text(ctx3.root).includes('SERVER-WORDS'));
  for (const e of findAll(ctx3.root, () => true)) for (const [, v] of e.attrs) assert.ok(!/SERVER-WORDS/.test(v));
});

test('TestListsShowClonedAndOrphanBadge', async () => {
  const hostile = { id: 'x-y', path: '/home/orch/repos/x-y', kind: 'registered', ai_context: false, provider: SECRET, gitlab: { project: SECRET, project_id: 5 }, auto_merge: { enabled: false }, cloned: false };
  const reg = {
    ...REG,
    projects: [...REG.projects, hostile, { id: 'plain', path: '/home/orch/repos/plain', kind: 'docs', ai_context: false, provider: null, gitlab: null, auto_merge: { enabled: false } }],
    orphans: [...REG.orphans, { provider: 'gl', project: SECRET, project_id: null, step: 'project', at: '2026-11-09T14:00:00Z' }, { provider: 'gl', project: 'team/ok-one', project_id: -3, step: 'project', at: 'x' }],
  };
  const ctx = await mountProjects({ '/api/v1/projects/registry': reg });
  assert.match(text(rowOf(ctx, 'app-one')), /복제됨/);
  assert.match(text(rowOf(ctx, 'x-y')), /복제 안 됨/);
  assert.equal(byLabel(rowOf(ctx, 'x-y'), '다시 복제').length, 1);
  assert.equal(byLabel(rowOf(ctx, 'app-one'), '다시 복제').length, 0, 'cloned: no button');
  assert.equal(byLabel(rowOf(ctx, 'plain'), '다시 복제').length, 0, 'a project without a provider is not clonable');
  // orphans: only well-formed rows, as text, with the notice
  const badge = byData(ctx.root, 'orphans')[0];
  assert.ok(badge, 'badge shown');
  const t = text(badge);
  assert.ok(t.includes('team/lost-app') && t.includes('team/lost-two'));
  assert.ok(!t.includes('SERVER-WORDS') && !t.includes('team/ok-one'), 'malformed orphan rows are dropped');
  assert.ok(t.includes('삭제하지 않습니다'));
  for (const e of findAll(ctx.root, () => true)) for (const [, v] of e.attrs) assert.ok(!/SERVER-WORDS|onerror/.test(v));
  // the old screens keep working with a registry that has no cloned/orphans/providers
  const old = await mountProjects({ '/api/v1/projects/registry': { projects: [{ id: 'docs', path: '/a', kind: 'docs', ai_context: false, gitlab: null, auto_merge: { enabled: false } }], auto_merge_global: {} } });
  assert.equal(byData(old.root, 'orphans').length, 0);
});

test('TestNoRetry429AndTimeouts', async () => {
  const ctx = await mountProjects();
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => posts(ctx, '/api/v1/projects/create/apply').length === 1, 'apply');
  for (const path of ['/api/v1/projects/create/propose', '/api/v1/projects/create/apply']) {
    assert.equal(posts(ctx, path)[0].opts.noRetry429, true, path);
  }
  assert.ok(posts(ctx, '/api/v1/projects/create/apply')[0].opts.timeoutMs >= 130000, 'longer than the 90 s the server may take');
  assert.equal(P.CREATE_APPLY_TIMEOUT_MS, posts(ctx, '/api/v1/projects/create/apply')[0].opts.timeoutMs);
  // the group list is a call to GitLab: no wait on 429, a long timeout
  const gl = ctx.calls.indexOf('/api/v1/providers/gl/groups');
  assert.ok(gl >= 0);
  // a 429 on the group list: fixed text, not repeated
  const c429 = await mountProjects({ '/api/v1/providers/gl/groups': () => { throw apiError(429, 'project_rate_limited', SECRET); } });
  click(byLabel(c429.root, '새 프로젝트 만들기')[0]);
  await until(() => text(c429.root).includes(P.ERROR_TEXT.project_rate_limited), 'group 429');
  assert.equal(c429.calls.filter((c) => c === '/api/v1/providers/gl/groups').length, 1);
  // the real api layer honours noRetry429 for these calls
  const api = await import('../static/api.js');
  let n = 0;
  let slept = 0;
  api.configure({
    fetch: async () => { n++; return new Response('{"error":{"code":"project_rate_limited","message":"x"}}', { status: 429, headers: { 'Retry-After': '3' } }); },
    sleep: async () => { slept++; },
  });
  try {
    await assert.rejects(() => api.post('/api/v1/projects/create/apply', {}, { noRetry429: true }), (e) => e.status === 429);
    assert.equal(n, 1);
    assert.equal(slept, 0);
    // and an error body with a detail only arrives on create_partial, validated by the view
    api.configure({ fetch: async () => new Response(JSON.stringify({ error: { code: 'create_partial', message: 'm', detail: { project: 'a/b' }, secret: 'x' } }), { status: 502 }) });
    await assert.rejects(() => api.post('/api/v1/projects/create/apply', {}), (e) => e.code === 'create_partial' && e.detail && e.detail.project === 'a/b' && e.secret === undefined);
    api.configure({ fetch: async () => new Response(JSON.stringify({ error: { code: 'bad_request', message: 'm', detail: { project: 'a/b' } } }), { status: 400 }) });
    await assert.rejects(() => api.post('/api/v1/projects/create/apply', {}), (e) => e.code === 'bad_request' && e.detail === undefined);
  } finally {
    api.configure();
  }
});

test('TestNoClockAndConstants', () => {
  const code = src('views/providers.js');
  for (const bad of ['Date.now', 'new Date', 'Date(', 'performance.now']) assert.ok(!code.includes(bad), bad);
  assert.equal(P.NAME_RE.test('ab'), true);
  assert.equal(P.NAME_RE.test('a'), false);
  assert.equal(P.PROVIDER_ID_RE.test('a'), true);
  assert.deepEqual(P.TEMPLATES, ['basic', 'go']);
  assert.deepEqual(P.PUBLISH_MODES, ['write', 'read']);
});

// ---- fix round 1 (C5 review) ----------------------------------------------------

const AI_ON_LINE = '[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다';
const AI_OFF_LINE = '이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)';

test('TestDescriptionRefusesWhatFindHostileRefuses', async () => {
  const { findHostile } = await import('../static/render.js');
  for (const ch of ['️', 'ㅤ', 'ᅟ', 'ᅠ', '͏', '᠋', '᠌', '᠍', '︀']) {
    assert.notEqual(findHostile('a' + ch + 'b'), null, 'premise: findHostile refuses U+' + ch.codePointAt(0).toString(16));
    assert.equal(P.validDescription('a' + ch + 'b'), false, 'validDescription refuses U+' + ch.codePointAt(0).toString(16));
  }
  assert.equal(P.validDescription('한글 😀 é'), true);
  // the form refuses one before any proposal
  const ctx = await mountProjects();
  await openCreateForm(ctx, { ...GOOD_CREATE, desc: 'check ❤️' });
  click(byLabel(ctx.root, '변경 내용 확인')[0]);
  await sleep(10);
  assert.match(text(byData(ctx.root, 'problem', 'form')[0]), /설명/);
  assert.equal(posts(ctx, '/api/v1/projects/create/propose').length, 0);
  assert.throws(() => P.canonicalChange('create_project', 'ab', { ...VEC.change.find((c) => c.name === 'create_project plain').fields, description: 'x️' }), { name: 'ChangeError' });
});

test('TestPreviewGateCountsOnlyFixedSentenceLines', async () => {
  const descs = ['읽기: 문서', '쓰기: x', AI_ON_LINE, AI_OFF_LINE, '게시 권한 — 쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다', '전송될 수 있게 허용합니다 AI 서비스로 전송되지 않습니다'];
  for (const desc of descs) {
    for (const [publish, ai] of [['write', false], ['read', true]]) {
      const ctx = await mountProjects();
      await toCreateConfirm(ctx, { ...GOOD_CREATE, desc, publish, ai });
      await waitSignable(ctx);
      assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 0, desc.slice(0, 20) + ' ' + publish);
    }
  }
  // the gate still catches a server that lies about the AI switch or the publish right (exact lines)
  for (const [name, fn] of Object.entries({
    'ai line says on while off': async (b) => { const r = await proposeFor(b); return { ...r, preview: r.preview.map((x) => (x === AI_OFF_LINE ? AI_ON_LINE : x)) }; },
    'publish line says read while write': async (b) => { const r = await proposeFor(b); return { ...r, preview: r.preview.map((x) => x.replace('게시 권한 — 쓰기:', '게시 권한 — 읽기:')) }; },
    'two ai lines': async (b) => { const r = await proposeFor(b); return { ...r, preview: [...r.preview, AI_OFF_LINE] }; },
  })) {
    const c = await mountProjects({ 'POST /api/v1/projects/create/propose': fn });
    await toCreateConfirm(c);
    assert.equal(byData(c.root, 'warn', 'mismatch').length, 1, name);
  }
});

test('TestUnknownOutcomeForStatusZeroAndGatewayAndNoCode5xx', async () => {
  const cases = [[0, 'network'], [0, 'timeout'], [500, 'http_500'], [502, 'http_502'], [503, 'http_503'], [504, 'http_504'], [502, 'bad_gateway'], [500, 'internal']];
  for (const [status, code] of cases) {
    const err = () => { throw apiError(status, code, SECRET); };
    // create
    const c = await mountProjects({ 'POST /api/v1/projects/create/apply': err });
    await toCreateConfirm(c);
    await waitSignable(c);
    const reg = c.calls.filter((x) => x === '/api/v1/projects/registry').length;
    const prov = c.calls.filter((x) => x === '/api/v1/providers').length;
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'error').length === 1, 'create ' + status + code);
    assert.ok(text(c.root).includes(P.ERROR_TEXT.outcome_unknown), status + code);
    assert.ok(!text(c.root).includes('적용되지 않았습니다'), 'no "not applied" claim: ' + status + code);
    await until(() => c.calls.filter((x) => x === '/api/v1/projects/registry').length > reg && c.calls.filter((x) => x === '/api/v1/providers').length > prov, 'lists re-read ' + status + code);
    assert.equal(posts(c, '/api/v1/projects/create/apply').length, 1);
    // provider write
    const p = await mountProviders({ 'POST /api/v1/providers/apply': err });
    await openProviderForm(p, GOOD_PROVIDER);
    click(byLabel(p.root, '변경 내용 확인')[0]);
    await waitSignable(p);
    const pn = p.calls.filter((x) => x === '/api/v1/providers').length;
    click(signBtn(p));
    await until(() => byData(p.root, 'result', 'error').length === 1, 'provider ' + status + code);
    assert.ok(text(p.root).includes(P.ERROR_TEXT.outcome_unknown) && !text(p.root).includes('적용되지 않았습니다'), status + code);
    await until(() => p.calls.filter((x) => x === '/api/v1/providers').length > pn, 'providers re-read');
    // group write
    const g = await mountProjects({ 'POST /api/v1/providers/apply': err });
    await openCreateForm(g);
    click(byLabel(g.root, '새 그룹 만들기')[0]);
    await until(() => byId(g.root, 'cg-name'), 'group form');
    byId(g.root, 'cg-name').value = 'grp-x';
    fire(byId(g.root, 'cg-name'), 'input');
    click(byLabel(g.root, '변경 내용 확인')[0]);
    await waitSignable(g);
    const gn = g.calls.filter((x) => x === '/api/v1/providers').length;
    click(signBtn(g));
    await until(() => byData(g.root, 'result', 'error').length === 1, 'group ' + status + code);
    assert.ok(text(g.root).includes(P.ERROR_TEXT.outcome_unknown) && !text(g.root).includes('적용되지 않았습니다'), status + code);
    await until(() => g.calls.filter((x) => x === '/api/v1/providers').length > gn, 'group: providers re-read');
  }
  // a code in the table keeps its own sentence (known refusal), an unknown 4xx claims nothing
  assert.equal(P.failureText(apiError(503, 'broker_unavailable', SECRET)), P.ERROR_TEXT.broker_unavailable);
  for (const st of [400, 403, 404, 409, 418]) {
    assert.ok(!P.failureText(apiError(st, 'something_new', SECRET)).includes('적용되지 않았습니다'), 'unknown ' + st);
  }
  assert.equal(P.isUnknown(apiError(502, 'http_502')), true);
  assert.equal(P.isUnknown(apiError(503, 'broker_unavailable')), false);
  assert.equal(P.isUnknown(apiError(409, 'project_name_taken')), false);
});

test('TestGroupListFailureRetryButtonAndDebouncedProviderSelect', async () => {
  let n = 0;
  const ctx = await mountProjects({ '/api/v1/providers/gl/groups': () => { n++; if (n === 1) throw apiError(503, 'broker_unavailable', SECRET); return GROUPS; } });
  click(byLabel(ctx.root, '새 프로젝트 만들기')[0]);
  await until(() => byId(ctx.root, 'cp-name') && text(ctx.root).includes(P.ERROR_TEXT.broker_unavailable), 'group error');
  assert.ok(!text(ctx.root).includes('SERVER-WORDS'));
  assert.equal(byLabel(ctx.root, '다시 불러오기').length, 1);
  const retry = byLabel(ctx.root, '다시 불러오기')[0];
  click(retry); click(retry);
  await until(() => byTag(byId(ctx.root, 'cp-group'), 'option').length > 1, 'groups after retry');
  assert.equal(n, 2, 'one more call (a double click is one call)');
  assert.equal(byLabel(ctx.root, '다시 불러오기').length, 0);
  // provider select: arrow keys fire many change events: one call after the select settles
  const sel = byId(ctx.root, 'cp-provider');
  const before = ctx.calls.filter((c) => c === '/api/v1/providers/gl2/groups').length;
  for (let i = 0; i < 4; i++) { sel.value = i % 2 ? 'gl' : 'gl2'; fire(sel, 'change'); }
  sel.value = 'gl2';
  fire(sel, 'change');
  assert.equal(ctx.calls.filter((c) => c === '/api/v1/providers/gl2/groups').length, before, 'no call per change event');
  await sleep(P.GROUP_SETTLE_MS + 150);
  assert.equal(ctx.calls.filter((c) => c === '/api/v1/providers/gl2/groups').length, before + 1, 'one call after the settle');
  // blur commits at once
  const gl = ctx.calls.filter((c) => c === '/api/v1/providers/gl/groups').length;
  sel.value = 'gl';
  fire(sel, 'change');
  fire(sel, 'blur');
  await sleep(30);
  assert.equal(ctx.calls.filter((c) => c === '/api/v1/providers/gl/groups').length, gl + 1, 'blur loads now');
  await sleep(P.GROUP_SETTLE_MS + 150);
  assert.equal(ctx.calls.filter((c) => c === '/api/v1/providers/gl/groups').length, gl + 1, 'and the pending timer does not call again');
});

test('TestBusyHeldUntilProposeAndSignerSettle', async () => {
  const seen = [];
  const ctx = ctxFor();
  const flow = P.makeFlow(ctx, { onChange: () => { if (flow.phase === 'confirm' && !flow.signer && flow.problems.length === 0) seen.push(flow.busy); }, onResult: async () => {} });
  const sent = { action: 'delete_provider', subject: 'gl2', fields: {} };
  const first = flow.propose(sent);
  const second = flow.propose(sent); // a second call while the first is out
  await Promise.all([first, second]);
  await until(() => !!flow.signer || !!flow.signerNote, 'signer settled');
  assert.equal(posts(ctx, '/api/v1/providers/propose').length, 1, 'one propose');
  assert.ok(seen.length > 0 && seen.every((b) => b === true), 'busy stays set while the signer is resolved: ' + seen);
  assert.equal(flow.busy, false, 'and is released after');
});

test('TestOrphanNoticesDistinguishUnrecordedAndInvalidDetail', async () => {
  const run = async (detail) => {
    const c = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail }); } });
    await toCreateConfirm(c);
    await waitSignable(c);
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'error').length === 1, 'partial');
    return text(c.root);
  };
  const d = CREATE_PARTIAL.error.detail;
  const NOT_LISTED = '이 목록에 남지 않았습니다. 아래 경로를 직접 기록해 두세요.';
  assert.ok(!(await run(d)).includes(NOT_LISTED), 'recorded: no such notice');
  assert.ok((await run({ ...d, orphan_recorded: false })).includes(NOT_LISTED));
  assert.ok((await run({ ...d, orphan_dropped: true })).includes(NOT_LISTED));
  const invalid = await run({ ...d, project: 'Bad Path' });
  assert.ok(invalid.includes('위치 정보를 확인할 수 없'), 'invalid detail says so');
  assert.ok(!invalid.includes('GitLab이 프로젝트 ID를 확인해 주지 않았습니다'));
  const unconfirmed = await run({ ...d, project_id: null, step: 'project' });
  assert.ok(unconfirmed.includes('GitLab이 프로젝트 ID를 확인해 주지 않았습니다'), 'GitLab did not confirm an id');
  assert.ok(!unconfirmed.includes('위치 정보를 확인할 수 없'));
});

test('TestConfirmationsShowProviderAddressKindAndKeyHint', async () => {
  const ctx = await mountProjects();
  await toCreateConfirm(ctx);
  await waitSignable(ctx);
  const f = facts(ctx);
  assert.ok(f.provider_url.includes('http://gitlab.example:18082') && /목록|이 화면/.test(f.provider_url) === false || f.provider_url.includes('gitlab.example'), f.provider_url);
  assert.match(text(byData(ctx.root, 'field', 'provider_url')[0]), /제공자 목록 기준/);
  assert.ok(text(ctx.root).includes('ssh-keygen -l -f'), 'hint to compare on the server');
  assert.ok(f.key_comment.includes('orchd@deploy'), 'the .pub comment tail');
  assert.ok(f.key_prefix.length > 60, 'a longer prefix');
  // group confirmation
  const g = await mountProjects();
  await openCreateForm(g);
  click(byLabel(g.root, '새 그룹 만들기')[0]);
  await until(() => byId(g.root, 'cg-name'), 'group form');
  byId(g.root, 'cg-name').value = 'grp-y';
  fire(byId(g.root, 'cg-name'), 'input');
  click(byLabel(g.root, '변경 내용 확인')[0]);
  await waitSignable(g);
  assert.ok(facts(g).provider_url.includes('gitlab.example:18082'));
  // set_provider shows the kind; delete shows the id and says the address is from the list
  const p = await mountProviders();
  await openProviderForm(p, GOOD_PROVIDER);
  click(byLabel(p.root, '변경 내용 확인')[0]);
  await waitSignable(p);
  assert.ok(facts(p).kind.includes('gitlab'));
  const d = await mountProviders();
  click(byLabel(rowOf(d, 'gl2-admin'), '삭제')[0]);
  await waitSignable(d);
  assert.ok(facts(d).id.includes('gl2') && /목록 기준/.test(facts(d).base_url));
});

test('TestNavIconOfProvidersIsDistinctFromServerStatus', () => {
  const html = src('index.html');
  const icon = (nav) => (new RegExp('data-nav="' + nav + '"><svg[^>]*>([^]*?)</svg>').exec(html) || [])[1];
  const a = icon('providers');
  const b = icon('ops');
  assert.ok(a && b && a !== b);
  assert.ok(!a.includes('<rect'), 'not the two stacked boxes of 서버 상태');
});

// Final review I1: the automatic clone found the folder already there. The
// project exists but this server may hold something else under that name: the
// result is "needs checking", not a success line, and no retry button.
test('TestCreateAlreadyClonedNeedsChecking', async () => {
  const ALREADY = { project: CREATE_CLONE_FAILED.project, clone: { status: 'already_cloned', code: 'already_cloned' } };
  const c = await mountProjects({ 'POST /api/v1/projects/create/apply': () => ALREADY });
  await toCreateConfirm(c, { ...GOOD_CREATE, name: 'app-one' });
  await waitSignable(c);
  click(signBtn(c));
  await until(() => byData(c.root, 'clone', 'check').length === 1, 'check state');
  const t = text(c.root);
  assert.ok(t.includes(P.CLONE_TEXT.check), 'the fixed "needs checking" sentence');
  assert.ok(!t.includes('이 서버에 복제했습니다'), 'not shown as a success');
  assert.equal(byLabel(c.root, '다시 복제').length, 0, 'a retry would only find the folder again');
  assert.ok(P.ERROR_TEXT.path_exists.includes('변경은 적용되지 않았습니다'));
  assert.ok(CODES.includes('path_exists'));
});
