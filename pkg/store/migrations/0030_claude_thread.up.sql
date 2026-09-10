-- Claude Code sends every thread in one process (main, each Task subagent, the
-- titler) under one X-Rafiki-Session value, so they land on one conversation
-- row and race on one ordinal space. These two columns are the observation
-- stage: they record which thread each turn belongs to without yet acting on
-- it.
--
-- No FK on either column. conversation_turn is a hypertable with columnstore,
-- which rejects ADD COLUMN ... REFERENCES; and thread_id points at another row
-- of this same table, which a compressed chunk cannot enforce anyway.

ALTER TABLE conversations.conversation_turn
	ADD COLUMN IF NOT EXISTS response_message_id TEXT;

ALTER TABLE conversations.conversation_turn
	ADD COLUMN IF NOT EXISTS thread_id UUID;

COMMENT ON COLUMN conversations.conversation_turn.response_message_id IS
	'Anthropic assistant message id (msg_...) from this turn''s response. The chain target for a later turn''s diagnostics.previous_message_id.';

COMMENT ON COLUMN conversations.conversation_turn.thread_id IS
	'conversation_turn.id of the turn that begins this turn''s thread. Self for a thread root. Resolved by walking diagnostics.previous_message_id back one hop.';

-- Chain lookup: given a previous_message_id, find its turn within a conversation.
CREATE INDEX IF NOT EXISTS conversation_turn_response_msg_idx
	ON conversations.conversation_turn (conversation_id, response_message_id)
	WHERE response_message_id IS NOT NULL;
