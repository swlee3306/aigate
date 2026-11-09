// guard.js: the "do not leave unsaved work" rule of the hash router, kept apart
// from app.js so it can be tested. A view that holds work (the AI plan)
// registers a guard function; while it answers true a hash change is undone
// and the person is told how to leave. A missing, released or failing guard
// never blocks (a bug here must not trap anyone).

export const LEAVE_BLOCKED_TEXT = '작성 중인 계획서가 있어 이동하지 않았습니다. 계획서 화면의 "계획서 버리기"로 나가세요.';

// makeHashGuard returns onHashChange(): true when the navigation was blocked.
// env: {guard() -> fn|null, current() -> the hash of the screen shown,
// restore(hash), notice(text)}.
export function makeHashGuard(env) {
  return function onHashChange() {
    const g = env.guard();
    if (typeof g !== 'function') return false;
    let hold = false;
    try {
      hold = g() === true;
    } catch {
      hold = false;
    }
    if (!hold) return false;
    env.restore(env.current() || '#/overview');
    env.notice(LEAVE_BLOCKED_TEXT);
    return true;
  };
}
