import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';

const api = await import('../static/api.js');

function res(status, body, headers = {}) {
  const h = new Map(Object.entries(headers).map(([k, v]) => [k.toLowerCase(), String(v)]));
  return { status, headers: { get: (k) => h.get(k.toLowerCase()) ?? null }, text: async () => (typeof body === 'string' ? body : JSON.stringify(body)) };
}
const envelope = (code) => ({ error: { code, message: 'm' } });

let calls;
let sleeps;
function stub(...responses) {
  calls = [];
  sleeps = [];
  const q = [...responses];
  api.configure({
    fetch: async (path, init) => {
      calls.push({ path, init });
      const next = q.shift();
      if (!next) throw new Error('unexpected extra fetch: ' + path);
      return typeof next === 'function' ? next(path, init) : next;
    },
    sleep: async (ms) => { sleeps.push(ms); },
  });
}
let expired;
beforeEach(() => {
  api.setCsrf(null);
  expired = 0;
});
api.onSessionExpired(() => expired++);

test('csrf header only on non-GET and only when held', async () => {
  stub(res(200, {}), res(200, {}), res(200, {}));
  await api.post('/api/v1/tasks', { a: 1 });
  assert.equal(calls[0].init.headers['X-Orch-CSRF'], undefined, 'no token held, none sent');
  api.setCsrf('tok1');
  await api.post('/api/v1/tasks', { a: 1 });
  assert.equal(calls[1].init.headers['X-Orch-CSRF'], 'tok1');
  assert.equal(calls[1].init.headers['Content-Type'], 'application/json');
  assert.equal(calls[1].init.credentials, 'same-origin');
  await api.get('/api/v1/stages');
  assert.equal(calls[2].init.headers['X-Orch-CSRF'], undefined, 'GET never carries it');
  assert.equal(calls[2].init.body, undefined);
});

test('csrf_failed refreshes the session once and retries once', async () => {
  api.setCsrf('old');
  stub(res(403, envelope('csrf_failed')), res(200, { principal: 'p', kind: 'session', csrf: 'new' }), res(200, { ok: true }));
  const out = await api.post('/api/v1/tasks/1/cancel');
  assert.deepEqual(out, { ok: true });
  assert.deepEqual(calls.map((c) => c.path), ['/api/v1/tasks/1/cancel', '/api/v1/auth/session', '/api/v1/tasks/1/cancel']);
  assert.equal(calls[1].init.method, 'GET');
  assert.equal(calls[2].init.headers['X-Orch-CSRF'], 'new', 'retry uses the rotated token');
});

test('csrf_failed twice does not loop', async () => {
  api.setCsrf('old');
  stub(res(403, envelope('csrf_failed')), res(200, { csrf: 'new' }), res(403, envelope('csrf_failed')));
  await assert.rejects(api.post('/api/v1/tasks/1/cancel'), (e) => e.status === 403 && e.code === 'csrf_failed');
  assert.equal(calls.length, 3);
});

test('csrf refresh that finds no session expires the session, no loop', async () => {
  api.setCsrf('old');
  stub(res(403, envelope('csrf_failed')), res(401, envelope('unauthenticated')));
  await assert.rejects(api.post('/api/v1/tasks/1/cancel'), (e) => e.status === 401);
  assert.equal(calls.length, 2);
  assert.equal(expired, 1);
  assert.equal(api.hasCsrf(), false);
});

test('429 waits Retry-After once (capped) then retries once', async () => {
  stub(res(429, envelope('rate_limited'), { 'Retry-After': '120' }), res(200, { ok: 1 }));
  assert.deepEqual(await api.get('/api/v1/overview'), { ok: 1 });
  assert.deepEqual(sleeps, [30000], 'capped at 30s');
  assert.equal(calls.length, 2);

  stub(res(429, envelope('rate_limited'), { 'Retry-After': '3' }), res(429, envelope('rate_limited'), { 'Retry-After': '3' }));
  await assert.rejects(api.get('/api/v1/overview'), (e) => e.status === 429 && e.code === 'rate_limited');
  assert.deepEqual(sleeps, [3000], 'waits once only');
  assert.equal(calls.length, 2, 'no third attempt');
  assert.equal(expired, 0, '429 is never treated as logged out');

  stub(res(429, envelope('rate_limited')), res(200, {}));
  await api.get('/api/v1/overview');
  assert.deepEqual(sleeps, [2000], 'default wait without a header');
});

test('opts.noRetry429: a 429 is reported at once, without sleeping or a second request', async () => {
  stub(res(429, envelope('refine_rate_limited'), { 'Retry-After': '3' }), res(200, { ok: 1 }));
  await assert.rejects(api.post('/api/v1/refine', { a: 1 }, { noRetry429: true }), (e) => e.status === 429 && e.code === 'refine_rate_limited' && e.retryAfter === 3);
  assert.deepEqual(sleeps, []);
  assert.equal(calls.length, 1, 'exactly one request');
  // without the flag the old behaviour stays
  stub(res(429, envelope('rate_limited')), res(200, { ok: 1 }));
  assert.deepEqual(await api.post('/api/v1/refine', {}), { ok: 1 });
  assert.equal(calls.length, 2);
});

test('opts.signal aborts the request: the caller sees an error and the fetch signal is aborted', async () => {
  const ctl = new AbortController();
  let seen;
  stub((path, init) => new Promise((_, reject) => { seen = init.signal; init.signal.addEventListener('abort', () => reject(new Error('aborted'))); }));
  const p = api.post('/api/v1/refine', {}, { signal: ctl.signal });
  await new Promise((r) => setTimeout(r, 5));
  ctl.abort();
  await assert.rejects(p, (e) => e.name === 'ApiError' && e.status === 0);
  assert.equal(seen.aborted, true);
  // an already aborted signal never sends
  stub(res(200, {}));
  await assert.rejects(api.post('/api/v1/refine', {}, { signal: ctl.signal }), (e) => e.status === 0);
  assert.equal(calls.length, 0);
});

test('401 emits session-expired and clears the token', async () => {
  api.setCsrf('t');
  stub(res(401, envelope('unauthenticated')));
  await assert.rejects(api.get('/api/v1/overview'), (e) => e.status === 401 && e.code === 'unauthenticated');
  assert.equal(expired, 1);
  assert.equal(api.hasCsrf(), false);
});

test('session(): boot probe 401 is quiet and returns null; csrf adopted on success', async () => {
  stub(res(401, envelope('unauthenticated')));
  assert.equal(await api.session(), null);
  assert.equal(expired, 0);
  stub(res(200, { principal: 'a', kind: 'session', csrf: 'c1', expires: 'x' }));
  assert.equal((await api.session()).principal, 'a');
  assert.equal(api.hasCsrf(), true);
  // token-kind sessions have no csrf: the held one is dropped
  stub(res(200, { principal: 'a', kind: 'token' }));
  await api.session();
  assert.equal(api.hasCsrf(), false);
});

test('logout: 401 is success, token cleared either way', async () => {
  api.setCsrf('t');
  stub(res(401, envelope('unauthenticated')));
  await api.logout();
  assert.equal(api.hasCsrf(), false);
  assert.equal(expired, 0);
  api.setCsrf('t');
  stub(res(200, { ok: true }));
  await api.logout();
  assert.equal(api.hasCsrf(), false);
  api.setCsrf('t');
  stub(res(500, envelope('internal')));
  await assert.rejects(api.logout(), (e) => e.status === 500);
  assert.equal(api.hasCsrf(), false);
});

test('guards: path, polling auth, non-JSON, oversized, network error', async () => {
  stub();
  for (const bad of ['https://evil.example/api/v1/x', '//evil.example/api/v1/x', '/api/v2/x', '/other', '/api/v1/../x y', '']) {
    await assert.rejects(api.get(bad), (e) => e.code === 'bad_path', bad);
  }
  await assert.rejects(api.get('/api/v1/auth/session', { polling: true }), (e) => e.code === 'bad_path');
  await assert.rejects(api.get('/api/v1/auth/signers', { polling: true }), (e) => e.code === 'bad_path');
  assert.equal(calls.length, 0, 'nothing was sent');

  stub(res(200, '<html>proxy page</html>', { 'Content-Type': 'text/html' }));
  await assert.rejects(api.get('/api/v1/stages'), (e) => e.code === 'bad_response');
  stub(res(502, '<html>bad gateway</html>'));
  await assert.rejects(api.get('/api/v1/stages'), (e) => e.status === 502 && e.code === 'http_502');
  stub(res(200, {}, { 'Content-Length': 100 * 1024 * 1024 }));
  await assert.rejects(api.get('/api/v1/stages'), (e) => e.code === 'too_large');
  stub(() => { throw new TypeError('boom'); });
  await assert.rejects(api.get('/api/v1/stages'), (e) => e.status === 0 && e.code === 'network');
});

test('request timeout aborts the fetch', async () => {
  stub((path, init) => new Promise((_, reject) => init.signal.addEventListener('abort', () => reject(new Error('aborted')))));
  await assert.rejects(api.request('GET', '/api/v1/stages', undefined, { timeoutMs: 10 }), (e) => e.code === 'timeout');
});

test('session(): 429 (after its one retry) and network errors are errors, not "logged out"', async () => {
  stub(res(429, envelope('rate_limited'), { 'Retry-After': '1' }), res(429, envelope('rate_limited')));
  await assert.rejects(api.session(), (e) => e.status === 429);
  assert.equal(expired, 0);
  stub(() => { throw new TypeError('down'); });
  await assert.rejects(api.session(), (e) => e.code === 'network');
  assert.equal(expired, 0);
});

// ---- opts.headers: Idempotency-Key only (W2a) ----------------------------------

const KEY = 'AbCdEfGh0123456789AbCdEfGh012345';

test('Idempotency-Key header is sent on POST, and the same key on the 429 and CSRF retries', async () => {
  api.setCsrf('tok');
  stub(res(201, { id: 1 }));
  await api.post('/api/v1/tasks', { a: 1 }, { headers: { 'Idempotency-Key': KEY } });
  assert.equal(calls[0].init.headers['Idempotency-Key'], KEY);
  assert.equal(calls[0].init.headers['X-Orch-CSRF'], 'tok');

  stub(res(429, envelope('queue_full')), res(201, { id: 1 }));
  await api.post('/api/v1/tasks', {}, { headers: { 'Idempotency-Key': KEY } });
  assert.deepEqual(calls.map((c) => c.init.headers['Idempotency-Key']), [KEY, KEY]);

  stub(res(403, envelope('csrf_failed')), res(200, { csrf: 'new' }), res(201, { id: 2 }));
  await api.post('/api/v1/tasks', {}, { headers: { 'Idempotency-Key': KEY } });
  assert.equal(calls.length, 3);
  assert.equal(calls[0].init.headers['Idempotency-Key'], KEY);
  assert.equal(calls[2].init.headers['Idempotency-Key'], KEY);
  assert.equal(calls[1].init.headers['Idempotency-Key'], undefined, 'the session refresh carries no key');
  assert.equal(calls[2].init.headers['X-Orch-CSRF'], 'new');
});

test('opts.headers: anything but a well-formed Idempotency-Key on a POST is refused before any request', async () => {
  const bad = [
    { 'X-Orch-CSRF': 'x' },
    { 'x-orch-csrf': 'x' },
    { 'Content-Type': 'text/plain' },
    { Accept: 'text/html' },
    { Cookie: 'a=b' },
    { Authorization: 'Bearer x' },
    { 'idempotency-key': KEY },
    { 'Idempotency-Key': KEY, 'X-Orch-CSRF': 'x' },
    { 'Idempotency-Key': 'short' },
    { 'Idempotency-Key': KEY + '\r\nX-Evil: 1' },
    { 'Idempotency-Key': 'a b'.repeat(8) },
    { 'Idempotency-Key': 123 },
    { 'Idempotency-Key': 'A'.repeat(65) },
    {},
    'Idempotency-Key',
    ['Idempotency-Key'],
  ];
  for (const h of bad) {
    stub(res(200, {}));
    await assert.rejects(() => api.post('/api/v1/tasks', {}, { headers: h }), (e) => e.code === 'bad_header', JSON.stringify(h));
    assert.equal(calls.length, 0, 'no request for ' + JSON.stringify(h));
  }
  stub(res(200, {}));
  await assert.rejects(() => api.get('/api/v1/tasks', { headers: { 'Idempotency-Key': KEY } }), (e) => e.code === 'bad_header');
  assert.equal(calls.length, 0);
  // no headers option: unchanged
  stub(res(200, {}));
  await api.post('/api/v1/tasks', {});
  assert.equal(calls[0].init.headers['Idempotency-Key'], undefined);
});

test('ApiError carries the server `reason` of a not_retryable refusal (bounded), nothing else extra', async () => {
  stub(res(409, { error: { code: 'not_retryable', message: 'm', reason: 'R'.repeat(900) } }));
  await assert.rejects(() => api.post('/api/v1/tasks/1/retry', {}), (e) => e.code === 'not_retryable' && e.reason.length === 500);
  stub(res(409, { error: { code: 'not_retryable', message: 'm', reason: 42, secret: 'x' } }));
  await assert.rejects(() => api.post('/api/v1/tasks/1/retry', {}), (e) => e.reason === undefined && e.secret === undefined);
});
