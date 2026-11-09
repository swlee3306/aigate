-- Schema version 7: who made each change.
--   task_events.actor is "kind:id" (socket:uid:1000, session:<signer>,
--   token:<signer>) or "system"; NULL for events recorded before v7.
ALTER TABLE task_events ADD COLUMN actor TEXT;
