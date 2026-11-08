// diff.js: read-only diff view (#/diff/ID) from GET /api/v1/tasks/{id}/diff.
//
// hashed_text is the gate's PROTECTED diff (unified diff of the protected
// files that changed, plus one trailing "A <path>" line per newly added
// guarded file); `files` is the numstat of the WHOLE change. So only
// protected files have a body here. The web never recomputes the hash.
// The selected file is view state (a button), never part of the URL.

import {
  el, inline, idOf, errorPane, capLine, splitLines, chunked,
} from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';

const LINE_CHUNK = 300;
const FILE_CHUNK = 200;
// A full server-sized text (256 KiB, at least 2 bytes per line) is at most ~131k
// lines, so the default never clips it; the cap only bounds hostile input.
export const MAX_PARSE_LINES = 200000;
const HASH_RE = /^[0-9a-f]{8,64}$/;

// parseHashedText splits the gate's text into per-file sections and the set
// of newly added guarded paths. Display only: nothing here feeds a decision.
// A section header is `diff --git a/P b/P`; lines of a hunk always start
// with ' ', '+', '-' or '\', so "diff --git " and "A " at a line start can
// not come from file content.
export function parseHashedText(text, limit) {
  const { lines, total } = splitLines(text, limit || MAX_PARSE_LINES);
  // clipped: the client cap cut the input, so later sections and `A` lines were never seen.
  const clipped = total > lines.length;
  let last = null;
  const sections = new Map();
  const added = new Set();
  let cur = null;
  let inHunk = false;
  for (const line of lines) {
    if (line.startsWith('diff --git a/')) {
      const rest = line.slice('diff --git a/'.length); // P b/P
      const n = (rest.length - 3) / 2;
      const path = Number.isInteger(n) && n > 0 && rest.slice(n, n + 3) === ' b/' && rest.slice(0, n) === rest.slice(n + 3)
        ? rest.slice(0, n) : null;
      cur = path === null ? { path: null, lines: [] } : sections.get(path) || { path, lines: [] };
      if (path !== null) { sections.set(path, cur); last = path; }
      inHunk = false;
      cur.lines.push({ kind: 'meta', text: line });
    } else if (line.startsWith('@@') && cur) {
      inHunk = true;
      cur.lines.push({ kind: 'hunk', text: line });
    } else if (inHunk && cur && ' +-\\'.includes(line[0] || '\n')) {
      cur.lines.push({ kind: line[0] === '+' ? 'add' : line[0] === '-' ? 'del' : line[0] === '\\' ? 'meta' : 'ctx', text: line });
    } else if (line.startsWith('A ')) {
      added.add(line.slice(2)); // trailing "A <path>": ends the last section
      cur = null;
      inHunk = false;
    } else if (cur) {
      if (!inHunk && line.startsWith('new file mode ')) cur.isNew = true;
      cur.lines.push({ kind: 'meta', text: line });
    }
  }
  return { sections, added, clipped, last };
}

const KIND_CLASS = { add: 'add', del: 'del', hunk: 'hunk', meta: 'meta', ctx: '' };

// fileEntries: the union of `files` (numstat) and protected_files, each with
// its protection kind: 'added' (new guarded file), 'changed' (protected,
// body present), 'unseen' (protected but not in the possibly cut text) or
// null (not protected: no body in this view).
export function fileEntries(view, limit) {
  const files = Array.isArray(view.files) ? view.files : [];
  const prot = new Set((Array.isArray(view.protected_files) ? view.protected_files : []).filter((p) => typeof p === 'string'));
  const { sections, added, clipped, last } = parseHashedText(view.hashed_text, limit);
  const cut = view.truncated === true || clipped; // the text may end mid-section
  const seen = new Set();
  const out = [];
  const kindOf = (path) => {
    if (!prot.has(path)) return null;
    if (!sections.has(path)) return added.has(path) ? 'added' : 'unseen';
    if (added.has(path) || sections.get(path).isNew) return 'added';
    return cut && path === last ? 'cut' : 'changed'; // the last section of a cut text may be partial
  };
  for (const f of files) {
    if (!f || typeof f.path !== 'string' || seen.has(f.path)) continue;
    seen.add(f.path);
    out.push({ path: f.path, added: f.added, deleted: f.deleted, kind: kindOf(f.path) });
  }
  for (const p of prot) {
    if (seen.has(p)) continue;
    seen.add(p);
    out.push({ path: p, added: null, deleted: null, kind: kindOf(p) });
  }
  return { entries: out, sections, clipped };
}

const KIND_BADGE = {
  added: '보호 파일 · 새 파일',
  changed: '보호 파일 · 변경',
  unseen: '보호 파일 · 내용 잘림',
  cut: '보호 파일 · 내용 잘림',
};

function fileLabel(e) {
  const stat = Number.isFinite(e.added) && Number.isFinite(e.deleted) ? '+' + e.added + ' −' + e.deleted : '';
  return [
    el('span', { class: 'fname mono' }, inline(e.path, 120)),
    el('span', { class: 'fmeta' }, [
      e.kind ? el('span', { class: ['tag', 'prot'] }, KIND_BADGE[e.kind]) : null,
      stat ? el('span', { class: 'mono' }, stat) : null,
    ]),
  ];
}

function renderBody(entry, sections, truncated) {
  if (!entry) return el('p', { class: 'note' }, '파일을 고르세요.');
  const head = el('h2', {}, [inline(entry.path, 160)]);
  if (!entry.kind) {
    return el('div', {}, [head, el('p', { class: 'note' },
      '보호 대상이 아니라서 이 화면에는 변경 줄이 없습니다. 위 숫자는 추가·삭제된 줄 수입니다.')]);
  }
  const sec = sections.get(entry.path);
  if (!sec) {
    // Never "no changes": a protected file whose section was not seen is cut off.
    return el('div', {}, [head, el('p', { class: 'note' },
      '보호 파일이지만 diff가 길어 잘린 부분에 있어 표시되지 않았습니다. 전체 diff는 `orch diff`로 확인하세요.')]);
  }
  const pre = el('div', { class: 'diff', role: 'region', tabindex: '0', 'aria-label': inline(entry.path, 80) + ' 변경 내용' });
  chunked(pre, sec.lines, LINE_CHUNK, (l) => el('div', { class: ['dl', KIND_CLASS[l.kind]] }, capLine(l.text) || ' '), '줄');
  return el('div', {}, [head, pre]);
}

// renderDiff: view is the diff response; state {selected: index}; onSelect(i).
export function renderDiff(view, state, onSelect, opts) {
  view = view && typeof view === 'object' ? view : {};
  state = state || {};
  const { entries, sections, clipped } = fileEntries(view, opts && opts.maxParseLines);
  const serverCut = view.truncated === true;
  const truncated = serverCut || clipped;
  const selected = Number.isInteger(state.selected) && state.selected >= 0 && state.selected < entries.length
    ? state.selected : Math.max(0, entries.findIndex((e) => e.kind));
  const pending = entries.filter((e) => e.kind).length;

  const list = el('ul', { class: 'files', 'aria-label': '변경된 파일' });
  chunked(list, entries, FILE_CHUNK, (e, i) => el('li', {}, el('button', {
    type: 'button', class: 'filebtn', 'aria-pressed': i === selected ? 'true' : 'false', 'data-focus-key': 'file-' + i,
    on: { click: () => onSelect && onSelect(i) },
  }, fileLabel(e))), '파일');

  const hash = typeof view.hash === 'string' && HASH_RE.test(view.hash) ? view.hash : null;
  return el('div', { class: 'ws' }, [
    el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-diff' }, '변경 내용'),
      el('p', { class: 'lead' }, '읽기 전용입니다. 보호 파일 ' + pending + '개가 승인 대상입니다. 승인은 승인 화면에서 합니다.'),
      truncated
        ? el('div', { class: 'trunc', role: 'alert' }, [
          el('b', {}, '일부만 표시됨'),
          ': 전체 diff는 `orch diff`로 확인',
          clipped ? el('small', {}, ' · 화면에서 일부 줄을 생략했습니다.') : null,
          el('small', {}, ' · 잘린 diff는 승인할 수 없습니다. 서버가 보여 주는 해시는 전체 diff의 값입니다.'),
        ])
        : null,
      entries.length === 0 ? el('p', { class: 'note', role: 'status' }, '변경된 파일이 없습니다.') : null,
      renderBody(entries[selected], sections, truncated),
    ]),
    el('aside', { class: ['panel', 'insp'], 'aria-label': '파일 목록' }, [
      el('h2', {}, '파일 ' + entries.length + '개'),
      list,
      el('p', { class: 'note' }, [
        '해시 ', el('span', { class: 'mono' }, hash || '-'),
        ' · 이 해시는 서버가 계산한 값이며 웹은 다시 계산하지 않습니다.',
      ]),
    ]),
  ]);
}

export function diffView(ctx) {
  const id = idOf(ctx.params && ctx.params[0]);
  ctx.setCrumb(id ? '변경 내용 #' + id : '변경 내용');
  if (!id) {
    ctx.root.append(errorPane({ status: 404 }, '#/tasks', '← 작업 목록'));
    return;
  }
  const st = { view: null, selected: null };
  const paint = () => {
    replaceContent(ctx.root, renderDiff(st.view, { selected: st.selected }, (i) => { st.selected = i; paint(); }));
  };
  const load = async () => {
    try {
      const view = await ctx.get('/api/v1/tasks/' + id + '/diff');
      if (ctx.signal && ctx.signal.aborted) return;
      st.view = view;
      st.selected = null;
      paint();
    } catch (err) {
      if (ctx.signal && ctx.signal.aborted) return;
      replaceContent(ctx.root, errorPane(err, '#/tasks/' + id, '← 작업 상세'));
    }
  };
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  // One request per visit (the server runs git for it): no polling.
  return load();
}

registerView('diff', diffView);
