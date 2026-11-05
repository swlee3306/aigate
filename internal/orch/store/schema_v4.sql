-- Schema version 4: the hash of the policy.yml a task started under ('' for tasks from before policies or without --policy).
ALTER TABLE tasks ADD COLUMN policy_hash TEXT NOT NULL DEFAULT '';
