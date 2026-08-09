-- Per-conversation tool state: a key-value store so tools with mutable
-- per-agent state (todo) survive daemon restarts. The (conversation_id, tool)
-- compound key means any future tool gets persistence with no schema change.
CREATE TABLE IF NOT EXISTS conversations.tool_state (
    conversation_id UUID NOT NULL REFERENCES conversations.conversation(id) ON DELETE CASCADE,
    tool            TEXT NOT NULL,
    state           JSONB NOT NULL DEFAULT '{}',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, tool)
);