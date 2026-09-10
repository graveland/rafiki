DROP INDEX IF EXISTS conversations.conversation_turn_response_msg_idx;
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS thread_id;
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS response_message_id;
