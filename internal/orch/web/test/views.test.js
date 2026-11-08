import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { installDocument, findAll, byClass, allElements } from './dom-stub.js';

const doc = installDocument();
const fx = (n) => JSON.parse(readFileSync(new URL('./fixtures/' + n, import.meta.url), 'utf8'));
const R = await import('../static/render.js');
const { el, poll, sanitize, clip, capLine, splitLines, ID_RE, idOf, errorText } = R;
const { renderTasks, taskHash, tasksView } = await import('../static/views/tasks.js');
const { renderTask, renderTimeline, barWidths, taskView, MIN_BAR_PCT } = await import('../static/views/task.js');
const { renderDiff, parseHashedText, fileEntries, diffView } = await import('../static/views/diff.js');
const { getView } = await import('../static/router.js');

const HOSTILE = [
  '<img src=x onerror=alert(1)>',
  '</script><script>alert(1)</script>',
  '"><svg onload=alert(1)>',
  'a\u0000b\u0007c\u001b[31mred\u001b[0m‮evil⁦x​',
  '&lt;b&gt;',
  'x\r\ny\u0085z\u007f',
];
const TAGNAMES = (root) => allElements(root).map((e) => e.tagName);
const text = (n) => n.textContent;
const buttons = (root) => findAll(root, (e) => e.tagName === 'BUTTON');
const links = (root) => findAll(root, (e) => e.tagName === 'A');
const noHostileElements = (root) => {
  for (const t of TAGNAMES(root)) assert.ok(!['IMG', 'SCRIPT', 'SVG', 'IFRAME', 'STYLE'].includes(t), 'element ' + t + ' created from data');
};

// ---- text safety ----------------------------------------------------------

test('sanitize: controls, bidi and zero-width become visible placeholders', () => {
  const s = sanitize('a\u001bb\u0000c\u007fd\u0085e‮f⁦g​h﻿i\r\nj');
  assert.equal(s, 'a␛b␀c␡d�e⟨U+202E⟩f⟨U+2066⟩g⟨U+200B⟩h⟨U+FEFF⟩i␍⏎j');
  assert.doesNotMatch(s, /[\u0000-\u001f\u007f-\u009f‪-‮⁦-⁩​-‏﻿]/);
  assert.equal(sanitize('a\tb\nc', { keepTab: true, keepNewline: true }), 'a\tb\nc');
  assert.equal(sanitize('a\tb'), 'a␉b');
  assert.equal(sanitize(null), '');
  assert.equal(sanitize('한글 😀'), '한글 😀');
});

test('clip and capLine: bounded, no split surrogates, marker with the dropped count', () => {
  assert.deepEqual(clip('abc', 5), { text: 'abc', dropped: 0 });
  const c = clip('a😀b', 2); // would cut inside the pair
  assert.equal(c.text, 'a');
  assert.equal(c.dropped, 3);
  const long = 'x'.repeat(10000);
  const out = capLine(long, 500);
  assert.ok(out.length < 520);
  assert.match(out, /…\(\+9500자\)$/);
  assert.equal(capLine('ok'), 'ok');
});

test('splitLines: counts every line, materialises at most the limit', () => {
  assert.deepEqual(splitLines('a\nb\nc\n', 10), { lines: ['a', 'b', 'c'], total: 3 });
  assert.deepEqual(splitLines('a\r\nb', 10), { lines: ['a', 'b'], total: 2 });
  const r = splitLines('l\n'.repeat(1000), 5);
  assert.equal(r.lines.length, 5);
  assert.equal(r.total, 1000);
});

// ---- tasks list -------------------------------------------------------------

test('TestTasksTableRendersHostileText', () => {
  const tasks = HOSTILE.map((h, i) => ({ id: 100 + i, repo: h, prompt: h, state: h, class: h, agent: h, attempts: 1,
    created_at: '2026-11-09T10:00:00Z', updated_at: '2026-11-09T10:03:00Z' }));
  const root = renderTasks(tasks, {});
  noHostileElements(root);
  const body = findAll(root, (e) => e.tagName === 'TBODY')[0];
  assert.equal(body.children.length, HOSTILE.length);
  const shown = text(root);
  assert.match(shown, /<img src=x onerror=alert\(1\)>/, 'markup is displayed literally');
  assert.match(shown, /<\/script><script>alert\(1\)<\/script>/);
  assert.match(shown, /&lt;b&gt;/, 'entities are not decoded');
  assert.doesNotMatch(shown, /[\u0000-\u0008\u000b-\u001f\u007f-\u009f‪-‮⁦-⁩​]/, 'control and bidi neutralised');
  assert.match(shown, /␛\[31mred/, 'ESC shown as a symbol');
  assert.match(shown, /⟨U\+202E⟩/);
  // hostile state text never lands in a class attribute
  for (const e of allElements(root)) assert.doesNotMatch(e.className, /[<>"=\s]alert|onerror|script/i);
  for (const chip of byClass(root, 'chip')) assert.match(chip.className, /\bc-idle\b/);
  // every task row is a native button with aria-pressed (not a clickable <tr>)
  const rowbtns = byClass(root, 'rowbtn');
  assert.equal(rowbtns.length, HOSTILE.length);
  for (const b of rowbtns) {
    assert.equal(b.tagName, 'BUTTON');
    assert.equal(b.getAttribute('type'), 'button');
    assert.ok(['true', 'false'].includes(b.getAttribute('aria-pressed')));
  }
  assert.deepEqual(findAll(root, (e) => e.tagName === 'TR').flatMap((tr) => Object.keys(tr.listeners)), [], 'no click handler on rows');
  // the table sits in a horizontal scroll wrapper and has a caption
  assert.equal(findAll(root, (e) => e.tagName === 'TABLE')[0].parentNode.className, 'tscroll');
  assert.equal(findAll(root, (e) => e.tagName === 'CAPTION').length, 1);
  assert.equal(findAll(root, (e) => e.tagName === 'TH').every((th) => th.getAttribute('scope') === 'col'), true);
});

test('tasks table from the real fixture: chips carry text, filters work, selection is aria-pressed', () => {
  const tasks = fx('tasks.json');
  let picked = null;
  let filtered = null;
  const root = renderTasks(tasks, { selectedId: '38', onPick: (id) => { picked = id; }, onFilter: (f) => { filtered = f; } });
  const chips = byClass(root, 'chip').map(text);
  assert.deepEqual(chips.sort(), ['대기', '승인 대기', '실패'].sort());
  assert.ok(byClass(root, 'c-wait').length === 1 && byClass(root, 'c-fail').length === 1, 'color classes accompany the text');
  const pressed = byClass(root, 'rowbtn').filter((b) => b.getAttribute('aria-pressed') === 'true');
  assert.equal(pressed.length, 1);
  assert.equal(text(pressed[0]), '#38');
  byClass(root, 'rowbtn')[0].listeners.click[0]();
  assert.ok(['39', '38', '37'].includes(picked));
  const fbtns = findAll(root, (e) => e.tagName === 'BUTTON' && (e.getAttribute('data-focus-key') || '').startsWith('f-'));
  assert.deepEqual(fbtns.map((b) => b.getAttribute('aria-pressed')), ['true', 'false', 'false', 'false']);
  assert.equal(fbtns[0].parentNode.getAttribute('role'), 'group');
  // filter
  const only = renderTasks(tasks, { filter: 'failed' });
  assert.equal(byClass(only, 'rowbtn').length, 1);
  assert.equal(text(byClass(only, 'rowbtn')[0]), '#37');
  fbtns[3].listeners.click[0]();
  assert.equal(filtered, 'failed');
  assert.match(text(renderTasks(tasks, { filter: 'done' })), /이 상태의 작업이 없습니다/);
});

test('invalid and hostile ids never reach an href, a route or a button', () => {
  for (const bad of ['38<img>', '-1', '1e3', '0', '', '1.5', null, undefined, {}, [], 1.5, -2, NaN, 2 ** 60, '9'.repeat(40), '38/../x', ' 38']) {
    assert.equal(idOf(bad), null, JSON.stringify(bad));
    assert.equal(taskHash(bad), null);
  }
  assert.equal(taskHash(38), '#/tasks/38');
  assert.equal(taskHash('38'), '#/tasks/38');
  assert.ok(ID_RE.test('38'));
  const root = renderTasks([{ id: '38<img src=x>', state: 'done' }, { id: -5, state: 'done' }, { id: 7, state: 'done' }], {});
  assert.equal(byClass(root, 'rowbtn').length, 1, 'only the valid id gets a button');
  assert.equal(links(root).length, 0);
  assert.match(text(root), /#\?/);
  // the click path: an invalid id is never navigated to
  const nav = [];
  const ctx = fakeCtx({ '/api/v1/tasks': [{ id: 7, state: 'done' }] }, { navigate: (h) => nav.push(h) });
  tasksView(ctx);
  return ctx.settle().then(() => {
    const b = byClass(ctx.root, 'rowbtn')[0];
    b.listeners.click[0]();
    assert.deepEqual(nav, ['#/tasks/7']);
    assert.equal(byClass(ctx.root, 'rowbtn')[0].getAttribute('aria-pressed'), 'true', 'selection kept after repaint');
    ctx.leave();
  });
});

test('tasks: empty state, error state with fixed text, long values capped', async () => {
  assert.match(text(renderTasks([], {})), /작업이 없습니다/);
  assert.match(text(renderTasks(null, {})), /작업이 없습니다/);
  const big = 'p'.repeat(5000);
  const root = renderTasks([{ id: 1, state: 'done', prompt: big, repo: big }], {});
  assert.ok(text(root).length < 1000, 'long values clipped');

  const ctx = fakeCtx({ '/api/v1/tasks': () => { throw apiError(500, 'internal', '<h1>Traceback</h1><script>x</script>'); } });
  tasksView(ctx);
  await ctx.settle();
  assert.match(text(ctx.root), /서버에서 오류가 발생했습니다/);
  assert.doesNotMatch(text(ctx.root), /Traceback|script/);
  noHostileElements(ctx.root);
  assert.equal(findAll(ctx.root, (e) => e.getAttribute('role') === 'alert').length, 1);
  ctx.leave();
});

// ---- task detail --------------------------------------------------------

function pctOf(row) {
  const fill = byClass(row, 'bar-f')[0];
  return Number.parseFloat(fill.style.width);
}

test('TestTaskTimelineBars', () => {
  const data = fx('events.json');
  const root = renderTask(data, {});
  const rows = byClass(root, 'bar-r');
  assert.equal(rows.length, data.steps.length);
  const total = data.steps.reduce((a, s) => a + s.seconds, 0);
  const want = data.steps.map((s) => Math.max(MIN_BAR_PCT, Math.round((100 * s.seconds) / total)));
  assert.deepEqual(rows.map(pctOf), want);
  assert.deepEqual(barWidths(data.steps.map((s) => s.seconds)), want);
  // widths are proportional: verifying (154s of 182.5s) dominates, the 0s step keeps a visible minimum
  assert.ok(want[4] > 80 && want[0] === MIN_BAR_PCT && want[5] === MIN_BAR_PCT);
  for (const r of rows) {
    const fill = byClass(r, 'bar-f')[0];
    assert.match(fill.style.width, /^\d+(\.\d+)?%$/, 'CSSOM width');
    assert.equal(fill.hasAttribute('style'), false, 'no style attribute');
    assert.equal(byClass(r, 'bar-t')[0].getAttribute('aria-hidden'), 'true', 'bars are decoration');
  }
  // accessible text alternative: label + duration per stage, in a labelled list
  const list = findAll(root, (e) => e.tagName === 'UL' && e.getAttribute('aria-label') === '단계별 소요 시간')[0];
  assert.ok(list);
  const spans = rows.map((r) => r.children.filter((c) => c.tagName === 'SPAN').map(text));
  assert.deepEqual(spans.map((s) => s[0]), ['대기', '준비', '에이전트', '수집', '검증', '승인 대기']);
  assert.deepEqual(spans.map((s) => s[1]).slice(0, 5), ['0.2초', '2.0초', '25초', '1.3초', '154초']);
  // the last, still-open step shows it is in progress when `now` is given
  const runningSteps = data.steps.slice(0, 6).map((s2, i) => (i === 5 ? { ...s2, state: 'verifying' } : s2));
  const live = renderTimeline(runningSteps, { now: new Date(Date.parse(data.steps[5].at) + 90000) });
  assert.match(text(byClass(live, 'bar-r').pop()), /진행 중 · 90초/);
  // waiting states (needs_approval, passed, error) never show a growing timer
  for (const st of ['needs_approval', 'passed', 'error']) {
    const steps = data.steps.map((s2, i) => (i === 5 ? { ...s2, state: st } : s2));
    const later = renderTimeline(steps, { now: new Date(Date.parse(data.steps[5].at) + 90000) });
    const row = text(byClass(later, 'bar-r').pop());
    assert.match(row, /대기 중/, st);
    assert.doesNotMatch(row, /진행 중|90초/, st);
  }
  // a finished task shows a plain duration
  const done = data.steps.map((s2, i) => (i === 5 ? { ...s2, state: 'done' } : s2));
  assert.doesNotMatch(text(byClass(renderTimeline(done, { now: new Date(Date.parse(data.steps[5].at) + 90000) }), 'bar-r').pop()), /진행 중|대기 중/);
  // verify summary from the real parsed note
  const shown = text(root);
  assert.match(shown, /복원55초/);
  assert.match(shown, /검사 합계153초/);
  assert.match(shown, /기준\(base\)79초/);
  // zero-length timeline: every bar keeps the minimum
  assert.deepEqual(barWidths([0, 0, 0]), [MIN_BAR_PCT, MIN_BAR_PCT, MIN_BAR_PCT]);
  assert.match(text(renderTimeline([], {})), /기록된 단계가 없습니다/);
});

test('timeline never uses a style attribute (stub throws on setAttribute(style))', () => {
  assert.throws(() => doc.createElement('div').setAttribute('style', 'width:1%'), /forbidden/);
  assert.throws(() => el('div', { style: 'width:1%' }), /not allowed/);
  renderTask(fx('events.json'), {}); // does not throw
});

test('task detail: links, approval box, hostile fields, error reason as text', () => {
  const data = fx('events.json');
  const root = renderTask(data, { approval: { reason: '<b>보호 파일</b> 수정' } });
  const hrefs = links(root).map((a) => a.getAttribute('href'));
  assert.deepEqual(hrefs.sort(), ['#/approvals', '#/diff/38', '#/tasks'].sort());
  assert.match(text(root), /사람의 승인을 기다리는 중/);
  assert.match(text(root), /<b>보호 파일<\/b> 수정/);
  noHostileElements(root);

  const hostile = structuredClone(data);
  Object.assign(hostile.task, { id: '38"><img>', repo: HOSTILE[0], base: HOSTILE[3], prompt: HOSTILE.join('\n'), error: HOSTILE[1], state: 'failed', head_sha: '<img>' });
  hostile.steps[2].note = HOSTILE[3];
  hostile.steps[2].state = HOSTILE[0];
  const h = renderTask(hostile, {});
  noHostileElements(h);
  assert.equal(links(h).filter((a) => /diff/.test(a.getAttribute('href'))).length, 0, 'no diff link without a valid id and head');
  assert.match(text(h), /head 커밋이 아직 없어/);
  assert.match(text(h), /실패 이유/);
  assert.match(text(h), /<\/script><script>alert\(1\)<\/script>/);
  assert.doesNotMatch(text(h), /[\u0000-\u0008\u000b-\u001f\u007f-\u009f‪-‮⁦-⁩​]/);
  for (const e of allElements(h)) assert.doesNotMatch(e.className, /alert|onerror/);
});

test('task detail: long prompt and report are bounded with "더 보기"', async () => {
  const data = fx('events.json');
  data.task.prompt = Array.from({ length: 1000 }, (_, i) => 'line ' + i).join('\n');
  const root = renderTask(data, {});
  const blocks = byClass(root, 'block');
  const promptBlock = blocks[0];
  const shownLines = () => promptBlock.children.filter((c) => c.tagName === 'DIV').length;
  assert.equal(shownLines(), 40);
  const more = byClass(promptBlock, 'more')[0];
  assert.match(text(more), /960개 남음/);
  more.listeners.click[0]();
  assert.equal(shownLines(), 80);

  // report: loaded on demand, shown as text, never as markup
  const ctx = fakeCtx({
    '/api/v1/tasks/38/events': fx('events.json'),
    '/api/v1/approvals': [],
    '/api/v1/tasks/38/report': { verdict: '<h1>x</h1>', note: HOSTILE[3], lines: 'z'.repeat(100000) },
  }, { params: ['38'] });
  taskView(ctx);
  await ctx.settle();
  const btn = buttons(ctx.root).find((b) => text(b) === '보고서 불러오기');
  assert.ok(btn, 'report is loaded on demand');
  assert.equal(ctx.calls.filter((c) => c.endsWith('/report')).length, 0);
  btn.listeners.click[0]();
  await ctx.settle();
  const rep = byClass(ctx.root, 'block').find((b) => b.getAttribute('aria-label').startsWith('검증 보고서'));
  assert.ok(rep);
  assert.equal(rep.tagName, 'DIV');
  assert.match(text(rep), /"verdict": "<h1>x<\/h1>"/);
  noHostileElements(ctx.root);
  assert.ok(text(rep).length < 5000, 'one long line is capped');
  assert.match(text(rep), /…\(\+\d+자\)/);
  ctx.leave();
});

test('task view: error envelope shown with fixed text, 404 id, bad id', async () => {
  const bad = fakeCtx({}, { params: ['38<img>'] });
  taskView(bad);
  await bad.settle();
  assert.deepEqual(bad.calls, [], 'no request for an invalid id');
  assert.match(text(bad.root), /찾을 수 없습니다/);

  const ctx = fakeCtx({ '/api/v1/tasks/9/events': () => { throw apiError(404, 'not_found', '<img src=x onerror=alert(1)>'); } }, { params: ['9'] });
  taskView(ctx);
  await ctx.settle();
  assert.match(text(ctx.root), /찾을 수 없습니다/);
  assert.doesNotMatch(text(ctx.root), /onerror/);
  noHostileElements(ctx.root);
  ctx.leave();
  assert.match(errorText({ status: 500 }), /서버에서 오류/);
  assert.match(errorText(new Error('x')), /서버에 연결하지 못했습니다/);
});

test('polling stops when the view is left', async () => {
  const ctx = fakeCtx({ '/api/v1/tasks': [{ id: 1, state: 'done' }] });
  tasksView(ctx);
  await ctx.settle();
  const n = ctx.calls.length;
  assert.ok(n >= 1);
  ctx.leave();
  await ctx.settle();
  assert.equal(ctx.pollers.every((p) => p.state === 'stopped'), true);
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(ctx.calls.length, n, 'no request after leaving');
  // the task view polls too and also stops
  const c2 = fakeCtx({ '/api/v1/tasks/38/events': fx('events.json'), '/api/v1/approvals': [] }, { params: ['38'] });
  taskView(c2);
  await c2.settle();
  c2.leave();
  assert.equal(c2.pollers[0].state, 'stopped');
});

test('views register under the router names', () => {
  for (const n of ['tasks', 'task', 'diff']) assert.equal(typeof getView(n), 'function', n);
});

// ---- diff -----------------------------------------------------------------

test('TestDiffViewEscapesAndMarksProtected', () => {
  const view = fx('diff.json');
  const picked = [];
  const root = renderDiff(view, {}, (i) => picked.push(i));
  noHostileElements(root);
  // file list: buttons with aria-pressed; protected files carry a text badge
  const files = findAll(root, (e) => e.tagName === 'UL' && e.getAttribute('aria-label') === '변경된 파일')[0];
  const fb = buttons(files);
  assert.equal(fb.length, 3);
  assert.equal(findAll(root, (e) => e.tagName === 'A').length, 0, 'file names are never hrefs');
  const names = fb.map((b) => text(byClass(b, 'fname')[0]));
  assert.deepEqual(names, ['pytest.ini', 'src/a.txt', 'tests/spec.txt']);
  assert.match(text(fb[0]), /보호 파일 · 새 파일/, 'AddedGuarded (A line)');
  assert.match(text(fb[2]), /보호 파일 · 변경/, 'Changed (section only)');
  assert.doesNotMatch(text(fb[1]), /보호 파일/, 'unprotected file has no badge');
  assert.deepEqual(fb.map((b) => b.getAttribute('aria-pressed')), ['true', 'false', 'false'], 'first protected file selected');
  // body of the selected (new guarded) file: classes by line kind, literal text
  const body = byClass(root, 'diff')[0];
  const lines = body.children.filter((c) => c.tagName === 'DIV');
  const cls = lines.map((l) => l.className.split(/\s+/).filter((c) => c && c !== 'dl')[0] || 'ctx');
  assert.deepEqual(cls, ['meta', 'meta', 'meta', 'meta', 'meta', 'hunk', 'add']);
  assert.ok(lines.some((l) => l.className.includes('add') && text(l) === '+[pytest]'));
  assert.ok(lines.some((l) => l.className.includes('hunk') && text(l).startsWith('@@')));
  assert.ok(!lines.some((l) => l.className.includes('add') && text(l).startsWith('+++')), '+++ header is meta, not an added line');
  // select the changed file with the hostile content
  fb[2].listeners.click[0]();
  assert.deepEqual(picked, [2]);
  const root2 = renderDiff(view, { selected: 2 }, () => {});
  noHostileElements(root2);
  const l2 = byClass(root2, 'diff')[0].children.filter((c) => c.tagName === 'DIV');
  const t2 = l2.map(text).join('\n');
  assert.match(t2, /^\+<script>alert\(1\)<\/script>$/m, 'markup literal');
  assert.match(t2, /esc ␛\[31mred␛\[0m bidi ⟨U\+202E⟩evil/, 'ESC and bidi neutralised');
  assert.doesNotMatch(t2, /[\u001b‮]/);
  assert.ok(l2.some((l) => /\bdel\b/.test(l.className) && text(l) === '-line2'));
  assert.ok(l2.some((l) => /\badd\b/.test(l.className) && text(l).includes('<script>')));
  assert.ok(!l2.some((l) => text(l).startsWith('A pytest.ini')), 'trailing A line is not part of a file body');
  // unprotected file
  const root3 = renderDiff(view, { selected: 1 }, () => {});
  assert.match(text(root3), /보호 대상이 아니라서 이 화면에는 변경 줄이 없습니다/);
  assert.equal(byClass(root3, 'diff').length, 0);
  // read-only note and no recomputation
  assert.match(text(root), /읽기 전용/);
  assert.match(text(root), /웹은 다시 계산하지 않습니다/);
  assert.match(text(root), /3273763058ed7862/);
  assert.equal(findAll(root, (e) => e.tagName === 'INPUT' || e.tagName === 'TEXTAREA').length, 0);
});

test('TestDiffTruncatedBanner', () => {
  const full = renderDiff(fx('diff.json'), {}, () => {});
  assert.equal(byClass(full, 'trunc').length, 0, 'no banner when complete');
  const view = fx('diff-truncated.json');
  assert.equal(view.truncated, true);
  const root = renderDiff(view, {}, () => {});
  const banner = byClass(root, 'trunc')[0];
  assert.ok(banner, 'banner present');
  assert.equal(banner.getAttribute('role'), 'alert');
  assert.match(text(banner), /일부만 표시됨: 전체 diff는 `orch diff`로 확인/);
  // the cut dropped the trailing A line: the file is still marked protected
  const fb = buttons(findAll(root, (e) => e.tagName === 'UL')[0]);
  assert.match(text(fb[0]), /보호 파일/);
  // a protected file whose body was cut says so instead of looking empty
  const cutHard = { ...view, hashed_text: view.hashed_text.slice(0, 20) };
  const r2 = renderDiff(cutHard, { selected: 2 }, () => {});
  assert.match(text(r2), /잘린 부분에 있어 표시되지 않았습니다/);
  assert.match(text(fileTag(r2, 2)), /내용 잘림/);
});
function fileTag(root, i) {
  return buttons(findAll(root, (e) => e.tagName === 'UL')[0])[i];
}

test('diff parsing: sections, added guarded, hostile lookalike lines stay inside their hunk', () => {
  const t = 'diff --git a/x b/x\nindex 1..2 100644\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-diff --git a/evil b/evil\n+A evil\n+++ b/fake\n ctx\nA new.txt\n';
  const { sections, added } = parseHashedText(t);
  assert.deepEqual([...sections.keys()], ['x']);
  assert.deepEqual([...added], ['new.txt'], 'content lines starting with A or diff --git are prefixed and ignored');
  const kinds = sections.get('x').lines.map((l) => l.kind);
  assert.deepEqual(kinds, ['meta', 'meta', 'meta', 'meta', 'hunk', 'del', 'add', 'add', 'ctx']);
  // paths with spaces and mismatching headers
  const odd = parseHashedText('diff --git a/a b/c b/a b/c b/c\n@@ -1 +1 @@\n+x\n');
  assert.equal(odd.sections.size, 0, 'unparseable header: no section key');
  // empty / non-string
  assert.equal(parseHashedText(undefined).sections.size, 0);
  assert.deepEqual(fileEntries({}).entries, []);
});

test('diff view: very long lines and many lines are capped, empty and error states', async () => {
  const lines = ['diff --git a/tests/big b/tests/big', '--- a/tests/big', '+++ b/tests/big', '@@ -0,0 +1,1000 @@'];
  for (let i = 0; i < 1000; i++) lines.push('+' + (i === 3 ? 'y'.repeat(100000) : 'row ' + i));
  const view = { hash: 'abcdef0123456789', hashed_text: lines.join('\n') + '\n', truncated: false, protected_files: ['tests/big'], files: [{ path: 'tests/big', added: 1000, deleted: 0 }] };
  const root = renderDiff(view, {}, () => {});
  const body = byClass(root, 'diff')[0];
  const shown = () => body.children.filter((c) => c.tagName === 'DIV').length;
  assert.equal(shown(), 300);
  assert.ok(text(body).length < 300 * 520, 'each line capped');
  assert.match(text(body), /…\(\+\d+자\)/);
  const more = byClass(body, 'more')[0];
  assert.match(text(more), /더 보기/);
  more.listeners.click[0]();
  assert.equal(shown(), 600);
  // empty
  const empty = renderDiff({ hash: '', hashed_text: '', truncated: false, protected_files: [], files: [] }, {}, () => {});
  assert.match(text(empty), /변경된 파일이 없습니다/);
  // errors: fixed text
  const ctx = fakeCtx({ '/api/v1/tasks/5/diff': () => { throw apiError(409, 'conflict', 'diff를 만들 수 없습니다: <img onerror=1>'); } }, { params: ['5'] });
  diffView(ctx);
  await ctx.settle();
  assert.match(text(ctx.root), /아직 볼 수 없는 상태입니다/);
  assert.doesNotMatch(text(ctx.root), /img|onerror/);
  assert.deepEqual(ctx.pollers, [], 'the diff is fetched once, never polled');
  const badId = fakeCtx({}, { params: ['x'] });
  diffView(badId);
  await badId.settle();
  assert.deepEqual(badId.calls, []);
});

test('diff view paints the selected file on click and keeps view state', async () => {
  const ctx = fakeCtx({ '/api/v1/tasks/38/diff': fx('diff.json') }, { params: ['38'] });
  diffView(ctx);
  await ctx.settle();
  const list = () => buttons(findAll(ctx.root, (e) => e.tagName === 'UL')[0]);
  list()[2].listeners.click[0]();
  assert.deepEqual(list().map((b) => b.getAttribute('aria-pressed')), ['false', 'false', 'true']);
  assert.match(text(ctx.root), /<script>alert\(1\)<\/script>/);
  assert.equal(ctx.calls.length, 1, 'selecting a file does not refetch');
});

// ---- fix round 1 ------------------------------------------------------------

function probeView(extra) {
  // 30k added lines of tests/a (60 KB), then a new guarded pytest.ini section and its A line
  const lines = ['diff --git a/tests/a b/tests/a', 'index 1..2 100644', '--- a/tests/a', '+++ b/tests/a', '@@ -0,0 +1,30000 @@'];
  for (let i = 0; i < 30000; i++) lines.push('+');
  lines.push('diff --git a/pytest.ini b/pytest.ini', 'new file mode 100644', 'index 0000..1111', '--- /dev/null', '+++ b/pytest.ini', '@@ -0,0 +1 @@', '+[pytest]', 'A pytest.ini');
  return { hash: 'abcdef0123456789', hashed_text: lines.join('\n') + '\n', truncated: false,
    protected_files: ['pytest.ini', 'tests/a'], files: [{ path: 'pytest.ini', added: 1, deleted: 0 }, { path: 'tests/a', added: 30000, deleted: 0 }], ...extra };
}

test('client parse cap clipping is shown as truncation, never as "no changes"', () => {
  const view = probeView();
  assert.ok(view.hashed_text.length < 70000);
  assert.equal(parseHashedText(view.hashed_text, 20000).clipped, true);
  const root = renderDiff(view, { selected: 1 }, () => {}, { maxParseLines: 20000 });
  const banner = byClass(root, 'trunc')[0];
  assert.ok(banner, 'banner although truncated:false');
  assert.match(text(banner), /일부만 표시됨/);
  assert.match(text(banner), /화면에서 일부 줄을 생략했습니다/);
  const ini = buttons(findAll(root, (e) => e.tagName === 'UL')[0]).find((b) => text(b).includes('pytest.ini'));
  assert.match(text(ini), /보호 파일/);
  assert.match(text(ini), /내용 잘림/);
  assert.doesNotMatch(text(root), /변경 줄이 없습니다/);
  // the partially seen last section is cut too, and its body says so nowhere as "no changes"
  const sel0 = renderDiff(view, { selected: 0 }, () => {}, { maxParseLines: 20000 });
  assert.doesNotMatch(text(sel0), /변경 줄이 없습니다/);
  assert.match(text(sel0), /잘린 부분에 있어 표시되지 않았습니다/);
  // with the default (large) cap this input is fully parsed: no banner, new file recognised
  const full = renderDiff(view, { selected: 0 }, () => {});
  assert.equal(byClass(full, 'trunc').length, 0);
  assert.match(text(buttons(findAll(full, (e) => e.tagName === 'UL')[0]).find((b) => text(b).includes('pytest.ini'))), /새 파일/);
});

test('new file mode marks a new guarded file even when the A lines were cut', () => {
  const t = 'diff --git a/pytest.ini b/pytest.ini\nnew file mode 100644\nindex 0000..1111\n--- /dev/null\n+++ b/pytest.ini\n@@ -0,0 +1 @@\n+x\n';
  const view = { hash: 'abcdef01', hashed_text: t, truncated: true, protected_files: ['pytest.ini', 'tests/z'], files: [{ path: 'pytest.ini', added: 1, deleted: 0 }] };
  assert.equal(parseHashedText(t).sections.get('pytest.ini').isNew, true);
  const root = renderDiff(view, {}, () => {});
  const fb = buttons(findAll(root, (e) => e.tagName === 'UL')[0]);
  // pytest.ini is the last section of a cut text: the section may be partial, but a "new file" mode line is seen
  assert.match(text(fb[0]), /보호 파일 · 새 파일/);
  assert.match(text(fb[1]), /내용 잘림/, 'protected file with no section at all');
  const complete = fileEntries({ ...view, truncated: false }).entries[0];
  assert.equal(complete.kind, 'added');
  // a content line that looks like the mode line cannot flag a section
  const fake = parseHashedText('diff --git a/x b/x\n@@ -1 +1 @@\n+new file mode 100644\n');
  assert.equal(fake.sections.get('x').isNew, undefined);
});

test('invisible and format characters are neutralised (all ranges)', () => {
  const cps = [0xad, 0x34f, 0x61c, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180e, 0x200b, 0x200f, 0x2028, 0x202e, 0x2060, 0x2069,
    0x3164, 0xfe00, 0xfe0f, 0xfeff, 0xfff9, 0xfffb, 0xe0000, 0xe0041, 0xe007f];
  for (const cp of cps) {
    const out = sanitize('a' + String.fromCodePoint(cp) + 'b');
    assert.match(out, /^a\u27e8U\+[0-9A-F]{4,5}\u27e9b$/, 'U+' + cp.toString(16));
    assert.equal(out.includes(String.fromCodePoint(cp)), false);
  }
  assert.equal(sanitize('\u{e0041}'), '\u27e8U+E0041\u27e9');
  assert.equal(sanitize('한글 😀 a-b'), '한글 😀 a-b', 'ordinary text untouched');
});

test('capLine clips before sanitizing: a 2 MiB line yields a short bounded result', () => {
  const huge = ('\u001b' + 'x').repeat(1 << 20); // 2 MiB
  const out = capLine(huge);
  assert.ok(out.length < 560, 'length ' + out.length);
  assert.match(out, /…\(\+\d{6,}자\)$/);
  assert.equal(out.includes('\u001b'), false);
  assert.equal(capLine('abc'), 'abc');
});

// ---- helpers ----------------------------------------------------------------

function apiError(status, code, message) {
  const e = new Error(message);
  e.name = 'ApiError';
  e.status = status;
  e.code = code;
  return e;
}

// fakeCtx builds the view context of app.js: a root, ctx.get serving fixtures
// by path (a function value may throw), ctx.poll bound to a stoppable poller.
function fakeCtx(routes, extra) {
  extra = extra || {};
  const root = el('main', { tabindex: '-1' });
  const ctl = new AbortController();
  const cleanups = [];
  const pending = new Set();
  const ctx = {
    root,
    params: extra.params || [],
    calls: [],
    pollers: [],
    signal: ctl.signal,
    navigate: extra.navigate,
    setCrumb() {},
    onLeave: (cb) => cleanups.push(cb),
    get: async (path) => {
      ctx.calls.push(path);
      const r = routes[path];
      if (r === undefined) throw apiError(404, 'not_found', 'nope');
      return typeof r === 'function' ? r() : structuredClone(r);
    },
    poll: (fn, ms) => {
      const p = poll(() => { const pr = fn(); const w = pr.then(() => {}, () => {}); pending.add(w); w.then(() => pending.delete(w)); return pr; }, ms, { document: doc });
      ctx.pollers.push(p);
      cleanups.push(() => p.stop());
      return p;
    },
    leave() {
      ctl.abort();
      cleanups.forEach((c) => c());
    },
    async settle() {
      for (let i = 0; i < 8; i++) {
        await Promise.allSettled([...pending]);
        await new Promise((r) => setTimeout(r, 0));
      }
    },
  };
  return ctx;
}
