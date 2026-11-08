-- Schema version 6: web console auth state.
--   sessions: id_hash/csrf_hash are SHA-256 hex; the raw tokens are never stored.
--   challenges: single-use WebAuthn/login challenges (random nonces, not credentials).
--   idempotency_keys: key_hash is SHA-256 hex of the client key; body_hash detects key reuse with another body.
-- All times are UnixNano UTC like the rest of the schema.
CREATE TABLE sessions (
    id_hash    TEXT PRIMARY KEY,
    principal  TEXT NOT NULL,
    kind       TEXT NOT NULL,
    csrf_hash  TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE challenges (
    id         TEXT PRIMARY KEY,
    purpose    TEXT NOT NULL,
    data       TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX challenges_expires ON challenges(expires_at);

CREATE TABLE idempotency_keys (
    key_hash   TEXT PRIMARY KEY,
    body_hash  TEXT NOT NULL,
    task_id    INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL
);
CREATE INDEX idempotency_expires ON idempotency_keys(expires_at);
