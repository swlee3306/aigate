// ops.js: server status (#/ops) from GET /api/v1/ops. The cloud block has
// four states and a failure of it is NOT a failure of the page: absent (no
// supplier), {ok:false, code}, ok with an unreadable VM ("unknown"), ok.
// Polled slowly (the server caches the cloud picture for 60 s and /ops may
// wait up to 6 s for it). Every string and number from the server is shown as
// text; numbers are accepted only when they are sane.

import { el, inline, formatSeconds, formatTime, errorPane } from '../render.js';
import { registerView } from '../router.js';
import { replaceContent } from '../shell.js';

// Never faster than every 15 s (the cloud picture is cached for 60 s).
export const OPS_POLL_MS = 20000;
// A quota line warns from this share of its limit (in_use + reserved), for
// gigabytes and volumes independently; an unlimited line never warns.
export const QUOTA_WARN_RATIO = 0.8;
// Name of the cleanup script only: the page never tells anyone how to run it.
const CLEANUP_SCRIPT = 'infra/cleanup-restore-volumes.sh';
const MAX_HISTORY = 10;
const MAX_PHASES = 12;

const isObj = (x) => x !== null && typeof x === 'object' && !Array.isArray(x);
// count: a non-negative safe integer, else null (shown as "-").
const count = (x) => (typeof x === 'number' && Number.isSafeInteger(x) && x >= 0 ? x : null);
const num = (x) => (typeof x === 'number' && Number.isFinite(x) && x >= 0 && x < 1e9 ? x : null);
const dash = (x) => (x === null ? '-' : String(x));

// quotaLevel judges one Cinder quota line {limit, in_use, reserved, unlimited}:
// {kind:'unlimited'} | {kind:'invalid'} | {kind:'limited', used, limit, reserved,
// inUse, ratio, warn}. limit < 0 means unlimited, like the `unlimited` flag.
export function quotaLevel(line) {
  if (!isObj(line)) return { kind: 'invalid' };
  if (line.unlimited === true || (typeof line.limit === 'number' && line.limit < 0)) return { kind: 'unlimited', inUse: count(line.in_use) };
  const limit = count(line.limit);
  const inUse = count(line.in_use);
  const reserved = line.reserved === undefined || line.reserved === null ? 0 : count(line.reserved);
  if (limit === null || inUse === null || reserved === null) return { kind: 'invalid' };
  const used = inUse + reserved;
  const ratio = limit === 0 ? (used > 0 ? Infinity : 0) : used / limit;
  return { kind: 'limited', limit, inUse, reserved, used, ratio, warn: ratio >= QUOTA_WARN_RATIO };
}

const QUOTA_LINES = [
  { key: 'gigabytes', label: '볼륨 용량', unit: 'GB' },
  { key: 'volumes', label: '볼륨 개수', unit: '개' },
];

function quotaRow(def, line) {
  const q = quotaLevel(line);
  const head = el('span', {}, def.label);
  if (q.kind === 'unlimited') {
    return { row: el('li', { class: 'quota-r', 'data-quota': def.key }, [head, el('span', { class: 'mono' }, '제한 없음' + (q.inUse === null ? '' : ' · 사용 ' + q.inUse + ' ' + def.unit))]), warn: null };
  }
  if (q.kind === 'invalid') {
    return { row: el('li', { class: 'quota-r', 'data-quota': def.key }, [head, el('span', { class: 'mono' }, '-')]), warn: null };
  }
  const pct = Number.isFinite(q.ratio) ? Math.round(q.ratio * 100) : 100;
  const fill = el('div', { class: q.warn ? ['bar-f', 'warn'] : 'bar-f' });
  fill.style.width = Math.min(100, pct) + '%'; // CSSOM: CSP forbids style attributes
  const used = q.used + ' / ' + q.limit + ' ' + def.unit + ' (' + pct + '%)'
    + (q.reserved > 0 ? ' · 예약 ' + q.reserved : '');
  const row = el('li', { class: q.warn ? ['quota-r', 'is-warn'] : 'quota-r', 'data-quota': def.key }, [
    head,
    el('div', { class: 'bar-t', 'aria-hidden': 'true' }, fill), // decoration; the text carries the data
    el('span', { class: 'mono' }, used),
  ]);
  const warn = q.warn
    ? el('p', { class: ['quota-warn', 'note', 'err'], role: 'status' }, [
      el('b', {}, def.label + ' 쿼터가 ' + Math.round(QUOTA_WARN_RATIO * 100) + '% 이상 찼습니다 (' + pct + '%). '),
      '복원이 쿼터 초과로 실패하기 전에 남은 복원 볼륨을 정리해야 합니다. 정리 스크립트 이름: ',
      el('code', {}, CLEANUP_SCRIPT),
      ' (서버 담당자가 운영 절차에 따라 실행합니다).',
    ])
    : null;
  return { row, warn };
}

const CLOUD_FAIL_TEXT = {
  cloud_unavailable: '클라우드 상태를 지금 읽을 수 없습니다. 아래의 나머지 정보는 정상입니다. 잠시 뒤 다시 확인됩니다.',
  cloud_timeout: '클라우드 응답이 늦어 이번에는 건너뜁니다. 갱신은 뒤에서 계속되며 다음 확인 때 보입니다.',
};
const CLOUD_FAIL_GENERIC = '클라우드 상태를 지금 읽을 수 없습니다. 아래의 나머지 정보는 정상입니다.';

function vmText(vm) {
  if (!isObj(vm)) return null;
  const status = typeof vm.status === 'string' ? vm.status : '';
  const name = inline(vm.name, 60) || '-';
  if (status === 'unknown') return name + ' · 알 수 없음 (서버를 읽지 못했습니다. 복원 중이거나 서버가 없을 수 있습니다)';
  return name + ' · ' + (inline(status, 40) || '-');
}

function phasesText(phases) {
  const list = Array.isArray(phases) ? phases.filter(isObj).slice(0, MAX_PHASES) : [];
  if (list.length === 0) return '-';
  return list.map((p) => inline(p.name, 40) + ' ' + formatSeconds(num(p.seconds)) + (p.ok === false ? ' ✗' : '')).join(' · ');
}

// restoreOutcome: only an explicit true/false is success/failure; a missing
// or odd `ok` is "unknown", never shown as a failure.
export function restoreOutcome(r) {
  if (isObj(r) && r.ok === true) return { cls: 'c-done', text: '성공' };
  if (isObj(r) && r.ok === false) return { cls: 'c-fail', text: '실패' };
  return { cls: 'c-idle', text: '알 수 없음' };
}

function restoreItem(r) {
  const o = restoreOutcome(r);
  return el('li', {}, [
    el('span', {}, [
      el('span', { class: ['chip', o.cls] }, o.text),
      ' ',
      el('small', { class: 'ell' }, phasesText(r.phases)),
    ]),
    el('span', { class: 'mono' }, formatTime(r.at) + ' · ' + formatSeconds(num(r.seconds))),
  ]);
}

function cloudSection(cloud) {
  if (cloud === undefined || cloud === null) {
    return el('div', { class: 'sec' }, [
      el('h2', {}, '클라우드'),
      el('p', { class: 'note' }, '이 서버는 클라우드 상태를 조회하지 않도록 설정되어 있습니다 (조회 공급자 없음).'),
    ]);
  }
  if (!isObj(cloud) || cloud.ok !== true) {
    const code = isObj(cloud) && typeof cloud.code === 'string' ? cloud.code : '';
    const text = Object.prototype.hasOwnProperty.call(CLOUD_FAIL_TEXT, code) ? CLOUD_FAIL_TEXT[code] : CLOUD_FAIL_GENERIC;
    return el('div', { class: 'sec' }, [
      el('h2', {}, '클라우드'),
      el('p', { class: ['note', 'err'], role: 'status' }, text),
      isObj(cloud) && cloud.checked_at ? el('p', { class: 'note' }, '확인 시도 ' + formatTime(cloud.checked_at)) : null,
    ]);
  }
  const quota = isObj(cloud.quota) ? cloud.quota : null;
  const rows = quota ? QUOTA_LINES.map((d) => quotaRow(d, quota[d.key])) : [];
  const vm = vmText(cloud.test_vm);
  return el('div', { class: 'sec' }, [
    el('h2', {}, '클라우드'),
    quota
      ? el('ul', { class: 'quota', 'aria-label': '볼륨 쿼터' }, rows.map((r) => r.row))
      : el('p', { class: 'note' }, '쿼터 정보를 받지 못했습니다.'),
    ...rows.map((r) => r.warn),
    el('dl', {}, [
      el('dt', {}, '테스트 VM'), el('dd', {}, vm === null ? '-' : vm),
      el('dt', {}, '복원 볼륨'), el('dd', { class: 'mono' }, dash(count(cloud.restore_volumes)) + '개'),
      el('dt', {}, '확인 시각'), el('dd', { class: 'mono' }, formatTime(cloud.checked_at)),
    ]),
  ]);
}

function historySection(cloud) {
  const history = isObj(cloud) && Array.isArray(cloud.history) ? cloud.history.filter(isObj).slice(0, MAX_HISTORY) : [];
  return el('div', { class: 'sec' }, [
    el('h2', {}, '최근 복원 기록'),
    history.length > 0
      ? el('ul', { class: 'rows', 'aria-label': '최근 복원 기록' }, history.map(restoreItem))
      : el('p', { class: 'note' }, '기록이 없습니다.'),
  ]);
}

// renderOps renders the /ops response; any shape is tolerated.
export function renderOps(ops) {
  const o = isObj(ops) ? ops : {};
  const empty = Object.keys(o).length === 0;
  const last = isObj(o.last_restore) ? o.last_restore : null;
  const main = el('div', { class: ['panel', 'canvas'] }, [
    el('h1', { id: 'h-ops' }, '서버 상태'),
    empty
      ? el('p', { class: 'lead', role: 'status' }, '이 서버는 운영 정보를 제공하지 않습니다.')
      : el('p', { class: 'lead' }, '오케스트레이터와 테스트 VM의 현재 상태입니다. 20초마다 갱신됩니다.'),
    empty ? null : cloudSection(o.cloud),
    // with a cloud history the last restore is its first entry: show it once
    empty ? null : isObj(o.cloud) && o.cloud.ok === true ? historySection(o.cloud) : null,
    empty || !last || (isObj(o.cloud) && o.cloud.ok === true) ? null : el('div', { class: 'sec' }, [
      el('h2', {}, '마지막 복원'),
      el('ul', { class: 'rows' }, [restoreItem(last)]),
    ]),
  ]);
  const info = el('aside', { class: ['panel', 'insp'], 'aria-label': '오케스트레이터 정보' }, [
    el('h2', {}, '오케스트레이터'),
    el('dl', {}, empty ? [] : [
      el('dt', {}, '빌드'), el('dd', { class: 'mono' }, inline(o.build, 60) || '-'),
      el('dt', {}, '시작'), el('dd', { class: 'mono' }, formatTime(o.started)),
      el('dt', {}, '정책 해시'), el('dd', { class: 'mono' }, inline(o.policy_hash, 64) || '-'),
      el('dt', {}, 'base 캐시'), el('dd', { class: 'mono' }, dash(count(o.base_cache_entries)) + '건'),
    ]),
  ]);
  return el('div', { class: 'ws' }, [main, info]);
}

export function opsView(ctx) {
  ctx.setCrumb('서버 상태');
  ctx.root.append(el('p', { class: 'lead' }, '불러오는 중…'));
  let last = '';
  let have = false;
  ctx.poll(async () => {
    try {
      const ops = await ctx.get('/api/v1/ops', { polling: true });
      if (ctx.signal && ctx.signal.aborted) return;
      const key = JSON.stringify(ops);
      if (have && key === last) return;
      last = key;
      have = true;
      replaceContent(ctx.root, renderOps(ops));
    } catch (err) {
      // A failed poll keeps the data on screen (the live pill shows the failure).
      if (!(ctx.signal && ctx.signal.aborted) && !have) replaceContent(ctx.root, errorPane(err, '#/overview', '← 개요'));
      throw err;
    }
  }, OPS_POLL_MS);
}

registerView('ops', opsView);
