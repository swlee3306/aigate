-- Schema version 2: what the task steps record about a task's result.
-- head_sha: the agent's head commit (fetched into refs/orch/<task>/head).
-- verdict: the gate category of the last verification ('' until verified).
-- report_path: where the verification report JSON was saved ('' until verified).
ALTER TABLE tasks ADD COLUMN head_sha    TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN verdict     TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN report_path TEXT NOT NULL DEFAULT '';
