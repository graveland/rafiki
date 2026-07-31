-- DESTRUCTIVE: drops the entire conversations schema and every captured
-- conversation with it. Only ever run against a database whose data is
-- disposable.
DROP TABLE IF EXISTS conversations.conversation_attachment;
DROP TABLE IF EXISTS conversations.conversation_turn;
DROP TABLE IF EXISTS conversations.conversation;
DROP SCHEMA IF EXISTS conversations;
