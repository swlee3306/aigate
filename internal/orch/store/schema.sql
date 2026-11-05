-- Schema version 1. Applied by migration 1; later changes become new migrations.

CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    repo        TEXT    NOT NULL,
    base        TEXT    NOT NULL,
    prompt      TEXT    NOT NULL,
    class       TEXT    NOT NULL DEFAULT '',
    agent       TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    retry_state TEXT    NOT NULL DEFAULT '',  -- step to resume when state = 'error'
    error_text  TEXT    NOT NULL DEFAULT '',
    running     INTEGER NOT NULL DEFAULT 0,   -- 1 while a worker holds the task
    created_at  INTEGER NOT NULL,             -- unix nanoseconds, UTC
    updated_at  INTEGER NOT NULL
);
CREATE INDEX tasks_runnable ON tasks (state, running, id);

CREATE TABLE task_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES tasks(id),
    from_state TEXT    NOT NULL,
    to_state   TEXT    NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    at         INTEGER NOT NULL
);
CREATE INDEX task_events_task ON task_events (task_id, id);

CREATE TABLE approvals_pending (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES tasks(id),
    kind       TEXT    NOT NULL CHECK (kind IN ('protected_modified','publish_outside_allowlist','destructive_blocked')),
    hash       TEXT    NOT NULL,
    base       TEXT    NOT NULL DEFAULT '',
    head       TEXT    NOT NULL DEFAULT '',
    reason     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    status     TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
    UNIQUE (task_id, kind, hash),
    -- destructive_blocked is a record only; no code path may make it approved.
    CHECK (NOT (kind = 'destructive_blocked' AND status = 'approved'))
);
