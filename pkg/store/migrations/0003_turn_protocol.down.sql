-- MANUAL USE FORBIDDEN ON ADOPTED DATABASES: scadmin's chain does not know
-- this column; dropping it out from under a live deployment loses the
-- anthropic/openai discrimination for captured turns.
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS protocol;
