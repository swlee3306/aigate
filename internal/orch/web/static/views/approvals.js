// approvals.js: pending approvals (#/approvals) and the signing screen of one
// approval (#/approvals/ID).
//
// What the human sees is what is signed: the signing screen shows the
// server's canonical message verbatim, its digest, the server's meaning text
// and the hashed diff, and signs THAT detail object (the one on screen) with
// the passkey; the WebAuthn challenge is the message digest (webauthn.js).
// The web does NOT recompute the diff hash (design 8.7): the screen says so,
// and never claims the hash was checked. The sign button is a native button
// that stays disabled until the user ticked "위 내용을 읽었습니다"; the grant
// request is sent only after the authenticator returned a signature. Anything
// cut or not fully visible (server truncation, client clipping, an unseen
// protected file, an omitted file name, a cut meaning) is refused outright, as
// `orch approve` does (R22): no confirmation controls, no authenticator, the
// user is sent to the terminal. Server error texts are never shown.

import { el, inline, idOf, errorPane, capLine, splitLines, sanitize, clip, textBlock, MAX_LINE } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent, copyButton } from '../shell.js';
import { checkMessage, consistency, signApproval, activeSigners, rememberedPasskey, supported, waText, WA_TEXT } from '../webauthn.js';
import { fileEntries } from './diff.js';

const POLL_MS = 10000;
const MAX_DETAILS = 10; // details fetched for the list (each runs git on the server)
const SNIPPET_LINES = 12;
const MAX_FILES = 50;
export const MAX_BLOCK_LINES = 5000;
export const NOT_RECOMPUTED = '이 화면은 해시를 다시 계산하지 않았습니다 (확인되지 않음). 서버가 보여 준 내용을 믿고 서명합니다. ' +
  '전체 diff는 터미널의 orch approve 로 직접 확인할 수 있습니다.';
export const CUT_REFUSAL = WA_TEXT.truncated;
const MAX_MEANING = 4000;

// actionLabel names a publish action derived by webauthn.js deriveAction.
export function actionLabel(action) {
  const fixed = { push_branch: '작업 브랜치를 원격 저장소로 push', draft_pr: '초안 PR 만들기', notify: '알림 보내기' };
  if (has(fixed, action)) return fixed[action] + ' (' + action + ')';
  if (typeof action === 'string' && action.startsWith('policy_changed:')) {
    return '작업 시작 뒤 바뀐 정책으로 게시를 계속하기 (정책 해시 ' + inline(action.slice('policy_changed:'.length), 70) + ')';
  }
  return inline(action, 120);
}

const KIND_LABEL = {
  protected_modified: '보호 파일 수정',
  publish_outside_allowlist: '허용 목록 밖 게시',
  destructive_blocked: '파괴적 변경 (기록 전용)',
};

// Fixed texts by the grant endpoint's error code.
export const GRANT_ERRORS = {
  hash_changed: '작업 내용이 바뀌어 이 승인은 더 이상 맞지 않습니다. 새 내용을 다시 불러왔습니다. 다시 읽고 확인하세요.',
  stale_signature: '서명 시각이 허용 범위(±5분)를 벗어났습니다. 내용을 다시 불러왔습니다. 다시 읽고 서명하세요.',
  not_pending: '이미 처리되었거나 대기 중이 아닌 항목입니다. 아무것도 바뀌지 않았습니다.',
  conflict: '서버가 승인을 처리하지 못했습니다. 아무것도 바뀌지 않았습니다.',
  bad_signature: '서버가 서명을 받아들이지 않았습니다. 아무것도 바뀌지 않았습니다.',
  unknown_signer: '이 패스키는 서버의 서명자 목록에 없습니다. 아무것도 바뀌지 않았습니다.',
  invalid_message: '서버가 서명 메시지를 만들지 못했습니다. 아무것도 바뀌지 않았습니다.',
  not_signed_mode: '이 서버는 서명 필수 모드가 아닙니다. 터미널의 orch approve 로 승인하세요.',
  signers_unavailable: '서버의 서명자 목록을 신뢰할 수 없어 승인을 처리할 수 없습니다. 관리자에게 알리세요.',
  forbidden: '이 항목은 승인할 수 없습니다.',
  csrf_failed: '세션 확인에 실패했습니다. 다시 로그인한 뒤 시도하세요.',
  rate_limited: '요청이 많아 잠시 쉬는 중입니다. 잠시 후 다시 시도하세요.',
  bad_request: '요청이 올바르지 않습니다. 아무것도 바뀌지 않았습니다.',
  body_too_large: '요청이 너무 큽니다.',
  not_found: '승인 항목을 찾을 수 없습니다.',
};

export function grantErrorText(err) {
  if (err && err.name === 'WebAuthnError') return waText(err.code);
  const code = err && typeof err.code === 'string' ? err.code : '';
  if (Object.prototype.hasOwnProperty.call(GRANT_ERRORS, code)) return GRANT_ERRORS[code];
  const st = err && typeof err.status === 'number' ? err.status : -1;
  if (st === 429) return GRANT_ERRORS.rate_limited;
  if (st === 0) return '서버에 연결하지 못했습니다. 승인이 처리되었는지 목록에서 확인하세요.';
  return '서버에서 오류가 발생했습니다. 승인이 처리되었는지 목록에서 확인하세요.';
}

const has = (o, k) => Object.prototype.hasOwnProperty.call(o, k);
const kindLabel = (k) => (typeof k === 'string' && has(KIND_LABEL, k) ? KIND_LABEL[k] : inline(k, 60) || '?');
const HEX_RE = /^[0-9a-f]{7,64}$/;
const short = (s) => (typeof s === 'string' && HEX_RE.test(s) ? s.slice(0, 12) : inline(s, 16) || '-');

// longText: server text that must not be cut silently (meaning carries the
// full hash): sanitized, cut only past a large cap with a marker.
function longText(s, max) {
  const c = clip(sanitize(s), max || MAX_MEANING);
  return c.dropped ? c.text + ' …(+' + c.dropped + '자)' : c.text;
}

// analyze decides, from the detail alone, whether it can be signed and
// whether anything is not visible on screen. problem is a webauthn.js code;
// cut is true when the server cut the text, the screen clips it (too many
// lines, an over-long line), a protected file has no visible section, not all
// protected file names are listed, or the meaning text is cut. cut is a hard
// refusal.
export function analyze(detail) {
  let problem = null;
  try {
    checkMessage(detail);
  } catch (err) {
    problem = err && err.code ? err.code : 'bad_message';
  }
  const text = detail && typeof detail.hashed_text === 'string' ? detail.hashed_text : '';
  const { lines, total } = splitLines(text, MAX_BLOCK_LINES);
  let clipped = total > lines.length;
  if (!clipped) {
    for (const l of lines) {
      if (l.length > MAX_LINE && capLine(l) !== sanitize(l, { keepTab: true })) {
        clipped = true;
        break;
      }
    }
  }
  const fe = detail ? fileEntries(detail, MAX_BLOCK_LINES) : { entries: [] };
  const unseen = fe.entries.some((e) => e.kind === 'unseen' || e.kind === 'cut');
  const serverCut = !!detail && detail.truncated === true;
  const files = detail && Array.isArray(detail.protected_files) ? detail.protected_files : [];
  const omitted = files.length > MAX_FILES; // names not all listed on screen
  const meaningCut = !!detail && sanitize(detail.meaning).length > MAX_MEANING;
  return { problem, clipped, unseen, serverCut, omitted, meaningCut, cut: serverCut || clipped || unseen || omitted || meaningCut };
}

// --- list ------------------------------------------------------------------

function keyOf(a) {
  return [idOf(a.id), a.hash, a.head, a.status].map((x) => String(x)).join('|');
}

function snippet(text) {
  if (typeof text !== 'string' || text === '') return null;
  const { lines, total } = splitLines(text, SNIPPET_LINES);
  return el('div', {}, [
    el('div', { class: 'block', role: 'region', tabindex: '0', 'aria-label': '변경 내용 일부' },
      lines.map((l) => el('div', {}, capLine(l) || ' '))),
    total > lines.length ? el('p', { class: 'note' }, '처음 ' + lines.length + '줄만 보입니다 (전체 ' + total + '줄). 열어서 전체를 읽습니다.') : null,
  ]);
}

function fileList(files) {
  const list = Array.isArray(files) ? files.filter((p) => typeof p === 'string') : [];
  if (list.length === 0) return null;
  const shown = list.slice(0, MAX_FILES);
  return el('ul', { class: 'pfiles', 'aria-label': '보호 파일' }, [
    shown.map((p) => el('li', { class: 'mono' }, inline(p, 200))),
    list.length > shown.length ? el('li', { class: 'note' }, '외 ' + (list.length - shown.length) + '개') : null,
  ]);
}

function card(item, entry) {
  const id = idOf(item.id);
  const d = entry && entry.detail && typeof entry.detail === 'object' ? entry.detail : null;
  const task = idOf(item.task_id);
  const cut = d ? analyze(d).cut : false;
  return el('article', { class: ['card', 'appr'], 'aria-labelledby': 'appr-' + id }, [
    el('div', { class: 'card-h' }, [
      el('h2', { id: 'appr-' + id }, '승인 #' + id + (task ? ' · 작업 #' + task : '')),
      el('span', { class: ['tag', 'prot'] }, kindLabel(item.kind)),
    ]),
    el('div', { class: 'card-b' }, [
      el('p', { class: 'q' }, '무엇을 허용하나요?'),
      d ? el('p', { class: 'meaning' }, longText(d.meaning))
        : el('p', { class: 'note' }, entry && entry.error ? '상세를 불러오지 못했습니다. 열어서 다시 시도하세요.' : '상세를 불러오는 중이거나 목록이 길어 생략했습니다. 열어서 확인하세요.'),
      d ? el('dl', {}, [
        el('dt', {}, '저장소'), el('dd', { class: 'mono' }, inline(d.repo, 200) || '-'),
      ]) : null,
      d ? fileList(d.protected_files) : null,
      d ? snippet(d.hashed_text) : null,
      cut ? el('p', { class: 'trunc' }, CUT_REFUSAL) : null,
      el('a', { class: 'btn', href: '#/approvals/' + id, 'data-focus-key': 'open-' + id }, '열어서 읽고 서명 →'),
    ]),
  ]);
}

export function renderApprovals(items, details) {
  items = Array.isArray(items) ? items.filter((a) => a && idOf(a.id)) : [];
  details = details || new Map();
  return el('div', { class: ['panel', 'canvas'] }, [
    el('h1', { id: 'h-approvals' }, '승인'),
    el('p', { class: 'lead' }, '사람이 확인해야 하는 항목입니다. 항목을 열어 내용을 읽고 패스키로 서명합니다.'),
    items.length === 0
      ? el('p', { class: 'note', role: 'status' }, '대기 중인 승인이 없습니다.')
      : el('div', { class: 'cards' }, items.map((a) => card(a, details.get(keyOf(a))))),
  ]);
}

export function approvalsView(ctx) {
  ctx.setCrumb('승인');
  const details = new Map(); // keyOf(item) -> {detail} | {error}; once per item per visit
  let last = null;
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  ctx.poll(async () => {
    try {
      const list = await ctx.get('/api/v1/approvals', { polling: true });
      const items = (Array.isArray(list) ? list : []).filter((a) => a && idOf(a.id));
      const keys = new Set(items.map(keyOf));
      for (const k of [...details.keys()]) if (!keys.has(k)) details.delete(k);
      for (const a of items.slice(0, MAX_DETAILS)) {
        const k = keyOf(a);
        if (details.has(k)) continue;
        try {
          details.set(k, { detail: await ctx.get('/api/v1/approvals/' + idOf(a.id), { polling: true }) });
        } catch (err) {
          if (err && err.status === 401) throw err;
          details.set(k, { error: true });
        }
        if (ctx.signal && ctx.signal.aborted) return;
      }
      if (ctx.signal && ctx.signal.aborted) return;
      const key = JSON.stringify(items.map((a) => [keyOf(a), details.has(keyOf(a))]));
      if (key === last) return;
      last = key;
      replaceContent(ctx.root, renderApprovals(items, details));
    } catch (err) {
      if (!(ctx.signal && ctx.signal.aborted) && last === null) replaceContent(ctx.root, errorPane(err, '#/overview', '← 개요'));
      throw err; // poll backs off
    }
  }, POLL_MS);
}

// --- one approval: read, confirm, sign -----------------------------------------

// resolveSigner: the passkey of the logged-in principal. The login of this
// tab is remembered in memory; otherwise GET /auth/signers once per page load.
async function resolveSigner(ctx) {
  const s = ctx.session;
  if (!s || s.kind !== 'session' || typeof s.principal !== 'string') return { error: '패스키로 로그인한 세션에서만 웹 서명을 할 수 있습니다.' };
  const rem = rememberedPasskey();
  if (rem && rem.signer === s.principal) return { id: rem.signer, credentialId: rem.credentialId, rpId: rem.rpId };
  const list = await activeSigners({ get: ctx.get });
  const found = list.find((x) => x.id === s.principal);
  if (!found) return { error: '로그인한 서명자(' + inline(s.principal, 64) + ')의 활성 패스키를 찾지 못했습니다.' };
  return { id: found.id, credentialId: found.credential_id };
}

const PROBLEM_TEXT = {
  not_signed_mode: '이 서버는 서명 필수 모드가 아닙니다. 웹에서는 서명하지 않습니다. 승인은 터미널의 orch approve 로 하세요.',
  no_message: '지금은 서명할 수 있는 상태가 아닙니다. 항목이 이미 처리되었거나, 작업 내용이 바뀌었거나, 승인할 수 없는 종류입니다.',
};

export function renderApproval(st, h) {
  const d = st.detail;
  const id = idOf(d.id);
  const a = st.analysis;
  const msgText = typeof d.message === 'string' ? d.message : '';
  const digest = typeof d.digest === 'string' && /^[0-9a-f]{64}$/.test(d.digest) ? d.digest : null;
  const problem = a.problem || st.problem;
  const mr = /\nrepo: ([0-9a-f]{12})\n/.exec(msgText);
  const msgRepo = mr ? mr[1] : null; // the message's repo id, shown verbatim

  const facts = el('dl', {}, [
    el('dt', {}, '작업'), el('dd', {}, idOf(d.task_id) ? '#' + idOf(d.task_id) : '-'),
    el('dt', {}, '저장소'), el('dd', { class: 'mono' }, inline(d.repo, 200) || '-'),
    el('dt', {}, '저장소 ID'), el('dd', { class: 'mono' }, msgRepo || '-'),
    el('dt', {}, '종류'), el('dd', {}, kindLabel(d.kind)),
    d.kind === 'publish_outside_allowlist'
      ? [el('dt', {}, '행위'), el('dd', {}, st.action ? actionLabel(st.action) : '해시에서 찾지 못함')]
      : null,
    el('dt', {}, 'base'), el('dd', { class: 'mono' }, short(d.base)),
    el('dt', {}, 'head'), el('dd', { class: 'mono' }, short(d.head)),
  ]);

  const text = typeof d.hashed_text === 'string' ? d.hashed_text : '';
  const diffSec = text
    ? el('section', { class: 'sec', 'aria-labelledby': 'h-hashed' }, [
      el('h2', { id: 'h-hashed' }, '해시 대상 diff (보호 파일)'),
      textBlock(text, '해시 대상 diff', { maxLines: MAX_BLOCK_LINES }),
    ])
    : null;

  const msgSec = el('section', { class: 'sec', 'aria-labelledby': 'h-msg' }, [
    el('h2', { id: 'h-msg' }, '서명할 메시지 (서버가 만든 원문 그대로)'),
    msgText
      ? el('pre', { class: 'msg', role: 'region', tabindex: '0', 'aria-label': '서명할 메시지' },
        sanitize(clip(msgText, 2048).text, { keepNewline: true }))
      : el('p', { class: 'note' }, '(서버가 서명할 메시지를 주지 않았습니다)'),
    digest
      ? el('p', { class: 'note' }, [
        '다이제스트(SHA-256, 패스키가 서명하는 값) ',
        el('span', { class: 'mono' }, digest.slice(0, 16) + '…'), ' ',
        el('span', { class: ['mono', 'full'] }, digest), ' ',
        copyButton(digest),
      ])
      : null,
    el('p', { class: 'note' }, '이 메시지의 시각(issued)은 서버가 정한 값이며, 이 값 그대로 서명합니다. 5분 안에 서명하지 않으면 다시 불러와야 합니다.'),
  ]);

  const truncBox = a.cut
    ? el('div', { class: 'trunc', role: 'alert' }, [
      el('b', {}, CUT_REFUSAL),
      a.serverCut ? el('small', {}, ' · 서버가 diff를 잘라서 보냈습니다.') : null,
      a.clipped ? el('small', {}, ' · 화면에서 일부 줄을 생략했습니다.') : null,
      a.unseen ? el('small', {}, ' · 보이지 않는 보호 파일이 있습니다.') : null,
      a.omitted ? el('small', {}, ' · 보호 파일 이름을 모두 보여 주지 못했습니다.') : null,
      a.meaningCut ? el('small', {}, ' · 설명 글이 너무 길어 잘렸습니다.') : null,
    ])
    : null;

  let controls = null;
  if (a.cut) {
    controls = el('p', { class: 'trunc', role: 'status' }, CUT_REFUSAL);
  } else if (problem && has(PROBLEM_TEXT, problem)) {
    controls = el('p', { class: 'trunc', role: 'status' }, PROBLEM_TEXT[problem]);
  } else if (problem) {
    controls = el('p', { class: 'trunc', role: 'alert' }, waText(problem));
  } else if (st.done) {
    controls = el('p', { role: 'status' }, [el('a', { href: '#/approvals', 'data-focus-key': 'back' }, '← 승인 목록으로')]);
  } else {
    controls = el('div', { class: 'confirm' }, [
      el('div', { class: 'row' }, [h.checkbox, el('label', { for: 'confirm-read' }, '위 내용을 읽었습니다')]),
      st.signerNote ? el('p', { class: 'note' }, st.signerNote) : null,
      h.signBtn,
    ]);
  }

  return el('div', { class: 'ws' }, [
    el('div', { class: ['panel', 'canvas'] }, [
      el('h1', { id: 'h-approval' }, '승인 #' + id + ' 서명'),
      el('p', { class: 'lead' }, '아래 내용을 끝까지 읽고 패스키로 서명합니다. 서명은 이 메시지 하나에만 적용됩니다.'),
      el('div', { class: ['ask', 'appr-main'] }, [
        el('p', { class: 'k' }, '무엇을 허용하나요?'),
        el('p', { class: 'meaning' }, longText(d.meaning)),
      ]),
      truncBox,
      diffSec,
      msgSec,
      el('p', { class: ['note', 'not-recomputed'], role: 'note' }, NOT_RECOMPUTED),
      controls,
      st.msg ? el('p', { class: st.msg.error ? 'note err' : 'note ok', role: st.msg.error ? 'alert' : 'status' }, st.msg.text) : null,
    ]),
    el('aside', { class: ['panel', 'insp'], 'aria-label': '승인 항목 정보' }, [
      el('h2', {}, '항목'),
      facts,
      el('h2', { class: 'sec' }, '보호 파일'),
      fileList(d.protected_files) || el('p', { class: 'note' }, '없음'),
    ]),
  ]);
}

export function approvalView(ctx) {
  const id = idOf(ctx.params && ctx.params[0]);
  ctx.setCrumb(id ? '승인 #' + id : '승인');
  if (!id) {
    ctx.root.append(errorPane({ status: 404 }, '#/approvals', '← 승인 목록'));
    return undefined;
  }
  let st = null;
  let seq = 0;
  let signerP = null; // the passkey is resolved once per visit
  const h = {};

  const canSign = () => !!st && !st.busy && !st.done && !st.analysis.problem && !st.analysis.cut && !st.problem &&
    st.digestChecked && !!st.signer && h.checkbox.checked === true && supported();
  const refresh = () => {
    if (!h.signBtn) return;
    if (canSign()) h.signBtn.removeAttribute('disabled');
    else h.signBtn.setAttribute('disabled', '');
  };

  function paint() {
    // Fresh controls on every paint: confirmation state lives in st, never in
    // a node that outlives a reload of the detail.
    h.checkbox = el('input', { type: 'checkbox', id: 'confirm-read', 'data-focus-key': 'read', on: { change: () => { st.read = h.checkbox.checked === true; refresh(); } } });
    h.checkbox.checked = st.read;
    h.signBtn = el('button', { type: 'button', class: 'btn primary', 'data-focus-key': 'sign', disabled: true, on: { click: onSign } },
      '패스키로 서명하고 승인');
    replaceContent(ctx.root, renderApproval(st, h));
    refresh();
  }

  async function load(msg) {
    const my = ++seq;
    if (st) {
      st.busy = true;
      refresh();
    }
    let detail;
    try {
      detail = await ctx.get('/api/v1/approvals/' + id);
    } catch (err) {
      if ((ctx.signal && ctx.signal.aborted) || my !== seq) return;
      replaceContent(ctx.root, errorPane(err, '#/approvals', '← 승인 목록'));
      return;
    }
    if ((ctx.signal && ctx.signal.aborted) || my !== seq) return;
    if (!detail || typeof detail !== 'object' || idOf(detail.id) !== id) {
      replaceContent(ctx.root, errorPane({ status: 500 }, '#/approvals', '← 승인 목록'));
      return;
    }
    st = {
      detail, analysis: analyze(detail), problem: null, digestChecked: false, signer: null, signerNote: null,
      read: false, busy: false, done: false, msg: msg || null, action: '',
    };
    paint();
    // Refused details never reach the authenticator or /auth/signers.
    if (st.analysis.problem || st.analysis.cut) return;
    try {
      st.action = (await consistency(detail)).action; // message, repo id, action, digest agree
      st.digestChecked = true;
    } catch (err) {
      st.problem = err && err.code ? err.code : 'digest_mismatch';
    }
    if (!st.problem) {
      if (!supported()) {
        st.problem = 'unsupported';
      } else {
        try {
          if (!signerP) signerP = resolveSigner(ctx);
          const s = await signerP;
          if (s.error) {
            signerP = null;
            st.signerNote = s.error;
          } else {
            st.signer = s;
            st.signerNote = '서명에 쓸 패스키: ' + inline(s.id, 64);
          }
        } catch (err) {
          signerP = null;
          st.signerNote = '패스키 목록을 불러오지 못했습니다. ' + grantErrorText(err);
        }
      }
    }
    if ((ctx.signal && ctx.signal.aborted) || my !== seq) return;
    paint();
  }

  async function onSign() {
    if (!canSign()) return; // also covers a double click: busy is set below before any await
    const cur = st;
    cur.busy = true;
    cur.msg = { text: '패스키 확인을 기다리는 중…' };
    paint();
    let body;
    try {
      body = await signApproval(cur.detail, cur.signer);
    } catch (err) {
      if (cur !== st) return;
      cur.busy = false;
      cur.read = false; // a new attempt needs a new confirmation
      cur.msg = { error: true, text: grantErrorText(err) };
      paint();
      return;
    }
    // The user left (or the detail was reloaded) while the passkey prompt was
    // open: drop the signature, send nothing.
    if ((ctx.signal && ctx.signal.aborted) || cur !== st) return;
    try {
      await ctx.post('/api/v1/approvals/' + id + '/grant', body);
    } catch (err) {
      if (cur !== st) return;
      if (err && (err.code === 'hash_changed' || err.code === 'stale_signature')) {
        await load({ error: true, text: grantErrorText(err) });
        return;
      }
      cur.busy = false;
      cur.read = false;
      cur.msg = { error: true, text: grantErrorText(err) };
      paint();
      return;
    }
    if (cur !== st) return;
    cur.done = true;
    cur.busy = false;
    cur.msg = { text: '승인했습니다. 작업을 다시 판정합니다.' };
    paint();
  }

  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  // One request per visit: the detail runs git on the server, and a refresh
  // would wipe the confirmation. Never polled.
  return load(null);
}

registerView('approvals', approvalsView);
registerView('approval', approvalView);
