import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { webcrypto, createHash } from 'node:crypto';
import { findAll } from './dom-stub.js';
import { fakeCtx, apiError } from './fake-ctx.js';

// W3f (Task F4): the deletion screens. Golden facts come from Go (fixtures/w3f-vectors.json and
// w3c-error-codes.json, both written from the real code). Written ALONGSIDE delete.js (not red-first);
// the guards are pinned by the mutation table of the report.

const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const src = (n) => readFileSync(new URL('../static/' + n, import.meta.url), 'utf8');
const wa = await import('../static/webauthn.js');
const P = await import('../static/views/providers.js');
const PR = await import('../static/views/projects.js');
const D = await import('../static/views/delete.js');
const api = await import('../static/api.js');

const VEC = fx('w3f-vectors.json');
const NAMES = fx('w3f-names.json');
const CODES = fx('w3c-error-codes.json').codes;
const SECRET = '<img src=x onerror=alert(1)> javascript:alert(1) SERVER-WORDS';
const SESSION = { principal: 'phone', kind: 'session', expires: '2026-11-11T00:00:00Z' };
const FP = 'SHA256:' + 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ'.slice(0, 43);
const STAMP = '서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다';

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
  for (let i = 0; i < 400; i++) {
    if (cond()) return;
    await sleep(2);
  }
  assert.fail('timed out waiting for ' + what);
}
const u8 = (b) => new Uint8Array(b);
const toAB = (b) => { const x = u8(b); return x.buffer.slice(x.byteOffset, x.byteOffset + x.byteLength); };
const CRED = new Uint8Array(32).fill(7);
const SIGNERS = { signers: [{ id: 'phone', credential_id: wa.b64uEncode(CRED), label: 'x' }] };
const sha = (s) => createHash('sha256').update(s).digest('hex');

class Auth {
  constructor() { this.gets = []; }
  async get(opts) {
    this.gets.push(opts);
    return { type: 'public-key', rawId: toAB(CRED), response: { authenticatorData: toAB([1, 2, 3]), clientDataJSON: toAB([4, 5, 6]), signature: toAB([7, 8, 9]) } };
  }
  async create() { throw new Error('not used'); }
}
let auth;
beforeEach(() => {
  auth = new Auth();
  wa.configure({ credentials: auth, subtle: webcrypto.subtle });
});

// the registry: one project per origin, a created one whose remote is already gone, and orphans of three origins
const PROVIDERS = [
  { id: 'gl', kind: 'gitlab', base_url: 'http://gitlab.example:18082', ssh_url: 'ssh://git@gitlab.example', token_ref: 'gl-admin', in_use: true },
  { id: 'gh', kind: 'github', base_url: 'https://api.github.com', ssh_url: 'ssh://git@github.com', token_ref: 'github-main', in_use: true },
];
const row = (id, over) => ({ id, path: '/home/orch/repos/' + id, kind: 'registered', ai_context: false, provider: null, gitlab: null, auto_merge: { enabled: false }, cloned: true, origin: 'unknown', ...over });
const ROWS = [
  row('mine', { provider: 'gh', gitlab: { project: 'octo/Hello-World', project_id: 123456789 }, origin: 'created' }),
  row('toy', { origin: 'legacy', kind: 'legacy' }),
  row('linked-app', { provider: 'gl', gitlab: { project: 'team/app-one', project_id: 7 }, origin: 'linked' }),
  row('plain', { origin: 'unknown' }),
  row('gone-app', { provider: 'gh', gitlab: null, origin: 'created', remote_deleted_at: '2026-11-12T10:04:05Z' }),
  row('noid', { provider: 'gh', gitlab: { project: 'octo/NoId', project_id: null }, origin: 'created' }),
  row('lab', { provider: 'gl', gitlab: { project: 'team/lab', project_id: 21 }, origin: 'created' }),
];
const ORPHANS = [
  { provider: 'gh', project: 'octo/half-made', project_id: null, step: 'protect', at: '2026-11-12T12:00:00Z', origin: 'created' },
  { provider: 'gh', project: 'octo/Existing-Repo', project_id: null, step: 'deploy_key', at: '2026-11-12T13:00:00Z', origin: 'linked' },
  { provider: 'gl', project: 'team/mystery', project_id: null, step: 'project', at: '2026-11-12T14:00:00Z' },
];
const REG = (over) => ({ projects: ROWS, providers: PROVIDERS, orphans: ORPHANS, auto_merge_global: { enabled: false, max_per_day: 5 }, ...(over || {}) });

const pvBody = (id, over, rowOver) => {
  const r = rowOver || ROWS.find((x) => x.id === id);
  return {
    id, origin: r.origin, legacy: r.origin === 'legacy', has_remote: r.gitlab !== null, remote_key_may_remain: r.gitlab !== null, remote_deleted_at: r.remote_deleted_at || null,
    static_remote: false, busy: false, busy_unknown: false, folder_exists: true, is_git_repo: true, local_removable: true, key_dir_exists: true,
    size_bytes: 3 * 1024 * 1024, size_known: true, unpushed_commits: 0, unpushed_known: true, changed_files: 0, changed_known: true,
    detach_key_available: false, deploy_key_title: null, deploy_key_fingerprint: null, deploy_key_shared: false, ...(over || {}),
  };
};

// The server's fixed lines (internal/orch/api deletePreview); the Go drift test pins the marks.
function previewFor(action, subject, f) {
  const path = '/home/orch/repos/' + subject;
  if (action === 'remove_project') {
    const out = ["프로젝트 '" + subject + "'를 이 서버에서 지웁니다: 프로젝트 등록, 서버의 복제본 폴더(" + path + '), 이 프로젝트의 배포 키 폴더', '원격 저장소는 지워지지 않고 그대로 남습니다',
      '[중요] 되돌릴 수 없습니다. 원격에 올라가지 않은 커밋과 저장하지 않은 변경은 이 서버의 복제본에만 있으므로 함께 사라집니다', '이 서버의 작업 기록은 남습니다'];
    if (f.detach_key) {
      const r = ROWS.find((x) => x.id === subject);
      out.splice(2, 0, "먼저 원격 저장소 '" + (r && r.gitlab ? r.gitlab.project : '?') + "'에서 이 서버의 배포 키(지문 " + FP + ')를 뗍니다. 키를 떼지 못하거나 결과를 알 수 없으면 프로젝트 등록과 서버의 파일을 지우지 않습니다');
    }
    if (f.legacy_ack) out.push('[중요] 처음부터 있던(legacy) 프로젝트입니다. 지우면 이 저장소로는 더 작업을 낼 수 없습니다(두 번째 확인을 했습니다)');
    return [...out, STAMP];
  }
  if (action === 'delete_remote_repo') {
    const out = ["[중요] 원격 저장소 '" + f.remote_path + "'를 제공자에서 삭제합니다. 되돌릴 수 없습니다(제공자 설정에 따라 복구 가능 기간이 있을 수 있으나 이 도구는 복구하지 않습니다)",
      "이 도구가 만든 저장소만 지울 수 있으며, 저장소 이름 '" + f.confirm_name + "'을 직접 입력해 확인했습니다",
      '삭제가 확실히 끝난 뒤에만 다음 단계를 합니다. 결과를 알 수 없으면 아무것도 더 하지 않고 자동으로 다시 시도하지 않습니다'];
    if (f.also_local) {
      out.push('원격 삭제가 끝나면 이 서버의 복제본 폴더(' + path + ')와 이 프로젝트의 배포 키 폴더도 지웁니다', '[중요] 원격이 지워지면 이 서버의 복제본이 유일한 사본일 수 있습니다. 원격에 올라가지 않은 커밋과 변경은 복제본과 함께 사라집니다',
        '원격 삭제가 끝나면 프로젝트 등록도 같은 서명으로 지웁니다. 서버의 파일을 지우지 못하면 등록 없는 폴더가 남을 수 있습니다(관리자가 정리)');
    } else out.push("이 서버의 복제본과 프로젝트 등록은 그대로 남습니다(등록은 '원격 없음' 상태가 됩니다)");
    return [...out, STAMP];
  }
  if (action === 'remove_deploy_key') {
    return ["프로젝트 '" + subject + "'의 저장소 '" + f.remote_path + "'에서 이 서버의 배포 키만 뗍니다(지문 " + FP + ")", '저장소와 다른 키는 지워지지 않습니다. 프로젝트 등록과 서버의 복제본은 그대로입니다',
      '[중요] 키를 떼면 이 서버는 그 저장소를 복제하거나 게시하지 못합니다', STAMP];
  }
  return ["제공자 '" + subject + "'의 고아 기록 하나('" + f.project + "')를 지웁니다", '외부에는 아무것도 보내지 않습니다. 제공자에 남아 있을 수 있는 프로젝트는 지워지지 않으니 제공자 화면에서 직접 정리하세요', STAMP];
}
// W3g: what orchd adds to a remove_project proposal (detach_key_available always, the key's identity only when asked)
const isShared = (id) => { const r = ROWS.find((x) => x.id === id); return !!r && PROVIDERS.some((p) => p.id === r.provider && p.kind === 'gitlab'); };
const rpExtra = (b) => ({ detach_key_available: !!b.fields.detach_key,
  ...(b.fields.detach_key ? { deploy_key_title: 'orchd ' + b.subject, deploy_key_fingerprint: FP, deploy_key_shared: isShared(b.subject) } : {}) });
let seq = 0;
async function proposeFor(body, over) {
  const change = await P.computeChange(body.action, body.subject, body.fields);
  seq++;
  const request = seq.toString(16).padStart(32, '0');
  const issued = '2026-11-12T03:00:00Z';
  return {
    action: body.action, subject: body.subject, fields: body.fields,
    message: 'aigate-registry-v1\naction: ' + body.action + '\nsubject: ' + body.subject + '\nchange: ' + change + '\nrequest: ' + request + '\nissued: ' + issued + '\n',
    change, request, issued, preview: previewFor(body.action, body.subject, body.fields), ...(body.action === 'remove_project' ? rpExtra(body) : {}), ...(over || {}),
  };
}

function routesFor(over) {
  const wire = [];
  const rec = (name, fn) => (body, opts) => { wire.push({ name, json: JSON.stringify(body), opts }); return fn(body, opts); };
  const routes = {
    '/api/v1/providers': { providers: PROVIDERS },
    '/api/v1/settings/tokens': { warn_days: 14, tokens: [] },
    '/api/v1/auth/signers': SIGNERS,
    '/api/v1/projects/registry': REG(),
    'POST /api/v1/projects/remove/propose': rec('rp', (b) => proposeFor(b, rpExtra(b))),
    'POST /api/v1/projects/remove/apply': rec('ra', (b) => ({ id: b.subject, removed: true, detach_key: b.fields.detach_key, ...(b.fields.detach_key ? { key_state: 'removed' } : {}), local: { clone: 'removed', key: 'removed' } })),
    'POST /api/v1/projects/delete-remote/propose': rec('dp', (b) => proposeFor(b)),
    'POST /api/v1/projects/delete-remote/apply': rec('da', (b) => ({ id: b.subject, remote_state: 'deleted', remote_deleted_at: '2026-11-13T01:02:03Z', also_local: b.fields.also_local, registry: b.fields.also_local ? 'removed' : 'kept', ...(b.fields.also_local ? { local: { clone: 'removed', key: 'removed' } } : {}) })),
    'POST /api/v1/projects/remove-key/propose': rec('kp', (b) => proposeFor(b, { deploy_key_fingerprint: FP })),
    'POST /api/v1/projects/remove-key/apply': rec('ka', (b) => ({ id: b.subject, key_removed: true })),
    'POST /api/v1/orphans/clear/propose': rec('op', (b) => proposeFor(b)),
    'POST /api/v1/orphans/clear/apply': rec('oa', (b) => ({ provider: b.subject, project: b.fields.project, cleared: true })),
    ...ROWS.reduce((m, r) => ({ ...m, ['/api/v1/projects/' + r.id + '/removal-preview']: rec('pv-' + r.id, () => pvBody(r.id)) }), {}),
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
const rowOf = (ctx, id) => findAll(ctx.root, (e) => e.tagName === 'TR' && text(findAll(e, (c) => c.getAttribute('data-label') === 'ID')[0] || e).trim() === id)[0];
const proposeBtn = (ctx) => byTag(ctx.root, 'button').find((b) => b.getAttribute('data-focus-key') === 'propose');
const fkey = (ctx, k) => byTag(ctx.root, 'button').find((b) => b.getAttribute('data-focus-key') === k);

async function mountProjects(over) {
  const ctx = ctxFor(over);
  PR.projectsView(ctx);
  await until(() => byLabel(ctx.root, '새 프로젝트 만들기').length > 0 || text(ctx.root).includes('불러오지 못했습니다'), 'projects list');
  return ctx;
}
async function openDelete(ctx, id) {
  click(byLabel(rowOf(ctx, id), '삭제…')[0]);
  await until(() => byData(ctx.root, 'choice').length > 0 || byData(ctx.root, 'choice', 'remote-not-offered').length > 0, 'choose screen');
}
async function pick(ctx, key) {
  click(fkey(ctx, 'choice-' + key));
  await until(() => !!proposeBtn(ctx) || byLabel(ctx.root, '다음: 두 번째 확인').length > 0 || byData(ctx.root, 'note', 'del').length > 0, 'preview');
}
const typeInto = (inp, v) => { inp.value = v; fire(inp, 'input'); };
const typed = (ctx) => byId(ctx.root, 'del-typed');
async function alsoAndAck(ctx) {
  byId(ctx.root, 'del-also').checked = true;
  fire(byId(ctx.root, 'del-also'), 'change');
  await until(() => !!byId(ctx.root, 'del-ack'), 'the acknowledgement appears with also_local');
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, 'off until the acknowledgement');
  byId(ctx.root, 'del-ack').checked = true;
  fire(byId(ctx.root, 'del-ack'), 'change');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'enabled');
}
async function sign(ctx) {
  await waitSignable(ctx);
  click(signBtn(ctx));
}
async function toConfirm(ctx, id, key, setup) {
  await openDelete(ctx, id);
  await pick(ctx, key);
  if (setup) await setup();
  click(proposeBtn(ctx));
  await until(() => !!signBtn(ctx) || byData(ctx.root, 'warn', 'mismatch').length > 0, 'confirm');
}

// ---- change-hash vectors from Go --------------------------------------------------

test('TestChangeVectorsMatchGoW3f', async () => {
  assert.ok(VEC.good.length >= 9);
  for (const g of VEC.good) {
    assert.equal(P.canonicalChange(g.action, g.subject, g.fields), g.canonical, g.name);
    assert.equal(await P.computeChange(g.action, g.subject, g.fields), g.change, g.name);
  }
  for (const r of VEC.refused) {
    assert.throws(() => P.canonicalChange(r.action, r.subject, r.fields), { name: 'ChangeError' }, r.name);
  }
  for (const a of ['remove_project', 'delete_remote_repo', 'remove_deploy_key', 'clear_orphan']) assert.ok(VEC.good.some((g) => g.action === a), a);
});

test('TestMessageVectorsAndValidatorsMatchGoW3f', async () => {
  for (const m of VEC.messages) {
    const c = wa.checkRegistryMessage(m.text);
    assert.deepEqual(c, { action: m.action, subject: m.subject, change: m.change, request: m.request, issued: m.issued }, m.name);
    assert.equal(sha(m.text), m.digest, m.name);
  }
  for (const v of VEC.remote_ids) assert.equal(P.REMOTE_ID_RE.test(v.value), v.ok, JSON.stringify(v.value));
  for (const v of VEC.confirm_names) assert.equal(P.validConfirmName(v.value), v.ok, JSON.stringify(v.value));
  for (const v of VEC.orphan_projects) assert.equal(P.validOrphanProject(v.value), v.ok, JSON.stringify(v.value));
});

test('TestSuggestProjectNameVectorsLookAlikes', () => {
  const tricky = ['Kelvin', 'KK', 'İstanbul', 'xİy', '\u{1F600}', 'a\u{1F600}b', '\u{1F600}\u{1F600}', 'ab\u{1F680}cd'];
  for (const r of tricky) assert.ok(NAMES.suggestions.some((s) => s.repo === r), 'the Go file has ' + JSON.stringify(r));
  for (const s of NAMES.suggestions) assert.equal(PR.suggestProjectName(s.repo), s.name, JSON.stringify(s.repo));
});

test('TestGitHubNameDiffersNoteIsCaseInsensitive', () => {
  const pj = src('views/projects.js');
  assert.ok(!/repoName !== s\.subject/.test(pj), 'no exact compare left');
  assert.equal((pj.match(/repoName\.toLowerCase\(\) !== s\.subject/g) || []).length, 2, 'both confirmations compare case-insensitively');
});

// ---- the menu and the preview --------------------------------------------------------

test('TestDeleteMenuDisabledForBusy', async () => {
  const ctx = await mountProjects();
  const btn = byLabel(rowOf(ctx, 'mine'), '삭제…')[0];
  assert.ok(btn, 'a delete button on every row');
  assert.equal(btn.hasAttribute('disabled'), false);
  assert.equal(byId(ctx.root, 'del-busy-hint'), undefined);
  // while the page is busy (a clone is running) the button is off, with the fixed reason in text
  let release;
  const gate = new Promise((r) => { release = r; });
  const ctx2 = await mountProjects({ '/api/v1/projects/registry': REG({ projects: ROWS.map((r) => (r.id === 'mine' ? { ...r, cloned: false } : r)) }), 'POST /api/v1/projects/mine/clone': async () => { await gate; return { cloned: true }; } });
  click(byLabel(rowOf(ctx2, 'mine'), '다시 복제')[0]);
  await until(() => byLabel(rowOf(ctx2, 'toy'), '삭제…')[0].hasAttribute('disabled'), 'disabled while cloning');
  const b2 = byLabel(rowOf(ctx2, 'toy'), '삭제…')[0];
  assert.equal(b2.getAttribute('aria-describedby'), 'del-busy-hint-toy', 'one hint per row');
  assert.equal(text(byId(ctx2.root, 'del-busy-hint-toy')), D.DEL_TEXT.menuBusy);
  click(b2);
  assert.equal(byData(ctx2.root, 'choice').length, 0, 'a click on a disabled button opens nothing');
  release();
  // the preview says busy: the propose button never turns on
  const ctx3 = await mountProjects({ '/api/v1/projects/mine/removal-preview': () => pvBody('mine', { busy: true }) });
  await openDelete(ctx3, 'mine');
  click(fkey(ctx3, 'choice-local'));
  await until(() => byData(ctx3.root, 'block', 'busy').length === 1, 'busy text');
  assert.equal(text(byData(ctx3.root, 'block', 'busy')[0]), D.DEL_TEXT.busy);
  assert.equal(proposeBtn(ctx3).hasAttribute('disabled'), true);
  click(proposeBtn(ctx3));
  await sleep(20);
  assert.equal(posts(ctx3, '/api/v1/projects/remove/propose').length, 0, 'no proposal for a busy project');
  // busy_unknown fails closed with its own sentence
  const ctx4 = await mountProjects({ '/api/v1/projects/mine/removal-preview': () => pvBody('mine', { busy_unknown: true }) });
  await openDelete(ctx4, 'mine');
  click(fkey(ctx4, 'choice-local'));
  await until(() => byData(ctx4.root, 'block', 'busy_unknown').length === 1, 'busy_unknown text');
  assert.equal(proposeBtn(ctx4).hasAttribute('disabled'), true);
});

test('TestPreviewRequestShapeAndCsrfOptIn', async () => {
  const ctx = await mountProjects();
  await openDelete(ctx, 'mine');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  const w = ctx.wire.find((x) => x.name === 'pv-mine');
  assert.equal(w.opts.timeoutMs, 30000);
  assert.equal(w.opts.noRetry429, true);
  assert.equal(w.opts.csrf, true, 'the write-tier preview asks api.js for the CSRF header');
  // api.js sends it on that one GET only
  const calls = [];
  api.configure({ fetch: async (path, init) => { calls.push({ path, init }); return new Response('{}', { status: 200 }); } });
  api.setCsrf('tok');
  await api.get('/api/v1/projects/mine/removal-preview', { csrf: true });
  await api.get('/api/v1/projects/mine/removal-preview');
  await api.get('/api/v1/projects/registry', { csrf: true });
  api.setCsrf(null);
  api.configure();
  assert.equal(calls[0].init.headers['X-Orch-CSRF'], 'tok');
  assert.equal(calls[1].init.headers['X-Orch-CSRF'], undefined, 'not without the opt-in');
  assert.equal(calls[2].init.headers['X-Orch-CSRF'], undefined, 'not for any other path');
});

test('TestDeleteConfirmationListsEveryTarget', async () => {
  const ctx = await mountProjects();
  await openDelete(ctx, 'mine');
  // choose screen: the three options of a created project with a remote
  for (const k of ['local', 'remote', 'key']) assert.ok(fkey(ctx, 'choice-' + k), k);
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  let t = text(byData(ctx.root, 'targets', 'list')[0]);
  assert.ok(t.includes('프로젝트 등록(mine)') && t.includes('/home/orch/repos/mine') && t.includes('배포 키 폴더'), t);
  assert.ok(text(byData(ctx.root, 'kept', 'list')[0]).includes('octo/Hello-World'), 'the remote is listed as staying');
  assert.ok(text(ctx.root).includes('[중요] 되돌릴 수 없습니다'), 'the bold irreversible warning');
  assert.equal(byTag(ctx.root, 'strong').filter((e) => text(e).includes('되돌릴 수 없습니다')).length, 1, 'in bold');
  // the remote option lists the repository, its address and number, and the warning is bold
  click(fkey(ctx, 'back-choose'));
  await until(() => !!fkey(ctx, 'choice-remote'), 'back');
  click(fkey(ctx, 'choice-remote'));
  await until(() => !!typed(ctx), 'remote preview');
  t = text(byData(ctx.root, 'targets', 'list')[0]);
  assert.ok(t.includes('octo/Hello-World') && t.includes('ssh://git@github.com/octo/Hello-World') && t.includes('123456789') && t.includes('GitHub'), t);
  assert.ok(byTag(ctx.root, 'strong').some((e) => text(e).startsWith('[중요] 원격 저장소 삭제는 되돌릴 수 없습니다')));
  // also_local adds the local targets to the list
  assert.ok(!text(byData(ctx.root, 'targets', 'list')[0]).includes('/home/orch/repos/mine'));
  byId(ctx.root, 'del-also').checked = true;
  fire(byId(ctx.root, 'del-also'), 'change');
  await until(() => text(byData(ctx.root, 'targets', 'list')[0]).includes('/home/orch/repos/mine'), 'local targets listed');
  // the key option
  click(fkey(ctx, 'back-choose'));
  await until(() => !!fkey(ctx, 'choice-key'), 'back');
  click(fkey(ctx, 'choice-key'));
  await until(() => !!proposeBtn(ctx), 'key preview');
  assert.ok(text(byData(ctx.root, 'targets', 'list')[0]).includes('octo/Hello-World') && text(byData(ctx.root, 'targets', 'list')[0]).includes('orchd mine'));
});

test('TestPreviewNamesAndContentsAreNeverRendered', async () => {
  // the preview carries counts only; anything else in it (names, contents) is not shown
  const evil = pvBody('mine', { branch: SECRET, file_names: [SECRET], commit_message: SECRET, path: SECRET });
  const ctx = await mountProjects({ '/api/v1/projects/mine/removal-preview': () => evil });
  await openDelete(ctx, 'mine');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  assert.ok(!text(ctx.root).includes('SERVER-WORDS') && !text(ctx.root).includes('<img'));
  assert.equal(findAll(ctx.root, (e) => e.tagName === 'IMG').length, 0);
  // a preview for another project, with a wrong origin or a wrong type, is refused whole
  for (const bad of [{ id: 'toy' }, { origin: 'legacy', legacy: true }, { origin: 'bogus' }, { unpushed_commits: '3' }, { size_bytes: -1 }, { busy: 'no' }, { legacy: true }]) {
    const c = await mountProjects({ '/api/v1/projects/mine/removal-preview': () => pvBody('mine', bad) });
    await openDelete(c, 'mine');
    click(fkey(c, 'choice-local'));
    await until(() => byData(c.root, 'note', 'del').length === 1, JSON.stringify(bad));
    assert.equal(text(byData(c.root, 'note', 'del')[0]), D.DEL_TEXT.previewBad);
    assert.equal(proposeBtn(c), undefined);
  }
  assert.deepEqual(Object.keys(pvBody('mine')).sort(), [...D.PREVIEW_KEYS].sort(), 'the JS key list is the fixture key list');
});

test('TestUnpushedWarningShown', async () => {
  // counts, 'unknown = may have changes' wording, and the acknowledgement that gates the propose button
  const ctx = await mountProjects({ '/api/v1/projects/mine/removal-preview': () => pvBody('mine', { unpushed_commits: 3, changed_files: 0, changed_known: false }) });
  await openDelete(ctx, 'mine');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  const counts = text(byData(ctx.root, 'counts', 'list')[0]);
  assert.ok(counts.includes('원격에 올라가지 않은 커밋: 3개'), counts);
  assert.ok(counts.includes('변경이 있을 수 있습니다'), 'unknown count says may have changes: ' + counts);
  assert.ok(!/수정한 파일: 0개/.test(counts), 'unknown is never shown as zero');
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, 'off until the acknowledgement is checked');
  byId(ctx.root, 'del-ack').checked = true;
  fire(byId(ctx.root, 'del-ack'), 'change');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'on after the check');
  // nothing at risk: no checkbox at all
  const c2 = await mountProjects();
  await openDelete(c2, 'mine');
  click(fkey(c2, 'choice-local'));
  await until(() => !!proposeBtn(c2), 'clean preview');
  assert.equal(byId(c2.root, 'del-ack'), undefined);
  assert.equal(proposeBtn(c2).hasAttribute('disabled'), false);
  // the remote is already gone: the stronger warning and the acknowledgement even for a clean clone
  const c3 = await mountProjects();
  await openDelete(c3, 'gone-app');
  assert.equal(fkey(c3, 'choice-remote'), undefined, 'a deleted remote is not offered again');
  click(fkey(c3, 'choice-local'));
  await until(() => !!proposeBtn(c3), 'gone preview');
  assert.ok(byData(c3.root, 'emph', 'remote-gone').length === 1);
  assert.ok(text(byData(c3.root, 'emph', 'remote-gone')[0]).includes('유일한 사본일 수 있습니다'));
  assert.equal(proposeBtn(c3).hasAttribute('disabled'), true);
});

test('TestRemoteKeyMayRemainHint', async () => {
  const ctx = await mountProjects();
  await openDelete(ctx, 'linked-app');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  assert.equal(byData(ctx.root, 'hint', 'detach-key').length, 1);
  click(fkey(ctx, 'detach-first'));
  await until(() => byData(ctx.root, 'action', 'remove_deploy_key').length === 1, 'key preview');
  const c2 = await mountProjects();
  await openDelete(c2, 'plain');
  click(fkey(c2, 'choice-local'));
  await until(() => !!proposeBtn(c2), 'plain preview');
  assert.equal(byData(c2.root, 'hint', 'detach-key').length, 0, 'no remote, no hint');
});

// ---- remote deletion: who may, and the typed name ----------------------------------------

test('TestLegacyRemoteNeverDeletable', async () => {
  for (const [id, why] of [['toy', 'legacy'], ['plain', 'unknown'], ['linked-app', 'linked'], ['gone-app', 'gone'], ['noid', 'id']]) {
    const ctx = await mountProjects();
    await openDelete(ctx, id);
    assert.equal(fkey(ctx, 'choice-remote'), undefined, id + ': no remote deletion button');
    assert.equal(byData(ctx.root, 'choice', 'remote').length, 0);
    assert.ok(text(byData(ctx.root, 'choice', 'remote-not-offered')[0]).includes(D.DEL_TEXT.noRemoteDelete[why]), id);
    assert.equal(D.deleteOptions(ROWS.find((r) => r.id === id)).remoteOffered, false, id);
  }
  // a created project with a remote and a number is the only one offered
  assert.equal(D.deleteOptions(ROWS.find((r) => r.id === 'mine')).remoteOffered, true);
  assert.equal(D.deleteOptions(ROWS.find((r) => r.id === 'lab')).remoteOffered, true);
  // a row whose origin is missing or odd is never guessed to be created
  assert.equal(D.deleteOptions({ ...ROWS[0], origin: undefined }).remoteOffered, false);
  assert.equal(D.deleteOptions({ ...ROWS[0], origin: 'Created' }).remoteOffered, false);
  assert.equal(D.deleteOptions({ ...ROWS[0], origin: 'bogus' }).origin, 'unknown');
  // the key option is for any project with a remote link, not for one without
  assert.equal(D.deleteOptions(ROWS.find((r) => r.id === 'linked-app')).key, true);
  assert.equal(D.deleteOptions(ROWS.find((r) => r.id === 'toy')).key, false);
});

test('TestTypedNameRequired', async () => {
  const ctx = await mountProjects();
  await openDelete(ctx, 'mine');
  click(fkey(ctx, 'choice-remote'));
  await until(() => !!typed(ctx), 'remote preview');
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, 'off before anything is typed');
  for (const wrong of ['hello-world', 'HELLO-WORLD', 'Hello-Worl', 'Hello-World ', ' Hello-World', 'octo/Hello-World', 'mine']) {
    typeInto(typed(ctx), wrong);
    await sleep(5);
    assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, JSON.stringify(wrong));
    click(proposeBtn(ctx));
    await sleep(5);
    assert.equal(posts(ctx, '/api/v1/projects/delete-remote/propose').length, 0, 'nothing proposed for ' + JSON.stringify(wrong));
  }
  typeInto(typed(ctx), 'Hello-World');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'on once the exact name is typed');
  // the typed name is the repository name, not the project id, and the field is a plain text input
  assert.equal(typed(ctx).getAttribute('type'), 'text');
  assert.equal(typed(ctx).getAttribute('autocomplete'), 'off');
  // a nested GitLab path asks for the last segment only
  const c2 = await mountProjects();
  await openDelete(c2, 'lab');
  click(fkey(c2, 'choice-remote'));
  await until(() => !!typed(c2), 'lab preview');
  typeInto(typed(c2), 'team/lab');
  await sleep(5);
  assert.equal(proposeBtn(c2).hasAttribute('disabled'), true);
  typeInto(typed(c2), 'lab');
  await until(() => !proposeBtn(c2).hasAttribute('disabled'), 'lab on');
});

test('TestRemoteDeleteSignedFieldsAndAlsoLocalSingleSignature', async () => {
  const ctx = await mountProjects();
  await toConfirm(ctx, 'mine', 'remote', async () => {
    typeInto(typed(ctx), 'Hello-World');
    await alsoAndAck(ctx);
  });
  const prop = JSON.parse(posts(ctx, '/api/v1/projects/delete-remote/propose')[0].body ? JSON.stringify(posts(ctx, '/api/v1/projects/delete-remote/propose')[0].body) : '{}');
  assert.deepEqual(prop, { action: 'delete_remote_repo', subject: 'mine', fields: { remote_path: 'octo/Hello-World', remote_id: '123456789', confirm_name: 'Hello-World', also_local: true } });
  assert.equal(posts(ctx, '/api/v1/projects/delete-remote/propose')[0].opts.timeoutMs, 30000);
  await sign(ctx);
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  const apply = posts(ctx, '/api/v1/projects/delete-remote/apply');
  assert.equal(apply.length, 1);
  assert.equal(apply[0].opts.timeoutMs, 130000);
  assert.equal(apply[0].opts.noRetry429, true);
  assert.deepEqual(apply[0].body.fields, prop.fields);
  assert.equal(auth.gets.length, 1, 'one passkey signature');
  // also_local never chains a second signature or a second apply by itself
  await sleep(40);
  assert.equal(auth.gets.length, 1);
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0, 'the follow-up is never started by itself');
  assert.equal(posts(ctx, '/api/v1/projects/remove/apply').length, 0);
  // the result: steps with fixed texts; W3g (G-D7): also_local removed the registration in the same signature,
  // so there is no follow-up button and the registry step says removed (W3f asserted '남음' and the follow-up here)
  const steps = text(findAll(ctx.root, (e) => e.className === 'steps')[0]);
  assert.ok(steps.includes('원격 저장소 삭제') && steps.includes('서버의 복제본 폴더') && !steps.includes('남음'), steps);
  assert.equal(byData(ctx.root, 'followup', 'remove').length, 0);
});

test('TestFollowUpRemoveRegistryIsSecondSignedChange', async () => {
  // W3g: the follow-up exists only after a remote deletion WITHOUT also_local (W3f ran this with also_local=true)
  let regAfter = REG();
  const ctx = await mountProjects({
    '/api/v1/projects/registry': () => regAfter,
    '/api/v1/projects/mine/removal-preview': () => pvBody('mine', {}, regAfter.projects.find((r) => r.id === 'mine')),
  });
  await toConfirm(ctx, 'mine', 'remote', async () => {
    typeInto(typed(ctx), 'Hello-World');
    await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'enabled');
  });
  // after the delete the registry row is: remote null, remote_deleted_at set
  regAfter = REG({ projects: ROWS.map((r) => (r.id === 'mine' ? { ...r, gitlab: null, remote_deleted_at: '2026-11-13T01:02:03Z' } : r)) });
  await sign(ctx);
  await until(() => byData(ctx.root, 'followup', 'remove').length === 1, 'result');
  assert.equal(text(fkey(ctx, 'follow-up')), '복제본과 등록 지우기…');
  click(fkey(ctx, 'follow-up'));
  await until(() => !!proposeBtn(ctx), 'follow-up preview');
  assert.equal(byData(ctx.root, 'note', 'follow-up').length, 1);
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0, 'still nothing proposed: the person must press the button');
  assert.ok(byData(ctx.root, 'emph', 'remote-gone').length === 1, 'the stronger warning: the clone may be the only copy');
  byId(ctx.root, 'del-ack').checked = true;
  fire(byId(ctx.root, 'del-ack'), 'change');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'enabled after the check');
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.equal(auth.gets.length, 1, 'no signature yet for the second change');
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'second result');
  assert.equal(auth.gets.length, 2, 'a second, separate signature');
  assert.equal(posts(ctx, '/api/v1/projects/remove/apply').length, 1);
  assert.deepEqual(posts(ctx, '/api/v1/projects/remove/apply')[0].body.fields, { scope: 'local', legacy_ack: false, detach_key: false });
});

// ---- legacy -------------------------------------------------------------------------------

test('TestLegacyDeleteNeedsSecondConfirmationAndLegacyAck', async () => {
  const ctx = await mountProjects({ '/api/v1/projects/toy/removal-preview': () => pvBody('toy', { unpushed_commits: 2, changed_files: 5 }) });
  await openDelete(ctx, 'toy');
  click(fkey(ctx, 'choice-local'));
  await until(() => byLabel(ctx.root, '다음: 두 번째 확인').length === 1, 'preview of a legacy project');
  // no propose button anywhere on the first screen, and clicking "next" is the only way on
  assert.equal(proposeBtn(ctx), undefined, 'no propose button before the second confirmation');
  byId(ctx.root, 'del-ack').checked = true;
  fire(byId(ctx.root, 'del-ack'), 'change');
  await until(() => !byLabel(ctx.root, '다음: 두 번째 확인')[0].hasAttribute('disabled'), 'next enabled');
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0);
  click(byLabel(ctx.root, '다음: 두 번째 확인')[0]);
  await until(() => byData(ctx.root, 'screen', 'legacy-confirm').length === 1, 'second screen');
  const t = text(ctx.root);
  assert.ok(t.includes('이것은 처음부터 있던 프로젝트입니다') && t.includes('서버의 복제본과 등록이 지워지고') && t.includes('더 작업을 낼 수 없습니다'), t);
  assert.ok(text(byData(ctx.root, 'counts', 'legacy')[0]).includes('2개') && text(byData(ctx.root, 'counts', 'legacy')[0]).includes('5개'));
  // the project name must be typed again, exactly
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true);
  for (const wrong of ['', 'Toy', 'toy ', 'to']) {
    typeInto(byId(ctx.root, 'del-legacy-typed'), wrong);
    await sleep(5);
    assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, JSON.stringify(wrong));
    click(proposeBtn(ctx));
    await sleep(5);
    assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0);
  }
  typeInto(byId(ctx.root, 'del-legacy-typed'), 'toy');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'on');
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.deepEqual(posts(ctx, '/api/v1/projects/remove/propose')[0].body, { action: 'remove_project', subject: 'toy', fields: { scope: 'local', legacy_ack: true, detach_key: false } });
  click(signBtn(ctx));
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  assert.equal(posts(ctx, '/api/v1/projects/remove/apply')[0].body.fields.legacy_ack, true, 'the signed legacy_ack travels in the apply');
  // a non-legacy project never sends legacy_ack true
  const c2 = await mountProjects();
  await toConfirm(c2, 'plain', 'local');
  assert.deepEqual(posts(c2, '/api/v1/projects/remove/propose')[0].body.fields, { scope: 'local', legacy_ack: false, detach_key: false });
});

test('TestLegacyProposalWithoutAckLineIsRefused', async () => {
  // the server answers a legacy proposal whose preview lacks (or adds) the legacy line: no sign button
  for (const [id, drop] of [['toy', true], ['plain', false]]) {
    const ctx = await mountProjects({
      'POST /api/v1/projects/remove/propose': async (b) => {
        const r = await proposeFor(b);
        r.preview = drop ? r.preview.filter((x) => !x.includes('(legacy)')) : [...r.preview.slice(0, -1), '[중요] 처음부터 있던(legacy) 프로젝트입니다. 지우면 이 저장소로는 더 작업을 낼 수 없습니다', STAMP];
        return r;
      },
    });
    await openDelete(ctx, id);
    click(fkey(ctx, 'choice-local'));
    await until(() => !!proposeBtn(ctx) || byLabel(ctx.root, '다음: 두 번째 확인').length === 1, 'preview');
    if (id === 'toy') {
      click(byLabel(ctx.root, '다음: 두 번째 확인')[0]);
      await until(() => !!byId(ctx.root, 'del-legacy-typed'), 'legacy');
      typeInto(byId(ctx.root, 'del-legacy-typed'), 'toy');
      await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'on');
    }
    click(proposeBtn(ctx));
    await until(() => byData(ctx.root, 'warn', 'mismatch').length === 1, 'mismatch for ' + id);
    assert.equal(signBtn(ctx), undefined);
  }
});

// ---- the sign gate ----------------------------------------------------------------------------

test('TestSignOnlyWhenChangeMatchesW3f', async () => {
  const cases = {
    'a wrong change hash': (r) => ({ ...r, change: sha('x') }),
    'a wrong message': (r) => ({ ...r, message: r.message.replace('request: ', 'request: 1') }),
    'a wrong subject in the message': (r) => ({ ...r, message: r.message.replace('subject: mine', 'subject: other') }),
    'a message that carries another change': (r) => ({ ...r, message: r.message.replace(r.change, sha('another')) }),
    'a wrong request id': (r) => ({ ...r, request: 'f'.repeat(32) }),
    'a wrong issued time': (r) => ({ ...r, issued: '2030-01-01T00:00:00Z' }),
    'another echoed field': (r) => ({ ...r, fields: { ...r.fields, also_local: !r.fields.also_local } }),
    'a changed remote id echoed': (r) => ({ ...r, fields: { ...r.fields, remote_id: '999' } }),
    'a preview that does not mention the repository': (r) => ({ ...r, preview: r.preview.map((x) => x.replace(/octo\/Hello-World/g, 'someone/else')) }),
    'a missing irreversible line': (r) => ({ ...r, preview: r.preview.filter((x) => !x.includes('[중요] 원격 저장소 ')) }),
    'an also_local line when not asked': (r) => ({ ...r, preview: [...r.preview, "원격 삭제가 끝나면 이 서버의 복제본 폴더(/x)와 이 프로젝트의 배포 키 폴더도 지웁니다", STAMP] }),
    'an empty preview': (r) => ({ ...r, preview: [] }),
    'a hostile preview line': (r) => ({ ...r, preview: [...r.preview, 'x‮evil'] }),
  };
  for (const [name, mutate] of Object.entries(cases)) {
    const ctx = await mountProjects({ 'POST /api/v1/projects/delete-remote/propose': async (b) => mutate(await proposeFor(b)) });
    await toConfirm(ctx, 'mine', 'remote', async () => {
      typeInto(typed(ctx), 'Hello-World');
      await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'enabled');
    });
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, name + ': the fixed warning');
    assert.equal(signBtn(ctx), undefined, name + ': no sign button');
    assert.equal(auth.gets.length, 0, name + ': nothing signed');
    assert.equal(posts(ctx, '/api/v1/projects/delete-remote/apply').length, 0);
  }
  // and the honest answer is signable
  const ok = await mountProjects();
  await toConfirm(ok, 'mine', 'remote', async () => { typeInto(typed(ok), 'Hello-World'); await until(() => !proposeBtn(ok).hasAttribute('disabled'), 'enabled'); });
  assert.ok(signBtn(ok));
  const facts = Object.fromEntries(byData(ok.root, 'field').map((e) => [e.getAttribute('data-field'), text(e)]));
  assert.ok(facts.remote_path.includes('octo/Hello-World') && facts.remote_id.includes('123456789') && facts.confirm_name.includes('Hello-World'));
});

test('TestSignGateForEveryDeleteAction', async () => {
  const actions = [
    ['mine', 'local', 'POST /api/v1/projects/remove/propose'],
    ['mine', 'key', 'POST /api/v1/projects/remove-key/propose'],
  ];
  for (const [id, key, route] of actions) {
    const ctx = await mountProjects({ [route]: async (b) => ({ ...(await proposeFor(b)), change: sha('x') }) });
    await toConfirm(ctx, id, key);
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, route);
    assert.equal(signBtn(ctx), undefined);
  }
  const co = await mountProjects({ 'POST /api/v1/orphans/clear/propose': async (b) => ({ ...(await proposeFor(b)), change: sha('x') }) });
  click(byLabel(co.root, '기록 지우기')[0]);
  await until(() => byData(co.root, 'orphan-origin').length === 1, 'orphan screen');
  click(proposeBtn(co));
  await until(() => byData(co.root, 'warn', 'mismatch').length === 1, 'orphan mismatch');
  assert.equal(signBtn(co), undefined);
});

// ---- results ------------------------------------------------------------------------------------

test('TestDeletePartialScreen', async () => {
  const detail = { remaining: ['clone'], clone: 'kept_in_place', key: 'removed', registry_removed: true, remote_deleted: false, reason: 'remove_failed' };
  const ctx = await mountProjects({ 'POST /api/v1/projects/remove/apply': () => { throw apiError(502, 'delete_partial_local', SECRET, { detail }); } });
  await toConfirm(ctx, 'plain', 'local');
  await sign(ctx);
  await until(() => byData(ctx.root, 'result', 'partial-local').length === 1, 'partial screen');
  const steps = findAll(ctx.root, (e) => e.className === 'steps')[0];
  const st = (k) => text(findAll(steps, (e) => e.getAttribute('data-step') === k)[0]);
  assert.ok(st('registry').includes('지웠습니다'), st('registry'));
  assert.ok(st('remote').includes('그대로 있습니다'));
  assert.ok(st('clone').includes('지우지 못했습니다(원래 자리에 그대로 있음)'), st('clone'));
  assert.ok(st('key').includes('지웠습니다'));
  assert.ok(text(byData(ctx.root, 'note', 'reason')[0]).includes('서버가 폴더를 지우지 못했습니다'));
  assert.ok(text(byData(ctx.root, 'note', 'todo')[0]).includes('등록은 이미 지워져'), 'what to do');
  assert.ok(!text(ctx.root).includes('SERVER-WORDS'));
  assert.equal(signBtn(ctx), undefined, 'a spent proposal is never signed again');
  assert.equal(auth.gets.length, 1);
  // the other shapes: moved aside, registry kept
  const d2 = { remaining: ['clone', 'key'], clone: 'moved_aside', key: 'kept_in_place', registry_removed: false, remote_deleted: true, reason: 'project_busy' };
  const c2 = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => { throw apiError(502, 'delete_partial_local', SECRET, { detail: d2 }); } });
  await toConfirm(c2, 'mine', 'remote', async () => { typeInto(typed(c2), 'Hello-World'); await alsoAndAck(c2); });
  await sign(c2);
  await until(() => byData(c2.root, 'result', 'partial-local').length === 1, 'partial 2');
  assert.ok(text(c2.root).includes('.deleting-'));
  assert.ok(text(byData(c2.root, 'note', 'todo')[0]).includes('등록은 남아 있습니다'));
  // an invalid detail is never shown; the page says it cannot tell
  for (const bad of [null, { ...detail, clone: 'weird' }, { ...detail, remaining: ['x'] }, { ...detail, reason: SECRET }, { ...detail, registry_removed: 'yes' }]) {
    const c3 = await mountProjects({ 'POST /api/v1/projects/remove/apply': () => { throw apiError(502, 'delete_partial_local', SECRET, bad === null ? {} : { detail: bad }); } });
    await toConfirm(c3, 'plain', 'local');
    await sign(c3);
    await until(() => byData(c3.root, 'result', 'partial-local').length === 1, 'bad detail');
    assert.equal(byData(c3.root, 'note', 'detail-invalid').length, 1);
    assert.ok(!text(c3.root).includes('SERVER-WORDS') && !text(c3.root).includes('weird'));
  }
});

test('TestRealApiLayerKeepsDeletePartialLocalDetail', async () => {
  api.configure({ fetch: async () => new Response(JSON.stringify({ error: { code: 'delete_partial_local', message: 'm', detail: { remaining: ['clone'] }, secret: 'x' } }), { status: 502 }) });
  let err;
  try { await api.post('/api/v1/projects/remove/apply', {}); } catch (e) { err = e; }
  assert.deepEqual(err.detail, { remaining: ['clone'] });
  api.configure({ fetch: async () => new Response(JSON.stringify({ error: { code: 'project_busy', message: 'm', detail: { a: 1 } } }), { status: 409 }) });
  try { await api.post('/api/v1/projects/remove/apply', {}); } catch (e) { err = e; }
  assert.equal(err.detail, undefined, 'a detail only arrives on the codes that carry one');
  api.configure();
});

test('TestOutcomeUnknownNoAutoRetry', async () => {
  const cases = [
    ['timeout', () => { throw apiError(0, 'timeout', 'x'); }],
    ['outcome_unknown', () => { throw apiError(504, 'outcome_unknown', SECRET); }],
    ['a gateway error', () => { throw apiError(502, 'bad_gateway', SECRET); }],
  ];
  for (const [name, thrower] of cases) {
    auth = new Auth();
    wa.configure({ credentials: auth, subtle: webcrypto.subtle });
    let regReads = 0;
    const ctx = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': thrower, '/api/v1/projects/registry': () => { regReads++; return REG(); } });
    await toConfirm(ctx, 'mine', 'remote', async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); });
    const before = regReads;
    await sign(ctx);
    await until(() => byData(ctx.root, 'result', 'unknown').length === 1, name + ' unknown screen');
    assert.equal(text(byData(ctx.root, 'result', 'unknown')[0]), D.DEL_TEXT.unknown.delete_remote_repo, name);
    await until(() => regReads > before, 'the lists are read again');
    await sleep(60);
    assert.equal(posts(ctx, '/api/v1/projects/delete-remote/apply').length, 1, name + ': never retried');
    assert.equal(posts(ctx, '/api/v1/projects/delete-remote/propose').length, 1, name + ': never proposed again');
    assert.equal(auth.gets.length, 1, name + ': never signed again');
    assert.equal(signBtn(ctx), undefined);
    assert.ok(!text(ctx.root).includes('SERVER-WORDS'));
    assert.equal(byLabel(ctx.root, '다시').length, 0, 'no retry button');
  }
  // delete_partial after a lost DELETE response: may or may not be deleted; check the provider; then remove_project
  auth = new Auth();
  wa.configure({ credentials: auth, subtle: webcrypto.subtle });
  const ctx = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => { throw apiError(504, 'delete_partial', SECRET); } });
  await toConfirm(ctx, 'mine', 'remote', async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); });
  await sign(ctx);
  await until(() => byData(ctx.root, 'note', 'partial-remote').length === 1, 'partial remote');
  const t = text(ctx.root);
  assert.ok(t.includes('지워졌을 수도, 아닐 수도 있습니다') && t.includes('GitHub·GitLab 화면에서') && t.includes('복제본과 등록 지우기'), t);
  await sleep(40);
  assert.equal(posts(ctx, '/api/v1/projects/delete-remote/apply').length, 1);
  assert.equal(auth.gets.length, 1);
});

test('TestAllNewErrorCodesFixedTextsW3f', async () => {
  const NEW = ['project_busy', 'busy_check_failed', 'local_unsafe', 'key_unavailable_for_removal', 'no_remote', 'delete_partial_local', 'delete_rate_limited', 'busy', 'orphan_unknown',
    'repo_being_removed', 'legacy_ack_required', 'origin_unknown', 'not_created_by_tool', 'remote_changed', 'confirm_mismatch', 'delete_partial', 'key_not_found', 'remove_refused'];
  for (const c of NEW) {
    assert.ok(CODES.includes(c), c + ' is in the Go-generated list');
    assert.ok(Object.prototype.hasOwnProperty.call(P.ERROR_TEXT, c), c + ' has a fixed sentence');
    assert.equal(P.failureText(apiError(409, c, SECRET)), P.ERROR_TEXT[c], c);
    assert.ok(!P.ERROR_TEXT[c].includes('SERVER'));
  }
  for (const code of CODES) assert.ok(Object.prototype.hasOwnProperty.call(P.ERROR_TEXT, code), 'no fixed text for ' + code);
  // as the real screens show them: a refusal at propose and one at apply, never the server's words
  for (const code of ['project_busy', 'origin_unknown', 'legacy_ack_required', 'not_created_by_tool', 'remote_changed', 'confirm_mismatch', 'local_unsafe', 'busy_check_failed', 'delete_rate_limited']) {
    const status = code === 'busy_check_failed' ? 503 : code === 'delete_rate_limited' ? 429 : 409;
    const c1 = await mountProjects({ 'POST /api/v1/projects/remove/propose': () => { throw apiError(status, code, SECRET); } });
    await openDelete(c1, 'plain');
    click(fkey(c1, 'choice-local'));
    await until(() => !!proposeBtn(c1), 'preview');
    click(proposeBtn(c1));
    await until(() => byData(c1.root, 'note', 'del').length === 1, code + ' at propose');
    assert.ok(text(byData(c1.root, 'note', 'del')[0]).includes(P.ERROR_TEXT[code]), code);
    assert.ok(!text(c1.root).includes('SERVER-WORDS'));
    assert.equal(signBtn(c1), undefined);
    const c2 = await mountProjects({ 'POST /api/v1/projects/remove/apply': () => { throw apiError(status, code, SECRET); } });
    await toConfirm(c2, 'plain', 'local');
    await sign(c2);
    await until(() => byData(c2.root, 'result', 'error').length === 1 || byData(c2.root, 'result', 'unknown').length === 1, code + ' at apply');
    assert.equal(byData(c2.root, 'result', 'error').length, 1, code + ': a refusal, not an unknown outcome');
    assert.equal(byData(c2.root, 'result', 'unknown').length, 0, code);
    assert.ok(text(byData(c2.root, 'result', 'error')[0]).includes(P.ERROR_TEXT[code]), code);
    assert.ok(!text(c2.root).includes('SERVER-WORDS'));
    assert.equal(signBtn(c2), undefined, 'a refused change is never signed again');
  }
  // an unknown code falls back to a fixed generic sentence
  const c3 = await mountProjects({ 'POST /api/v1/projects/remove/apply': () => { throw apiError(418, 'something_new_w3f', SECRET); } });
  await toConfirm(c3, 'plain', 'local');
  await sign(c3);
  await until(() => byData(c3.root, 'result', 'error').length === 1, 'generic');
  assert.ok(!text(c3.root).includes('SERVER-WORDS'));
});

// ---- other actions and orphans -----------------------------------------------------------------

test('TestRemoveDeployKeyFlow', async () => {
  const ctx = await mountProjects();
  await toConfirm(ctx, 'linked-app', 'key');
  assert.deepEqual(posts(ctx, '/api/v1/projects/remove-key/propose')[0].body, { action: 'remove_deploy_key', subject: 'linked-app', fields: { remote_path: 'team/app-one' } });
  await sign(ctx);
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  assert.deepEqual(posts(ctx, '/api/v1/projects/remove-key/apply')[0].body.fields, { remote_path: 'team/app-one' });
  assert.equal(posts(ctx, '/api/v1/projects/remove-key/apply')[0].opts.timeoutMs, 130000);
  assert.ok(text(ctx.root).includes('배포 키를 제공자 저장소에서 뗐습니다'));
  // key_not_found
  const c2 = await mountProjects({ 'POST /api/v1/projects/remove-key/apply': () => { throw apiError(404, 'key_not_found', SECRET); } });
  await toConfirm(c2, 'linked-app', 'key');
  await sign(c2);
  await until(() => byData(c2.root, 'result', 'error').length === 1, 'not found');
  assert.ok(text(c2.root).includes(P.ERROR_TEXT.key_not_found));
});

test('TestOrphanClearOriginAwareTexts', async () => {
  const ctx = await mountProjects();
  const btns = byLabel(ctx.root, '기록 지우기');
  assert.equal(btns.length, 3, 'one per orphan');
  const want = [D.DEL_TEXT.orphanCreated, D.DEL_TEXT.orphanLinked, D.DEL_TEXT.orphanUnknown];
  const subj = [['gh', 'octo/half-made', 'created'], ['gh', 'octo/Existing-Repo', 'linked'], ['gl', 'team/mystery', 'unknown']];
  for (let i = 0; i < 3; i++) {
    const c = await mountProjects();
    click(byLabel(c.root, '기록 지우기')[i]);
    await until(() => byData(c.root, 'orphan-origin').length === 1, 'screen ' + i);
    assert.equal(byData(c.root, 'orphan-origin')[0].getAttribute('data-orphan-origin'), subj[i][2]);
    assert.ok(text(byData(c.root, 'orphan-origin')[0]).includes(want[i]));
    click(proposeBtn(c));
    await waitSignable(c);
    assert.deepEqual(posts(c, '/api/v1/orphans/clear/propose')[0].body, { action: 'clear_orphan', subject: subj[i][0], fields: { project: subj[i][1] } });
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'ok').length === 1, 'cleared ' + i);
    assert.deepEqual(posts(c, '/api/v1/orphans/clear/apply')[0].body.fields, { project: subj[i][1] });
    assert.ok(text(c.root).includes('제공자에는 아무것도 하지 않았습니다'));
  }
  assert.ok(D.DEL_TEXT.orphanCreated.includes('직접 지운 뒤') && D.DEL_TEXT.orphanLinked.includes('절대 지우지 마세요') && D.DEL_TEXT.orphanUnknown.includes('먼저 확인'));
  // a hostile orphan record (bad project path) gets no button at all
  const evil = await mountProjects({ '/api/v1/projects/registry': REG({ orphans: [{ ...ORPHANS[0], project: 'octo/' + SECRET }] }) });
  assert.equal(byLabel(evil.root, '기록 지우기').length, 0);
});

test('TestSubmitOnceAndNoRetry429', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const ctx = await mountProjects({ 'POST /api/v1/projects/remove/propose': async (b) => { await gate; return proposeFor(b); } });
  await openDelete(ctx, 'plain');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  const pb = proposeBtn(ctx);
  click(pb);
  click(pb);
  await sleep(10);
  release();
  await waitSignable(ctx);
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 1, 'a double click proposes once');
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose')[0].opts.noRetry429, true);
  const sb = signBtn(ctx);
  click(sb);
  click(sb);
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  assert.equal(posts(ctx, '/api/v1/projects/remove/apply').length, 1, 'a double click applies once');
  assert.equal(auth.gets.length, 1);
  // cancel before the signature sends nothing and clears the screen
  const c2 = await mountProjects();
  await toConfirm(c2, 'plain', 'local');
  click(fkey(c2, 'cancel'));
  await until(() => !signBtn(c2), 'left the confirmation');
  assert.equal(posts(c2, '/api/v1/projects/remove/apply').length, 0);
  // a 429 at apply is shown as the fixed sentence, once, not retried
  const c3 = await mountProjects({ 'POST /api/v1/projects/remove/apply': () => { throw apiError(429, 'delete_rate_limited', SECRET); } });
  await toConfirm(c3, 'plain', 'local');
  await sign(c3);
  await until(() => byData(c3.root, 'result', 'error').length === 1, '429');
  assert.equal(posts(c3, '/api/v1/projects/remove/apply').length, 1);
  assert.equal(posts(c3, '/api/v1/projects/remove/apply')[0].opts.noRetry429, true);
});

// ---- static rules ---------------------------------------------------------------------------------

test('TestNoEmptyNotesAndUniqueDescribedByIds', async () => {
  const ctx = await mountProjects();
  await openDelete(ctx, 'plain');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  for (const e of byTag(ctx.root, 'p')) assert.ok(text(e).trim() !== '', 'an empty <p>: ' + e.className);
  // every aria-describedby of the list points at an element that exists, and ids are unique
  const c2 = await mountProjects();
  const ids = findAll(c2.root, (e) => e.hasAttribute('id')).map((e) => e.getAttribute('id'));
  assert.equal(new Set(ids).size, ids.length, 'ids are unique');
  let release;
  const gate = new Promise((r) => { release = r; });
  const c3 = await mountProjects({ '/api/v1/projects/registry': REG({ projects: ROWS.map((r) => (r.id === 'mine' ? { ...r, cloned: false } : r)) }), 'POST /api/v1/projects/mine/clone': async () => { await gate; return { cloned: true }; } });
  click(byLabel(rowOf(c3, 'mine'), '다시 복제')[0]);
  await until(() => byLabel(rowOf(c3, 'toy'), '삭제…')[0].hasAttribute('disabled'), 'disabled');
  const all = findAll(c3.root, (e) => e.hasAttribute('id')).map((e) => e.getAttribute('id'));
  assert.equal(new Set(all).size, all.length, 'one hint per row, unique ids even while busy');
  for (const b of byLabel(c3.root, '삭제…')) {
    const d = b.getAttribute('aria-describedby');
    assert.ok(d && all.includes(d), 'describedby ' + d + ' exists');
  }
  // the orphan button is off as well while a clone runs
  for (const b of byLabel(c3.root, '기록 지우기')) assert.equal(b.hasAttribute('disabled'), true);
  release();
});

test('TestNoStorageNoNavigateTrapW3f', async () => {
  for (const name of ['views/delete.js', 'views/providers.js', 'views/projects.js', 'api.js']) {
    const c = src(name);
    for (const bad of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'caches.', 'history.pushState', 'history.replaceState', 'navigator.storage', 'ctx.navigate',
      'navigate(', 'location.hash', 'confirm(', 'alert(', 'innerHTML', 'outerHTML', 'insertAdjacentHTML', 'Date.now', 'new Date', 'eval(', 'new Function', 'document.write']) {
      assert.ok(!c.includes(bad), name + ' uses ' + bad);
    }
    assert.ok(!/(?:^|[^.\w])(?:confirm|alert|prompt)\s*\(/.test(c), name);
    assert.ok(!/\.setAttribute\(/.test(c.replace(/node\.setAttribute\(k, String\(value\)\)/g, '')) || name === 'api.js', name + ': attributes go through el()');
  }
  // every ctx member the new view uses exists in the REAL app.js makeCtx
  const app = src('app.js');
  const makeCtx = app.slice(app.indexOf('function makeCtx'), app.indexOf('async function show'));
  for (const m of new Set([...src('views/delete.js').matchAll(/\bctx\.([A-Za-z]+)/g)].map((x) => x[1]))) {
    assert.ok(new RegExp('\\b' + m + '\\b').test(makeCtx), 'ctx.' + m + ' is not in app.js makeCtx');
  }
  // no password-like markup anywhere
  const dir = new URL('../static/', import.meta.url);
  const walk = (u) => readdirSync(u, { withFileTypes: true }).flatMap((d) => (d.isDirectory() ? walk(new URL(d.name + '/', u)) : [new URL(d.name, u)]));
  for (const f of walk(dir)) {
    if (!/\.(js|css|html)$/.test(f.pathname)) continue;
    const t = readFileSync(f, 'utf8');
    assert.ok(!/text-?security/i.test(t) && !/type\s*[:=]\s*['"`]?password/i.test(t) && !/autocomplete['"]?\s*[:=]\s*['"](?:new-password|current-password|username)/i.test(t), f.pathname);
  }
  // the real app context has no navigate, and nothing is stored while the flows run
  const had = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  try {
    const ctx = await mountProjects();
    assert.equal(ctx.navigate, undefined);
    await toConfirm(ctx, 'plain', 'local');
    await sign(ctx);
    await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'done');
    assert.deepEqual(ctx.nav, [], 'the flow never writes location.hash');
  } finally {
    if (had) Object.defineProperty(globalThis, 'localStorage', had); else delete globalThis.localStorage;
    delete globalThis.sessionStorage;
  }
  // the new inputs are plain text fields without credential hints
  const dj = src('views/delete.js');
  assert.ok(/id: 'del-typed'[^}]*autocomplete: 'off'/.test(dj));
  assert.ok(/id: 'del-legacy-typed'[^}]*autocomplete: 'off'/.test(dj));
});

test('TestHostileServerTextNeverRenderedAsMarkupW3f', async () => {
  const hostile = SECRET + '‮';
  const ctx = await mountProjects({
    '/api/v1/projects/registry': REG({ projects: [...ROWS, row('evil', { path: '/home/orch/repos/' + hostile, origin: 'created' })] }),
    '/api/v1/projects/evil/removal-preview': () => pvBody('evil', {}, row('evil', { origin: 'created' })),
  });
  await openDelete(ctx, 'evil');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'preview');
  assert.equal(findAll(ctx.root, (e) => e.tagName === 'IMG').length, 0);
  assert.ok(!text(ctx.root).includes('‮'));
  for (const e of findAll(ctx.root, () => true)) for (const [k, v] of e.attrs) assert.ok(!v.includes('<img') && !v.includes('SERVER-WORDS'), k);
});

// ===== F4 review round 1 (red first) =====================================================

test('TestAlsoLocalNeedsAckAndSingleSignatureWarning', async () => {
  // clean counts, nothing at risk by the numbers: also_local still needs the acknowledgement
  const ctx = await mountProjects();
  await openDelete(ctx, 'mine');
  click(fkey(ctx, 'choice-remote'));
  await until(() => !!typed(ctx), 'remote preview');
  assert.equal(byId(ctx.root, 'del-ack'), undefined, 'no acknowledgement for a remote-only deletion with clean counts');
  assert.equal(byData(ctx.root, 'emph', 'also-local').length, 0);
  typeInto(typed(ctx), 'Hello-World');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'remote only enabled');
  byId(ctx.root, 'del-also').checked = true;
  fire(byId(ctx.root, 'del-also'), 'change');
  await until(() => !!byId(ctx.root, 'del-ack'), 'ack appears');
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, 'off until acknowledged');
  const w = byData(ctx.root, 'emph', 'also-local');
  assert.equal(w.length, 1);
  assert.ok(text(w[0]).includes('같은 서명으로') && text(w[0]).includes('유일한 사본일 수 있') && text(w[0]).includes('복제본'), text(w[0]));
  assert.equal(byTag(w[0], 'strong').length, 1, 'bold');
  assert.equal(byData(ctx.root, 'emph', 'irreversible').length, 1);
  click(proposeBtn(ctx));
  await sleep(10);
  assert.equal(posts(ctx, '/api/v1/projects/delete-remote/propose').length, 0);
  // unchecking also_local drops the requirement again
  byId(ctx.root, 'del-also').checked = false;
  fire(byId(ctx.root, 'del-also'), 'change');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled') && !byId(ctx.root, 'del-ack'), 'back to remote only');
});

test('TestUnverifiableFolderIsNeverAlreadyGone', async () => {
  const unsafe = { folder_exists: false, local_removable: false };
  const ctx = await mountProjects({ '/api/v1/projects/plain/removal-preview': () => pvBody('plain', unsafe) });
  await openDelete(ctx, 'plain');
  click(fkey(ctx, 'choice-local'));
  await until(() => byData(ctx.root, 'block', 'local_unsafe').length === 1, 'local_unsafe gate');
  assert.equal(text(byData(ctx.root, 'block', 'local_unsafe')[0]), D.DEL_TEXT.localUnsafe);
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true);
  const t = text(byData(ctx.root, 'targets', 'list')[0]);
  assert.ok(t.includes('확인하지 못함') && !t.includes('이미 없음'), t);
  // really absent (and removable): "이미 없음" and no gate
  const c2 = await mountProjects({ '/api/v1/projects/plain/removal-preview': () => pvBody('plain', { folder_exists: false, local_removable: true }) });
  await openDelete(c2, 'plain');
  click(fkey(c2, 'choice-local'));
  await until(() => !!proposeBtn(c2), 'absent preview');
  assert.ok(text(byData(c2.root, 'targets', 'list')[0]).includes('이미 없음'));
  assert.equal(byData(c2.root, 'block', 'local_unsafe').length, 0);
  assert.equal(proposeBtn(c2).hasAttribute('disabled'), false);
  // also_local on an unverifiable folder is gated the same way
  const c3 = await mountProjects({ '/api/v1/projects/mine/removal-preview': () => pvBody('mine', { local_removable: false }) });
  await openDelete(c3, 'mine');
  click(fkey(c3, 'choice-remote'));
  await until(() => !!typed(c3), 'remote');
  typeInto(typed(c3), 'Hello-World');
  byId(c3.root, 'del-also').checked = true;
  fire(byId(c3.root, 'del-also'), 'change');
  await until(() => byData(c3.root, 'block', 'local_unsafe').length === 1, 'also_local gated');
  assert.equal(proposeBtn(c3).hasAttribute('disabled'), true);
});

test('TestDeletePartialGuidanceTwoPathsAndSessionMemory', async () => {
  assert.ok(/아직 있으면/.test(P.ERROR_TEXT.delete_partial) && /원격 저장소 지우기/.test(P.ERROR_TEXT.delete_partial) && /이미 없으면/.test(P.ERROR_TEXT.delete_partial) && /복제본과 등록 지우기/.test(P.ERROR_TEXT.delete_partial));
  const ctx = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => { throw apiError(504, 'delete_partial', SECRET); } });
  await toConfirm(ctx, 'mine', 'remote', async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); });
  await sign(ctx);
  await until(() => byData(ctx.root, 'note', 'partial-remote').length === 1, 'partial');
  const guide = text(byData(ctx.root, 'note', 'partial-remote')[0]);
  assert.ok(guide.includes('아직 있으면') && guide.includes('새 서명') && guide.includes('이미 없으면'), guide);
  assert.ok(fkey(ctx, 'again-remote') && fkey(ctx, 'again-local'));
  assert.equal(text(fkey(ctx, 'again-remote')), '원격 저장소 다시 지우기…');
  assert.equal(text(fkey(ctx, 'again-local')), '복제본과 등록 지우기…');
  await sleep(40);
  assert.equal(posts(ctx, '/api/v1/projects/delete-remote/apply').length, 1, 'nothing was retried by itself');
  assert.equal(auth.gets.length, 1);
  // path 1: the repository still exists: delete it again, with a NEW proposal and a new signature
  click(fkey(ctx, 'again-remote'));
  await until(() => !!typed(ctx), 'remote preview again');
  assert.equal(posts(ctx, '/api/v1/projects/delete-remote/propose').length, 1, 'no proposal until the person presses the button');
  typeInto(typed(ctx), 'Hello-World');
  await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'enabled');
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.equal(posts(ctx, '/api/v1/projects/delete-remote/propose').length, 2);
  assert.equal(auth.gets.length, 1, 'a new signature is still to come');
  click(fkey(ctx, 'cancel'));
  await until(() => !signBtn(ctx), 'left');
  // path 2 (same view, same session): the project is treated like "remote possibly deleted": strong warning, forced ack even with clean counts
  click(fkey(ctx, 'back-choose'));
  await until(() => !!fkey(ctx, 'choice-local'), 'choose');
  click(fkey(ctx, 'choice-local'));
  await until(() => !!proposeBtn(ctx), 'local preview');
  assert.equal(byData(ctx.root, 'emph', 'remote-gone').length, 1);
  assert.ok(text(byData(ctx.root, 'emph', 'remote-gone')[0]).includes('이미 지워졌을 수 있'), text(byData(ctx.root, 'emph', 'remote-gone')[0]));
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, 'forced acknowledgement');
  // a fresh view (new page visit) does not remember: nothing is stored
  const fresh = await mountProjects();
  await openDelete(fresh, 'mine');
  click(fkey(fresh, 'choice-local'));
  await until(() => !!proposeBtn(fresh), 'fresh preview');
  assert.equal(byData(fresh.root, 'emph', 'remote-gone').length, 0);
  assert.equal(proposeBtn(fresh).hasAttribute('disabled'), false);
});

test('TestDeletePartialLocalButtonGoesStraightToLocalPreview', async () => {
  const ctx = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => { throw apiError(504, 'delete_partial', SECRET); } });
  await toConfirm(ctx, 'mine', 'remote', async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); });
  await sign(ctx);
  await until(() => !!fkey(ctx, 'again-local'), 'partial');
  click(fkey(ctx, 'again-local'));
  await until(() => !!proposeBtn(ctx), 'local preview');
  assert.equal(byData(ctx.root, 'emph', 'remote-gone').length, 1);
  assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true);
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0);
  // the note must not claim the remote deletion finished: after a delete_partial it is unknown
  const note = text(byData(ctx.root, 'note', 'follow-up')[0]);
  assert.ok(!note.includes('이미 끝났습니다') && note.includes('알 수 없'), note);
});

test('TestCreatedWithRemoteLocalOnlyWarnsTheToolLosesTheRemote', async () => {
  for (const [id, expect] of [['mine', true], ['lab', true], ['linked-app', false], ['plain', false], ['toy', false]]) {
    const ctx = await mountProjects();
    await openDelete(ctx, id);
    click(fkey(ctx, 'choice-local'));
    await until(() => !!proposeBtn(ctx) || byLabel(ctx.root, '다음: 두 번째 확인').length === 1, 'preview ' + id);
    assert.equal(byData(ctx.root, 'note', 'created-local-only').length, expect ? 1 : 0, id);
    if (expect) {
      const t = text(byData(ctx.root, 'note', 'created-local-only')[0]);
      assert.ok(t.includes('더 이상 그 원격 저장소를 지울 수 없게') && t.includes('"원격 저장소 지우기"를 하세요'), t);
    }
  }
});

test('TestPreviewLinesAreVerifiedExactly', async () => {
  const extra = '원격 저장소는 지워지지 않고 그대로 남습니다(이 줄은 서버가 덧붙인 것)';
  const cases = [
    ['remove_project', 'plain', 'local', 'POST /api/v1/projects/remove/propose', async () => {}],
    ['delete_remote_repo', 'mine', 'remote', 'POST /api/v1/projects/delete-remote/propose', async (ctx) => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); }],
    ['remove_deploy_key', 'linked-app', 'key', 'POST /api/v1/projects/remove-key/propose', async () => {}],
  ];
  const tamper = {
    'an extra misleading line': (pv) => [pv[0], extra, ...pv.slice(1)],
    'a line appended after the stamp': (pv) => [...pv, extra],
    'a line dropped': (pv) => pv.filter((_, i) => i !== 1),
    'two lines swapped': (pv) => [pv[1], pv[0], ...pv.slice(2)],
    'a word changed': (pv) => pv.map((x, i) => (i === 0 ? x + '.' : x)),
    'the stamp changed': (pv) => pv.map((x) => (x === STAMP ? x + '.' : x)),
  };
  for (const [action, id, key, route, setup] of cases) {
    for (const [name, fn] of Object.entries(tamper)) {
      const ctx = await mountProjects({ [route]: async (b) => { const r = await proposeFor(b, action === 'remove_deploy_key' ? { deploy_key_fingerprint: FP } : {}); r.preview = fn(r.preview); return r; } });
      await toConfirm(ctx, id, key, () => setup(ctx));
      assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, action + ': ' + name);
      assert.equal(signBtn(ctx), undefined, action + ': ' + name);
    }
    // the honest answer passes
    const ok = await mountProjects();
    await toConfirm(ok, id, key, () => setup(ok));
    assert.ok(signBtn(ok), action + ': honest');
  }
  // the key fingerprint line takes its value from the server's own field, which must be a SHA256 fingerprint
  for (const bad of [undefined, 'SHA256:x', 'MD5:aa', SECRET]) {
    const ctx = await mountProjects({ 'POST /api/v1/projects/remove-key/propose': async (b) => ({ ...(await proposeFor(b, bad === undefined ? {} : { deploy_key_fingerprint: bad })) }) });
    await toConfirm(ctx, 'linked-app', 'key');
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, String(bad));
  }
  // legacy and static lines come from what the browser knows (ack, the preview's static_remote)
  const st = await mountProjects({ '/api/v1/projects/plain/removal-preview': () => pvBody('plain', { static_remote: true }) });
  await toConfirm(st, 'plain', 'local');
  assert.equal(byData(st.root, 'warn', 'mismatch').length, 1, 'a static remote the proposal does not mention');
  const st2 = await mountProjects({ '/api/v1/projects/plain/removal-preview': () => pvBody('plain', { static_remote: true }),
    'POST /api/v1/projects/remove/propose': async (b) => { const r = await proposeFor(b); r.preview = [...r.preview.slice(0, -1), '이 프로젝트에는 서버 설정(--git-remote-for)에 정해진 원격이 있습니다. 그 설정은 바뀌지 않으니 관리자가 따로 정리해야 합니다', STAMP]; return r; } });
  await toConfirm(st2, 'plain', 'local');
  assert.ok(signBtn(st2), 'the static line is accepted when the preview said static_remote');
});

test('TestDeleteSpecificFailureTextsNeverTalkAboutCreating', async () => {
  const codes = ['github_refused', 'gitlab_refused', 'provider_unreachable', 'bad_url', 'token_ref_unknown', 'provider_rate_limited', 'remove_refused'];
  for (const code of codes) {
    const t = D.DEL_FAIL[code];
    assert.ok(typeof t === 'string' && t.includes('아무것도 지우지 않았습니다'), code);
    assert.ok(!/만들|생성|만드는/.test(t), code + ': ' + t);
    const status = code === 'provider_rate_limited' ? 429 : code === 'bad_url' ? 400 : 502;
    const c2 = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => { throw apiError(status, code, SECRET); } });
    await toConfirm(c2, 'mine', 'remote', async () => { typeInto(typed(c2), 'Hello-World'); await until(() => !proposeBtn(c2).hasAttribute('disabled'), 'en'); });
    await sign(c2);
    await until(() => byData(c2.root, 'result', 'error').length === 1, code + ' at apply');
    assert.equal(text(byData(c2.root, 'result', 'error')[0]), t, code);
    const c1 = await mountProjects({ 'POST /api/v1/projects/remove/propose': () => { throw apiError(status, code, SECRET); } });
    await openDelete(c1, 'plain');
    click(fkey(c1, 'choice-local'));
    await until(() => !!proposeBtn(c1), 'preview');
    click(proposeBtn(c1));
    await until(() => byData(c1.root, 'note', 'del').length === 1, code + ' at propose');
    assert.equal(text(byData(c1.root, 'note', 'del')[0]), t, code + ' at propose');
  }
});

test('TestMinorFixesF4Round1', async () => {
  // the key option needs a verifiable repository number
  assert.equal(D.deleteOptions(ROWS.find((r) => r.id === 'noid')).key, false);
  assert.equal(D.deleteOptions(ROWS.find((r) => r.id === 'mine')).key, true);
  // the follow-up waits for a fresh list before it looks the row up
  let reads = 0;
  let afterDelete = false;
  const ctx = await mountProjects({
    '/api/v1/projects/registry': () => { reads++; return afterDelete ? REG({ projects: ROWS.map((r) => (r.id === 'mine' ? { ...r, gitlab: null, remote_deleted_at: '2026-11-13T01:02:03Z' } : r)) }) : REG(); },
    '/api/v1/projects/mine/removal-preview': () => pvBody('mine', {}, afterDelete ? { ...ROWS[0], gitlab: null, remote_deleted_at: '2026-11-13T01:02:03Z' } : ROWS[0]),
  });
  await toConfirm(ctx, 'mine', 'remote', async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); });
  afterDelete = true;
  await sign(ctx);
  await until(() => !!fkey(ctx, 'follow-up'), 'result');
  const before = reads;
  click(fkey(ctx, 'follow-up'));
  await until(() => !!proposeBtn(ctx), 'follow-up preview');
  assert.ok(reads > before, 'the list was read again before the follow-up');
  // an apply answer that does not describe what was asked is shown neutrally, not as a success
  for (const [route, bad, key, id] of [
    ['POST /api/v1/projects/remove/apply', { id: 'someone-else', removed: true, local: { clone: 'removed', key: 'removed' } }, 'local', 'plain'],
    ['POST /api/v1/projects/remove/apply', { id: 'plain', removed: false }, 'local', 'plain'],
    ['POST /api/v1/projects/remove-key/apply', { id: 'linked-app', key_removed: false }, 'key', 'linked-app'],
    ['POST /api/v1/orphans/clear/apply', { provider: 'gh', project: 'x/y', cleared: true }, null, null],
  ]) {
    const c = await mountProjects({ [route]: () => bad });
    if (key) await toConfirm(c, id, key);
    else { click(byLabel(c.root, '기록 지우기')[0]); await until(() => !!proposeBtn(c), 'orphan'); click(proposeBtn(c)); await waitSignable(c); }
    click(signBtn(c));
    await until(() => byData(c.root, 'result', 'check').length === 1, 'neutral result for ' + route);
    assert.equal(byData(c.root, 'result', 'ok').length, 0);
    assert.equal(text(byData(c.root, 'result', 'check')[0]), D.DEL_TEXT.noResult);
  }
  // a remote deletion with an unknown remote_state is neutral too
  const cd = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => ({ id: 'mine', remote_state: 'weird' }) });
  await toConfirm(cd, 'mine', 'remote', async () => { typeInto(typed(cd), 'Hello-World'); await until(() => !proposeBtn(cd).hasAttribute('disabled'), 'en'); });
  await sign(cd);
  await until(() => byData(cd.root, 'result', 'check').length === 1, 'neutral remote result');
  // cancelling while a proposal is out: the late answer does not bring the confirmation back
  let release;
  const gate = new Promise((r) => { release = r; });
  const cc = await mountProjects({ 'POST /api/v1/projects/remove/propose': async (b) => { await gate; return proposeFor(b); } });
  await openDelete(cc, 'plain');
  click(fkey(cc, 'choice-local'));
  await until(() => !!proposeBtn(cc), 'preview');
  click(proposeBtn(cc));
  await sleep(10);
  const g0 = auth.gets.length;
  PRcancel(cc);
  release();
  await sleep(60);
  assert.equal(signBtn(cc), undefined, 'a cancelled proposal never reaches the confirmation');
  assert.equal(auth.gets.length, g0, 'nothing signed');
});
function PRcancel(ctx) {
  // the flow screen has no cancel button while proposing: leaving the page is the cancel
  ctx.leave();
}

// ---- F5 adversarial ----------------------------------------------------------------------------

// A hostile or buggy server answers with a proposal that is INTERNALLY CONSISTENT (its change hash
// and message belong to the values it echoes) but for a different change than the one the person
// asked for: another remote id or confirm name, also_local flipped, legacy_ack downgraded, another
// subject, another key path, another orphan. The browser compares what the server echoes with its
// own values, so none of these can be signed: no sign button, no passkey prompt, no apply.
test('TestAdversarialSelfConsistentForgedProposalIsNeverSignableW3f', async () => {
  const forge = (alter) => async (b) => {
    const a = alter(b);
    return proposeFor({ action: b.action, subject: a.subject ?? b.subject, fields: a.fields ?? b.fields });
  };
  const cases = [
    ['remote: another remote id', 'POST /api/v1/projects/delete-remote/propose', 'mine', 'remote', (b) => ({ fields: { ...b.fields, remote_id: '999' } })],
    ['remote: another confirm name', 'POST /api/v1/projects/delete-remote/propose', 'mine', 'remote', (b) => ({ fields: { ...b.fields, confirm_name: 'Other' } })],
    ['remote: another repository path', 'POST /api/v1/projects/delete-remote/propose', 'mine', 'remote', (b) => ({ fields: { ...b.fields, remote_path: 'octo/Other' } })],
    ['remote: also_local flipped on', 'POST /api/v1/projects/delete-remote/propose', 'mine', 'remote', (b) => ({ fields: { ...b.fields, also_local: !b.fields.also_local } })],
    ['remote: another subject', 'POST /api/v1/projects/delete-remote/propose', 'mine', 'remote', () => ({ subject: 'lab', fields: { remote_path: 'team/lab', remote_id: '21', confirm_name: 'lab', also_local: false } })],
    ['local: legacy_ack forced on', 'POST /api/v1/projects/remove/propose', 'plain', 'local', (b) => ({ fields: { ...b.fields, legacy_ack: true } })],
    ['local: another subject', 'POST /api/v1/projects/remove/propose', 'plain', 'local', (b) => ({ subject: 'mine', fields: b.fields })],
    ['key: another repository path', 'POST /api/v1/projects/remove-key/propose', 'mine', 'key', () => ({ fields: { remote_path: 'octo/Other' } })],
    ['key: another subject', 'POST /api/v1/projects/remove-key/propose', 'mine', 'key', (b) => ({ subject: 'lab', fields: { remote_path: 'team/lab' } })],
  ];
  for (const [name, route, id, key, alter] of cases) {
    const ctx = await mountProjects({ [route]: forge(alter) });
    await toConfirm(ctx, id, key, key === 'remote' ? async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'enabled'); } : undefined);
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, name + ': the fixed warning');
    assert.equal(signBtn(ctx), undefined, name + ': no sign button');
    assert.equal(auth.gets.length, 0, name + ': the passkey was never asked');
    assert.equal(posts(ctx, route.replace('POST ', '').replace('/propose', '/apply')).length, 0, name + ': nothing applied');
  }
  // a legacy project: the server downgrades legacy_ack to false after the second confirmation
  const lg = await mountProjects({ '/api/v1/projects/toy/removal-preview': () => pvBody('toy', { unpushed_commits: 2, changed_files: 5 }), 'POST /api/v1/projects/remove/propose': forge((b) => ({ fields: { ...b.fields, legacy_ack: false } })) });
  await openDelete(lg, 'toy');
  click(fkey(lg, 'choice-local'));
  await until(() => byLabel(lg.root, '다음: 두 번째 확인').length === 1, 'legacy preview');
  byId(lg.root, 'del-ack').checked = true;
  fire(byId(lg.root, 'del-ack'), 'change');
  await until(() => !byLabel(lg.root, '다음: 두 번째 확인')[0].hasAttribute('disabled'), 'next');
  click(byLabel(lg.root, '다음: 두 번째 확인')[0]);
  await until(() => !!byId(lg.root, 'del-legacy-typed'), 'second screen');
  typeInto(byId(lg.root, 'del-legacy-typed'), 'toy');
  await until(() => !proposeBtn(lg).hasAttribute('disabled'), 'on');
  click(proposeBtn(lg));
  await until(() => byData(lg.root, 'warn', 'mismatch').length === 1, 'legacy downgrade refused');
  assert.equal(signBtn(lg), undefined);
  assert.equal(auth.gets.length, 0);
  // a forged orphan proposal for another record
  const co = await mountProjects({ 'POST /api/v1/orphans/clear/propose': forge((b) => ({ fields: { project: 'octo/Another-Record' } })) });
  click(byLabel(co.root, '기록 지우기')[0]);
  await until(() => byData(co.root, 'orphan-origin').length === 1, 'orphan screen');
  click(proposeBtn(co));
  await until(() => byData(co.root, 'warn', 'mismatch').length === 1, 'orphan forged');
  assert.equal(signBtn(co), undefined);
  assert.equal(auth.gets.length, 0);
});

// Each unknown count alone is enough to demand the "delete anyway" acknowledgement: an unknown
// number is never read as zero (the mutation run found the changed-count half unguarded).
test('TestEachUnknownCountAloneForcesTheAcknowledgementW3f', async () => {
  for (const [what, over] of [['changed count', { changed_known: false }], ['unpushed count', { unpushed_known: false }]]) {
    const ctx = await mountProjects({ '/api/v1/projects/plain/removal-preview': () => pvBody('plain', over) });
    await openDelete(ctx, 'plain');
    await pick(ctx, 'local');
    assert.ok(byId(ctx.root, 'del-ack'), what + ': the acknowledgement is offered');
    assert.equal(proposeBtn(ctx).hasAttribute('disabled'), true, what + ': propose is off until it is ticked');
    click(proposeBtn(ctx));
    await sleep(5);
    assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0, what + ': nothing proposed without it');
    assert.ok(text(ctx.root).includes('알 수 없음'), what + ': shown as unknown, not as a number');
    byId(ctx.root, 'del-ack').checked = true;
    fire(byId(ctx.root, 'del-ack'), 'change');
    await until(() => !proposeBtn(ctx).hasAttribute('disabled'), what + ': enabled after the tick');
  }
  // control: both known and zero asks for nothing
  const ok = await mountProjects();
  await openDelete(ok, 'plain');
  await pick(ok, 'local');
  assert.equal(byId(ok.root, 'del-ack'), undefined);
  assert.equal(proposeBtn(ok).hasAttribute('disabled'), false);
});

// ===== W3g (Task G3): detach the deploy key with the removal; one signature for remote deletion =====
// Written red-first against the W3f page (which never offered the option and still drew the also_local follow-up).

const FP2 = 'SHA256:' + 'ZYXWVUTSRQPONMLKJIHGFEDCBAzyxwvutsrqponmlkj'.slice(0, 43);
const AV = (id) => ({ detach_key_available: true, deploy_key_title: 'orchd ' + id, deploy_key_fingerprint: FP, deploy_key_shared: isShared(id) });
const pvAvail = (id) => ({ ['/api/v1/projects/' + id + '/removal-preview']: () => pvBody(id, AV(id)) });
const detachBox = (ctx) => byId(ctx.root, 'del-detach');
const factsOf = (ctx) => Object.fromEntries(byData(ctx.root, 'field').map((e) => [e.getAttribute('data-field'), text(e)]));
const rpDetachLine = (remote, fp) => "먼저 원격 저장소 '" + remote + "'에서 이 서버의 배포 키(지문 " + fp + ')를 뗍니다. 키를 떼지 못하거나 결과를 알 수 없으면 프로젝트 등록과 서버의 파일을 지우지 않습니다';
async function ackIfAsked(ctx) {
  if (byId(ctx.root, 'del-ack')) {
    byId(ctx.root, 'del-ack').checked = true;
    fire(byId(ctx.root, 'del-ack'), 'change');
  }
  await until(() => !!proposeBtn(ctx) && !proposeBtn(ctx).hasAttribute('disabled'), 'propose on');
}
const setDetach = async (ctx, on) => {
  detachBox(ctx).checked = on;
  fire(detachBox(ctx), 'change');
  await until(() => (byData(ctx.root, 'emph', 'key-remains').length === 1) === !on, 'repaint after the detach option changed');
};

test('TestDetachKeyCheckboxShownOnlyWhenAvailableDefaultChecked', async () => {
  // the preview says the key line is usable: the option is there, checked, and the proposal signs detach_key true
  for (const id of ['mine', 'linked-app', 'lab']) {
    const ctx = await mountProjects(pvAvail(id));
    await openDelete(ctx, id);
    await pick(ctx, 'local');
    const box = detachBox(ctx);
    assert.ok(box, id + ': the option is shown');
    assert.equal(box.getAttribute('type'), 'checkbox');
    assert.equal(box.checked, true, id + ': checked by default');
    assert.ok(text(byData(ctx.root, 'detach', 'option')[0]).includes('배포 키도 함께 떼기'), id);
    assert.equal(byData(ctx.root, 'hint', 'detach-key').length, 0, id + ': the W3f "detach first" hint gives way to the option');
    assert.ok(text(byData(ctx.root, 'targets', 'list')[0]).includes('배포 키'), id + ': the provider key is listed as a target');
    click(proposeBtn(ctx));
    await waitSignable(ctx);
    assert.deepEqual(posts(ctx, '/api/v1/projects/remove/propose')[0].body.fields, { scope: 'local', legacy_ack: false, detach_key: true }, id);
  }
  // not available (no usable key line; no remote; remote already deleted): no option and detach_key false
  for (const id of ['mine', 'linked-app', 'plain', 'gone-app', 'noid']) {
    const ctx = await mountProjects();
    await openDelete(ctx, id);
    await pick(ctx, 'local');
    assert.equal(detachBox(ctx), undefined, id + ': no option');
    assert.ok(!text(ctx.root).includes('배포 키도 함께 떼기'), id);
    await ackIfAsked(ctx);
    click(proposeBtn(ctx));
    await waitSignable(ctx);
    assert.equal(posts(ctx, '/api/v1/projects/remove/propose')[0].body.fields.detach_key, false, id);
  }
  // the other choices never carry the option
  const ctx = await mountProjects(pvAvail('mine'));
  await openDelete(ctx, 'mine');
  await pick(ctx, 'remote');
  assert.equal(detachBox(ctx), undefined, 'not on the remote deletion');
  click(fkey(ctx, 'back-choose'));
  await until(() => !!fkey(ctx, 'choice-key'), 'back');
  await pick(ctx, 'key');
  assert.equal(detachBox(ctx), undefined, 'not on the key-only removal');
  // a preview that claims the option where the row cannot have it, or whose key identity is off, is refused whole
  const bad = [
    ['plain', AV('plain')], ['gone-app', AV('gone-app')], ['noid', AV('noid')],
    ['mine', { ...AV('mine'), deploy_key_fingerprint: null }], ['mine', { ...AV('mine'), deploy_key_fingerprint: 'SHA256:x' }],
    ['mine', { ...AV('mine'), deploy_key_title: 'orchd other' }], ['mine', { ...AV('mine'), deploy_key_shared: 'no' }],
    ['mine', { ...AV('mine'), detach_key_available: 'yes' }], ['mine', { deploy_key_fingerprint: FP }], ['mine', { deploy_key_title: 'orchd mine' }],
    ['mine', { deploy_key_shared: true }], ['mine', { detach_key_available: undefined }],
  ];
  for (const [id, over] of bad) {
    const c = await mountProjects({ ['/api/v1/projects/' + id + '/removal-preview']: () => pvBody(id, over) });
    await openDelete(c, id);
    click(fkey(c, 'choice-local'));
    await until(() => byData(c.root, 'note', 'del').length === 1, id + ' ' + JSON.stringify(over));
    assert.equal(text(byData(c.root, 'note', 'del')[0]), D.DEL_TEXT.previewBad, id + ' ' + JSON.stringify(over));
    assert.equal(proposeBtn(c), undefined);
  }
  // a legacy project with a remote: the option survives the second confirmation and is signed with legacy_ack
  const lrow = row('oldgh', { origin: 'legacy', kind: 'legacy', provider: 'gh', gitlab: { project: 'octo/Old-Repo', project_id: 55 } });
  const lg = await mountProjects({
    '/api/v1/projects/registry': REG({ projects: [...ROWS, lrow] }),
    '/api/v1/projects/oldgh/removal-preview': () => pvBody('oldgh', AV('oldgh'), lrow),
    'POST /api/v1/projects/remove/propose': async (b) => {
      const r = await proposeFor(b, rpExtra(b));
      const out = r.preview.slice(0, -1);
      if (b.fields.detach_key) out.splice(2, 1, rpDetachLine('octo/Old-Repo', FP));
      r.preview = [...out, STAMP];
      return r;
    },
  });
  await openDelete(lg, 'oldgh');
  click(fkey(lg, 'choice-local'));
  await until(() => byLabel(lg.root, '다음: 두 번째 확인').length === 1, 'legacy preview');
  assert.equal(detachBox(lg).checked, true);
  click(byLabel(lg.root, '다음: 두 번째 확인')[0]);
  await until(() => !!byId(lg.root, 'del-legacy-typed'), 'second screen');
  typeInto(byId(lg.root, 'del-legacy-typed'), 'oldgh');
  await until(() => !proposeBtn(lg).hasAttribute('disabled'), 'on');
  click(proposeBtn(lg));
  await waitSignable(lg);
  assert.deepEqual(posts(lg, '/api/v1/projects/remove/propose')[0].body.fields, { scope: 'local', legacy_ack: true, detach_key: true });
});

test('TestConfirmationNamesRepoAndKeyTitleAndFingerprint', async () => {
  // GitHub: the repository (name and address) and the key (title and fingerprint), on the preview and on the confirmation
  const ctx = await mountProjects(pvAvail('mine'));
  await openDelete(ctx, 'mine');
  await pick(ctx, 'local');
  const pre = text(byData(ctx.root, 'detach', 'option')[0]);
  for (const want of ['octo/Hello-World', 'ssh://git@github.com/octo/Hello-World', 'orchd mine', FP]) assert.ok(pre.includes(want), want + ' in ' + pre);
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  let f = factsOf(ctx);
  assert.ok(f.detach_key.includes('예'), f.detach_key);
  assert.ok(f.key_repo.includes('octo/Hello-World') && f.key_repo.includes('ssh://git@github.com/octo/Hello-World') && f.key_repo.includes('GitHub'), f.key_repo);
  assert.ok(f.key_title.includes('orchd mine'), f.key_title);
  assert.ok(f.key_fingerprint.includes(FP), f.key_fingerprint);
  assert.equal(byData(ctx.root, 'emph', 'key-remains').length, 0);
  assert.ok(text(ctx.root).includes('배포 키'), 'the lead says the key goes first');
  // GitLab (shared key): the fingerprint is the identity; the title is not claimed (the provider may show another project's)
  const gl = await mountProjects(pvAvail('lab'));
  await openDelete(gl, 'lab');
  await pick(gl, 'local');
  const gpre = text(byData(gl.root, 'detach', 'option')[0]);
  assert.ok(gpre.includes('team/lab') && gpre.includes('ssh://git@gitlab.example/team/lab') && gpre.includes(FP) && !gpre.includes('orchd lab'), gpre);
  click(proposeBtn(gl));
  await waitSignable(gl);
  f = factsOf(gl);
  assert.equal(f.key_title, undefined, 'no title claimed for a shared GitLab key');
  assert.ok(f.key_shared.includes('지문') && f.key_shared.includes('공용'), f.key_shared);
  assert.ok(f.key_fingerprint.includes(FP) && f.key_repo.includes('team/lab') && f.key_repo.includes('GitLab'), JSON.stringify(f));
});

test('TestUncheckedShowsKeyRemainsWarning', async () => {
  const ctx = await mountProjects(pvAvail('mine'));
  await openDelete(ctx, 'mine');
  await pick(ctx, 'local');
  assert.equal(byData(ctx.root, 'emph', 'key-remains').length, 0, 'no warning while checked');
  await setDetach(ctx, false);
  const w = byData(ctx.root, 'emph', 'key-remains')[0];
  assert.ok(text(w).includes('키가 원격에 남습니다. 직접 지우세요') && text(w).includes('octo/Hello-World'), text(w));
  assert.equal(byTag(w, 'strong').length, 1, 'bold');
  assert.ok(!text(byData(ctx.root, 'targets', 'list')[0]).includes('제공자의 배포 키'), 'the key is not a target any more');
  assert.ok(text(byData(ctx.root, 'kept', 'list')[0]).includes('배포 키'), 'the key is listed as staying');
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.deepEqual(posts(ctx, '/api/v1/projects/remove/propose')[0].body.fields, { scope: 'local', legacy_ack: false, detach_key: false });
  assert.ok(factsOf(ctx).detach_key.includes('아니오'));
  assert.ok(text(byData(ctx.root, 'emph', 'key-remains')[0]).includes('키가 원격에 남습니다. 직접 지우세요'), 'the confirmation says it too');
  await sign(ctx);
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  assert.equal(posts(ctx, '/api/v1/projects/remove/apply')[0].body.fields.detach_key, false);
  assert.ok(text(byData(ctx.root, 'note', 'key-remains')[0]).includes('직접 지우세요'), 'the result repeats where the key stays');
  // checking it again takes the warning away
  const c2 = await mountProjects(pvAvail('mine'));
  await openDelete(c2, 'mine');
  await pick(c2, 'local');
  await setDetach(c2, false);
  await setDetach(c2, true);
  assert.equal(byData(c2.root, 'emph', 'key-remains').length, 0);
});

test('TestSignGateRecomputesChangeWithDetachKey', async () => {
  const forge = (alter) => async (b) => {
    const fields = alter(b.fields);
    return proposeFor({ action: b.action, subject: b.subject, fields }, rpExtra({ subject: b.subject, fields }));
  };
  const tamper = (fn) => async (b) => fn(await proposeFor(b, rpExtra(b)));
  const on = [
    ['detach flipped off (self-consistent)', forge((f) => ({ ...f, detach_key: false }))],
    ['the change of the other value', tamper(async (r) => ({ ...r, change: await P.computeChange('remove_project', 'mine', { ...r.fields, detach_key: false }) }))],
    ['the detach line dropped', tamper((r) => ({ ...r, preview: r.preview.filter((x) => !x.startsWith('먼저 원격 저장소')) }))],
    ['the detach line names another repository', tamper((r) => ({ ...r, preview: r.preview.map((x) => x.replace('octo/Hello-World', 'someone/else')) }))],
    ['another fingerprint than the preview showed', tamper((r) => ({ ...r, deploy_key_fingerprint: FP2, preview: r.preview.map((x) => x.replace(FP, FP2)) }))],
    ['a bad fingerprint', tamper((r) => ({ ...r, deploy_key_fingerprint: 'SHA256:x' }))],
    ['another title', tamper((r) => ({ ...r, deploy_key_title: 'orchd other' }))],
    ['no title', tamper((r) => { const x = { ...r }; delete x.deploy_key_title; return x; })],
    ['shared not a bool', tamper((r) => ({ ...r, deploy_key_shared: 'false' }))],
    ['shared flipped', tamper((r) => ({ ...r, deploy_key_shared: true }))],
    ['detach_key_available false', tamper((r) => ({ ...r, detach_key_available: false }))],
    ['detach_key_available missing', tamper((r) => { const x = { ...r }; delete x.detach_key_available; return x; })],
  ];
  for (const [name, route] of on) {
    const ctx = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/propose': route });
    await toConfirm(ctx, 'mine', 'local');
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, name);
    assert.equal(signBtn(ctx), undefined, name);
    assert.equal(auth.gets.length, 0, name);
  }
  const off = [
    ['detach flipped on (self-consistent)', forge((f) => ({ ...f, detach_key: true }))],
    ['the detach line added', tamper((r) => ({ ...r, preview: [...r.preview.slice(0, 2), rpDetachLine('octo/Hello-World', FP), ...r.preview.slice(2)] }))],
    ['a key identity when not asked', tamper((r) => ({ ...r, deploy_key_title: 'orchd mine', deploy_key_fingerprint: FP, deploy_key_shared: false }))],
    ['detach_key_available not a bool', tamper((r) => ({ ...r, detach_key_available: 'true' }))],
  ];
  for (const [name, route] of off) {
    const ctx = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/propose': route });
    await toConfirm(ctx, 'mine', 'local', () => setDetach(ctx, false));
    assert.equal(byData(ctx.root, 'warn', 'mismatch').length, 1, name);
    assert.equal(signBtn(ctx), undefined, name);
  }
  // the honest answer: signable, and the signed change is the browser's own hash over detach_key true
  const ok = await mountProjects(pvAvail('mine'));
  await toConfirm(ok, 'mine', 'local');
  assert.ok(signBtn(ok));
  await sign(ok);
  await until(() => byData(ok.root, 'result', 'ok').length === 1, 'result');
  const body = posts(ok, '/api/v1/projects/remove/apply')[0].body;
  assert.deepEqual(body.fields, { scope: 'local', legacy_ack: false, detach_key: true });
  assert.equal(body.change, await P.computeChange('remove_project', 'mine', { scope: 'local', legacy_ack: false, detach_key: true }));
  assert.notEqual(body.change, await P.computeChange('remove_project', 'mine', { scope: 'local', legacy_ack: false, detach_key: false }));
  assert.equal(auth.gets.length, 1);
  assert.ok(auth.gets[0].publicKey.challenge, 'the passkey signed the server message');
});

test('TestAlsoLocalResultHasNoFollowUpSignature', async () => {
  // the hint under the option says the registration goes too (W3f said it stays)
  assert.ok(D.DEL_TEXT.alsoHint.includes('등록') && D.DEL_TEXT.alsoHint.includes('같은 서명') && !D.DEL_TEXT.alsoHint.includes('남습니다'), D.DEL_TEXT.alsoHint);
  const ctx = await mountProjects();
  await openDelete(ctx, 'mine');
  await pick(ctx, 'remote');
  typeInto(typed(ctx), 'Hello-World');
  await alsoAndAck(ctx);
  assert.ok(text(byData(ctx.root, 'targets', 'list')[0]).includes('프로젝트 등록(mine)'), 'the registration is a target');
  const kept = text(byData(ctx.root, 'kept', 'list')[0]);
  assert.ok(!kept.includes('원격 없음') && !kept.includes('별도 서명'), kept);
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.ok(factsOf(ctx).also_local.includes('등록'), factsOf(ctx).also_local);
  await sign(ctx);
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  const steps = findAll(ctx.root, (e) => e.className === 'steps')[0];
  const reg = findAll(steps, (e) => e.getAttribute('data-step') === 'registry')[0];
  assert.equal(reg.getAttribute('data-status'), 'removed', text(reg));
  assert.equal(byData(ctx.root, 'followup', 'remove').length, 0, 'no follow-up box');
  assert.equal(fkey(ctx, 'follow-up'), undefined, 'no follow-up button');
  assert.ok(!text(ctx.root).includes('별도의 서명'), text(ctx.root));
  await sleep(40);
  assert.equal(auth.gets.length, 1);
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose').length, 0);
  // an answer that does not say the registration went (or says it for a remote-only deletion) is not shown as done
  for (const [also, answer] of [[true, 'kept'], [true, undefined], [false, 'removed']]) {
    const c = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': (b) => ({ id: 'mine', remote_state: 'deleted', remote_deleted_at: '2026-11-13T01:02:03Z', also_local: also, ...(answer ? { registry: answer } : {}), ...(also ? { local: { clone: 'removed', key: 'removed' } } : {}) }) });
    await toConfirm(c, 'mine', 'remote', async () => { typeInto(typed(c), 'Hello-World'); if (also) await alsoAndAck(c); else await until(() => !proposeBtn(c).hasAttribute('disabled'), 'en'); });
    await sign(c);
    await until(() => byData(c.root, 'result', 'check').length === 1, 'neutral for ' + also + ' ' + answer);
    assert.equal(fkey(c, 'follow-up'), undefined);
  }
});

test('TestFollowUpStillOfferedForRemoteDeletedRows', async () => {
  // a remote deletion without also_local: the registration stays, the follow-up is offered (second signature, by hand)
  let after = false;
  const goneMine = { ...ROWS[0], gitlab: null, remote_deleted_at: '2026-11-13T01:02:03Z' };
  const ctx = await mountProjects({
    '/api/v1/projects/registry': () => (after ? REG({ projects: ROWS.map((r) => (r.id === 'mine' ? goneMine : r)) }) : REG()),
    '/api/v1/projects/mine/removal-preview': () => (after ? pvBody('mine', {}, goneMine) : pvBody('mine', AV('mine'))),
  });
  await toConfirm(ctx, 'mine', 'remote', async () => { typeInto(typed(ctx), 'Hello-World'); await until(() => !proposeBtn(ctx).hasAttribute('disabled'), 'en'); });
  after = true;
  await sign(ctx);
  await until(() => byData(ctx.root, 'followup', 'remove').length === 1, 'follow-up offered');
  const reg = findAll(ctx.root, (e) => e.getAttribute('data-step') === 'registry')[0];
  assert.equal(reg.getAttribute('data-status'), 'kept');
  click(fkey(ctx, 'follow-up'));
  await until(() => !!proposeBtn(ctx), 'follow-up preview');
  assert.equal(detachBox(ctx), undefined, 'the remote is gone: no key to detach');
  await ackIfAsked(ctx);
  click(proposeBtn(ctx));
  await waitSignable(ctx);
  assert.equal(posts(ctx, '/api/v1/projects/remove/propose')[0].body.fields.detach_key, false);
  // a W3f leftover row (remote_deleted_at already set): the local removal is offered, without the key option
  const c2 = await mountProjects();
  await openDelete(c2, 'gone-app');
  assert.ok(fkey(c2, 'choice-local'));
  await pick(c2, 'local');
  assert.equal(detachBox(c2), undefined);
  assert.ok(byData(c2.root, 'emph', 'remote-gone').length === 1);
  await ackIfAsked(c2);
  click(proposeBtn(c2));
  await waitSignable(c2);
  assert.deepEqual(posts(c2, '/api/v1/projects/remove/propose')[0].body.fields, { scope: 'local', legacy_ack: false, detach_key: false });
});

test('TestChangeVectorsMatchGoW3g', async () => {
  const rp = VEC.good.filter((g) => g.action === 'remove_project');
  assert.ok(rp.some((g) => g.fields.detach_key === true) && rp.some((g) => g.fields.detach_key === false), 'vectors for detach_key true and false');
  for (const g of rp) {
    assert.deepEqual(Object.keys(g.fields).sort(), ['detach_key', 'legacy_ack', 'scope'], g.name);
    assert.equal(P.canonicalChange(g.action, g.subject, g.fields), g.canonical, g.name);
    assert.equal(await P.computeChange(g.action, g.subject, g.fields), g.change, g.name);
  }
  // the two values give two hashes for the same project
  const t = rp.find((g) => g.fields.detach_key === true && g.fields.legacy_ack === false);
  const f = rp.find((g) => g.fields.detach_key === false && g.fields.legacy_ack === false && g.subject === t.subject);
  assert.ok(t && f && t.change !== f.change);
  // Go refuses a string or a missing detach_key, and so does the browser
  const ref = VEC.refused.filter((r) => r.action === 'remove_project');
  assert.ok(ref.some((r) => typeof r.fields.detach_key === 'string') && ref.some((r) => !Object.prototype.hasOwnProperty.call(r.fields, 'detach_key')));
  for (const r of ref) assert.throws(() => P.canonicalChange(r.action, r.subject, r.fields), { name: 'ChangeError' }, r.name);
  for (const m of VEC.messages.filter((x) => x.action === 'remove_project')) assert.equal(wa.checkRegistryMessage(m.text).change, m.change, m.name);
});

test('TestNoStorageNoNavigateTrap', async () => {
  const dj = src('views/delete.js');
  for (const bad of ['localStorage', 'sessionStorage', 'indexedDB', 'document.cookie', 'history.pushState', 'history.replaceState', 'ctx.navigate', 'location.hash', 'innerHTML', 'beforeunload', 'onbeforeunload']) {
    assert.ok(!dj.includes(bad), 'delete.js uses ' + bad);
  }
  assert.ok(/id: 'del-detach'/.test(dj) && !/id: 'del-detach'[^}]*autocomplete/.test(dj), 'a plain checkbox');
  const had = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, get() { throw new Error('storage touched'); } });
  try {
    const ctx = await mountProjects(pvAvail('mine'));
    await toConfirm(ctx, 'mine', 'local', () => setDetach(ctx, false).then(() => setDetach(ctx, true)));
    await sign(ctx);
    await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'detach done');
    assert.deepEqual(ctx.nav, [], 'no navigation');
    const c2 = await mountProjects();
    await toConfirm(c2, 'mine', 'remote', async () => { typeInto(typed(c2), 'Hello-World'); await alsoAndAck(c2); });
    await sign(c2);
    await until(() => byData(c2.root, 'result', 'ok').length === 1, 'also_local done');
    assert.deepEqual(c2.nav, []);
    // leaving mid-flow is free: no trap, nothing sent afterwards
    const c3 = await mountProjects(pvAvail('mine'));
    await toConfirm(c3, 'mine', 'local');
    c3.leave();
    await sleep(20);
    assert.equal(posts(c3, '/api/v1/projects/remove/apply').length, 0);
  } finally {
    if (had) Object.defineProperty(globalThis, 'localStorage', had); else delete globalThis.localStorage;
    delete globalThis.sessionStorage;
  }
});

test('TestRemoveApplyWaitOutlastsOrchdW3g', async () => {
  // orchd waits up to 70 s for the broker (key detach included) plus up to 15 s for the local removal
  assert.ok(P.DELETE_APPLY_TIMEOUT_MS > 70000 + 15000, String(P.DELETE_APPLY_TIMEOUT_MS));
  const ctx = await mountProjects(pvAvail('mine'));
  await toConfirm(ctx, 'mine', 'local');
  await sign(ctx);
  await until(() => byData(ctx.root, 'result', 'ok').length === 1, 'result');
  const a = posts(ctx, '/api/v1/projects/remove/apply')[0];
  assert.equal(a.opts.timeoutMs, P.DELETE_APPLY_TIMEOUT_MS);
  assert.equal(a.opts.noRetry429, true);
});

test('TestRemoveResultShowsKeyStateW3g', async () => {
  for (const [ks, want] of [['removed', '뗐습니다'], ['already_gone', '이미 없었습니다']]) {
    const ctx = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/apply': (b) => ({ id: 'mine', removed: true, detach_key: true, key_state: ks, local: { clone: 'removed', key: 'removed' } }) });
    await toConfirm(ctx, 'mine', 'local');
    await sign(ctx);
    await until(() => byData(ctx.root, 'result', 'ok').length === 1, ks);
    const st = findAll(ctx.root, (e) => e.getAttribute('data-step') === 'remote-key')[0];
    assert.ok(text(st).includes(want), text(st));
    assert.ok(!text(byData(ctx.root, 'result', 'ok')[0]).includes('원격 저장소는 그대로입니다.') || text(byData(ctx.root, 'result', 'ok')[0]).includes('배포 키'), 'the note mentions the key');
  }
  // an ok answer that does not match what was signed is shown neutrally
  for (const [fields, answer] of [
    [true, { id: 'mine', removed: true, detach_key: true, local: { clone: 'removed', key: 'removed' } }],
    [true, { id: 'mine', removed: true, detach_key: true, key_state: 'weird', local: { clone: 'removed', key: 'removed' } }],
    [true, { id: 'mine', removed: true, detach_key: false, local: { clone: 'removed', key: 'removed' } }],
    [false, { id: 'mine', removed: true, detach_key: false, key_state: 'removed', local: { clone: 'removed', key: 'removed' } }],
    [false, { id: 'mine', removed: true, detach_key: true, local: { clone: 'removed', key: 'removed' } }],
  ]) {
    const ctx = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/apply': () => answer });
    await toConfirm(ctx, 'mine', 'local', fields ? undefined : () => setDetach(ctx, false));
    await sign(ctx);
    await until(() => byData(ctx.root, 'result', 'check').length === 1, JSON.stringify(answer));
    assert.equal(byData(ctx.root, 'result', 'ok').length, 0);
  }
});

test('TestRemoveFlowErrorTextsW3g', async () => {
  const RT = D.DEL_REMOVE_TEXT;
  // key line unavailable at propose: the remove flow's own sentence offers turning the option off
  assert.ok(RT.keyUnavailable.includes('배포 키도 함께 떼기') && RT.keyUnavailable.includes('끄고') && RT.keyUnavailable.includes('아무것도 지우지 않았습니다'), RT.keyUnavailable);
  const c1 = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/propose': () => { throw apiError(409, 'key_unavailable_for_removal', SECRET); } });
  await openDelete(c1, 'mine');
  await pick(c1, 'local');
  click(proposeBtn(c1));
  await until(() => byData(c1.root, 'note', 'del').length === 1, 'refused at propose');
  assert.equal(text(byData(c1.root, 'note', 'del')[0]), RT.keyUnavailable);
  assert.ok(detachBox(c1), 'back on the preview, where the option can be turned off');
  // the key-only flow keeps the W3f sentence
  const c1b = await mountProjects({ 'POST /api/v1/projects/remove-key/propose': () => { throw apiError(409, 'key_unavailable_for_removal', SECRET); } });
  await openDelete(c1b, 'mine');
  await pick(c1b, 'key');
  click(proposeBtn(c1b));
  await until(() => byData(c1b.root, 'note', 'del').length === 1, 'key flow');
  assert.equal(text(byData(c1b.root, 'note', 'del')[0]), P.ERROR_TEXT.key_unavailable_for_removal);
  // a mixed-version or changed-state refusal (broker_unavailable 503): nothing deleted, read the list, not an unknown outcome
  assert.ok(RT.brokerUnavailable.includes('버전') && RT.brokerUnavailable.includes('상태가 바뀌었') && RT.brokerUnavailable.includes('아무것도 지우지 않았습니다'), RT.brokerUnavailable);
  for (const [route, key] of [['POST /api/v1/projects/remove/apply', 'local'], ['POST /api/v1/projects/delete-remote/apply', 'remote']]) {
    const c = await mountProjects({ ...pvAvail('mine'), [route]: () => { throw apiError(503, 'broker_unavailable', SECRET); } });
    await toConfirm(c, 'mine', key, key === 'remote' ? async () => { typeInto(typed(c), 'Hello-World'); await until(() => !proposeBtn(c).hasAttribute('disabled'), 'en'); } : undefined);
    await sign(c);
    await until(() => byData(c.root, 'result', 'error').length === 1 || byData(c.root, 'result', 'unknown').length === 1, route);
    assert.equal(text(byData(c.root, 'result', 'error')[0]), RT.brokerUnavailable, route);
  }
  const cp = await mountProjects({ 'POST /api/v1/projects/remove/propose': () => { throw apiError(503, 'broker_unavailable', SECRET); } });
  await openDelete(cp, 'plain');
  await pick(cp, 'local');
  click(proposeBtn(cp));
  await until(() => byData(cp.root, 'note', 'del').length === 1, 'at propose');
  assert.equal(text(byData(cp.root, 'note', 'del')[0]), RT.brokerUnavailable);
  // outcome unknown with detach: the key may already be detached; without detach the W3f sentence
  for (const detach of [true, false]) {
    const c = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/apply': () => { throw apiError(504, 'outcome_unknown', SECRET); } });
    await toConfirm(c, 'mine', 'local', detach ? undefined : () => setDetach(c, false));
    await sign(c);
    await until(() => byData(c.root, 'result', 'unknown').length === 1, 'unknown ' + detach);
    const t = text(byData(c.root, 'result', 'unknown')[0]);
    if (detach) assert.ok(t === RT.unknownDetach && t.includes('이미 떼어졌을 수'), t);
    else assert.equal(t, D.DEL_TEXT.unknown.remove_project);
    await sleep(30);
    assert.equal(posts(c, '/api/v1/projects/remove/apply').length, 1, 'never retried');
  }
  // delete_partial on the remove flow (the key detach): its own screen, no remote-deletion buttons, never retried
  const c3 = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/apply': () => { throw apiError(504, 'delete_partial', SECRET); } });
  await toConfirm(c3, 'mine', 'local');
  await sign(c3);
  await until(() => byData(c3.root, 'result', 'unknown').length === 1, 'partial detach');
  const t3 = text(c3.root);
  assert.ok(t3.includes(RT.partialDetach) && RT.partialDetach.includes('이미 떼어졌을 수') && RT.partialDetach.includes('지우지 않았습니다'), t3);
  assert.ok(!t3.includes('원격 저장소 삭제 결과'), 'not the remote deletion screen');
  assert.equal(fkey(c3, 'again-remote'), undefined);
  assert.equal(fkey(c3, 'again-local'), undefined);
  await sleep(30);
  assert.equal(posts(c3, '/api/v1/projects/remove/apply').length, 1);
  assert.equal(auth.gets.length >= 1, true);
  // delete_partial_local after the key was detached: the key row is shown, and the recovery says to turn the option off
  for (const [ks, want] of [['removed', '뗐습니다'], ['already_gone', '이미 없었습니다']]) {
    const detail = { remaining: ['clone'], clone: 'kept_in_place', key: 'removed', registry_removed: true, remote_deleted: false, reason: 'remove_failed', key_state: ks, key_detached: true };
    const c = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/apply': () => { throw apiError(502, 'delete_partial_local', SECRET, { detail }); } });
    await toConfirm(c, 'mine', 'local');
    await sign(c);
    await until(() => byData(c.root, 'result', 'partial-local').length === 1, ks);
    const st = findAll(c.root, (e) => e.getAttribute('data-step') === 'remote-key')[0];
    assert.ok(st && text(st).includes(want), st ? text(st) : 'no key row');
    const todo = text(byData(c.root, 'note', 'todo')[0]);
    assert.ok(todo.includes('등록은 이미 지워져') && todo.includes('"배포 키도 함께 떼기"를 끄고'), todo);
  }
  for (const bad of [{ key_state: 'weird', key_detached: true }, { key_state: 'removed', key_detached: false }, { key_state: 'removed' }, { key_detached: true }]) {
    const detail = { remaining: ['clone'], clone: 'kept_in_place', key: 'removed', registry_removed: true, remote_deleted: false, reason: 'remove_failed', ...bad };
    const c = await mountProjects({ ...pvAvail('mine'), 'POST /api/v1/projects/remove/apply': () => { throw apiError(502, 'delete_partial_local', SECRET, { detail }); } });
    await toConfirm(c, 'mine', 'local');
    await sign(c);
    await until(() => byData(c.root, 'result', 'partial-local').length === 1, JSON.stringify(bad));
    assert.equal(byData(c.root, 'note', 'detail-invalid').length, 1, JSON.stringify(bad));
  }
  // delete_partial_local after also_local (remote deleted, registration removed): the same concise recovery
  const d4 = { remaining: ['clone'], clone: 'kept_in_place', key: 'removed', registry_removed: true, remote_deleted: true, reason: 'remove_failed' };
  const c4 = await mountProjects({ 'POST /api/v1/projects/delete-remote/apply': () => { throw apiError(502, 'delete_partial_local', SECRET, { detail: d4 }); } });
  await toConfirm(c4, 'mine', 'remote', async () => { typeInto(typed(c4), 'Hello-World'); await alsoAndAck(c4); });
  await sign(c4);
  await until(() => byData(c4.root, 'result', 'partial-local').length === 1, 'also_local partial');
  const todo4 = text(byData(c4.root, 'note', 'todo')[0]);
  assert.ok(todo4.includes('등록은 이미 지워져') && todo4.includes('"배포 키도 함께 떼기"를 끄고'), todo4);
  // delete_partial on the remote flow: the local path is taken with the option off (the repository may be gone)
  assert.ok(D.DEL_TEXT.partialRemoteDo.includes('"배포 키도 함께 떼기"를 끄고'), D.DEL_TEXT.partialRemoteDo);
  assert.ok(!text(c4.root).includes('SERVER-WORDS'));
});
