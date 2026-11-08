// login.js: the login screen (#/login, and every route while logged out).
// Passkey login (pick an active passkey, then the browser's passkey prompt)
// and passkey enrollment with a one-time code from `orch admin enroll`.
// Enrollment only ever creates a PENDING credential: an administrator must
// activate it on the server. Errors are fixed Korean texts chosen by the
// server's error code; the server's own message is never shown. Nothing
// secret (code, challenge, CSRF, credential ids) is put in the DOM.

import { el, inline } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent, copyButton } from '../shell.js';
import {
  login, register, activeSigners, supported, waText, WebAuthnError, normalizeEnrollCode, ENROLL_INPUT_MAX,
} from '../webauthn.js';

// Fixed texts by server error code (login and enrollment endpoints).
export const LOGIN_ERRORS = {
  bad_assertion: '패스키 로그인에 실패했습니다. 등록된 패스키로 처음부터 다시 시도하세요.',
  bad_challenge: '요청이 만료되었거나 이미 사용되었습니다. 처음부터 다시 시도하세요.',
  signers_unavailable: '서버의 서명자 목록을 신뢰할 수 없어 지금은 처리할 수 없습니다. 관리자에게 알리세요.',
  webauthn_unconfigured: '이 서버에는 패스키가 설정되어 있지 않습니다.',
  busy: '진행 중인 요청이 많아 지금은 처리할 수 없습니다. 잠시 뒤 다시 시도하세요.',
  rate_limited: '요청이 많아 잠시 쉬는 중입니다. 잠시 후 다시 시도하세요.',
  bad_enroll_code: '등록 코드가 올바르지 않거나 만료되었거나 이미 사용되었습니다. 서버에서 orch admin enroll 로 새 코드를 받으세요.',
  bad_attestation: '패스키 등록 응답이 올바르지 않습니다. 처음부터 다시 등록하세요.',
  bad_origin: '이 주소는 서버에 설정된 주소(origin)와 다릅니다. 서버에 설정된 주소로 접속하세요.',
  already_pending: '이 패스키나 ID는 이미 대기 목록에 있습니다. 관리자의 활성화를 기다리세요.',
  bad_request: '요청이 올바르지 않습니다. 처음부터 다시 시도하세요.',
  body_too_large: '요청이 너무 큽니다.',
};

// loginErrorText: fixed text for any failure of this screen.
export function loginErrorText(err) {
  if (err instanceof WebAuthnError || (err && err.name === 'WebAuthnError')) return waText(err.code);
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (Object.prototype.hasOwnProperty.call(LOGIN_ERRORS, code)) return LOGIN_ERRORS[code];
  const st = err && typeof err.status === 'number' ? err.status : -1;
  if (st === 429) return LOGIN_ERRORS.rate_limited;
  if (st === 0) return '서버에 연결하지 못했습니다.';
  return '요청을 처리하지 못했습니다. 잠시 후 다시 시도하세요.';
}

// pendingCommand is the activation command an administrator runs. id and
// fingerprint were validated by register() (signer id / SHA256:base64).
export function pendingCommand(r) {
  return 'sudo orch admin signers add --pending ' + r.id + ' --fingerprint ' + r.fingerprint;
}

function statusLine(msg) {
  if (!msg) return el('p', { class: 'note', role: 'status' });
  return el('p', { class: msg.error ? 'note err' : 'note', role: msg.error ? 'alert' : 'status' }, msg.text);
}

export function loginView(ctx) {
  ctx.setCrumb('로그인');
  const ok = supported();
  const st = {
    signers: null, // null while loading
    loadErr: null,
    picked: -1,
    busy: false,
    loginMsg: ok ? null : { error: true, text: waText('unsupported') },
    enrollMsg: null,
    enrolled: null, // {id, label, fingerprint}
  };
  let codeInput = null;
  let codeDraft = ''; // kept in memory only so a repaint keeps what was typed

  const doLogin = async () => {
    if (st.busy || !ok || !st.signers || !st.signers[st.picked]) return;
    st.busy = true;
    st.loginMsg = { text: '패스키 확인을 기다리는 중…' };
    paint();
    let sess = null;
    try {
      sess = await login(st.signers[st.picked].id, { post: ctx.post });
    } catch (err) {
      if (ctx.signal && ctx.signal.aborted) return;
      st.loginMsg = { error: true, text: loginErrorText(err) };
    } finally {
      st.busy = false;
    }
    if (sess) {
      await ctx.onAuthenticated(sess);
      return;
    }
    paint();
  };

  const doEnroll = async () => {
    if (st.busy || !ok) return;
    const typed = codeInput && typeof codeInput.value === 'string' ? codeInput.value : codeDraft;
    if (!normalizeEnrollCode(typed)) {
      st.enrollMsg = { error: true, text: waText('bad_code_format') };
      paint();
      return;
    }
    st.busy = true;
    st.enrolled = null;
    st.enrollMsg = { text: '패스키 만들기를 기다리는 중…' };
    // The code is single use: forget it as soon as it is sent.
    codeDraft = '';
    if (codeInput) codeInput.value = '';
    paint();
    try {
      st.enrolled = await register(typed, { post: ctx.post });
      st.enrollMsg = null;
    } catch (err) {
      if (ctx.signal && ctx.signal.aborted) return;
      st.enrollMsg = { error: true, text: loginErrorText(err) };
    } finally {
      st.busy = false;
    }
    paint();
  };

  function passkeyList() {
    if (st.loadErr) return el('p', { class: 'note err', role: 'alert' }, st.loadErr);
    if (st.signers === null) return el('p', { class: 'note' }, '패스키 목록을 불러오는 중…');
    if (st.signers.length === 0) {
      return el('p', { class: 'note' }, '활성화된 패스키가 없습니다. 아래에서 패스키를 등록한 뒤 관리자에게 활성화를 요청하세요.');
    }
    return el('ul', { class: 'files', 'aria-label': '활성 패스키' }, st.signers.map((s, i) => el('li', {}, el('button', {
      type: 'button', class: 'filebtn', 'aria-pressed': i === st.picked ? 'true' : 'false', 'data-focus-key': 'pk-' + i,
      on: { click: () => { st.picked = i; paint(); } },
    }, [
      el('span', { class: 'fname' }, inline(s.label, 64) || '(라벨 없음)'),
      el('span', { class: 'fmeta mono' }, inline(s.id, 64)),
    ]))));
  }

  function enrolledBox() {
    const r = st.enrolled;
    if (!r) return null;
    const cmd = pendingCommand(r);
    return el('div', { class: 'ask', role: 'status' }, [
      el('p', {}, [el('b', { class: 'k' }, '대기 중'), ': 패스키를 서버의 대기 목록에 올렸습니다. 아직 로그인이나 서명에 쓸 수 없습니다.']),
      el('p', { class: 'note' }, '관리자가 서버에서 지문을 확인하고 아래 명령으로 활성화해야 합니다.'),
      el('dl', {}, [
        el('dt', {}, 'ID'), el('dd', { class: 'mono' }, r.id),
        el('dt', {}, '라벨'), el('dd', {}, inline(r.label, 64) || '-'),
        el('dt', {}, '지문'), el('dd', { class: 'mono' }, r.fingerprint),
      ]),
      el('div', { class: 'cmd' }, [el('code', {}, cmd), copyButton(cmd)]),
    ]);
  }

  function paint() {
    codeInput = el('input', {
      type: 'text', id: 'enroll-code', name: 'enroll-code', class: 'codein', maxlength: String(ENROLL_INPUT_MAX),
      autocomplete: 'off', spellcheck: 'false', inputmode: 'text', placeholder: 'XXXX-XXXX',
      'aria-describedby': 'enroll-help', disabled: st.busy || !ok,
      on: { input: () => { codeDraft = typeof codeInput.value === 'string' ? codeInput.value.slice(0, ENROLL_INPUT_MAX) : ''; } },
    });
    codeInput.value = codeDraft;
    const canLogin = ok && !st.busy && !!st.signers && st.picked >= 0 && st.picked < st.signers.length;
    replaceContent(ctx.root, el('div', { class: ['panel', 'canvas', 'login'] }, [
      el('h1', { id: 'h-login' }, '로그인'),
      el('p', { class: 'lead' }, '등록된 패스키로 로그인합니다. 승인 서명은 로그인과 별도로 매번 패스키로 합니다.'),
      el('section', { class: 'sec', 'aria-labelledby': 'h-pk' }, [
        el('h2', { id: 'h-pk' }, '패스키 고르기'),
        passkeyList(),
        el('button', { type: 'button', class: 'btn primary', 'data-focus-key': 'login', disabled: !canLogin, on: { click: doLogin } },
          '패스키로 로그인'),
        statusLine(st.loginMsg),
      ]),
      el('section', { class: 'sec', 'aria-labelledby': 'h-enroll' }, [
        el('h2', { id: 'h-enroll' }, '새 패스키 등록'),
        el('p', { class: 'note', id: 'enroll-help' },
          '서버에서 관리자가 orch admin enroll 로 받은 8자리 등록 코드를 입력하세요. 등록한 패스키는 관리자가 활성화하기 전까지 쓸 수 없습니다.'),
        el('label', { for: 'enroll-code' }, '등록 코드'),
        el('div', { class: 'row' }, [
          codeInput,
          el('button', { type: 'button', class: 'btn', 'data-focus-key': 'enroll', disabled: st.busy || !ok, on: { click: doEnroll } },
            '패스키 등록'),
        ]),
        statusLine(st.enrollMsg),
        enrolledBox(),
      ]),
    ]));
  }

  paint();
  if (!ok) return undefined;
  // Once per visit of this screen, never polled (CF19; api.js waits out one 429).
  return activeSigners({ get: ctx.get }, { fresh: true }).then((list) => {
    if (ctx.signal && ctx.signal.aborted) return;
    st.signers = list;
    if (list.length === 1) st.picked = 0;
    paint();
  }, (err) => {
    if (ctx.signal && ctx.signal.aborted) return;
    st.loadErr = loginErrorText(err);
    paint();
  });
}

registerView('login', loginView);
