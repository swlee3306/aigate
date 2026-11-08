import { test } from 'node:test';
import assert from 'node:assert/strict';

const { parseRoute, registerView, getView, navItem } = await import('../static/router.js');

test('parseRoute', () => {
  const eq = (h, name, params = []) => assert.deepEqual(parseRoute(h), { name, params }, h);
  eq('', 'overview'); eq('#', 'overview'); eq('#/', 'overview');
  eq('#/overview', 'overview'); eq('#/overview/', 'overview');
  eq('#/tasks', 'tasks'); eq('#/tasks/38', 'task', ['38']);
  eq('#/approvals', 'approvals'); eq('#/diff/38', 'diff', ['38']); eq('#/diff/38/main.go', 'diff', ['38', 'main.go']);
  eq('#/tasks/abc', 'notfound'); eq('#/tasks/1/2', 'notfound'); eq('#/nope', 'notfound'); eq('tasks', 'notfound');
  eq('#/diff/38/..', 'notfound'); eq('#/diff/38/a%2Fb', 'notfound'); eq('#/tasks/38?x=1', 'task', ['38']);
  eq('#/tasks/' + '9'.repeat(30), 'notfound'); eq(undefined, 'overview');
  assert.equal(navItem('task'), 'tasks');
});

test('registerView', () => {
  const f = () => {};
  registerView('login', f);
  assert.equal(getView('login'), f);
  assert.equal(getView('nothing'), null);
  assert.throws(() => registerView('Bad Name', f));
  assert.throws(() => registerView('x', 'not a function'));
});
