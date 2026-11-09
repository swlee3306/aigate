// fake-ctx.js: a view context for the W2a tests: GET/POST routes (values or
// functions), recorded calls, a real poll() bound to the stub document and a
// recording navigate().
import { installDocument } from './dom-stub.js';

export const doc = installDocument();
const R = await import('../static/render.js');
const { el, poll } = R;

export function apiError(status, code, message, extra) {
  const e = new Error(message || code);
  e.name = 'ApiError';
  e.status = status;
  e.code = code || '';
  Object.assign(e, extra || {});
  return e;
}

// routes: {'/api/v1/x': value | (body, opts) => value}; keys for POST are
// 'POST /api/v1/x'. A missing route is a 404.
export function fakeCtx(routes, extra) {
  extra = extra || {};
  const root = el('main', { tabindex: '-1' });
  const ctl = new AbortController();
  const cleanups = [];
  const pending = new Set();
  const track = (pr) => { const w = pr.then(() => {}, () => {}); pending.add(w); w.then(() => pending.delete(w)); return pr; };
  const run = async (key, path, body, opts) => {
    const r = routes[key];
    if (r === undefined) throw apiError(404, 'not_found', 'nope');
    return typeof r === 'function' ? r(body, opts) : structuredClone(r);
  };
  const ctx = {
    root,
    params: extra.params || [],
    calls: [],
    posts: [],
    nav: [],
    pollers: [],
    pollMs: [],
    signal: ctl.signal,
    navigate: (h) => ctx.nav.push(h),
    setCrumb() {},
    guard: null,
    setLeaveGuard: (fn) => { ctx.guard = typeof fn === 'function' ? fn : null; },
    onLeave: (cb) => cleanups.push(cb),
    get: (path, opts) => { ctx.calls.push(path); return track(run(path, path, undefined, opts)); },
    post: (path, body, opts) => { ctx.posts.push({ path, body, opts }); return track(run('POST ' + path, path, body, opts)); },
    poll: (fn, ms) => {
      ctx.pollMs.push(ms);
      const p = poll(() => track(fn()), ms, { document: doc });
      ctx.pollers.push(p);
      cleanups.push(() => p.stop());
      return p;
    },
    leave() { ctl.abort(); cleanups.forEach((c) => c()); ctx.guard = null; },
    async settle() {
      for (let i = 0; i < 8; i++) {
        await Promise.allSettled([...pending]);
        await new Promise((r) => setTimeout(r, 0));
      }
    },
  };
  return ctx;
}
