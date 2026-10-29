-- Re-creates the column empty. The horizons the up migration dropped are NOT
-- restored: every historical conversation reads resume_from_ordinal IS NULL,
-- and nothing writes it again.
ALTER TABLE conversations.conversation ADD COLUMN resume_from_ordinal INT;
