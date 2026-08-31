-- 0025_inbox_attachments.down.sql
ALTER TABLE conversations.agent_inbox DROP COLUMN IF EXISTS attachments;
