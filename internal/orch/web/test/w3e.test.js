import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { createHash, webcrypto } from 'node:crypto';
import { findAll } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

// W3e (Task E5): the GitHub provider kind, the owner select, the visibility choice,
// the per-repository deploy key, "기존 저장소 연결" (link_project) and "원격 연결"
// (attach_remote). The golden facts come from Go (fixtures/w3e-vectors.json and the
// other fixtures written by TestWebFixturesW3e*); the helpers are copies of the W3c
// ones (providers.test.js) on purpose: that file stays untouched.

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const src = (n) => readFileSync(new URL('../static/' + n, import.meta.url), 'utf8');
const wa = await import('../static/webauthn.js');
const P = await import('../static/views/providers.js');
const PR = await import('../static/views/projects.js');
const { findHostile } = await import('../static/render.js');

const VEC = fx('w3e-vectors.json');
const W3C = fx('w3c-vectors.json');
const CODES = fx('w3c-error-codes.json').codes;
const PROVIDERS_E = fx('providers-w3e.json');
const OWNERS = fx('provider-owners-github.json');
const GROUPS = fx('provider-groups.json');
const REG_E = fx('registry-w3e.json');
const CREATE_GH_OK = fx('project-create-github-ok.json');
const LINK_OK = fx('project-link-ok.json');
const ATTACH_OK = fx('project-attach-ok.json');
const ATTACH_KEY_FAILED = fx('project-attach-key-failed.json');
const LINK_PARTIAL = fx('project-link-partial.json');
const CREATE_GH_PARTIAL = fx('project-create-github-partial.json');
const CLONE_OK = fx('project-clone-ok.json');
const TOKEN_LIST = fx('tokens-list.json');
const KEY = VEC.deploy_keys[0];
const KEY2 = VEC.deploy_keys[1];
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
  constructor() { this.gets = []; }
  async get(opts) {
    this.gets.push(opts);
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

const TOKENS = {
  warn_days: 14,
  tokens: [
    ...TOKEN_LIST.tokens,
    { ref: 'gl-admin', kind: 'gitlab', last4: 'Ab12', expires: '2027-03-01', set_at: '2026-10-01T09:00:00Z', source: 'store', days_left: 111, usable: true },
    { ref: 'github-main', kind: 'github', last4: 'Gh77', expires: '2027-02-01', set_at: '2026-10-01T09:00:00Z', source: 'store', days_left: 80, usable: true },
  ],
};

// The JS copy of the server's fixed sentences for the W3e proposals (createPreview with
// visibility, remotePreview, providerPreview for github). TestChangeVectorsMatchGoW3e pins it to Go.
const STAMP = '서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다';
const AI_ON = '[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다';
const AI_OFF = '이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)';
const VIS_PUBLIC = '[중요] 공개 저장소로 만듭니다: 인터넷의 누구나 이 저장소의 코드를 읽을 수 있습니다';
const VIS_PRIVATE = '비공개 저장소로 만듭니다: 초대받은 사람만 볼 수 있습니다';
const KEY_WARN = '[중요] 이미 있는 저장소의 설정에 이 서버의 배포 키가 등록됩니다. 이 도구는 저장소를 만들거나 지우거나 다른 설정을 바꾸지 않으며, 등록한 키도 지우지 않습니다';
const kindOfProvider = (id) => (id === 'gh' ? 'github' : 'gitlab');
function previewFor(action, subject, f, fingerprint) {
  const str = (k) => f[k];
  const pub = f.publish === 'read' ? '읽기: 배포 키는 읽기 전용으로 등록합니다(이 서버는 브랜치를 올리지 못합니다)' : '쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다';
  if (action === 'set_provider') {
    if (f.kind === 'github') {
      return ["제공자 '" + subject + "'를 저장합니다(종류: github). 같은 id가 있으면 이 값으로 바뀝니다", 'GitHub API 주소: ' + f.base_url, 'ssh 주소: ' + f.ssh_url,
        '사용할 토큰 이름: ' + f.token_ref + ' (토큰 값은 이 화면에 나오지 않습니다)', '[중요] 이 토큰의 권한으로 위 주소의 GitHub에 저장소를 만들고 저장소별 배포 키를 등록합니다',
        '이 제공자를 쓰는 프로젝트가 있으면 주소는 바꿀 수 없습니다', STAMP];
    }
    return ["제공자 '" + subject + "'를 저장합니다(종류: " + f.kind + '). 같은 id가 있으면 이 값으로 바뀝니다', 'GitLab 주소: ' + f.base_url, 'ssh 주소: ' + f.ssh_url,
      '사용할 토큰 이름: ' + f.token_ref + ' (토큰 값은 이 화면에 나오지 않습니다)', '[중요] 이 토큰의 권한으로 위 주소의 GitLab에 그룹과 프로젝트를 만듭니다',
      '이 제공자를 쓰는 프로젝트가 있으면 주소는 바꿀 수 없습니다', STAMP];
  }
  if (action === 'delete_provider') {
    return ["제공자 '" + subject + "'를 삭제합니다", '이 제공자를 쓰는 프로젝트가 있으면 삭제되지 않습니다', 'GitLab의 그룹과 프로젝트는 지워지지 않습니다', STAMP];
  }
  const keyLine = kindOfProvider(f.provider) === 'github'
    ? '저장소 전용 배포 키 지문(SHA-256): ' + fingerprint + ' (이 저장소 하나를 위해 지금 새로 만든 키입니다)'
    : '배포 키 지문(SHA-256): ' + fingerprint + ' (이 서버의 공용 배포 키 공개 키 파일 기준)';
  if (action === 'attach_remote') {
    return ["등록된 프로젝트 '" + subject + "'에 제공자 '" + f.provider + "'의 저장소 '" + f.remote_path + "'를 연결합니다",
      '연결한 뒤 이 프로젝트의 게시(push)와 복제는 이 원격을 씁니다. 프로젝트의 파일과 AI 전송 설정은 바뀌지 않습니다', '게시 권한 — ' + pub, keyLine, KEY_WARN, STAMP];
  }
  if (action === 'link_project') {
    return ["제공자 '" + f.provider + "'에 이미 있는 저장소 '" + f.remote_path + "'를 새 프로젝트 '" + subject + "'로 가져옵니다", f.ai_context ? AI_ON : AI_OFF, '게시 권한 — ' + pub, keyLine, KEY_WARN,
      '등록 후 정책 종류(kind)는 registered 입니다', '가져온 뒤 이 서버에 자동으로 한 번 복제합니다', STAMP];
  }
  const desc = f.description === '' ? '(없음)' : f.description;
  if (has(f, 'visibility')) {
    return ["GitHub 제공자 '" + f.provider + "'의 소유자 '" + str('group_path') + "'에 새 저장소 '" + subject + "'를 만듭니다", f.visibility === 'public' ? VIS_PUBLIC : VIS_PRIVATE,
      '템플릿: ' + f.template + ' (코드에 고정된 파일들이 첫 커밋으로 들어갑니다)', '설명: ' + desc, f.ai_context ? AI_ON : AI_OFF, '게시 권한 — ' + pub, keyLine,
      '등록 후 정책 종류(kind)는 registered 입니다',
      '브랜치 보호는 공개 저장소이거나 유료 요금제일 때만 켜집니다(안 되면 일부만 만들어진 것으로 알려 줍니다). 만든 뒤 이 도구는 저장소를 삭제하지 않습니다',
      '만든 뒤 이 서버에 자동으로 한 번 복제합니다', STAMP];
  }
  return ["GitLab 제공자 '" + f.provider + "'의 그룹 '" + f.group_path + "'에 새 프로젝트 '" + subject + "'를 만듭니다",
    '템플릿: ' + f.template + ' (코드에 고정된 파일들이 첫 커밋으로 들어갑니다)', '설명: ' + desc, f.ai_context ? AI_ON : AI_OFF, '게시 권한 — ' + pub, keyLine,
    '등록 후 정책 종류(kind)는 registered 입니다', 'GitLab 프로젝트는 비공개로 만들어지고, 만든 뒤 이 도구는 삭제하지 않습니다', '만든 뒤 이 서버에 자동으로 한 번 복제합니다', STAMP];
}
const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);

let seq = 0;
async function proposeFor(body, over) {
  let fields = body.fields;
  let extra = {};
  if (['create_project', 'link_project', 'attach_remote'].includes(body.action)) {
    fields = { ...body.fields, deploy_key_sha256: KEY.line_sha256 };
    extra = { deploy_key: { public_key: KEY.line, fingerprint: KEY.fingerprint } };
  }
  const change = await P.computeChange(body.action, body.subject, fields);
  seq++;
  const request = seq.toString(16).padStart(32, '0');
  const issued = '2026-11-12T03:00:00Z';
  return {
    action: body.action, subject: body.subject, fields,
    message: 'aigate-registry-v1\naction: ' + body.action + '\nsubject: ' + body.subject + '\nchange: ' + change + '\nrequest: ' + request + '\nissued: ' + issued + '\n',
    change, request, issued, preview: previewFor(body.action, body.subject, fields, KEY.fingerprint), ...extra, ...(over || {}),
  };
}
// retarget rewrites a proposal so that it is consistent again (change and message follow the new fields)
async function retarget(b, mutate) {
  const r = await proposeFor(b);
  const fields = mutate({ ...r.fields });
  const change = await P.computeChange(b.action, b.subject, fields);
  return { ...r, fields, change, message: r.message.replace(r.change, change), preview: previewFor(b.action, b.subject, fields, KEY.fingerprint) };
}

function routesFor(over) {
  const wire = [];
  const rec = (name, fn) => (body, opts) => {
    wire.push({ name, json: JSON.stringify(body) });
    return fn(body, opts);
  };
  const routes = {
    '/api/v1/providers': PROVIDERS_E,
    '/api/v1/providers/gl/groups': GROUPS,
    '/api/v1/providers/gh/groups': OWNERS,
    '/api/v1/settings/tokens': TOKENS,
    '/api/v1/auth/signers': SIGNERS,
    '/api/v1/projects/registry': REG_E,
    'POST /api/v1/providers/propose': rec('pp', (b) => proposeFor(b)),
    'POST /api/v1/providers/apply': rec('pa', (b) => ({ provider: { id: b.subject, ...b.fields } })),
    'POST /api/v1/projects/create/propose': rec('cp', (b) => proposeFor(b)),
    'POST /api/v1/projects/create/apply': rec('ca', (b) => ({ ...CREATE_GH_OK, project: { ...CREATE_GH_OK.project, id: b.subject } })),
    'POST /api/v1/projects/link/propose': rec('lp', (b) => proposeFor(b)),
    'POST /api/v1/projects/link/apply': rec('la', (b) => ({ ...LINK_OK, project: { ...LINK_OK.project, id: b.subject } })),
    'POST /api/v1/projects/attach/propose': rec('ap', (b) => proposeFor(b)),
    'POST /api/v1/projects/attach/apply': rec('aa', () => ATTACH_OK),
    'POST /api/v1/projects/hello-world/clone': rec('clone', () => CLONE_OK),
    ...(over || {}),
  };
  return { routes, wire };
}
function ctxFor(over) {
  const { routes, wire } = routesFor(over);
  const ctx = fakeCtx(routes, { session: SESSION });
  ctx.wire = wire;
  return ctx;
}
const posts = (ctx, path) => ctx.posts.filter((p) => p.path === path);
const signBtn = (ctx) => byLabel(ctx.root, '패스키로 서명')[0];
const waitSignable = (ctx) => until(() => !!signBtn(ctx), 'sign button');
const facts = (ctx) => Object.fromEntries(byData(ctx.root, 'field').map((e) => [e.getAttribute('data-field'), text(e)]));
const rowOf = (ctx, needle) => findAll(ctx.root, (e) => e.tagName === 'TR' && text(e).includes(needle))[0];
const bodyOf = (ctx, path) => JSON.parse(JSON.stringify(posts(ctx, path)[0].body));

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

// provider form
async function openProviderForm(ctx) {
  click(byLabel(ctx.root, '제공자 추가')[0]);
  await until(() => byId(ctx.root, 'pv-id'), 'provider form');
}
const setKind = (ctx, kind) => { const k = byId(ctx.root, 'pv-kind'); k.value = kind; fire(k, 'change'); };
const optionValues = (sel) => byTag(sel, 'option').map((o) => o.getAttribute('value'));
function fillProvider(ctx, o) {
  if (o.id !== undefined) byId(ctx.root, 'pv-id').value = o.id;
  if (o.base !== undefined) byId(ctx.root, 'pv-base').value = o.base;
  if (o.ssh !== undefined) byId(ctx.root, 'pv-ssh').value = o.ssh;
  if (o.token !== undefined) byId(ctx.root, 'pv-token').value = o.token;
}
const problemText = (ctx, what) => text(byData(ctx.root, 'problem', what || 'form')[0]);

// create form
async function openCreateForm(ctx) {
  click(byLabel(ctx.root, '새 프로젝트 만들기')[0]);
  await until(() => byId(ctx.root, 'cp-name'), 'create form');
  await until(() => byTag(byId(ctx.root, 'cp-group'), 'option').length > 1, 'groups');
}
async function pickProvider(ctx, id) {
  const sel = byId(ctx.root, 'cp-provider');
  sel.value = id;
  fire(sel, 'change');
  fire(sel, 'blur');
  await until(() => ctx.calls.includes('/api/v1/providers/' + id + '/groups'), 'groups call of ' + id);
  await until(() => byTag(byId(ctx.root, 'cp-group'), 'option').length > 1 || byTag(byId(ctx.root, 'cp-group'), 'option')[0].textContent.includes('없습니다'), 'groups of ' + id);
}
function fillCreate(ctx, o) {
  const set = (id, v) => { byId(ctx.root, id).value = v; fire(byId(ctx.root, id), 'change'); };
  if (o.group !== undefined) set('cp-group', o.group);
  if (o.name !== undefined) byId(ctx.root, 'cp-name').value = o.name;
  if (o.desc !== undefined) byId(ctx.root, 'cp-desc').value = o.desc;
  if (o.template !== undefined) set('cp-template', o.template);
  if (o.publish !== undefined) set('cp-publish', o.publish);
  if (o.visibility !== undefined) set('cp-visibility', o.visibility);
  if (o.ai !== undefined) byId(ctx.root, 'cp-ai').checked = o.ai;
}
const GH_CREATE = { group: 'SampleUser', name: 'hello-gh', desc: '', template: 'go', publish: 'write', ai: false, visibility: 'private' };
const GL_CREATE = { group: 'team/sub', name: 'app-two', desc: '두 번째', template: 'go', publish: 'write', ai: false };
const proposeBtn = (ctx) => byLabel(ctx.root, '변경 내용 확인')[0];
async function toCreateConfirm(ctx, provider, vals) {
  await openCreateForm(ctx);
  if (provider !== 'gl') await pickProvider(ctx, provider);
  fillCreate(ctx, vals);
  click(proposeBtn(ctx));
  await until(() => byData(ctx.root, 'warn', 'mismatch').length > 0 || !!signBtn(ctx) || byData(ctx.root, 'problem', 'form').some((e) => text(e) !== ''), 'confirm');
}

// link / attach forms
async function openLinkForm(ctx) {
  click(byLabel(ctx.root, '기존 저장소 연결')[0]);
  await until(() => byId(ctx.root, 'lk-remote'), 'link form');
}
function fillRemote(ctx, o) {
  const set = (id, v) => { byId(ctx.root, id).value = v; fire(byId(ctx.root, id), 'change'); };
  if (o.provider !== undefined) { set('lk-provider', o.provider); fire(byId(ctx.root, 'lk-provider'), 'blur'); }
  if (o.remote !== undefined) { byId(ctx.root, 'lk-remote').value = o.remote; fire(byId(ctx.root, 'lk-remote'), 'input'); }
  if (o.publish !== undefined) set('lk-publish', o.publish);
  if (o.ai !== undefined && byId(ctx.root, 'lk-ai')) byId(ctx.root, 'lk-ai').checked = o.ai;
}
async function toLinkConfirm(ctx, vals) {
  await openLinkForm(ctx);
  fillRemote(ctx, vals);
  click(proposeBtn(ctx));
  await until(() => byData(ctx.root, 'warn', 'mismatch').length > 0 || !!signBtn(ctx) || byData(ctx.root, 'problem', 'form').some((e) => text(e) !== ''), 'link confirm');
}
async function openAttachForm(ctx, id) {
  const row = rowOf(ctx, id);
  click(byLabel(row, '원격 연결')[0]);
  await until(() => byId(ctx.root, 'lk-remote'), 'attach form');
}
async function toAttachConfirm(ctx, id, vals) {
  await openAttachForm(ctx, id);
  fillRemote(ctx, vals);
  click(proposeBtn(ctx));
  await until(() => byData(ctx.root, 'warn', 'mismatch').length > 0 || !!signBtn(ctx) || byData(ctx.root, 'problem', 'form').some((e) => text(e) !== ''), 'attach confirm');
}

// ---- the tests named in the brief ------------------------------------------------

test('TestProviderKindGitHubFormDefaults', async () => {
  const ctx = await mountProviders();
  await openProviderForm(ctx);
  const kind = byId(ctx.root, 'pv-kind');
  assert.deepEqual(optionValues(kind), ['gitlab', 'github']);
  assert.equal(kind.value, 'gitlab', 'the form starts as the W3c form did');
  assert.equal(byId(ctx.root, 'pv-base').value, '');
  assert.deepEqual(optionValues(byId(ctx.root, 'pv-token')).sort(), ['gitlab-docs', 'gitlab-main', 'gl-admin'], 'only usable gitlab tokens');
  // choosing github fills the github defaults and lists only usable github tokens (never values)
  setKind(ctx, 'github');
  assert.equal(P.GITHUB_API_URL, 'https://api.github.com');
  assert.equal(P.GITHUB_SSH_URL, 'ssh://git@github.com');
  assert.equal(byId(ctx.root, 'pv-base').value, 'https://api.github.com');
  assert.equal(byId(ctx.root, 'pv-ssh').value, 'ssh://git@github.com');
  const toks = optionValues(byId(ctx.root, 'pv-token')).sort();
  assert.deepEqual(toks, ['github-main', 'github-old'], 'usable github tokens only: not github-lost (unusable), not gitlab, not openai');
  assert.ok(!text(byId(ctx.root, 'pv-token')).includes('Gh77'), 'no token value or tail in the select');
  // back to gitlab: the github defaults are not left behind
  setKind(ctx, 'gitlab');
  assert.equal(byId(ctx.root, 'pv-base').value, '');
  assert.equal(byId(ctx.root, 'pv-ssh').value, '');
  assert.deepEqual(optionValues(byId(ctx.root, 'pv-token')).sort(), ['gitlab-docs', 'gitlab-main', 'gl-admin']);
  // a value the person typed survives a kind change
  setKind(ctx, 'github');
  byId(ctx.root, 'pv-base').value = 'https://ghe.example.com';
  setKind(ctx, 'gitlab');
  assert.equal(byId(ctx.root, 'pv-base').value, 'https://ghe.example.com');
  setKind(ctx, 'github');
  // validated like Go (https, not the github.com web host, shapes of registrymsg)
  const go = () => click(proposeBtn(ctx));
  for (const [bad, expect] of [
    [{ id: 'gh2', base: 'https://github.com', ssh: 'ssh://git@github.com', token: 'github-main' }, /api\.github\.com/],
    [{ id: 'gh2', base: 'https://www.github.com', ssh: 'ssh://git@github.com', token: 'github-main' }, /api\.github\.com/],
    [{ id: 'gh2', base: 'http://api.github.com', ssh: 'ssh://git@github.com', token: 'github-main' }, /https/],
    [{ id: 'gh2', base: 'https://api.github.com/user', ssh: 'ssh://git@github.com', token: 'github-main' }, /GitHub API 주소/],
    [{ id: 'gh2', base: 'https://api.github.com', ssh: 'ssh://root@github.com', token: 'github-main' }, /ssh 주소/],
    [{ id: 'GH', base: 'https://api.github.com', ssh: 'ssh://git@github.com', token: 'github-main' }, /id/],
  ]) {
    fillProvider(ctx, bad);
    go();
    assert.match(problemText(ctx), expect, JSON.stringify(bad));
    assert.equal(posts(ctx, '/api/v1/providers/propose').length, 0);
  }
  // a GitHub Enterprise host is accepted (the server decides about reachability)
  fillProvider(ctx, { id: 'ghe', base: 'https://ghe.example.com', ssh: 'ssh://git@ghe.example.com', token: 'github-main' });
  go();
  await waitSignable(ctx);
  assert.deepEqual(bodyOf(ctx, '/api/v1/providers/propose'), {
    action: 'set_provider', subject: 'ghe', fields: { kind: 'github', base_url: 'https://ghe.example.com', ssh_url: 'ssh://git@ghe.example.com', token_ref: 'github-main' },
  });
  const f = facts(ctx);
  assert.match(f.kind, /github/);
  assert.ok(f.base_url.includes('https://ghe.example.com'));
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'saved');
  assert.equal(posts(ctx, '/api/v1/providers/apply')[0].body.fields.kind, 'github');
  // a token of the wrong kind is never sent: a gitlab token for a github provider
  const c2 = await mountProviders();
  await openProviderForm(c2);
  setKind(c2, 'github');
  fillProvider(c2, { id: 'gh3', token: 'gitlab-main' });
  click(proposeBtn(c2));
  assert.match(problemText(c2), /github/);
  assert.equal(posts(c2, '/api/v1/providers/propose').length, 0);
  // edit and delete: the W3c warnings stay; delete is off for a provider in use; the kind of an existing provider is fixed
  const c3 = await mountProviders({ '/api/v1/providers': { providers: [PROVIDERS_E.providers[0], { ...PROVIDERS_E.providers[1], in_use: true }] } });
  const ghRow = rowOf(c3, 'github-main');
  assert.ok(ghRow, 'the github provider is listed');
  assert.ok(text(ghRow).includes('github'), 'its kind is shown');
  assert.ok(byLabel(ghRow, '삭제')[0].hasAttribute('disabled'), 'delete is off while in use');
  click(byLabel(ghRow, '수정')[0]);
  await until(() => byId(c3.root, 'pv-id'), 'edit form');
  assert.ok(byData(c3.root, 'warn', 'edit').length === 1, 'the edit warning is shown');
  assert.ok(byId(c3.root, 'pv-kind').hasAttribute('disabled'), 'the kind of an existing provider cannot be changed');
  assert.equal(byId(c3.root, 'pv-kind').value, 'github');
  assert.deepEqual(optionValues(byId(c3.root, 'pv-token')).sort(), ['github-main', 'github-old']);
  // a free github provider can be deleted
  const c4 = await mountProviders();
  assert.ok(!byLabel(rowOf(c4, 'github-main'), '삭제')[0].hasAttribute('disabled'));
});

test('TestOwnerSelectNoGroupCreationForGitHub', async () => {
  const ctx = await mountProjects();
  await openCreateForm(ctx);
  assert.equal(byLabel(ctx.root, '새 그룹 만들기').length, 1, 'gitlab: the control is there');
  assert.ok(!byId(ctx.root, 'cp-visibility'), 'gitlab: no visibility choice');
  await pickProvider(ctx, 'gh');
  assert.equal(byLabel(ctx.root, '새 그룹 만들기').length, 0, 'github: no group creation control at all');
  assert.ok(!text(ctx.root).includes('새 그룹 만들기'));
  const sel = byId(ctx.root, 'cp-group');
  assert.deepEqual(optionValues(sel), ['', 'SampleUser', 'acme-org']);
  const label = findAll(ctx.root, (e) => e.tagName === 'LABEL' && e.getAttribute('for') === 'cp-group')[0];
  assert.match(text(label), /소유자/);
  // the owner rows say what they are (the account and organisations)
  const optText = byTag(sel, 'option').map(text);
  assert.ok(optText[1].includes('SampleUser') && optText[1].includes('내 계정'), optText[1]);
  assert.ok(optText[2].includes('acme-org') && optText[2].includes('조직'), optText[2]);
  // back to gitlab: the control returns and the gitlab group list is read
  await pickProvider(ctx, 'gl');
  assert.equal(byLabel(ctx.root, '새 그룹 만들기').length, 1);
  assert.deepEqual(optionValues(byId(ctx.root, 'cp-group')), ['', 'team', 'team/sub']);
  assert.ok(!byId(ctx.root, 'cp-visibility'));
  // hostile / wrong owner rows are dropped (a path with slashes, a bad kind, a bad login)
  const evil = { groups: [{ path: 'ok-owner', name: 'x', kind: 'user' }, { path: 'a/b', name: 'x', kind: 'org' }, { path: 'neither', name: 'x', kind: 'robot' },
    { path: '<img src=x onerror=alert(1)>', name: 'x', kind: 'org' }, { path: 'trailing-', name: 'x', kind: 'org' }, { path: 'A'.repeat(40), name: 'x', kind: 'org' }, { path: 'Corp_User', name: 'x', kind: 'org' }], truncated: false };
  const c2 = await mountProjects({ '/api/v1/providers/gh/groups': evil });
  await openCreateForm(c2);
  await pickProvider(c2, 'gh');
  assert.deepEqual(optionValues(byId(c2.root, 'cp-group')), ['', 'ok-owner', 'Corp_User']);
  // github can never be asked to create a group: no group form can be reached
  assert.equal(byLabel(c2.root, '새 그룹 만들기').length, 0);
  // the owner pre-validation mirrors Go
  assert.equal(P.validGitHubOwner('SampleUser'), true);
  assert.equal(P.validGitHubOwner('a/b'), false);
  assert.equal(P.validOwnerForKind('github', 'Corp_User'), true);
  assert.equal(P.validOwnerForKind('gitlab', 'Corp_User'), false);
  assert.equal(P.validOwnerForKind('gitlab', 'team/sub'), true);
  assert.equal(P.validOwnerForKind('github', 'team/sub'), false);
});

test('TestVisibilityRequiredNoDefault', async () => {
  const ctx = await mountProjects();
  await openCreateForm(ctx);
  await pickProvider(ctx, 'gh');
  const vis = byId(ctx.root, 'cp-visibility');
  assert.ok(vis, 'github: the visibility select exists');
  assert.equal(vis.value, '', 'no default');
  assert.deepEqual(optionValues(vis), ['', 'public', 'private']);
  assert.equal(P.VISIBILITIES.length, 2);
  fillCreate(ctx, { group: 'SampleUser', name: 'hello-gh' });
  assert.ok(proposeBtn(ctx).hasAttribute('disabled'), 'the propose button is disabled until a choice is made');
  // a click that reaches the handler anyway (no browser would deliver it) is refused too
  click(proposeBtn(ctx));
  assert.match(problemText(ctx), /공개|비공개/);
  assert.equal(posts(ctx, '/api/v1/projects/create/propose').length, 0, 'nothing is proposed without a choice');
  fillCreate(ctx, { visibility: 'public' });
  assert.ok(!proposeBtn(ctx).hasAttribute('disabled'), 'enabled once chosen');
  // going back to the placeholder disables it again
  fillCreate(ctx, { visibility: '' });
  assert.ok(proposeBtn(ctx).hasAttribute('disabled'));
  // switching provider never carries a choice over
  fillCreate(ctx, { visibility: 'private' });
  await pickProvider(ctx, 'gl');
  await pickProvider(ctx, 'gh');
  assert.equal(byId(ctx.root, 'cp-visibility').value, '', 'a new github form has no choice');
  assert.ok(proposeBtn(ctx).hasAttribute('disabled'));
  // gitlab: the field is not offered and never sent
  const gl = await mountProjects();
  await toCreateConfirm(gl, 'gl', GL_CREATE);
  await waitSignable(gl);
  assert.ok(!has(bodyOf(gl, '/api/v1/projects/create/propose').fields, 'visibility'), 'gitlab: no visibility key on the wire');
  // github: the chosen value goes on the wire as the signed field
  const gh = await mountProjects();
  await toCreateConfirm(gh, 'gh', { ...GH_CREATE, visibility: 'public' });
  await waitSignable(gh);
  assert.deepEqual(bodyOf(gh, '/api/v1/projects/create/propose'), {
    action: 'create_project', subject: 'hello-gh',
    fields: { provider: 'gh', group_path: 'SampleUser', template: 'go', description: '', ai_context: false, publish: 'write', visibility: 'public' },
  });
});

test('TestVisibilityNoDefaultWhenGitHubIsTheFirstProviderW3e', async () => {
  // a form that OPENS on a github provider has no choice either (not only after a provider switch)
  const ghFirst = { providers: [PROVIDERS_E.providers[1], PROVIDERS_E.providers[0]] };
  const ctx = await mountProjects({ '/api/v1/providers': ghFirst });
  click(byLabel(ctx.root, '새 프로젝트 만들기')[0]);
  await until(() => byId(ctx.root, 'cp-visibility'), 'github form');
  assert.equal(byId(ctx.root, 'cp-visibility').value, '');
  await until(() => byTag(byId(ctx.root, 'cp-group'), 'option').length > 1, 'owners');
  fillCreate(ctx, { group: 'SampleUser', name: 'first-gh' });
  assert.ok(proposeBtn(ctx).hasAttribute('disabled'));
  click(proposeBtn(ctx));
  assert.equal(posts(ctx, '/api/v1/projects/create/propose').length, 0);
});

test('TestPublicConfirmationBold', async () => {
  const pub = await mountProjects();
  await toCreateConfirm(pub, 'gh', { ...GH_CREATE, visibility: 'public' });
  await waitSignable(pub);
  const v = byData(pub.root, 'field', 'visibility')[0];
  assert.ok(v, 'the visibility fact is shown');
  const bold = findAll(v, (e) => e.tagName === 'STRONG').filter((e) => text(e).includes('공개'));
  assert.ok(bold.length >= 1, 'in bold');
  assert.ok(text(v).includes('공개') && !text(v).includes('비공개'));
  const warn = byData(pub.root, 'emph', 'public-warning');
  assert.equal(warn.length, 1, 'an explicit warning for a public repository');
  assert.match(text(warn[0]), /누구나/);
  assert.match(text(warn[0]), /읽을 수 있/);
  // the warning is text of this page, bold text carries no server words
  assert.equal(findAll(pub.root, (e) => e.tagName === 'IMG' || e.tagName === 'SCRIPT').length, 0);
  // private: bold, no public warning
  const priv = await mountProjects();
  await toCreateConfirm(priv, 'gh', { ...GH_CREATE, visibility: 'private' });
  await waitSignable(priv);
  const pv = byData(priv.root, 'field', 'visibility')[0];
  assert.ok(findAll(pv, (e) => e.tagName === 'STRONG').some((e) => text(e).includes('비공개')));
  assert.equal(byData(priv.root, 'emph', 'public-warning').length, 0);
  // gitlab: no visibility fact, no warning
  const gl = await mountProjects();
  await toCreateConfirm(gl, 'gl', GL_CREATE);
  await waitSignable(gl);
  assert.equal(byData(gl.root, 'field', 'visibility').length, 0);
  assert.equal(byData(gl.root, 'emph', 'public-warning').length, 0);
  // nothing public is applied without the person pressing the button
  assert.equal(posts(pub, '/api/v1/projects/create/apply').length, 0);
  assert.equal(auth.gets.length, 0);
  // the server's own public line is required: a proposal for a public repository whose
  // explanation says private (or says nothing) is not signable
  for (const [name, fn] of Object.entries({
    'public chosen, preview says private': async (b) => ({ ...(await proposeFor(b)), preview: previewFor(b.action, b.subject, { ...b.fields, visibility: 'private', deploy_key_sha256: KEY.line_sha256 }, KEY.fingerprint) }),
    'public chosen, preview without a visibility line': async (b) => { const r = await proposeFor(b); return { ...r, preview: r.preview.filter((x) => x !== VIS_PUBLIC) }; },
    'both visibility lines': async (b) => { const r = await proposeFor(b); return { ...r, preview: [...r.preview, VIS_PRIVATE] }; },
    'the server says private in the fields': async (b) => retarget(b, (f) => ({ ...f, visibility: 'private' })),
  })) {
    const c = await mountProjects({ 'POST /api/v1/projects/create/propose': fn });
    await toCreateConfirm(c, 'gh', { ...GH_CREATE, visibility: 'public' });
    assert.equal(byData(c.root, 'warn', 'mismatch').length, 1, name);
    assert.equal(byLabel(c.root, '패스키로 서명').length, 0, name);
  }
});

test('TestCreateConfirmationShowsPerRepoKeyFingerprint', async () => {
  const ctx = await mountProjects();
  await toCreateConfirm(ctx, 'gh', GH_CREATE);
  await waitSignable(ctx);
  const f = facts(ctx);
  assert.ok(f.fingerprint.includes(KEY.fingerprint), 'fingerprint computed by the browser from the public key line');
  assert.ok(f.key_prefix.includes(KEY.line.slice(0, 40)) && !f.key_prefix.includes(KEY.line), 'the front of the .pub line, not the whole key');
  assert.ok(f.key_comment.includes('orchd-deploy'));
  assert.ok(f.key_hash.includes(KEY.line_sha256.slice(0, 16)));
  assert.ok(f.group_path.includes('SampleUser') || (f.owner || '').includes('SampleUser'), 'the owner');
  const hint = byData(ctx.root, 'hint', 'fingerprint')[0];
  assert.match(text(hint), /이 저장소 전용/);
  assert.ok(f.provider_url.includes('https://api.github.com'));
  // the browser never sends the hash; the apply carries the browser's own hash and the visibility
  assert.ok(!has(bodyOf(ctx, '/api/v1/projects/create/propose').fields, 'deploy_key_sha256'));
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'created');
  const applied = bodyOf(ctx, '/api/v1/projects/create/apply');
  assert.equal(applied.fields.deploy_key_sha256, KEY.line_sha256);
  assert.equal(applied.fields.visibility, 'private');
  assert.equal(applied.change, await P.computeChange('create_project', 'hello-gh', applied.fields));
  assert.equal(createHash('sha256').update(KEY.line).digest('hex'), applied.fields.deploy_key_sha256, 'the hash of the TEXT of the line, as Go hashes it');
  // gitlab: the shared key, said so
  const gl = await mountProjects();
  await toCreateConfirm(gl, 'gl', GL_CREATE);
  await waitSignable(gl);
  assert.match(text(byData(gl.root, 'hint', 'fingerprint')[0]), /공용|서버의 배포 키/);
  assert.ok(!text(byData(gl.root, 'hint', 'fingerprint')[0]).includes('이 저장소 전용'));
  // the steps of a github create use github words
  assert.ok(text(ctx.root).includes('저장소를 만들고 등록했습니다') || text(ctx.root).includes('프로젝트를 만들고 등록했습니다'));
});

test('TestSignOnlyWhenChangeMatchesW3e', async () => {
  const flip = (s) => (s[0] === 'a' ? 'b' : 'a') + s.slice(1);
  const common = {
    'wrong change': async (b) => ({ ...(await proposeFor(b)), change: '0'.repeat(64) }),
    'message with another change': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace(r.change, '1'.repeat(64)) }; },
    'message for another subject': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace('subject: ' + b.subject, 'subject: other-one') }; },
    'message for another action': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message.replace(b.action, b.action === 'attach_remote' ? 'link_project' : 'attach_remote') }; },
    'message not canonical': async (b) => { const r = await proposeFor(b); return { ...r, message: r.message + '\n' }; },
    'deploy key hash of another key (change recomputed to match)': async (b) => {
      const r = await proposeFor(b);
      const f = { ...r.fields, deploy_key_sha256: flip(r.fields.deploy_key_sha256) };
      const change = await P.computeChange(b.action, b.subject, f);
      return { ...r, fields: f, change, message: r.message.replace(r.change, change) };
    },
    'deploy key line of another key': async (b) => { const r = await proposeFor(b); return { ...r, deploy_key: { ...r.deploy_key, public_key: KEY2.line } }; },
    'fingerprint of another key': async (b) => { const r = await proposeFor(b); return { ...r, deploy_key: { ...r.deploy_key, fingerprint: KEY2.fingerprint } }; },
    'deploy key missing': async (b) => { const r = await proposeFor(b); delete r.deploy_key; return r; },
    'preview of another fingerprint': async (b) => { const r = await proposeFor(b); return { ...r, preview: previewFor(b.action, b.subject, r.fields, KEY2.fingerprint) }; },
    'publish switched by the server': async (b) => retarget(b, (f) => ({ ...f, publish: f.publish === 'read' ? 'write' : 'read' })),
    'provider switched by the server': async (b) => retarget(b, (f) => ({ ...f, provider: f.provider === 'gh' ? 'gl' : 'gh' })),
    'hostile preview': async (b) => ({ ...(await proposeFor(b)), preview: ['ok‮evil'] }),
    'empty preview': async (b) => ({ ...(await proposeFor(b)), preview: [] }),
    'not an object': async () => 'nope',
  };
  const cases = {
    create: {
      path: 'create', mount: async (over) => { const c = await mountProjects(over); await toCreateConfirm(c, 'gh', { ...GH_CREATE, visibility: 'public' }); return c; },
      extra: {
        'visibility switched by the server': async (b) => retarget(b, (f) => ({ ...f, visibility: 'private' })),
        'visibility dropped by the server': async (b) => retarget(b, (f) => { delete f.visibility; return f; }),
        'owner switched by the server': async (b) => retarget(b, (f) => ({ ...f, group_path: 'acme-org' })),
        'ai switched on by the server': async (b) => retarget(b, (f) => ({ ...f, ai_context: true })),
      },
    },
    link: {
      path: 'link', mount: async (over) => { const c = await mountProjects(over); await toLinkConfirm(c, { provider: 'gh', remote: 'octo/Hello-World', ai: false, publish: 'read' }); return c; },
      extra: {
        'remote path switched by the server': async (b) => retarget(b, (f) => ({ ...f, remote_path: 'octo/Other-Repo' })),
        'ai switched on by the server': async (b) => retarget(b, (f) => ({ ...f, ai_context: true })),
        'preview without the key warning': async (b) => { const r = await proposeFor(b); return { ...r, preview: r.preview.filter((x) => x !== KEY_WARN) }; },
      },
    },
    attach: {
      path: 'attach', mount: async (over) => { const c = await mountProjects(over); await toAttachConfirm(c, 'plain-app', { provider: 'gh', remote: 'octo/Plain-App', publish: 'write' }); return c; },
      extra: {
        'remote path switched by the server': async (b) => retarget(b, (f) => ({ ...f, remote_path: 'octo/Other-Repo' })),
        'preview without the key warning': async (b) => { const r = await proposeFor(b); return { ...r, preview: r.preview.filter((x) => x !== KEY_WARN) }; },
      },
    },
  };
  for (const [kind, c] of Object.entries(cases)) {
    // control: the untampered proposal is signable
    const ok = await c.mount();
    await waitSignable(ok);
    assert.equal(byData(ok.root, 'warn', 'mismatch').length, 0, kind);
    for (const [name, fn] of Object.entries({ ...common, ...c.extra })) {
      const ctx = await c.mount({ ['POST /api/v1/projects/' + c.path + '/propose']: fn });
      assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, kind + ' / ' + name + ': warning shown');
      assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, kind + ' / ' + name + ': no sign button');
      assert.equal(auth.gets.length, 0, kind + ' / ' + name + ': nothing signed');
      assert.equal(posts(ctx, '/api/v1/projects/' + c.path + '/apply').length, 0, kind + ' / ' + name);
    }
  }
  // the github provider proposal too
  for (const [name, fn] of Object.entries({
    'provider: wrong change': async (b) => ({ ...(await proposeFor(b)), change: '0'.repeat(64) }),
    'provider: kind switched': async (b) => retarget(b, (f) => ({ ...f, kind: 'gitlab' })),
    'provider: another address': async (b) => retarget(b, (f) => ({ ...f, base_url: 'https://evil.example' })),
  })) {
    const ctx = await mountProviders({ 'POST /api/v1/providers/propose': fn });
    await openProviderForm(ctx);
    setKind(ctx, 'github');
    fillProvider(ctx, { id: 'gh2', token: 'github-main' });
    click(proposeBtn(ctx));
    await until(() => byData(ctx.root, 'warn', 'mismatch').length === 1, name);
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, name);
  }
  // Go's own previews and messages are accepted for the new actions
  for (const [name, mk] of Object.entries({
    'create_project public': async (over) => { const c = await mountProjects(over); await toCreateConfirm(c, 'gh', { ...GH_CREATE, name: 'hello', group: 'SampleUser', visibility: 'public' }); return c; },
  })) {
    void name;
    const real = await mk({
      'POST /api/v1/projects/create/propose': async (b) => {
        const r = await proposeFor(b);
        return { ...r, preview: previewFor(b.action, b.subject, r.fields, KEY.fingerprint) };
      },
    });
    await waitSignable(real);
  }
});

test('TestLinkProjectFlow', async () => {
  const ctx = await mountProjects();
  await openLinkForm(ctx);
  // the form: provider, remote path, ai (off), publish (explicit), and the id it derives
  assert.deepEqual(optionValues(byId(ctx.root, 'lk-provider')), ['gl', 'gh']);
  assert.equal(byId(ctx.root, 'lk-ai').checked, false, 'AI transfer is off by default');
  assert.equal(byId(ctx.root, 'lk-publish').value, 'read');
  fillRemote(ctx, { provider: 'gh', remote: 'octo/Hello-World' });
  const derived = byData(ctx.root, 'field', 'derived-id')[0];
  assert.ok(derived && text(derived).includes('hello-world'), 'the derived project id is shown (github: the lower-cased repository name)');
  fillRemote(ctx, { provider: 'gl', remote: 'team/sub/app-two' });
  assert.ok(text(byData(ctx.root, 'field', 'derived-id')[0]).includes('app-two'), 'gitlab: the repository name itself');
  // refused before any proposal
  const refuse = (provider, remote, re) => {
    fillRemote(ctx, { provider, remote });
    click(proposeBtn(ctx));
    assert.match(problemText(ctx), re, provider + ' ' + remote);
    assert.equal(posts(ctx, '/api/v1/projects/link/propose').length, 0, remote);
  };
  refuse('gh', 'solo', /경로/);
  refuse('gh', 'octo/my_repo', /ID|이름/); // the id would be my_repo: not a project name
  refuse('gh', 'octo/Repo.js', /ID|이름/);
  refuse('gh', 'octo/x', /ID|이름/); // one letter
  refuse('gh', 'a/b/c', /경로/); // github is owner/repo
  refuse('gh', 'octo/repo.git', /경로/);
  refuse('gh', 'octo/../x', /경로/);
  refuse('gh', 'octo/re%2fpo', /경로/);
  refuse('gh', 'octo/저장소', /경로/);
  refuse('gh', 'octo/' + 'a'.repeat(101), /경로/);
  refuse('gl', 'Team/app-two', /경로/);
  refuse('gl', 'team/App-Two', /경로|ID|이름/);
  refuse('gl', 'a/b/c/d/e/f', /경로/);
  refuse('gh', '', /경로/);
  // a good one: nothing is sent before the button
  fillRemote(ctx, { provider: 'gh', remote: 'octo/Hello-World', publish: 'read' });
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.deepEqual(bodyOf(ctx, '/api/v1/projects/link/propose'), {
    action: 'link_project', subject: 'hello-world', fields: { provider: 'gh', remote_path: 'octo/Hello-World', ai_context: false, publish: 'read' },
  }, 'the browser sends neither deploy_key_sha256 nor anything else');
  const f = facts(ctx);
  assert.ok(f.remote_path.includes('octo/Hello-World'));
  assert.ok(f.id.includes('hello-world'));
  assert.match(f.publish, /읽기/);
  assert.match(f.ai_context, /아니오/);
  assert.ok(f.fingerprint.includes(KEY.fingerprint));
  assert.ok(f.provider_url.includes('https://api.github.com'));
  assert.equal(byData(ctx.root, 'warn', 'key-on-existing').length, 1, 'the key is added to an EXISTING repository: said in the page\'s own words');
  assert.equal(auth.gets.length, 0);
  assert.equal(posts(ctx, '/api/v1/projects/link/apply').length, 0);
  assert.ok(posts(ctx, '/api/v1/projects/link/propose')[0].opts.timeoutMs > 15000, 'the propose reads the broker (up to 15 s)');
  assert.equal(posts(ctx, '/api/v1/projects/link/propose')[0].opts.noRetry429, true);
  const regBefore = ctx.calls.filter((c) => c === '/api/v1/projects/registry').length;
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'linked');
  const applied = bodyOf(ctx, '/api/v1/projects/link/apply');
  assert.deepEqual(applied.fields, { provider: 'gh', remote_path: 'octo/Hello-World', ai_context: false, publish: 'read', deploy_key_sha256: KEY.line_sha256 });
  assert.equal(applied.change, await P.computeChange('link_project', 'hello-world', applied.fields));
  const ao = posts(ctx, '/api/v1/projects/link/apply')[0].opts;
  assert.ok(ao.timeoutMs >= 130000 && ao.noRetry429 === true);
  assert.equal(P.LINK_APPLY_TIMEOUT_MS, ao.timeoutMs);
  assert.ok(text(ctx.root).includes('hello-world'));
  assert.ok(byData(ctx.root, 'clone', 'ok').length === 1, 'the clone result is shown');
  await until(() => ctx.calls.filter((c) => c === '/api/v1/projects/registry').length > regBefore, 'list re-read');
  assert.equal(auth.gets.length, 1);
  // AI on and publish write are said as such; the clone failing offers the signature-less retry
  const c2 = await mountProjects({ 'POST /api/v1/projects/link/apply': () => ({ ...LINK_OK, clone: { status: 'failed', code: 'host_not_pinned' } }) });
  await toLinkConfirm(c2, { provider: 'gh', remote: 'octo/Hello-World', ai: true, publish: 'write' });
  await waitSignable(c2);
  assert.match(facts(c2).ai_context, /예/);
  assert.equal(byData(c2.root, 'emph', 'ai-on').length, 1);
  assert.match(facts(c2).publish, /쓰기/);
  click(signBtn(c2));
  await until(() => byLabel(c2.root, '다시 복제').length === 1, 'clone retry');
  click(byLabel(c2.root, '다시 복제')[0]);
  await until(() => posts(c2, '/api/v1/projects/hello-world/clone').length === 1, 'clone call');
  assert.equal(auth.gets.length, 2, 'the retry needs no signature');
  // a partial link: the repository existed; the notice says do NOT delete it
  const c3 = await mountProjects({ 'POST /api/v1/projects/link/apply': () => { throw apiError(502, 'link_partial', SECRET, { detail: LINK_PARTIAL.error.detail }); } });
  await toLinkConfirm(c3, { provider: 'gh', remote: 'octo/Hello-World', publish: 'read' });
  await waitSignable(c3);
  click(signBtn(c3));
  await until(() => byData(c3.root, 'result', 'error').length === 1, 'link_partial');
  assert.ok(text(c3.root).includes(P.ERROR_TEXT.link_partial));
  assert.match(text(byData(c3.root, 'orphan', 'notice')[0]), /원래 있던 저장소입니다\. 지우지 마세요\. 배포 키만 직접 제거하세요/);
  assert.ok(!text(c3.root).includes('이 도구가 만든 것'));
  assert.equal(text(byData(c3.root, 'orphan', 'project')[0]).trim(), 'octo/Hello-World');
  assert.ok(!text(c3.root).includes('SERVER-WORDS'));
  assert.equal(posts(c3, '/api/v1/projects/link/apply').length, 1);
});

test('TestAttachRemoteFlow', async () => {
  const ctx = await mountProjects();
  // the button exists only for a project without a remote
  assert.equal(byLabel(rowOf(ctx, 'plain-app'), '원격 연결').length, 1);
  assert.equal(byLabel(rowOf(ctx, 'hello-world'), '원격 연결').length, 0, 'github-linked: no button');
  assert.equal(byLabel(rowOf(ctx, 'app-one'), '원격 연결').length, 0, 'gitlab-linked: no button');
  await openAttachForm(ctx, 'plain-app');
  assert.ok(text(byData(ctx.root, 'field', 'subject')[0]).includes('plain-app'), 'the project is fixed, not typed');
  assert.ok(!byId(ctx.root, 'lk-ai'), 'attach does not touch the AI setting: no such control');
  // the name rule: the repository must carry the project id (github: up to case)
  const refuse = (remote, re, provider) => {
    fillRemote(ctx, { provider: provider || 'gh', remote });
    click(proposeBtn(ctx));
    assert.match(problemText(ctx), re, remote);
    assert.equal(posts(ctx, '/api/v1/projects/attach/propose').length, 0, remote);
  };
  refuse('octo/other-name', /같아야/);
  refuse('solo', /경로/);
  refuse('a/b/c', /경로/);
  refuse('team/Plain-App', /경로|같아야/, 'gl');
  fillRemote(ctx, { provider: 'gh', remote: 'octo/Plain-App', publish: 'write' });
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.deepEqual(bodyOf(ctx, '/api/v1/projects/attach/propose'), {
    action: 'attach_remote', subject: 'plain-app', fields: { provider: 'gh', remote_path: 'octo/Plain-App', publish: 'write' },
  });
  assert.ok(posts(ctx, '/api/v1/projects/attach/propose')[0].opts.timeoutMs > 15000);
  const f = facts(ctx);
  assert.ok(f.remote_path.includes('octo/Plain-App') && f.id.includes('plain-app'));
  assert.ok(!has(f, 'ai_context'));
  assert.equal(byData(ctx.root, 'warn', 'key-on-existing').length, 1);
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'attached');
  const applied = bodyOf(ctx, '/api/v1/projects/attach/apply');
  assert.deepEqual(applied.fields, { provider: 'gh', remote_path: 'octo/Plain-App', publish: 'write', deploy_key_sha256: KEY.line_sha256 });
  assert.ok(posts(ctx, '/api/v1/projects/attach/apply')[0].opts.timeoutMs >= 130000);
  assert.equal(P.ATTACH_APPLY_TIMEOUT_MS, posts(ctx, '/api/v1/projects/attach/apply')[0].opts.timeoutMs);
  // the key could not be moved: the page says so (fixed text), the project is attached
  const k = await mountProjects({ 'POST /api/v1/projects/attach/apply': () => ATTACH_KEY_FAILED });
  await toAttachConfirm(k, 'plain-app', { provider: 'gh', remote: 'octo/Plain-App', publish: 'write' });
  await waitSignable(k);
  click(signBtn(k));
  await until(() => byData(k.root, 'result', 'ok').length === 1, 'attached with key note');
  assert.ok(text(k.root).includes(P.ERROR_TEXT.key_unavailable));
  // the refusals: static remote and already linked, in FIXED texts, at propose and at apply time
  for (const code of ['static_remote_wins', 'already_linked', 'remote_name_mismatch', 'repo_not_found', 'project_unknown']) {
    const atPropose = await mountProjects({ 'POST /api/v1/projects/attach/propose': () => { throw apiError(409, code, SECRET); } });
    await openAttachForm(atPropose, 'plain-app');
    fillRemote(atPropose, { provider: 'gh', remote: 'octo/Plain-App' });
    click(proposeBtn(atPropose));
    await until(() => problemText(atPropose).includes(P.ERROR_TEXT[code]), code + ' at propose');
    assert.ok(!text(atPropose.root).includes('SERVER-WORDS'));
    assert.equal(byLabel(atPropose.root, '패스키로 서명').length, 0);
    const atApply = await mountProjects({ 'POST /api/v1/projects/attach/apply': () => { throw apiError(409, code, SECRET); } });
    await toAttachConfirm(atApply, 'plain-app', { provider: 'gh', remote: 'octo/Plain-App' });
    await waitSignable(atApply);
    click(signBtn(atApply));
    await until(() => byData(atApply.root, 'result', 'error').length === 1, code + ' at apply');
    assert.ok(text(atApply.root).includes(P.ERROR_TEXT[code]), code);
    assert.ok(!text(atApply.root).includes('SERVER-WORDS'));
    assert.equal(byLabel(atApply.root, '패스키로 서명').length, 0, 'a refused change is never signed again');
    assert.equal(posts(atApply, '/api/v1/projects/attach/apply').length, 1);
  }
  assert.match(P.ERROR_TEXT.static_remote_wins, /--git-remote-for|서버 설정/);
});

test('TestAllNewErrorCodesFixedTextsW3e', async () => {
  const NEW = ['repo_not_found', 'already_linked', 'project_exists', 'project_unknown', 'remote_name_mismatch', 'static_remote_wins', 'key_expired', 'key_staging_failed',
    'key_unavailable', 'github_refused', 'provider_rate_limited', 'not_supported_for_provider', 'visibility_required', 'visibility_not_allowed', 'link_partial'];
  for (const c of NEW) assert.ok(CODES.includes(c), c + ' is in the generated list');
  assert.ok(CODES.length >= 46);
  for (const code of CODES) {
    assert.ok(has(P.ERROR_TEXT, code), 'no fixed text for ' + code);
    const t = P.ERROR_TEXT[code];
    assert.ok(typeof t === 'string' && t.length > 8 && !t.includes('SERVER'), code);
    assert.equal(P.failureText(apiError(400, code, SECRET)), t, code);
  }
  const texts = CODES.filter((c) => c !== 'broker_error').map((c) => P.ERROR_TEXT[c]);
  assert.equal(new Set(texts).size, texts.length, 'every code has its own sentence');
  // unknown codes fall back to fixed text (status), never to the server's words
  for (const st of [400, 409, 429, 502, 503]) assert.ok(!P.failureText(apiError(st, 'something_new_w3e', SECRET)).includes('SERVER'), String(st));
  // provider-neutral baseline, kind-aware wording where the screen knows the kind
  for (const code of ['provider_unreachable', 'bad_url', 'group_unknown', 'project_name_taken', 'create_partial']) {
    assert.ok(!/GitLab/.test(P.ERROR_TEXT[code]) || /GitHub/.test(P.ERROR_TEXT[code]), code + ': the baseline does not claim GitLab only');
    const g = P.failureText(apiError(502, code, SECRET), 'github');
    assert.ok(/GitHub/.test(g) && !/GitLab/.test(g), code + ' (github): ' + g);
  }
  assert.ok(/GitHub/.test(P.ERROR_TEXT.github_refused) && !/GitLab/.test(P.ERROR_TEXT.github_refused));
  assert.ok(/다시 제안/.test(P.ERROR_TEXT.key_expired), 'key_expired says: propose again');
  assert.ok(/삭제|지우/.test(P.ERROR_TEXT.link_partial) && /배포 키/.test(P.ERROR_TEXT.link_partial));
  // each of the codes, as the real screens show them (apply of create, link and attach)
  const screens = {
    create: async (code) => {
      const c = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(409, code, SECRET); } });
      await toCreateConfirm(c, 'gh', GH_CREATE);
      return c;
    },
    link: async (code) => {
      const c = await mountProjects({ 'POST /api/v1/projects/link/apply': () => { throw apiError(409, code, SECRET); } });
      await toLinkConfirm(c, { provider: 'gh', remote: 'octo/Hello-World' });
      return c;
    },
  };
  for (const code of ['repo_not_found', 'already_linked', 'project_exists', 'remote_name_mismatch', 'static_remote_wins', 'key_expired', 'key_staging_failed', 'key_unavailable',
    'github_refused', 'provider_rate_limited', 'not_supported_for_provider', 'visibility_required', 'visibility_not_allowed']) {
    for (const [name, mk] of Object.entries(screens)) {
      const c = await mk(code);
      await waitSignable(c);
      click(signBtn(c));
      await until(() => byData(c.root, 'result', 'error').length === 1, name + ' ' + code);
      const shown = text(c.root);
      const want = name === 'create' && ['provider_unreachable', 'group_unknown', 'project_name_taken', 'bad_url'].includes(code) ? P.failureText(apiError(0, code), 'github') : P.ERROR_TEXT[code];
      assert.ok(shown.includes(want), name + ' ' + code);
      assert.ok(!shown.includes('SERVER-WORDS'), name + ' ' + code);
      assert.equal(byLabel(c.root, '패스키로 서명').length, 0, name + ' ' + code + ': spent, not signable again');
    }
  }
  // key_expired at propose-time or apply-time: the screen says to propose again and offers a new start, no re-sign
  const keSigned = auth.gets.length;
  const ke = await screens.link('key_expired');
  await waitSignable(ke);
  click(signBtn(ke));
  await until(() => byData(ke.root, 'result', 'error').length === 1, 'key_expired');
  assert.match(text(ke.root), /다시 제안/);
  assert.ok(byLabel(ke.root, '목록으로').length === 1);
  assert.equal(auth.gets.length, keSigned + 1, 'one signature, never reused on the stale data');
  // github create: the 502 github_refused shows the github wording; a gateway error without a code is unknown, not a refusal
  const gr = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'github_refused', SECRET); } });
  await toCreateConfirm(gr, 'gh', GH_CREATE);
  await waitSignable(gr);
  click(signBtn(gr));
  await until(() => byData(gr.root, 'result', 'error').length === 1, 'github_refused');
  assert.ok(text(gr.root).includes(P.ERROR_TEXT.github_refused));
  assert.equal(P.isUnknown(apiError(502, 'github_refused')), false);
  assert.equal(P.isUnknown(apiError(503, 'key_staging_failed')), false);
  assert.equal(P.isUnknown(apiError(502, 'link_partial')), false);
  assert.equal(P.isUnknown(apiError(502, 'some_proxy_thing')), true);
});

test('TestOutcomeUnknownNoAutoRetryW3e', async () => {
  const flows = {
    'link timeout': { path: 'link', err: () => apiError(0, 'timeout', SECRET), mount: async (over) => { const c = await mountProjects(over); await toLinkConfirm(c, { provider: 'gh', remote: 'octo/Hello-World' }); return c; } },
    'link outcome_unknown': { path: 'link', err: () => apiError(504, 'outcome_unknown', SECRET), mount: async (over) => { const c = await mountProjects(over); await toLinkConfirm(c, { provider: 'gh', remote: 'octo/Hello-World' }); return c; } },
    'link gateway 502 without a code': { path: 'link', err: () => apiError(502, '', SECRET), mount: async (over) => { const c = await mountProjects(over); await toLinkConfirm(c, { provider: 'gl', remote: 'team/app-two' }); return c; } },
    'attach network failure': { path: 'attach', err: () => apiError(0, 'network', SECRET), mount: async (over) => { const c = await mountProjects(over); await toAttachConfirm(c, 'plain-app', { provider: 'gh', remote: 'octo/Plain-App' }); return c; } },
    'attach outcome_unknown': { path: 'attach', err: () => apiError(504, 'outcome_unknown', SECRET), mount: async (over) => { const c = await mountProjects(over); await toAttachConfirm(c, 'plain-app', { provider: 'gh', remote: 'octo/Plain-App' }); return c; } },
    'github create timeout': { path: 'create', err: () => apiError(0, 'timeout', SECRET), mount: async (over) => { const c = await mountProjects(over); await toCreateConfirm(c, 'gh', GH_CREATE); return c; } },
    'github create 504 outcome_unknown': { path: 'create', err: () => apiError(504, 'outcome_unknown', SECRET), mount: async (over) => { const c = await mountProjects(over); await toCreateConfirm(c, 'gh', GH_CREATE); return c; } },
  };
  for (const [name, fl] of Object.entries(flows)) {
    const ctx = await fl.mount({ ['POST /api/v1/projects/' + fl.path + '/apply']: () => { throw fl.err(); } });
    await waitSignable(ctx);
    const signedBefore = auth.gets.length;
    const regBefore = ctx.calls.filter((c) => c === '/api/v1/projects/registry').length;
    const provBefore = ctx.calls.filter((c) => c === '/api/v1/providers').length;
    click(signBtn(ctx));
    await until(() => byData(ctx.root, 'result', 'error').length === 1, name);
    assert.ok(text(ctx.root).includes(P.ERROR_TEXT.outcome_unknown), name);
    assert.ok(!text(ctx.root).includes('SERVER-WORDS'), name);
    if (fl.path !== 'create') assert.match(text(ctx.root), /배포 키/, name + ': a deploy key may be attached: check the provider');
    await until(() => ctx.calls.filter((c) => c === '/api/v1/projects/registry').length > regBefore, name + ': registry re-read');
    await until(() => ctx.calls.filter((c) => c === '/api/v1/providers').length > provBefore, name + ': providers re-read');
    await sleep(40);
    assert.equal(posts(ctx, '/api/v1/projects/' + fl.path + '/apply').length, 1, name + ': never retried');
    assert.equal(posts(ctx, '/api/v1/projects/' + fl.path + '/propose').length, 1, name + ': never proposed again');
    assert.equal(auth.gets.length, signedBefore + 1, name + ': never signed again');
    assert.equal(byLabel(ctx.root, '패스키로 서명').length, 0, name);
    assert.equal(byLabel(ctx.root, '다시 복제').length, 0, name);
    assert.ok(!text(ctx.root).includes('변경은 적용되지 않았습니다'), name + ': claims nothing about "not applied"');
  }
  // 429: the page never waits it out or repeats the call
  for (const [path, mk] of Object.entries({
    link: async (over) => { const c = await mountProjects(over); await toLinkConfirm(c, { provider: 'gh', remote: 'octo/Hello-World' }); return c; },
    attach: async (over) => { const c = await mountProjects(over); await toAttachConfirm(c, 'plain-app', { provider: 'gh', remote: 'octo/Plain-App' }); return c; },
  })) {
    const c = await mk({ ['POST /api/v1/projects/' + path + '/apply']: () => { throw apiError(429, 'project_rate_limited', SECRET); } });
    await waitSignable(c);
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'error').length === 1, '429 ' + path);
    assert.ok(text(c.root).includes(P.ERROR_TEXT.project_rate_limited));
    assert.equal(posts(c, '/api/v1/projects/' + path + '/apply')[0].opts.noRetry429, true);
    await sleep(30);
    assert.equal(posts(c, '/api/v1/projects/' + path + '/apply').length, 1);
  }
  // the GitHub owner list is a provider call: no wait on 429, fixed text, not repeated
  const o = await mountProjects({ '/api/v1/providers/gh/groups': () => { throw apiError(429, 'provider_rate_limited', SECRET); } });
  await openCreateForm(o);
  const sel = byId(o.root, 'cp-provider');
  sel.value = 'gh';
  fire(sel, 'change');
  fire(sel, 'blur');
  await until(() => text(o.root).includes(P.ERROR_TEXT.provider_rate_limited), 'owner 429');
  assert.equal(o.calls.filter((c) => c === '/api/v1/providers/gh/groups').length, 1);
  // double submit: the second click on the sign button signs nothing more
  const d = await mountProjects();
  await toLinkConfirm(d, { provider: 'gh', remote: 'octo/Hello-World' });
  await waitSignable(d);
  const dSigned = auth.gets.length;
  const sb = signBtn(d);
  click(sb);
  click(sb);
  click(sb);
  await until(() => byData(d.root, 'result', 'ok').length === 1, 'once');
  assert.equal(auth.gets.length, dSigned + 1);
  assert.equal(posts(d, '/api/v1/projects/link/apply').length, 1);
});

test('TestChangeVectorsMatchGoW3e', async () => {
  assert.ok(VEC.good.length >= 6 && VEC.refused.length >= 15 && VEC.messages.length === VEC.good.length);
  for (const c of VEC.good) {
    assert.equal(P.canonicalChange(c.action, c.subject, c.fields), c.canonical, c.name);
    assert.equal(await P.computeChange(c.action, c.subject, c.fields), c.change, c.name);
    const rev = {};
    for (const k of Object.keys(c.fields).reverse()) rev[k] = c.fields[k];
    assert.equal(P.canonicalChange(c.action, c.subject, rev), c.canonical, c.name + ' (reversed)');
    assert.equal(createHash('sha256').update(c.canonical).digest('hex'), c.change, c.name);
  }
  for (const r of VEC.refused) {
    await assert.rejects(async () => P.computeChange(r.action, r.subject, r.fields), { name: 'ChangeError' }, r.name);
  }
  // visibility is optional exactly where Go says: with or without it, no other field may be added
  const cp = VEC.good.find((c) => c.name === 'create_project private');
  const { visibility, ...without } = cp.fields;
  assert.equal(visibility, 'private');
  assert.doesNotThrow(() => P.canonicalChange('create_project', 'hello', without));
  assert.throws(() => P.canonicalChange('create_project', 'hello', { ...cp.fields, extra: 'x' }), { name: 'ChangeError' });
  assert.throws(() => P.canonicalChange('create_project', 'hello', { ...cp.fields, visibility: undefined }), { name: 'ChangeError' });
  assert.throws(() => P.canonicalChange('link_project', 'hello', { ...VEC.good.find((c) => c.name === 'link_project github').fields, remote_path: 'octo/a\ud800b' }), { name: 'ChangeError' });
  assert.deepEqual(P.OPTIONAL_CREATE_FIELDS, ['visibility']);
  // golden messages: the browser's message check accepts exactly Go's text for each action, including the two new ones
  for (const m of VEC.messages) {
    assert.equal(createHash('sha256').update(m.text).digest('hex'), m.digest, m.name);
    assert.deepEqual(wa.checkRegistryMessage(m.text), { action: m.action, subject: m.subject, change: m.change, request: m.request, issued: m.issued }, m.name);
  }
  assert.ok(VEC.messages.some((m) => m.action === 'link_project') && VEC.messages.some((m) => m.action === 'attach_remote'));
  // the preview helper of these tests is Go's text
  for (const c of VEC.good) {
    assert.deepEqual(previewFor(c.action, c.subject, c.fields, KEY.fingerprint), VEC.previews[c.name], c.name);
  }
  // the page's own constants are the lines Go says
  const pubPrev = VEC.previews['create_project public'];
  const privPrev = VEC.previews['create_project private'];
  assert.ok(pubPrev.includes(P.PREVIEW_VIS_PUBLIC) && !pubPrev.includes(P.PREVIEW_VIS_PRIVATE));
  assert.ok(privPrev.includes(P.PREVIEW_VIS_PRIVATE) && !privPrev.includes(P.PREVIEW_VIS_PUBLIC));
  assert.ok(VEC.previews.attach_remote.includes(P.PREVIEW_KEY_WARN) && VEC.previews['link_project github'].includes(P.PREVIEW_KEY_WARN));
  // deploy keys: the hash of the line's text, the fingerprint of its blob
  for (const k of VEC.deploy_keys) {
    assert.equal(await P.deployKeyHash(k.line), k.line_sha256, k.name);
    assert.equal(await P.sshFingerprint(k.line), k.fingerprint, k.name);
    assert.match(k.line, P.DEPLOY_KEY_RE);
  }
  // the name rules agree with registrymsg on every vector
  const agree = (fn, set, what) => {
    assert.ok(set.length >= 5 && set.some((v) => v.ok) && set.some((v) => !v.ok), what);
    for (const v of set) assert.equal(fn(v.value), v.ok, what + ': ' + JSON.stringify(v.value));
  };
  agree(P.validGitHubOwner, VEC.github_owners, 'github owner');
  agree(P.validGitHubRepoName, VEC.github_repos, 'github repo');
  agree(P.validRemotePath, VEC.remote_paths, 'remote path');
  agree((s) => P.validRemotePathForKind('github', s), VEC.remote_paths_github, 'remote path (github)');
  agree((s) => P.validRemotePathForKind('gitlab', s), VEC.remote_paths_gitlab, 'remote path (gitlab)');
  agree((s) => P.validOwnerForKind('github', s), VEC.owner_github, 'owner (github)');
  agree((s) => P.validOwnerForKind('gitlab', s), VEC.owner_gitlab, 'owner (gitlab)');
  // and the W3c vectors still hold (GitLab is unchanged)
  for (const c of W3C.change) assert.equal(await P.computeChange(c.action, c.subject, c.fields), c.change, c.name);
  for (const r of W3C.refused.filter((x) => x.action !== 'set_provider' || x.name !== 'provider kind github')) {
    await assert.rejects(async () => P.computeChange(r.action, r.subject, r.fields), { name: 'ChangeError' }, r.name);
  }
});

test('TestNoPasswordLikeMarkupW3e', async () => {
  // no secret input anywhere in the new screens: no password type, no masking, no credential autocomplete
  const dir = new URL('../static/', import.meta.url);
  const walk = (u) => readdirSync(u, { withFileTypes: true }).flatMap((d) => (d.isDirectory() ? walk(new URL(d.name + '/', u)) : [new URL(d.name, u)]));
  for (const f of walk(dir)) {
    if (!/\.(js|css|html)$/.test(f.pathname)) continue;
    const t = readFileSync(f, 'utf8');
    assert.ok(!/text-?security/i.test(t) && !/type\s*[:=]\s*['"`]?password/i.test(t) && !/autocomplete['"]?\s*[:=]\s*['"](?:new-password|current-password|username)/i.test(t), f.pathname);
  }
  // the static bans cover the code the E5 flows live in
  for (const name of ['views/providers.js', 'views/projects.js', 'webauthn.js']) {
    const c = src(name);
    for (const bad of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'caches.', 'history.pushState', 'history.replaceState', 'navigator.storage', 'ctx.navigate',
      'navigate(', 'location.hash', 'confirm(', 'alert(', 'innerHTML', 'outerHTML', 'insertAdjacentHTML', 'Date.now', 'new Date', 'eval(', 'new Function', 'document.write']) {
      assert.ok(!c.includes(bad), name + ' uses ' + bad);
    }
    assert.ok(!/\.setAttribute\(/.test(c), name + ': attributes go through el()');
  }
  // the new inputs are plain, named text fields
  const pj = src('views/projects.js');
  assert.ok(/id: 'lk-remote'[^}]*autocomplete: 'off'/.test(pj), 'the remote path input is plain text');
  // hostile server text on the new screens never reaches an attribute and never becomes an element
  const evil = SECRET + '‮\u0000';
  const hostileReg = { ...REG_E, projects: [...REG_E.projects, { id: 'ev', path: '/home/orch/repos/ev', kind: 'registered', ai_context: false, provider: null, gitlab: null, auto_merge: { enabled: false }, cloned: true }],
    orphans: [...REG_E.orphans, { provider: 'gh', project: 'octo/' + evil, project_id: null, step: 'protect', at: '2026-11-12T12:00:00Z', origin: 'created' }, { provider: 'gh', project: 'octo/ok', project_id: null, step: 'protect', at: '2026-11-12T12:00:00Z', origin: 'bogus' }] };
  const ctx = await mountProjects({
    '/api/v1/projects/registry': hostileReg,
    '/api/v1/providers/gh/groups': { groups: [{ path: evil, name: evil, kind: 'user' }, { path: 'fine', name: evil, kind: 'org' }], truncated: true },
    'POST /api/v1/projects/link/apply': () => { throw apiError(502, 'link_partial', evil, { detail: { ...LINK_PARTIAL.error.detail, project: 'octo/<img src=x onerror=alert(1)>' }, ...{} }); },
  });
  const scan = () => {
    assert.equal(findAll(ctx.root, (e) => ['IMG', 'SCRIPT', 'IFRAME', 'B'].includes(e.tagName) && text(e).includes('<')).length, 0);
    for (const e of findAll(ctx.root, () => true)) {
      for (const [k, v] of e.attrs) {
        assert.ok(!/onerror|javascript:|<img|SERVER-WORDS|<script/.test(k + '=' + v), 'attribute ' + k + ' carries server text: ' + v);
        assert.ok(!k.startsWith('on'), k);
      }
    }
  };
  scan();
  assert.ok(!text(ctx.root).includes('SERVER-WORDS'), 'a malformed orphan row is dropped, not shown');
  await openCreateForm(ctx);
  await pickProvider(ctx, 'gh');
  assert.deepEqual(optionValues(byId(ctx.root, 'cp-group')), ['', 'fine']);
  scan();
  click(byLabel(ctx.root, '취소')[0]);
  await until(() => byLabel(ctx.root, '기존 저장소 연결').length > 0, 'list');
  await toLinkConfirm(ctx, { provider: 'gh', remote: 'octo/Hello-World' });
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'error').length === 1, 'result');
  scan();
  assert.ok(!text(ctx.root).includes('<img'), 'an invalid detail is not displayed');
  assert.ok(text(ctx.root).includes('octo/Hello-World'), 'the page falls back to what it sent itself');
  assert.equal(findHostile(text(byData(ctx.root, 'orphan', 'project')[0])), null);
});

// ---- the rest of the E5 contract ----------------------------------------------------

test('TestOrphanOriginsRenderDifferentlyW3e', async () => {
  const ctx = await mountProjects();
  const box = byData(ctx.root, 'orphans', 'list')[0];
  assert.ok(box, 'the orphan block');
  const items = findAll(box, (e) => e.tagName === 'LI');
  assert.equal(items.length, 2);
  const created = items.find((li) => text(li).includes('SampleUser/half-made'));
  const linked = items.find((li) => text(li).includes('octo/Existing-Repo'));
  assert.match(text(created), /이 도구가 만든 것/);
  assert.match(text(created), /GitHub에서 직접 지우세요/);
  assert.ok(!/지우지 마세요/.test(text(created)));
  assert.match(text(linked), /원래 있던 저장소입니다\. 지우지 마세요\. 배포 키만 직접 제거하세요/);
  assert.ok(!/직접 지우세요/.test(text(linked)), 'a linked repository is never described as something to delete');
  // the create_partial screen of a github create: created wording, steps in github words
  const c = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail: CREATE_GH_PARTIAL.error.detail }); } });
  await toCreateConfirm(c, 'gh', GH_CREATE);
  await waitSignable(c);
  click(signBtn(c));
  await until(() => byData(c.root, 'result', 'error').length === 1, 'create_partial');
  assert.match(text(byData(c.root, 'orphan', 'notice')[0]), /이 도구가 만든 것: GitHub에서 직접 지우세요/);
  assert.ok(text(c.root).includes('SampleUser/hello'));
  assert.ok(text(c.root).includes(P.failureText(apiError(502, 'create_partial'), 'github')));
  // an origin that is not one of the two falls back to what this page sent, never to a guess
  const bad = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail: { ...CREATE_GH_PARTIAL.error.detail, origin: 'bogus' } }); } });
  await toCreateConfirm(bad, 'gh', GH_CREATE);
  await waitSignable(bad);
  click(signBtn(bad));
  await until(() => byData(bad.root, 'result', 'error').length === 1, 'bad origin');
  assert.equal(byData(bad.root, 'orphan', 'invalid').length, 1);
  // a GitLab created partial still says "GitLab에서 직접"
  const gl = await mountProjects({ 'POST /api/v1/projects/create/apply': () => { throw apiError(502, 'create_partial', SECRET, { detail: fx('project-create-partial.json').error.detail }); } });
  await toCreateConfirm(gl, 'gl', GL_CREATE);
  await waitSignable(gl);
  click(signBtn(gl));
  await until(() => byData(gl.root, 'result', 'error').length === 1, 'gitlab partial');
  assert.ok(text(gl.root).includes('GitLab에서 직접'));
});

test('TestRealApiLayerKeepsLinkPartialDetailW3e', async () => {
  // found by the visual check: the fake ctx of the tests bypasses api.js, which dropped the detail of link_partial
  const api = await import('../static/api.js');
  try {
    api.configure({ fetch: async () => new Response(JSON.stringify({ error: { code: 'link_partial', message: 'm', detail: LINK_PARTIAL.error.detail, secret: 'x' } }), { status: 502 }) });
    await assert.rejects(() => api.post('/api/v1/projects/link/apply', {}), (e) => e.code === 'link_partial' && e.detail && e.detail.origin === 'linked' && e.secret === undefined);
    api.configure({ fetch: async () => new Response(JSON.stringify({ error: { code: 'already_linked', message: 'm', detail: { project: 'a/b' } } }), { status: 409 }) });
    await assert.rejects(() => api.post('/api/v1/projects/link/apply', {}), (e) => e.code === 'already_linked' && e.detail === undefined);
  } finally {
    api.configure();
  }
});

test('TestTimeoutsAndConstantsW3e', async () => {
  assert.ok(P.CREATE_APPLY_TIMEOUT_MS >= 130000 && P.LINK_APPLY_TIMEOUT_MS >= 130000 && P.ATTACH_APPLY_TIMEOUT_MS >= 130000);
  assert.ok(P.LINK_PROPOSE_TIMEOUT_MS > 15000);
  const ctx = await mountProjects();
  await toCreateConfirm(ctx, 'gh', GH_CREATE);
  await waitSignable(ctx);
  click(signBtn(ctx));
  await until(() => posts(ctx, '/api/v1/projects/create/apply').length === 1, 'apply');
  assert.ok(posts(ctx, '/api/v1/projects/create/apply')[0].opts.timeoutMs >= 130000);
  assert.equal(posts(ctx, '/api/v1/projects/create/propose')[0].opts.noRetry429, true);
  assert.equal(posts(ctx, '/api/v1/projects/create/apply')[0].opts.noRetry429, true);
  assert.deepEqual(P.PROVIDER_KINDS, ['gitlab', 'github']);
  assert.deepEqual(P.VISIBILITIES, ['public', 'private']);
  assert.equal(P.ACTION_LINK_PROJECT, 'link_project');
  assert.equal(P.ACTION_ATTACH_REMOTE, 'attach_remote');
  assert.equal(P.MAX_REMOTE_SEGMENTS, 5);
  assert.equal(P.MAX_REMOTE_SEGMENT_LEN, 100);
  assert.equal(P.MAX_REMOTE_PATH_LEN, 255);
  // the page's own flow never touches the clock, the storage or the location
  assert.deepEqual(ctx.nav, []);
});

test('TestGitHubOwnerWithUppercaseAndPublishDefaultsW3e', async () => {
  // a real personal account has capitals: the owner select, the signed field and the project path accept it
  const ctx = await mountProjects();
  await toCreateConfirm(ctx, 'gh', { ...GH_CREATE, group: 'SampleUser' });
  await waitSignable(ctx);
  assert.equal(bodyOf(ctx, '/api/v1/projects/create/propose').fields.group_path, 'SampleUser');
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'created');
  assert.ok(byData(ctx.root, 'clone', 'ok').length === 1 || text(ctx.root).includes('복제'));
  assert.equal(P.validRemotePath('SampleUser/hello'), true);
});
const gone = () => {};
void gone;
