-- Inverse of 0036_recall.up.sql. Extensions are deliberately NOT dropped:
-- uninstalling them would destroy data (vector columns elsewhere, bm25 access
-- method) for no benefit, and re-running the up migration recreates nothing.

DROP TABLE IF EXISTS conversations.recall_state;
DROP TABLE IF EXISTS conversations.recall_summary_failure;
DROP TABLE IF EXISTS conversations.conversation_summary;
DROP TABLE IF EXISTS conversations.conversation_window;
DROP TABLE IF EXISTS conversations.memory;
ALTER TABLE conversations.conversation DROP COLUMN IF EXISTS closed_at;
ALTER TABLE conversations.child RENAME COLUMN closed_at TO deleted_at;
