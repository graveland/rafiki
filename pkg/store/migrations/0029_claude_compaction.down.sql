ALTER TABLE conversations.conversation_message DROP COLUMN IF EXISTS kind;
ALTER TABLE conversations.conversation DROP COLUMN IF EXISTS resume_from_ordinal;
