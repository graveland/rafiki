-- Per-conversation Resume counter: agentloop.Resume increments it and refuses
-- past the cap, guarding against crash loops where a tool execution itself
-- kills the process (recovery design, resolved 2026-07-10).
ALTER TABLE conversations.conversation
	ADD COLUMN IF NOT EXISTS resume_attempts INT NOT NULL DEFAULT 0;
