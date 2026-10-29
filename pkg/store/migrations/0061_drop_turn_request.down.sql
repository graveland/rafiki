-- Re-creates the column empty. The bodies the up migration dropped are NOT
-- restored: every historical turn reads request IS NULL, and only a write path
-- that still sends one (a daemon older than 0061) can fill it again. 0005
-- already relaxed the NOT NULL, so a nullable column is the shape that chain
-- produces.
ALTER TABLE conversations.conversation_turn
  ADD COLUMN request jsonb;
