// router.js: hash routes and the view registry. It has no DOM access so views
// (loaded by app.js) can import it without a cycle with app.js.
//
// Routes: #/overview  #/tasks  #/tasks/ID  #/new  #/new/plan  #/projects  #/providers  #/tokens  #/ops  #/approvals  #/approvals/ID  #/diff/ID
// View names: overview, tasks, task, new, plan, projects, providers, tokens, ops, approvals, approval, diff, login (and notfound).

import { ID_RE } from './render.js';

const views = new Map();
const NAME_RE = /^[a-z][a-z-]{0,31}$/;

// registerView(name, fn): fn(ctx) renders into ctx.root (see app.js for ctx).
// Registering again replaces the earlier view.
export function registerView(name, fn) {
  if (typeof name !== 'string' || !NAME_RE.test(name)) throw new Error('registerView: bad name');
  if (typeof fn !== 'function') throw new Error('registerView: fn must be a function');
  views.set(name, fn);
}

export function getView(name) {
  return views.get(name) || null;
}

// shouldRoute: only `#/...` hashes are routes. Other fragments (`#main` from
// the skip link, `#`, empty) must never change the view.
export function shouldRoute(hash) {
  return typeof hash === 'string' && hash.startsWith('#/');
}

// parseRoute maps location.hash to {name, params}. An empty hash (or `#`) is
// the overview; a `#/...` hash that is malformed or unknown is
// {name: 'notfound'}; any other fragment is not a route: null.
export function parseRoute(hash) {
  let h = typeof hash === 'string' ? hash : '';
  const q = h.indexOf('?');
  if (q >= 0) h = h.slice(0, q);
  if (h === '' || h === '#' || h === '#/') return { name: 'overview', params: [] };
  if (!h.startsWith('#/')) return null;
  const segs = h.slice(2).split('/');
  if (segs[segs.length - 1] === '') segs.pop();
  const [head, ...rest] = segs;
  const bad = { name: 'notfound', params: [] };
  switch (head) {
    case 'overview':
      return rest.length === 0 ? { name: head, params: [] } : bad;
    case 'approvals':
      if (rest.length === 0) return { name: 'approvals', params: [] };
      return rest.length === 1 && ID_RE.test(rest[0]) ? { name: 'approval', params: [rest[0]] } : bad;
    case 'tasks':
      if (rest.length === 0) return { name: 'tasks', params: [] };
      return rest.length === 1 && ID_RE.test(rest[0]) ? { name: 'task', params: [rest[0]] } : bad;
    case 'new':
      if (rest.length === 0) return { name: 'new', params: [] };
      return rest.length === 1 && rest[0] === 'plan' ? { name: 'plan', params: [] } : bad;
    case 'ops':
    case 'projects':
    case 'providers':
    case 'tokens':
      return rest.length === 0 ? { name: head, params: [] } : bad;
    case 'diff':
      // The file shown is view state (a button), never part of the URL.
      return rest.length === 1 && ID_RE.test(rest[0]) ? { name: 'diff', params: rest } : bad;
    default:
      return bad;
  }
}

// navItem is the main-menu entry a view name highlights.
export function navItem(name) {
  return { task: 'tasks', diff: 'tasks', approval: 'approvals', plan: 'new' }[name] || name;
}
