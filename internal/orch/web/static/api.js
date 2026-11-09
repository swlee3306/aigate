// api.js: the only place the console talks to the server. JSON only, same
// origin, cookies by the browser (the session cookie is HttpOnly and never
// visible here). The CSRF token lives in a module variable: never in storage
// or the DOM. Keep this file free of top-level document/window access.

const MAX_BODY_BYTES = 8 << 20; // above the largest server cap (report, 2 MiB)
const DEFAULT_TIMEOUT_MS = 15000;
const MAX_RETRY_WAIT_S = 30;
const DEFAULT_RETRY_WAIT_S = 2;
// Only API paths of this origin are ever requested, so the CSRF header can
// not be sent anywhere else.
const PATH_RE = /^\/api\/v1\/[A-Za-z0-9._~\/?=&%-]*$/;

// Request headers a caller may add through opts.headers: ONLY the idempotency
// key of a POST (a fixed shape). Everything else, the CSRF header included, is
// api.js's own business and is refused here, not silently dropped.
const IDEMPOTENCY_HEADER = 'Idempotency-Key';
const IDEMPOTENCY_RE = /^[A-Za-z0-9]{16,64}$/;

let csrf = null;
const deps = {
  fetch: (...a) => globalThis.fetch(...a),
  sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
};

export const bus = new EventTarget();

// setCsrf stores the token from the login finish body or GET /auth/session
// (a token-kind session has none: null clears it).
export function setCsrf(token) {
  csrf = typeof token === 'string' && token ? token : null;
}
export function hasCsrf() {
  return csrf !== null;
}

// configure swaps fetch/sleep (tests). Pass nothing to restore the defaults.
export function configure(over) {
  deps.fetch = (over && over.fetch) || ((...a) => globalThis.fetch(...a));
  deps.sleep = (over && over.sleep) || ((ms) => new Promise((resolve) => setTimeout(resolve, ms)));
}

export function onSessionExpired(fn) {
  const h = () => fn();
  bus.addEventListener('session-expired', h);
  return () => bus.removeEventListener('session-expired', h);
}

export class ApiError extends Error {
  constructor(status, code, message, retryAfter) {
    super(message || code || 'request failed');
    this.name = 'ApiError';
    this.status = status;
    this.code = code || '';
    this.retryAfter = retryAfter;
  }
}

function expire() {
  csrf = null;
  bus.dispatchEvent(new Event('session-expired'));
}

function retryWaitSeconds(res) {
  const v = res.headers && res.headers.get ? res.headers.get('Retry-After') : null;
  const n = v === null ? NaN : Number(v);
  const s = Number.isFinite(n) && n >= 0 ? n : DEFAULT_RETRY_WAIT_S;
  return Math.min(s, MAX_RETRY_WAIT_S);
}

async function readJSON(res) {
  const len = Number(res.headers.get('Content-Length'));
  if (Number.isFinite(len) && len > MAX_BODY_BYTES) throw new ApiError(res.status, 'too_large', '응답이 너무 큽니다');
  const text = await res.text();
  if (text.length > MAX_BODY_BYTES) throw new ApiError(res.status, 'too_large', '응답이 너무 큽니다');
  if (text === '') return null;
  try {
    return JSON.parse(text);
  } catch {
    return undefined; // not JSON
  }
}

async function once(method, path, body, opts) {
  const headers = { Accept: 'application/json' };
  const init = { method, headers, credentials: 'same-origin', cache: 'no-store', redirect: 'error' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  if (opts.headers) headers[IDEMPOTENCY_HEADER] = opts.headers[IDEMPOTENCY_HEADER];
  if (method !== 'GET' && method !== 'HEAD' && csrf) headers['X-Orch-CSRF'] = csrf;
  const ctl = new AbortController();
  init.signal = ctl.signal;
  // opts.signal: the caller's own abort (a cancel button). Aborting it aborts
  // the fetch; the caller treats the resulting error as its own doing.
  const ext = opts.signal && typeof opts.signal.addEventListener === 'function' ? opts.signal : null;
  const onExt = () => ctl.abort();
  if (ext) {
    if (ext.aborted) throw new ApiError(0, 'aborted', '요청이 취소되었습니다');
    ext.addEventListener('abort', onExt);
  }
  const timer = setTimeout(() => ctl.abort(), opts.timeoutMs || DEFAULT_TIMEOUT_MS);
  try {
    const res = await deps.fetch(path, init);
    const data = await readJSON(res);
    return { res, data };
  } catch (err) {
    if (err instanceof ApiError) throw err;
    throw new ApiError(0, ctl.signal.aborted ? 'timeout' : 'network', '서버에 연결할 수 없습니다');
  } finally {
    clearTimeout(timer);
    if (ext) ext.removeEventListener('abort', onExt);
  }
}

function toError(res, data) {
  const e = data && typeof data === 'object' ? data.error : null;
  const code = e && typeof e.code === 'string' ? e.code : '';
  const msg = e && typeof e.message === 'string' ? e.message : '';
  const err = new ApiError(res.status, code || 'http_' + res.status, msg || '요청을 처리하지 못했습니다');
  // `reason` is the fixed sentence of a not_retryable refusal (server-built);
  // views show it only as sanitized text.
  if (e && typeof e.reason === 'string' && e.reason) err.reason = e.reason.slice(0, 500);
  return err;
}

function checkHeaders(method, h) {
  if (h === undefined || h === null) return;
  const proto = typeof h === 'object' ? Object.getPrototypeOf(h) : null;
  const names = typeof h === 'object' && (proto === Object.prototype || proto === null) ? Object.keys(h) : null;
  const ok = names !== null && names.length === 1 && names[0] === IDEMPOTENCY_HEADER && method === 'POST'
    && typeof h[IDEMPOTENCY_HEADER] === 'string' && IDEMPOTENCY_RE.test(h[IDEMPOTENCY_HEADER]);
  if (!ok) throw new ApiError(0, 'bad_header', '허용되지 않은 요청 헤더입니다');
}

// request performs one API call with at most one CSRF refresh (CF15) and at
// most one rate-limit wait (CF19; opts.noRetry429 skips the wait and the second
// request, for calls that cost money or a quota slot); it never loops. opts.headers may carry only
// an Idempotency-Key (POST); the same key is sent on the CSRF and 429 retries. A 401 emits
// `session-expired` unless opts.quiet401 is set (boot probe, logout).
export async function request(method, path, body, opts) {
  opts = opts || {};
  method = String(method).toUpperCase();
  if (typeof path !== 'string' || !PATH_RE.test(path)) throw new ApiError(0, 'bad_path', '허용되지 않은 경로입니다');
  if (opts.polling && path.startsWith('/api/v1/auth/')) {
    throw new ApiError(0, 'bad_path', '인증 경로는 주기적으로 호출하지 않습니다'); // CF19
  }
  checkHeaders(method, opts.headers);
  let refreshed = false;
  let waited = false;
  for (;;) {
    const { res, data } = await once(method, path, body, opts);
    if (res.status >= 200 && res.status < 300) {
      if (data === undefined) throw new ApiError(res.status, 'bad_response', '서버 응답을 읽을 수 없습니다');
      return data;
    }
    const err = toError(res, data === undefined ? null : data);
    if (res.status === 429 && !waited && !opts.noRetry429) {
      waited = true;
      await deps.sleep(retryWaitSeconds(res) * 1000);
      continue;
    }
    if (res.status === 429) { // still limited, or the caller opted out of the wait (noRetry429)
      err.retryAfter = retryWaitSeconds(res);
      throw err; // still limited: report it, do not treat it as logged out
    }
    if (res.status === 403 && err.code === 'csrf_failed' && !refreshed && !opts.noRefresh) {
      refreshed = true;
      await refreshSession(); // throws if the session is gone
      continue;
    }
    if (res.status === 401 && !opts.quiet401) expire();
    throw err;
  }
}

export const get = (path, opts) => request('GET', path, undefined, opts);
export const post = (path, body, opts) => request('POST', path, body === undefined ? {} : body, opts);

function adopt(s) {
  if (s && typeof s === 'object') setCsrf(s.csrf);
  return s;
}

// refreshSession calls GET /auth/session, which rotates the CSRF token. It is
// called at boot and on a csrf_failed, never on a timer.
async function refreshSession() {
  return adopt(await request('GET', '/api/v1/auth/session', undefined, { noRefresh: true }));
}

// session returns the current session ({principal, kind, csrf?, expires}) or
// null when nobody is logged in (the expected 401 at boot is not an error).
export async function session() {
  try {
    return adopt(await request('GET', '/api/v1/auth/session', undefined, { noRefresh: true, quiet401: true }));
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      csrf = null;
      return null;
    }
    throw err;
  }
}

// logout treats a 401 as success (the session is already gone, CF17) and
// forgets the token either way.
export async function logout() {
  try {
    await request('POST', '/api/v1/auth/logout', undefined, { quiet401: true });
  } catch (err) {
    if (!(err instanceof ApiError && err.status === 401)) {
      csrf = null;
      throw err;
    }
  }
  csrf = null;
}
