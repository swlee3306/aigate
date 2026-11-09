import { test } from 'node:test';
import assert from 'node:assert/strict';

const { parseRoute, shouldRoute, registerView, getView, navItem } = await import('../static/router.js');

test('parseRoute', () => {
  const eq = (h, name, params = []) => assert.deepEqual(parseRoute(h), { name, params }, h);
  eq('', 'overview'); eq('#', 'overview'); eq('#/', 'overview');
  eq('#/overview', 'overview'); eq('#/overview/', 'overview');
  eq('#/tasks', 'tasks'); eq('#/tasks/38', 'task', ['38']);
  eq('#/approvals', 'approvals'); eq('#/diff/38', 'diff', ['38']); eq('#/diff/38/main.go', 'notfound'); eq('#/diff/38/x', 'notfound');
  eq('#/tasks/abc', 'notfound'); eq('#/tasks/1/2', 'notfound'); eq('#/nope', 'notfound');
  eq('#/diff/38/..', 'notfound'); eq('#/diff/38/a%2Fb', 'notfound'); eq('#/tasks/38?x=1', 'task', ['38']);
  eq('#/tasks/' + '9'.repeat(30), 'notfound'); eq(undefined, 'overview');
  eq('#/new', 'new'); eq('#/new/plan', 'plan'); eq('#/new/plan/', 'plan'); eq('#/new/other', 'notfound'); eq('#/new/plan/x', 'notfound');
  assert.equal(navItem('task'), 'tasks');
  assert.equal(navItem('plan'), 'new');
});

test('registerView', () => {
  const f = () => {};
  registerView('login', f);
  assert.equal(getView('login'), f);
  assert.equal(getView('nothing'), null);
  assert.throws(() => registerView('Bad Name', f));
  assert.throws(() => registerView('x', 'not a function'));
});

test('non-route fragments are never routes and never notfound', () => {
  for (const h of ['#main', '#content', 'tasks', '#x/y']) {
    assert.equal(parseRoute(h), null, h);
    assert.equal(shouldRoute(h), false, h);
  }
  for (const h of ['#', '', undefined, null]) assert.equal(shouldRoute(h), false, String(h));
  assert.equal(shouldRoute('#/nope'), true);
  assert.deepEqual(parseRoute('#/nope'), { name: 'notfound', params: [] }, 'a real unknown route still is notfound');
  assert.deepEqual(parseRoute('#/tasks/abc'), { name: 'notfound', params: [] });
});
