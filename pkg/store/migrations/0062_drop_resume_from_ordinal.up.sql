-- The legacy resume horizon. Nothing writes it since the conversation tree
-- (conversation.head_id and the parent chain are the working set), and its only
-- reader was the pre-start cutover comparison in `rafikid tree check`.
ALTER TABLE conversations.conversation DROP COLUMN resume_from_ordinal;
