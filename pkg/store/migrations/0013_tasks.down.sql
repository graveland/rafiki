DROP INDEX IF EXISTS conversations.conversation_repo_root_idx;
ALTER TABLE conversations.conversation
    DROP COLUMN IF EXISTS repo_root,
    DROP COLUMN IF EXISTS cwd;
DROP TABLE IF EXISTS conversations.tasks;
