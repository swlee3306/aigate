// expiry.js: the "tokens that are about to expire" banner, shared by the token
// screen and the project screen. It has no clock: it works from each row's
// `days_left` (counted by the broker in UTC) and the `warn_days` of the same
// answer. Every name is a text node.

import { el, inline } from './render.js';

export const WARN_DAYS_DEFAULT = 14;
const WARN_DAYS_MAX = 3650;
const MAX_NAMES = 10;

// warnDaysOf: the server's number when it is a plausible one, else 14.
function warnDaysOf(w) {
  return Number.isInteger(w) && w >= 0 && w <= WARN_DAYS_MAX ? w : WARN_DAYS_DEFAULT;
}

// expiringRows: the rows that are due within warnDays or already past
// (days_left <= warnDays, negative included). A row without a number never counts.
export function expiringRows(rows, warnDays) {
  const w = warnDaysOf(warnDays);
  return (Array.isArray(rows) ? rows : []).filter((r) => r !== null && typeof r === 'object' && Number.isInteger(r.days_left) && r.days_left <= w);
}

// expiryBanner returns the banner node, or null when nothing is due.
// opts.link adds a link to the token screen (used on the project screen).
export function expiryBanner(rows, warnDays, opts) {
  const due = expiringRows(rows, warnDays);
  if (due.length === 0) return null;
  const w = warnDaysOf(warnDays);
  const past = due.filter((r) => r.days_left < 0).length;
  const names = due.slice(0, MAX_NAMES).map((r) => inline(r.ref, 40));
  const more = due.length > MAX_NAMES ? ' 외 ' + (due.length - MAX_NAMES) + '개' : '';
  const head = '토큰 ' + due.length + '개가 ' + w + '일 안에 만료됩니다.' +
    (past > 0 ? ' 이미 만료일이 지난 토큰이 ' + past + '개 있습니다.' : '');
  return el('div', { class: 'expbar', role: 'status', 'data-banner': 'expiry' }, [
    el('p', { class: 'expbar-head' }, head),
    el('p', { class: 'expbar-names mono' }, '대상: ' + names.join(', ') + more),
    el('p', { class: 'note' }, '남은 일수는 UTC 날짜로 셉니다. 만료일은 사용자가 직접 적은 날짜이며 서버가 GitLab·GitHub에 확인한 값이 아니라서, 실제 만료와 다를 수 있습니다. 확인하지 않은 날짜이니 토큰 발급 화면에서 직접 보세요.'),
    opts && opts.link ? el('p', {}, [el('a', { href: '#/tokens', class: 'btn' }, '토큰 화면으로')]) : null,
  ]);
}
