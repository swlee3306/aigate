// app.js: boot sequence, hash router and the page chrome.
//
// Boot: GET /api/v1/auth/session ONCE. 401 -> the login view; otherwise the
// stage menu and the routed view. Later 401s arrive as `session-expired`.
//
// View contract (Tasks 24/25): registerView(name, fn) from ./router.js, then
// fn(ctx) is called on every navigation to that route with
//   ctx.root         the <main> element, already emptied
//   ctx.params       route parameters (strings, e.g. ['38'])
//   ctx.session      {principal, kind, expires} of the session (null on login)
//   ctx.get/post     API helpers from ./api.js (errors are ApiError)
//   ctx.poll(fn, ms) poll() bound to the live pill and pause button; stopped
//                    automatically when the user leaves the view
//   ctx.setCrumb(s)  text of the breadcrumb
//   ctx.onLeave(cb)  cleanup callback, run before the next view
//   ctx.signal       AbortSignal, aborted on leave
//   ctx.onAuthenticated(session)   (login view) enter the console
// A view renders only through el() from ./render.js. Add the file name to
// VIEW_MODULES below to load it at start.

import { get, post, session, logout, onSessionExpired, setCsrf, ApiError } from './api.js';
import { el, clear, poll, formatLive, renderStageMenu, renderOverview } from './render.js';
import { registerView, getView, parseRoute, navItem, shouldRoute } from './router.js';
import { makeHashGuard } from './guard.js';
import { bindSkipLink, showNotice, replaceContent, renderBootFailure } from './shell.js';

export { registerView };

// Files under /views/ that register views (added by later tasks).
const VIEW_MODULES = ['tasks', 'task', 'new', 'plan', 'projects', 'providers', 'tokens', 'ops', 'diff', 'approvals', 'login'];

const POLL_MS = 10000;
const $ = (id) => document.getElementById(id);

const state = {
  session: null,
  pending: 0,
  cleanups: [],
  ctl: null,
  livePoll: null,
  seq: 0,
  guard: null, // a view holding unsaved work: () => boolean (true = do not leave)
  hash: '',
};

function banner(msg, kind) {
  showNotice($('banner'), kind || 'error', msg);
}

function errText(err) {
  if (err instanceof ApiError && err.status === 429) return '요청이 많아 잠시 쉬는 중입니다. 잠시 후 다시 시도합니다.';
  if (err instanceof ApiError && err.message) return err.message;
  return '알 수 없는 오류가 발생했습니다.';
}

// --- live pill ----------------------------------------------------------

let lastAnnounced = '';
function onPollUpdate(u) {
  $('livetxt').textContent = formatLive(u.state, u.lastUpdated, u.failures);
  const pill = $('livepill');
  pill.classList.toggle('paused', u.state === 'paused' || u.state === 'hidden');
  pill.classList.toggle('stale', u.failures > 0);
  // Screen readers hear state changes only, not every tick.
  const key = u.state + ':' + (u.failures > 0);
  if (key !== lastAnnounced) {
    lastAnnounced = key;
    $('livestatus').textContent = u.state === 'paused' ? '실시간 갱신을 멈췄습니다'
      : u.state === 'running' ? (u.failures > 0 ? '갱신에 실패했습니다. 다시 시도합니다' : '실시간 갱신 중')
        : '';
  }
  const btn = $('pausebtn');
  btn.setAttribute('aria-pressed', u.state === 'paused' ? 'true' : 'false');
  btn.textContent = u.state === 'paused' ? '다시 시작' : '일시정지';
  btn.setAttribute('aria-label', u.state === 'paused' ? '실시간 갱신 다시 시작' : '실시간 갱신 일시정지');
}

function bindPoll(fn, ms) {
  const p = poll(fn, ms, { onUpdate: onPollUpdate });
  state.livePoll = p;
  state.cleanups.push(() => {
    p.stop();
    if (state.livePoll === p) state.livePoll = null;
    $('pausebtn').hidden = true;
    $('livetxt').textContent = formatLive('stopped');
  });
  $('pausebtn').hidden = false;
  return p;
}

// --- views ------------------------------------------------------------

function leave() {
  state.guard = null;
  if (state.ctl) state.ctl.abort();
  state.ctl = null;
  const cs = state.cleanups;
  state.cleanups = [];
  for (const c of cs) {
    try { c(); } catch { /* a cleanup must not block navigation */ }
  }
}

function placeholder(title, text) {
  return el('div', { class: 'panel canvas' }, [el('h1', {}, title), el('p', { class: 'lead' }, text)]);
}

function setAuthUI(authed) {
  document.querySelector('.shell').classList.toggle('unauth', !authed);
  $('logoutbtn').hidden = !authed;
}

function setNav(name) {
  const item = navItem(name);
  for (const a of document.querySelectorAll('#mainnav a.nav')) {
    if (a.getAttribute('data-nav') === item) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
}

function makeCtx(params, authed) {
  state.ctl = new AbortController();
  return {
    root: $('main'),
    params,
    session: authed ? state.session : null,
    get,
    post,
    poll: bindPoll,
    setCrumb: (s) => { $('crumb').textContent = ''; $('crumb').append(el('b', {}, String(s))); },
    onLeave: (cb) => { if (typeof cb === 'function') state.cleanups.push(cb); },
    setLeaveGuard: (fn) => { state.guard = typeof fn === 'function' ? fn : null; },
    signal: state.ctl.signal,
    onAuthenticated: (s) => enter(s),
  };
}

async function show() {
  const seq = ++state.seq;
  state.hash = location.hash;
  leave();
  const main = clear($('main'));
  banner('');
  let route = parseRoute(location.hash) || { name: 'overview', params: [] };
  const authed = state.session !== null;
  if (!authed) route = { name: 'login', params: [] };
  setNav(route.name);
  const ctx = makeCtx(route.params, authed);
  const fn = getView(route.name);
  try {
    if (fn) {
      await fn(ctx);
    } else if (route.name === 'notfound') {
      main.append(placeholder('찾을 수 없는 화면', '주소를 확인하거나 메뉴에서 고르세요.'));
    } else if (route.name === 'login') {
      main.append(placeholder('로그인', '로그인 화면을 준비하고 있습니다.'));
    } else {
      main.append(placeholder('준비 중', '이 화면은 아직 만들어지지 않았습니다.'));
    }
  } catch (err) {
    if (seq === state.seq) banner(errText(err));
  }
  if (seq === state.seq) main.focus({ preventScroll: true });
}

// --- overview view ------------------------------------------------------

function paintMenu(stages, pending) {
  const nav = clear($('stagenav'));
  nav.append(el('h2', {}, '단계'), renderStageMenu(stages, { pending }));
  const badge = $('approvals-badge');
  badge.hidden = !(pending > 0);
  badge.textContent = pending > 0 ? String(pending) : '';
  if (pending > 0) badge.setAttribute('aria-label', '대기 ' + pending + '건');
}

registerView('overview', (ctx) => {
  ctx.setCrumb('개요');
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  let last = '';
  ctx.poll(async () => {
    try {
      const [stages, ov] = await Promise.all([
        get('/api/v1/stages', { polling: true }),
        get('/api/v1/overview', { polling: true }),
      ]);
      if (ctx.signal.aborted) return;
      banner('');
      const key = JSON.stringify([stages, ov]);
      if (key === last) return; // unchanged: do not re-render or re-announce
      last = key;
      state.pending = Number.isFinite(ov && ov.pending_approvals) ? ov.pending_approvals : 0;
      paintMenu(stages, state.pending);
      replaceContent(ctx.root, renderOverview(ov));
    } catch (err) {
      if (!ctx.signal.aborted && !(err instanceof ApiError && err.status === 401)) banner(errText(err));
      throw err; // poll backs off
    }
  }, POLL_MS);
});

// --- boot ---------------------------------------------------------------

async function enter(sess) {
  state.session = sess;
  setAuthUI(true);
  if (!location.hash || location.hash === '#/login') history.replaceState(null, '', '#/overview');
  await show();
}

function showLogin(msg, kind) {
  state.session = null;
  setCsrf(null);
  clear($('stagenav'));
  setAuthUI(false);
  show().then(() => { if (msg) banner(msg, kind); });
}

async function boot() {
  $('pausebtn').addEventListener('click', () => {
    const p = state.livePoll;
    if (!p) return;
    if (p.state === 'paused') p.resume(); else p.pause();
  });
  $('logoutbtn').addEventListener('click', async () => {
    try { await logout(); } catch { /* shown as a logged-out screen anyway */ }
    showLogin('로그아웃했습니다.', 'info');
  });
  onSessionExpired(() => showLogin('세션이 만료되었습니다. 다시 로그인하세요.'));
  const hashGuard = makeHashGuard({
    guard: () => state.guard,
    current: () => state.hash,
    restore: (h) => history.replaceState(null, '', h),
    notice: (t) => banner(t, 'info'),
  });
  window.addEventListener('hashchange', () => {
    if (!shouldRoute(location.hash)) return;
    if (hashGuard()) return; // a view holding unsaved work: the hash went back
    show();
  });
  bindSkipLink(document.querySelector('.skip'), $('main'));

  for (const name of VIEW_MODULES) {
    try { await import('./views/' + name + '.js'); } catch { /* optional view not shipped */ }
  }

  await probe();
}

// probe asks who is logged in, once per call (boot and each retry click).
// Only a 401 (session() returns null) means the login screen; any other
// failure shows the connection-failure pane with a retry button.
async function probe() {
  let sess = null;
  try {
    sess = await session(); // the only auth probe: never polled
  } catch {
    leave();
    setAuthUI(false);
    banner('');
    replaceContent($('main'), renderBootFailure(probe));
    return;
  }
  if (sess) await enter(sess);
  else { setAuthUI(false); await show(); }
}

if (typeof document !== 'undefined' && !globalThis.__ORCH_NO_BOOT__) boot();
