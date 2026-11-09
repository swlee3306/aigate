-- Schema version 8: the merge request of a published task.
--   branch    the branch the task was pushed to ("orch/<id>"), NULL until the MR step ran
--   mr_iid    the GitLab merge request number (NULL unless created)
--   mr_url    its web address (http(s) only; NULL unless created)
--   mr_state  NULL/'' not attempted, 'created', 'failed', 'skipped'
--   mr_code   fixed failure code of a failed MR ([a-z_]{1,32}), NULL otherwise
-- Existing rows keep NULL in all five columns.
ALTER TABLE tasks ADD COLUMN branch TEXT;
ALTER TABLE tasks ADD COLUMN mr_iid INTEGER;
ALTER TABLE tasks ADD COLUMN mr_url TEXT;
ALTER TABLE tasks ADD COLUMN mr_state TEXT;
ALTER TABLE tasks ADD COLUMN mr_code TEXT;
