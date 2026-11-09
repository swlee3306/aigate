import { test } from 'node:test';
import assert from 'node:assert/strict';

const { makeHashGuard, LEAVE_BLOCKED_TEXT } = await import('../static/guard.js');

function env(over) {
  const log = { restored: [], notices: [] };
  const e = {
    guard: () => null,
    current: () => '#/new/plan',
    restore: (h) => log.restored.push(h),
    notice: (t) => log.notices.push(t),
    ...(over || {}),
  };
  return { e, log, onChange: makeHashGuard(e) };
}

test('TestHashGuardRestoresHashAndTellsTheUser', () => {
  const a = env({ guard: () => () => true });
  assert.equal(a.onChange(), true, 'navigation blocked');
  assert.deepEqual(a.log.restored, ['#/new/plan']);
  assert.deepEqual(a.log.notices, [LEAVE_BLOCKED_TEXT]);
  assert.match(LEAVE_BLOCKED_TEXT, /계획서 버리기/);
  // no hash known: back to the overview, never an empty hash
  const b = env({ guard: () => () => true, current: () => '' });
  b.onChange();
  assert.deepEqual(b.log.restored, ['#/overview']);
});

test('TestHashGuardReleasedOrAbsentNeverBlocks', () => {
  for (const guard of [() => null, () => undefined, () => () => false, () => 'not a function']) {
    const a = env({ guard });
    assert.equal(a.onChange(), false);
    assert.deepEqual(a.log.restored, []);
    assert.deepEqual(a.log.notices, []);
  }
  // a guard that throws must never trap the person
  const t = env({ guard: () => () => { throw new Error('boom'); } });
  assert.equal(t.onChange(), false);
  assert.deepEqual(t.log.restored, []);
  // the guard is asked each time: released later = allowed later
  let hold = true;
  const c = env({ guard: () => () => hold });
  assert.equal(c.onChange(), true);
  hold = false;
  assert.equal(c.onChange(), false);
});
