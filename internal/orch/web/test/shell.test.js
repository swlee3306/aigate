import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDocument, findAll, byClass } from './dom-stub.js';

const doc = installDocument();
const { el } = await import('../static/render.js');
const { bindSkipLink, showNotice, replaceContent, renderBootFailure } = await import('../static/shell.js');
const { shouldRoute, parseRoute } = await import('../static/router.js');

test('skip link focuses main and does not change the route or content', () => {
  const main = el('main', { tabindex: '-1' }, [el('p', {}, 'content')]);
  const link = el('a', { class: 'skip', href: '#/overview' }, 'skip');
  bindSkipLink(link, main);
  const ev = { defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
  link.listeners.click[0](ev);
  assert.equal(ev.defaultPrevented, true, 'default navigation (hash change) is cancelled');
  assert.equal(doc.activeElement, main);
  assert.equal(main.textContent, 'content', 'content untouched');
  // and the hashchange path ignores what such a link would have produced
  for (const h of ['#main', '#', '']) assert.equal(shouldRoute(h), false);
  assert.equal(parseRoute('#main'), null);
  // a real unknown route still routes (to the not-found pane)
  assert.equal(shouldRoute('#/nope'), true);
  assert.equal(parseRoute('#/nope').name, 'notfound');
});

test('notices: errors alert, info is a neutral status', () => {
  const b = el('div', { class: 'banner', role: 'alert', hidden: true });
  showNotice(b, 'info', '로그아웃했습니다.');
  assert.equal(b.getAttribute('role'), 'status');
  assert.match(b.className, /\binfo\b/);
  assert.doesNotMatch(b.className, /\berror\b/);
  assert.equal(b.textContent, '로그아웃했습니다.');
  assert.equal(b.hidden, false);
  showNotice(b, 'error', '<b>x</b>');
  assert.equal(b.getAttribute('role'), 'alert');
  assert.match(b.className, /\berror\b/);
  assert.equal(b.childNodes.length, 1);
  assert.equal(b.children.length, 0, 'text only');
  showNotice(b, 'error', '');
  assert.equal(b.hidden, true);
});

test('boot failure pane: retry runs once per click, not while pending', async () => {
  let calls = 0;
  let release;
  const pane = renderBootFailure(() => { calls++; return new Promise((r) => { release = r; }); });
  assert.match(pane.textContent, /서버에 연결하지 못했습니다/);
  assert.match(pane.textContent, /다시 시도/);
  assert.doesNotMatch(pane.textContent, /로그인/);
  const btn = findAll(pane, (e) => e.tagName === 'BUTTON')[0];
  btn.listeners.click[0]();
  btn.listeners.click[0]();
  assert.equal(calls, 1, 'second click while pending is ignored');
  release();
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  btn.listeners.click[0]();
  assert.equal(calls, 2, 'next click retries again');
});

test('replaceContent restores focus by data-focus-key, else to the heading', () => {
  const root = el('main', {});
  const first = el('div', {}, [el('h1', {}, '개요'), el('a', { href: '#/approvals', 'data-focus-key': 'todo-approvals' }, '승인')]);
  replaceContent(root, first);
  const link = findAll(root, (e) => e.tagName === 'A')[0];
  link.focus();
  const second = el('div', {}, [el('h1', {}, '개요'), el('a', { href: '#/approvals', 'data-focus-key': 'todo-approvals' }, '승인')]);
  replaceContent(root, second);
  const newLink = findAll(root, (e) => e.tagName === 'A')[0];
  assert.notEqual(newLink, link);
  assert.equal(doc.activeElement, newLink, 'equivalent element focused');
  // key gone: heading takes focus
  const third = el('div', {}, [el('h1', {}, '개요'), el('p', {}, 'calm')]);
  replaceContent(root, third);
  const h1 = findAll(root, (e) => e.tagName === 'H1')[0];
  assert.equal(doc.activeElement, h1);
  assert.equal(h1.getAttribute('tabindex'), '-1');
  // focus outside root is left alone
  const outside = el('button', {}, 'x');
  outside.focus();
  replaceContent(root, el('div', {}, [el('h1', {}, 'z')]));
  assert.equal(doc.activeElement, outside);
});
