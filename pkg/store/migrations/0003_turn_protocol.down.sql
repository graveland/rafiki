-- DESTRUCTIVE: dropping this column out from under a live deployment loses
-- the anthropic/openai discrimination for every captured turn.
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS protocol;
