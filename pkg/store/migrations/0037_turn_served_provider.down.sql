-- Inverse of 0037_turn_served_provider.up.sql.
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS served_provider;
