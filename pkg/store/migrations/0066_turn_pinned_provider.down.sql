-- Inverse of 0066_turn_pinned_provider.up.sql.
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS pinned_provider;
