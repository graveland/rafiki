-- Wire protocol of the captured turn: 'anthropic' (/v1/messages, the typed
-- core) or 'openai' (/v1/chat/completions pass-through face). Mining code
-- reads this to know how to interpret the request/response JSONB. All
-- existing rows predate the OpenAI face.
ALTER TABLE conversations.conversation_turn
	ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT 'anthropic';
