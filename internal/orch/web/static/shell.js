// shell.js: small DOM behaviours of the page chrome, kept apart from app.js so
// they can be tested without booting the app. No top-level document access.

import { el, clear } from './render.js';

const doc = () => globalThis.document;

// bindSkipLink makes the skip link focus <main> without touching the hash
// (a changed hash would be routed and wipe the page).
export function bindSkipLink(link, main) {
  link.addEventListener('click', (ev) => {
    ev.preventDefault();
    main.focus({ preventScroll: false });
  });
}

// showNotice fills the banner. Errors are role="alert" (assertive); other
// notices are role="status" with a neutral style.
export function showNotice(banner, kind, msg) {
  clear(banner);
  if (!msg) {
    banner.hidden = true;
    return;
  }
  const error = kind === 'error';
  banner.setAttribute('role', error ? 'alert' : 'status');
  banner.setAttribute('class', 'banner ' + (error ? 'error' : 'info'));
  banner.appendChild(doc().createTextNode(msg));
  banner.hidden = false;
}

function findByFocusKey(node, key) {
  if (node.nodeType !== 1) return null;
  if (node.getAttribute('data-focus-key') === key) return node;
  for (const c of node.childNodes) {
    const f = findByFocusKey(c, key);
    if (f) return f;
  }
  return null;
}

function firstHeading(node) {
  if (node.nodeType !== 1) return null;
  if (node.tagName === 'H1') return node;
  for (const c of node.childNodes) {
    const f = firstHeading(c);
    if (f) return f;
  }
  return null;
}

// replaceContent swaps root's children for node. If the focused element was
// inside root it is restored to the element with the same data-focus-key in
// the new content, or else to the main heading, so a refresh never drops a
// keyboard user back to <body>.
export function replaceContent(root, node) {
  const active = doc().activeElement;
  const had = !!active && active !== root && typeof root.contains === 'function' && root.contains(active);
  const key = had && active.getAttribute ? active.getAttribute('data-focus-key') : null;
  clear(root);
  root.appendChild(node);
  if (!had) return;
  let target = key ? findByFocusKey(node, key) : null;
  if (!target) {
    target = firstHeading(node);
    if (target) target.setAttribute('tabindex', '-1');
  }
  if (target) target.focus();
}

// renderBootFailure is shown when the boot probe fails for a reason other
// than "not logged in" (429 after its retry, network down): not the login
// screen. The retry button runs onRetry once per click and ignores clicks
// while it runs.
export function renderBootFailure(onRetry) {
  let busy = false;
  const btn = el('button', {
    type: 'button',
    class: 'btn',
    'data-focus-key': 'retry',
    on: {
      click: async () => {
        if (busy) return;
        busy = true;
        btn.setAttribute('disabled', '');
        try {
          await onRetry();
        } finally {
          busy = false;
          btn.removeAttribute('disabled');
        }
      },
    },
  }, '다시 시도');
  return el('div', { class: ['panel', 'canvas'] }, [
    el('h1', {}, '서버에 연결하지 못했습니다'),
    el('p', { class: 'lead' }, '잠시 후 다시 시도하세요.'),
    btn,
  ]);
}

// copyButton copies text to the clipboard (when the browser allows it) and
// says whether it worked. The text is shown elsewhere on the page.
export function copyButton(text) {
  const btn = el('button', {
    type: 'button', class: 'btn',
    on: {
      click: async () => {
        const c = globalThis.navigator && globalThis.navigator.clipboard;
        try {
          if (!c) throw new Error('no clipboard');
          await c.writeText(text);
          clear(btn).append('복사됨');
        } catch {
          clear(btn).append('복사 안 됨');
        }
      },
    },
  }, '복사');
  return btn;
}
