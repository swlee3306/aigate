-- Schema version 3: approvals_pending gains the status 'superseded' (an entry
-- whose task got a newer hash/head; it can never be approved). SQLite cannot
-- alter a CHECK, so the table is rebuilt with the same ids and rows.
CREATE TABLE approvals_pending_v3 (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES tasks(id),
    kind       TEXT    NOT NULL CHECK (kind IN ('protected_modified','publish_outside_allowlist','destructive_blocked')),
    hash       TEXT    NOT NULL,
    base       TEXT    NOT NULL DEFAULT '',
    head       TEXT    NOT NULL DEFAULT '',
    reason     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    status     TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','superseded')),
    UNIQUE (task_id, kind, hash),
    -- destructive_blocked is a record only; no code path may make it approved.
    CHECK (NOT (kind = 'destructive_blocked' AND status = 'approved'))
);
INSERT INTO approvals_pending_v3(id, task_id, kind, hash, base, head, reason, created_at, status)
    SELECT id, task_id, kind, hash, base, head, reason, created_at, status FROM approvals_pending;
DROP TABLE approvals_pending;
ALTER TABLE approvals_pending_v3 RENAME TO approvals_pending;
