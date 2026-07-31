-- MANUAL USE FORBIDDEN ON ADOPTED DATABASES: on a scadmin-adopted database
-- this drops the conversations schema that scadmin's own migration chain
-- (0007-0009) still records as applied, forking the two histories. Only ever
-- run against a database whose baseline was executed (adopted=false) and
-- whose data is disposable.
DROP TABLE IF EXISTS conversations.conversation_attachment;
DROP TABLE IF EXISTS conversations.conversation_turn;
DROP TABLE IF EXISTS conversations.conversation;
DROP SCHEMA IF EXISTS conversations;
