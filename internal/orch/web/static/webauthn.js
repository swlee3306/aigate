// webauthn.js: passkey ceremonies of the console. Approval signing, passkey
// login and passkey enrollment (into the server's PENDING list only).
//
// Approval signing (design 8.2): the WebAuthn challenge is EXACTLY the
// 32-byte SHA-256 digest of the canonical message the server sent
// (detail.digest, hex). Before the authenticator is asked, the client checks
// that the message is a well-formed aigate-approval-v1 text, that each of its
// fields equals the same detail's fields, and that sha256(utf8(message)) is
// the digest. These are consistency checks between server values (the page
// trusts the server for what the diff hash covers: design 8.7). The issued
// time is the server's detail.issued, unchanged; this file never reads the
// clock.
//
// Nothing secret (challenges, CSRF tokens, credential material) is written to
// the DOM or to storage; the CSRF token goes to api.js memory only.
// Keep this file free of top-level document/window access (tests stub deps).

import { get as apiGet, post as apiPost, setCsrf } from './api.js';

const deps = {
  credentials: () => globalThis.navigator && globalThis.navigator.credentials,
  subtle: () => globalThis.crypto && globalThis.crypto.subtle,
};

// configure swaps the authenticator and WebCrypto (tests). Pass nothing to
// restore the browser's.
export function configure(over) {
  const c = over && over.credentials;
  const s = over && over.subtle;
  deps.credentials = c ? () => c : () => globalThis.navigator && globalThis.navigator.credentials;
  deps.subtle = s ? () => s : () => globalThis.crypto && globalThis.crypto.subtle;
  remembered = null;
  signersOnce = null;
}

export class WebAuthnError extends Error {
  constructor(code) {
    super(code);
    this.name = 'WebAuthnError';
    this.code = code;
  }
}

// Fixed texts per failure code. Server text is never shown.
export const WA_TEXT = {
  unsupported: '이 브라우저에서는 패스키를 쓸 수 없습니다. 최신 브라우저에서 HTTPS 주소로 접속하세요.',
  cancelled: '패스키 확인이 취소되었거나 시간이 지났습니다. 아무것도 보내지 않았습니다.',
  security: '이 주소에서는 패스키를 쓸 수 없습니다(주소와 서버 설정이 다릅니다). 아무것도 보내지 않았습니다.',
  invalid_state: '이 기기에는 이미 같은 패스키가 있습니다.',
  authenticator_failed: '패스키 처리에 실패했습니다. 아무것도 보내지 않았습니다.',
  not_signed_mode: '이 서버는 서명 필수 모드가 아닙니다. 웹에서는 서명하지 않습니다.',
  no_message: '지금은 서명할 수 있는 상태가 아닙니다(서버가 서명할 메시지를 주지 않았습니다).',
  hostile_message: '서명할 메시지에 보이지 않거나 제어용인 문자가 있습니다. 정상 메시지에는 없는 문자라서 서명하지 않습니다.',
  bad_message: '서명할 메시지의 형식이 올바르지 않아 서명하지 않습니다.',
  message_mismatch: '서명할 메시지의 내용이 이 승인 항목과 다릅니다. 서명하지 않습니다.',
  bad_digest: '서버가 준 다이제스트 형식이 올바르지 않아 서명하지 않습니다.',
  digest_mismatch: '서버가 준 메시지와 다이제스트가 서로 맞지 않습니다. 서명하지 않습니다.',
  truncation_unconfirmed: '잘린 내용이 있어 서명 전에 잘림 확인이 필요합니다.',
  bad_signer: '서명에 쓸 패스키 정보가 올바르지 않습니다.',
  bad_options: '서버가 보낸 패스키 요청이 올바르지 않습니다.',
  bad_response: '패스키 응답이 올바르지 않습니다. 아무것도 보내지 않았습니다.',
  bad_code_format: '등록 코드 형식이 올바르지 않습니다. 서버에서 받은 8자리 코드를 입력하세요.',
};

export function waText(code) {
  return Object.prototype.hasOwnProperty.call(WA_TEXT, code) ? WA_TEXT[code] : WA_TEXT.authenticator_failed;
}

// --- base64url (unpadded, canonical) ---------------------------------

const B64U_RE = /^[A-Za-z0-9_-]*$/;

function toBytes(x) {
  if (x instanceof Uint8Array) return x;
  if (x instanceof ArrayBuffer) return new Uint8Array(x);
  if (ArrayBuffer.isView(x)) return new Uint8Array(x.buffer, x.byteOffset, x.byteLength);
  throw new WebAuthnError('bad_response');
}

// b64uEncode encodes bytes (Uint8Array/ArrayBuffer/view) without padding.
export function b64uEncode(x) {
  const b = toBytes(x);
  let s = '';
  for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode.apply(null, b.subarray(i, i + 0x8000));
  return globalThis.btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// b64uDecode decodes canonical unpadded base64url and throws on anything
// else: padding, the standard alphabet, whitespace, impossible lengths and
// non-zero trailing bits (the input must re-encode to itself).
export function b64uDecode(s) {
  if (typeof s !== 'string' || !B64U_RE.test(s) || s.length % 4 === 1) throw new WebAuthnError('bad_options');
  let bin;
  try {
    bin = globalThis.atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4));
  } catch {
    throw new WebAuthnError('bad_options');
  }
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  if (b64uEncode(out) !== s) throw new WebAuthnError('bad_options');
  return out;
}

export function hexToBytes(h) {
  if (typeof h !== 'string' || !/^(?:[0-9a-f]{2})*$/.test(h)) throw new WebAuthnError('bad_digest');
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(h.slice(2 * i, 2 * i + 2), 16);
  return out;
}

function equalBytes(a, b) {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a[i] ^ b[i];
  return d === 0;
}

// --- support -------------------------------------------------------------

export function supported() {
  const c = deps.credentials();
  const s = deps.subtle();
  return !!c && typeof c.get === 'function' && typeof c.create === 'function' && !!s && typeof s.digest === 'function';
}

function requireSupport() {
  if (!supported()) throw new WebAuthnError('unsupported');
}

// fromDOMException maps an authenticator failure to a fixed code.
function fromDOMException(err) {
  if (err instanceof WebAuthnError) return err;
  const name = err && typeof err.name === 'string' ? err.name : '';
  if (name === 'NotAllowedError' || name === 'AbortError') return new WebAuthnError('cancelled');
  if (name === 'SecurityError') return new WebAuthnError('security');
  if (name === 'InvalidStateError') return new WebAuthnError('invalid_state');
  if (name === 'NotSupportedError') return new WebAuthnError('unsupported');
  return new WebAuthnError('authenticator_failed');
}

async function ask(kind, options) {
  requireSupport();
  let cred;
  try {
    cred = await deps.credentials()[kind](options);
  } catch (err) {
    throw fromDOMException(err);
  }
  if (!cred || cred.type !== 'public-key' || !cred.response) throw new WebAuthnError('bad_response');
  return cred;
}

const MAX_TIMEOUT_MS = 300000;
function timeoutOf(v) {
  return Number.isInteger(v) && v > 0 ? Math.min(v, MAX_TIMEOUT_MS) : 120000;
}

// --- canonical approval message ----------------------------------------

// The whole text of a valid aigate-approval-v1 message (signing.Message.Text):
// seven lines in a fixed order, each ending with \n, nothing else.
const MESSAGE_RE = new RegExp('^aigate-approval-v1\\n' +
  'kind: (protected_modified|publish_outside_allowlist)\\n' +
  'repo: ([0-9a-f]{12})\\n' +
  'task: ([1-9][0-9]{0,17})\\n' +
  'base: ([0-9a-f]{40}|[0-9a-f]{64})\\n' +
  'hash: ([0-9a-f]{16}|[0-9a-f]{64})\\n' +
  'issued: ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)\\n$');
// Characters that never occur in a canonical message but can change what a
// human sees: controls other than \n, bidi/zero-width/format characters.
const HOSTILE_RE = /[\u0000-\u0009\u000b-\u001f\u007f-\u009f\u00ad\u034f\u061c\u115f\u1160\u17b4\u17b5\u180b-\u180e\u200b-\u200f\u2028-\u202e\u2060-\u2069\u3164\ufe00-\ufe0f\ufeff\ufff9-\ufffb\u{e0000}-\u{e007f}]/u;
const MAX_MESSAGE = 1024;

// checkMessage throws unless detail.message is a canonical message whose
// fields are exactly this detail's kind, task, base, hash and issued. It
// returns the parsed fields. Synchronous: used for display and before signing.
export function checkMessage(detail) {
  if (!detail || typeof detail !== 'object') throw new WebAuthnError('no_message');
  if (detail.requires_signature !== true) throw new WebAuthnError('not_signed_mode');
  const msg = detail.message;
  if (typeof msg !== 'string' || msg === '') throw new WebAuthnError('no_message');
  if (msg.length > MAX_MESSAGE) throw new WebAuthnError('bad_message');
  if (HOSTILE_RE.test(msg)) throw new WebAuthnError('hostile_message');
  const m = MESSAGE_RE.exec(msg);
  if (!m) throw new WebAuthnError('bad_message');
  const [, kind, repo, task, base, hash, issued] = m;
  if (kind !== detail.kind || task !== String(detail.task_id) || base !== detail.base || hash !== detail.hash ||
      issued !== detail.issued) {
    throw new WebAuthnError('message_mismatch');
  }
  return { kind, repo, task, base, hash, issued };
}

// digestBytes returns the 32 bytes of detail.digest after checking that they
// are the SHA-256 of the UTF-8 message (WebCrypto, never a JS hash).
export async function digestBytes(detail) {
  checkMessage(detail);
  if (typeof detail.digest !== 'string' || !/^[0-9a-f]{64}$/.test(detail.digest)) throw new WebAuthnError('bad_digest');
  const want = hexToBytes(detail.digest);
  requireSupport();
  const got = new Uint8Array(await deps.subtle().digest('SHA-256', new TextEncoder().encode(detail.message)));
  if (want.length !== 32 || !equalBytes(got, want)) throw new WebAuthnError('digest_mismatch');
  return want;
}

const SIGNER_RE = /^[a-z0-9][a-z0-9._-]{0,62}$/;
const ISSUED_RE = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$/;

// signApproval asks the passkey to sign one approval detail and returns the
// grant request body:
//   {signer, alg: 'webauthn-es256', sig, issued, webauthn: {authenticator_data, client_data_json}}
// signer = {id, credentialId (base64url), rpId?}. opts.acknowledgeTruncation
// must be true when detail.truncated (the human typed the confirmation).
// Every check runs BEFORE the authenticator is called; a failure throws a
// WebAuthnError and nothing is signed.
export async function signApproval(detail, signer, opts) {
  opts = opts || {};
  const challenge = await digestBytes(detail);
  if (!ISSUED_RE.test(detail.issued)) throw new WebAuthnError('bad_message');
  if (detail.truncated === true && opts.acknowledgeTruncation !== true) throw new WebAuthnError('truncation_unconfirmed');
  if (!signer || typeof signer.id !== 'string' || !SIGNER_RE.test(signer.id)) throw new WebAuthnError('bad_signer');
  let credId;
  try {
    credId = b64uDecode(signer.credentialId);
  } catch {
    throw new WebAuthnError('bad_signer');
  }
  if (credId.length === 0) throw new WebAuthnError('bad_signer');
  const publicKey = {
    challenge,
    allowCredentials: [{ id: credId, type: 'public-key' }],
    userVerification: 'required',
    timeout: 120000,
  };
  if (typeof signer.rpId === 'string' && signer.rpId) publicKey.rpId = signer.rpId;
  const cred = await ask('get', { publicKey });
  const r = cred.response;
  let rawId;
  try {
    rawId = toBytes(cred.rawId);
  } catch {
    throw new WebAuthnError('bad_response');
  }
  if (!equalBytes(rawId, credId)) throw new WebAuthnError('bad_response'); // not the passkey asked for
  if (!r.signature || !r.authenticatorData || !r.clientDataJSON) throw new WebAuthnError('bad_response');
  return {
    signer: signer.id,
    alg: 'webauthn-es256',
    sig: b64uEncode(r.signature), // DER ECDSA, as the authenticator returned it
    issued: detail.issued, // the server's value: the digest was computed over it
    webauthn: {
      authenticator_data: b64uEncode(r.authenticatorData),
      client_data_json: b64uEncode(r.clientDataJSON),
    },
  };
}

// --- login ---------------------------------------------------------------

const QUIET = { quiet401: true }; // a refused login is not an expired session
const CHALLENGE_ID_RE = /^[A-Za-z0-9_-]{1,128}$/;

let remembered = null; // {signer, credentialId, rpId} of this tab's passkey login
let signersOnce = null; // one GET /auth/signers per page load (CF19)

export function rememberedPasskey() {
  return remembered ? { ...remembered } : null;
}

// activeSigners returns GET /auth/signers' list, fetched at most once per
// page load (never polled: it shares the auth rate bucket). opts.fresh
// refetches (login screen, once per visit).
export function activeSigners(client, opts) {
  client = client || { get: apiGet };
  if (!signersOnce || (opts && opts.fresh)) {
    const p = client.get('/api/v1/auth/signers', QUIET).then((r) => {
      const list = r && Array.isArray(r.signers) ? r.signers : [];
      return list.filter((s) => s && typeof s.id === 'string' && SIGNER_RE.test(s.id) && typeof s.credential_id === 'string');
    });
    signersOnce = p;
    p.catch(() => { if (signersOnce === p) signersOnce = null; });
  }
  return signersOnce;
}

function decodeChallenge(v) {
  const c = b64uDecode(v);
  if (c.length < 16 || c.length > 64) throw new WebAuthnError('bad_options');
  return c;
}

// login runs passkey login for signer id: begin -> navigator.credentials.get
// -> finish. The CSRF token is handed to api.js; the returned session object
// has no secret in it.
export async function login(signerId, client) {
  client = client || { post: apiPost };
  if (typeof signerId !== 'string' || !SIGNER_RE.test(signerId)) throw new WebAuthnError('bad_signer');
  requireSupport();
  const begin = await client.post('/api/v1/auth/webauthn/login/begin', { signer: signerId }, QUIET);
  const o = begin && begin.options;
  if (!o || typeof begin.challenge_id !== 'string' || !CHALLENGE_ID_RE.test(begin.challenge_id)) throw new WebAuthnError('bad_options');
  const allow = Array.isArray(o.allowCredentials) ? o.allowCredentials : [];
  if (allow.length === 0 || allow.length > 16) throw new WebAuthnError('bad_options');
  const publicKey = {
    challenge: decodeChallenge(o.challenge),
    allowCredentials: allow.map((c) => ({ id: b64uDecode(c && c.id), type: 'public-key' })),
    userVerification: 'required',
    timeout: timeoutOf(o.timeout),
  };
  if (typeof o.rpId === 'string' && o.rpId) publicKey.rpId = o.rpId;
  const cred = await ask('get', { publicKey });
  const r = cred.response;
  if (!r.signature || !r.authenticatorData || !r.clientDataJSON) throw new WebAuthnError('bad_response');
  const rawId = b64uEncode(toBytes(cred.rawId));
  const done = await client.post('/api/v1/auth/webauthn/login/finish', {
    challenge_id: begin.challenge_id,
    credential: {
      id: rawId,
      rawId,
      type: 'public-key',
      response: {
        clientDataJSON: b64uEncode(r.clientDataJSON),
        authenticatorData: b64uEncode(r.authenticatorData),
        signature: b64uEncode(r.signature),
      },
    },
  }, QUIET);
  if (!done || typeof done.csrf !== 'string' || typeof done.signer !== 'string' || !SIGNER_RE.test(done.signer)) {
    throw new WebAuthnError('bad_response');
  }
  setCsrf(done.csrf);
  remembered = { signer: done.signer, credentialId: rawId, rpId: publicKey.rpId || '' };
  return { principal: done.signer, kind: 'session', expires: typeof done.expires === 'string' ? done.expires : '' };
}

// --- enrollment ------------------------------------------------------------

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
export const ENROLL_INPUT_MAX = 20;

// normalizeEnrollCode mirrors the server: upper case, '-' and spaces dropped,
// O->0, I/L->1, exactly 8 Crockford base32 characters. null when invalid.
export function normalizeEnrollCode(s) {
  if (typeof s !== 'string' || s.length > ENROLL_INPUT_MAX) return null;
  let out = '';
  for (let ch of s.toUpperCase()) {
    if (ch === '-' || ch === ' ') continue;
    if (ch === 'O') ch = '0';
    else if (ch === 'I' || ch === 'L') ch = '1';
    if (!CROCKFORD.includes(ch)) return null;
    out += ch;
  }
  return out.length === 8 ? out : null;
}

const FP_RE = /^SHA256:[A-Za-z0-9+/]{43}$/;

// register enrolls a new passkey with a one-time code: begin -> create ->
// finish. The result is a PENDING credential {id, label, fingerprint}; it is
// not a signer until an administrator activates it.
export async function register(code, client) {
  client = client || { post: apiPost };
  const norm = normalizeEnrollCode(code);
  if (!norm) throw new WebAuthnError('bad_code_format');
  requireSupport();
  const begin = await client.post('/api/v1/auth/webauthn/register/begin', { code: norm }, QUIET);
  const o = begin && begin.options;
  if (!o || typeof begin.challenge_id !== 'string' || !CHALLENGE_ID_RE.test(begin.challenge_id) ||
      !o.rp || typeof o.rp.id !== 'string' || !o.rp.id || !o.user) {
    throw new WebAuthnError('bad_options');
  }
  const userId = b64uDecode(o.user.id);
  if (userId.length === 0 || userId.length > 64) throw new WebAuthnError('bad_options');
  const publicKey = {
    rp: { id: o.rp.id, name: typeof o.rp.name === 'string' && o.rp.name ? o.rp.name : 'aigate' },
    user: {
      id: userId,
      name: typeof o.user.name === 'string' ? o.user.name : '',
      displayName: typeof o.user.displayName === 'string' ? o.user.displayName : '',
    },
    challenge: decodeChallenge(o.challenge),
    pubKeyCredParams: [{ type: 'public-key', alg: -7 }], // ES256 only: the server accepts nothing else
    timeout: timeoutOf(o.timeout),
    attestation: 'none',
    authenticatorSelection: { residentKey: 'preferred', userVerification: 'required' },
  };
  const cred = await ask('create', { publicKey });
  const r = cred.response;
  if (!r.attestationObject || !r.clientDataJSON) throw new WebAuthnError('bad_response');
  const rawId = b64uEncode(toBytes(cred.rawId));
  const done = await client.post('/api/v1/auth/webauthn/register/finish', {
    challenge_id: begin.challenge_id,
    credential: {
      id: rawId,
      rawId,
      type: 'public-key',
      response: {
        clientDataJSON: b64uEncode(r.clientDataJSON),
        attestationObject: b64uEncode(r.attestationObject),
      },
    },
  }, QUIET);
  if (!done || typeof done.id !== 'string' || !SIGNER_RE.test(done.id) || typeof done.fingerprint !== 'string' ||
      !FP_RE.test(done.fingerprint)) {
    throw new WebAuthnError('bad_response');
  }
  return { id: done.id, label: typeof done.label === 'string' ? done.label : '', fingerprint: done.fingerprint };
}
