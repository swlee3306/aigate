// tasks.js: the task list (#/tasks). A table of tasks from GET /api/v1/tasks,
// a state filter, live updates through ctx.poll. Every server string is shown
// as sanitized text; ids become hrefs/routes only after ID_RE validation.

import { el, inline, stateChip, idOf, formatTime, errorPane, STATE_LABELS } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';

const POLL_MS = 10000;
const ALL = 'all';

// navigate is the only place the hash is written. It refuses anything that
// is not a validated numeric id.
export function taskHash(id) {
  const ok = idOf(id);
  return ok ? '#/tasks/' + ok : null;
}

function defaultNavigate(hash) {
  globalThis.location.hash = hash;
}

// stateKey: known states keep their name; anything else (including hostile
// text) falls into the single "기타" bucket '?'.
function stateKey(t) {
  return t && typeof t.state === 'string' && Object.prototype.hasOwnProperty.call(STATE_LABELS, t.state) ? t.state : '?';
}

// renderTasks renders the list. opts: {filter, selectedId, onFilter(state),
// onPick(id)}; unknown shapes are tolerated (non-array -> empty).
export function renderTasks(tasks, opts) {
  opts = opts || {};
  const all = Array.isArray(tasks) ? tasks.filter((t) => t && typeof t === 'object') : [];
  const filter = opts.filter || ALL;
  const counts = new Map();
  for (const t of all) counts.set(stateKey(t), (counts.get(stateKey(t)) || 0) + 1);
  const rows = filter === ALL ? all : all.filter((t) => stateKey(t) === filter);

  const chips = [[ALL, '전체', all.length],
    ...Object.keys(STATE_LABELS).filter((s) => counts.has(s)).map((s) => [s, STATE_LABELS[s], counts.get(s)])];
  if (counts.has('?')) chips.push(['?', '기타', counts.get('?')]);
  const filters = el('div', { class: 'filters', role: 'group', 'aria-label': '상태로 거르기' }, chips.map(([key, label, n]) =>
    el('button', {
      type: 'button', class: 'btn', 'aria-pressed': filter === key ? 'true' : 'false',
      'data-focus-key': 'f-' + (key === ALL ? 'all' : key === '?' ? 'other' : key),
      on: { click: () => opts.onFilter && opts.onFilter(key) },
    }, label + ' ' + n)));

  const body = rows.map((t) => {
    const id = idOf(t.id);
    const label = id ? '#' + id : '#?';
    const idCell = id
      ? el('button', {
        type: 'button', class: 'rowbtn mono', 'aria-pressed': opts.selectedId === id ? 'true' : 'false',
        'aria-label': '작업 ' + id + ' 열기', 'data-focus-key': 'row-' + id,
        on: { click: () => opts.onPick && opts.onPick(id) },
      }, label)
      : el('span', { class: 'mono' }, label);
    return el('tr', { 'data-task': id || '' }, [
      el('td', {}, idCell),
      el('td', {}, stateChip(t.state)),
      el('td', { class: 'mono' }, inline(t.repo, 60)),
      el('td', { class: ['ell', 'hide-m'] }, inline(t.prompt, 80)),
      el('td', { class: 'mono hide-m' }, formatTime(t.updated_at)),
      el('td', { class: 'mono hide-m' }, Number.isFinite(t.attempts) ? String(t.attempts) : '-'),
    ]);
  });

  const empty = all.length === 0 ? '작업이 없습니다'
    : rows.length === 0 ? '이 상태의 작업이 없습니다' : null;
  const table = el('div', { class: 'tscroll' }, [
    el('table', {}, [
      el('caption', { class: 'sr' }, '작업 목록'),
      el('thead', {}, [el('tr', {}, [
        el('th', { scope: 'col' }, '번호'), el('th', { scope: 'col' }, '상태'), el('th', { scope: 'col' }, '저장소'),
        el('th', { scope: 'col', class: 'hide-m' }, '요청'), el('th', { scope: 'col', class: 'hide-m' }, '갱신'),
        el('th', { scope: 'col', class: 'hide-m' }, '시도'),
      ])]),
      el('tbody', {}, body),
    ]),
  ]);

  return el('div', { class: ['panel', 'canvas'] }, [
    el('h1', { id: 'h-tasks' }, '작업'),
    el('p', { class: 'lead' }, all.length === 0 ? '제출된 작업이 아직 없습니다.' : '최근 작업 ' + all.length + '건 (최대 200건). 번호를 누르면 상세로 이동합니다.'),
    el('div', { class: 'card' }, [
      el('div', { class: 'card-h' }, [el('h2', {}, '작업 목록')]),
      filters,
      empty ? el('p', { class: 'note', role: 'status' }, empty) : null,
      table,
    ]),
  ]);
}

// tasksView is the view function; ctx.navigate may be injected (tests).
export function tasksView(ctx) {
  ctx.setCrumb('작업');
  const navigate = ctx.navigate || defaultNavigate;
  const st = { filter: ALL, selected: null, data: null, loaded: false };
  const paint = () => replaceContent(ctx.root, renderTasks(st.data, {
    filter: st.filter, selectedId: st.selected,
    onFilter: (f) => { st.filter = f; paint(); },
    onPick: (id) => {
      const hash = taskHash(id);
      if (!hash) return;
      st.selected = id;
      paint();
      navigate(hash);
    },
  }));
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  let last = '';
  ctx.poll(async () => {
    try {
      const data = await ctx.get('/api/v1/tasks', { polling: true });
      if (ctx.signal && ctx.signal.aborted) return;
      const key = JSON.stringify(data);
      if (st.loaded && key === last) return;
      last = key;
      st.data = data;
      st.loaded = true;
      paint();
    } catch (err) {
      if (!(ctx.signal && ctx.signal.aborted) && !st.loaded) replaceContent(ctx.root, errorPane(err));
      throw err; // poll backs off and the live pill shows the failure
    }
  }, POLL_MS);
}

registerView('tasks', tasksView);
