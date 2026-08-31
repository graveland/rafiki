-- 0025_inbox_attachments.up.sql
--
-- Non-text payloads on an inbox row. A pasted image rides alongside the text
-- rather than replacing it: a message is usually "here is a screenshot" plus a
-- question about it, and `body` remains what every human-readable surface and
-- the coalescing rules work from.
--
-- JSONB rather than a side table: an attachment has no identity or lifetime of
-- its own, it is part of the message and dies with it. A NULL column and an
-- empty array mean the same thing (no attachments) and both must be accepted,
-- since every row written before this migration has NULL.
ALTER TABLE conversations.agent_inbox
    ADD COLUMN IF NOT EXISTS attachments JSONB;

COMMENT ON COLUMN conversations.agent_inbox.attachments IS
    'Array of {media_type, data} objects; data is base64. NULL and [] both mean none.';
