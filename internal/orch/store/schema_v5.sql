-- Schema version 5: operator retry of tasks that failed on an infrastructure error.
--   infra_failed = 1 when the task ended failed because a step kept failing (not by a verdict);
--   retry_state keeps the step that failed while state = 'failed';
--   retries counts operator retries (capped by the machine).
ALTER TABLE tasks ADD COLUMN infra_failed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN retries INTEGER NOT NULL DEFAULT 0;

-- Conservative backfill: only a failed task with a non-empty error_text whose last
-- failure event reads '<step> failed after N attempts...' (and <step> is that event's from_state).
UPDATE tasks SET
    infra_failed = 1,
    retry_state = (SELECT from_state FROM task_events WHERE task_id = tasks.id AND to_state = 'failed' ORDER BY id DESC LIMIT 1)
WHERE state = 'failed' AND error_text <> ''
  AND (SELECT note FROM task_events WHERE task_id = tasks.id AND to_state = 'failed' ORDER BY id DESC LIMIT 1)
      LIKE (SELECT from_state FROM task_events WHERE task_id = tasks.id AND to_state = 'failed' ORDER BY id DESC LIMIT 1) || ' failed after % attempts%';
