DROP INDEX IF EXISTS conversations.conversation_top_created_idx;
DROP INDEX IF EXISTS conversations.conversation_top_recent_idx;
DROP INDEX IF EXISTS conversations.conversation_parent_recent_idx;
ALTER TABLE conversations.conversation DROP COLUMN IF EXISTS parent_conversation_id;
