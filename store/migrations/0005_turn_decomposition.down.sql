ALTER TABLE conversations.conversation_turn
	DROP COLUMN IF EXISTS response_ordinal,
	DROP COLUMN IF EXISTS prefix_content,
	DROP COLUMN IF EXISTS cache_breakpoints;
-- NB: cannot restore NOT NULL on request if rows have NULLs; left relaxed.
