-- Message-granularity conversation state (design: store, two granularities).
-- One row per message in Anthropic block format: the working state a
-- library-driven Conversation loads and appends. conversation_turn remains
-- the wire-level evidence; library sends write both.
CREATE TABLE IF NOT EXISTS conversations.conversation_message (
	id              UUID NOT NULL DEFAULT uuidv7() PRIMARY KEY,
	conversation_id UUID NOT NULL REFERENCES conversations.conversation(id),
	ordinal         INT NOT NULL,             -- position in the conversation, 0-based
	role            TEXT NOT NULL,            -- 'user' | 'assistant'
	content         JSONB NOT NULL,           -- Anthropic content-block array
	-- tool_use ids present in this row's blocks (tool_use for assistant rows,
	-- tool_result for user rows): makes orphaned-tool_use detection for
	-- crash recovery a SQL query instead of a JSONB scan.
	tool_use_ids    TEXT[],
	input_tokens    BIGINT,
	output_tokens   BIGINT,
	stop_reason     TEXT,                     -- assistant rows: how the turn ended (resume consults it)
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- Ordinal is the append-idempotency key: a Resume that re-appends an
	-- already-persisted message collides here instead of duplicating it.
	-- The UNIQUE constraint's index also serves (conversation_id, ordinal)
	-- lookups; no separate index needed.
	UNIQUE (conversation_id, ordinal)
);
