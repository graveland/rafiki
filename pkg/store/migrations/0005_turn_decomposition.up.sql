-- Turn decomposition: response_ordinal/prefix_content/cache_breakpoints let a
-- single API turn be recorded as multiple response segments (mirrors admindb
-- 0013 in savannah-client pkg/admindb/sql). request is no longer NOT NULL:
-- a decomposed turn's prefix segment carries no request of its own.
ALTER TABLE conversations.conversation_turn
	ADD COLUMN IF NOT EXISTS response_ordinal INT,
	ADD COLUMN IF NOT EXISTS prefix_content   JSONB,
	ADD COLUMN IF NOT EXISTS cache_breakpoints JSONB;
ALTER TABLE conversations.conversation_turn ALTER COLUMN request DROP NOT NULL;
