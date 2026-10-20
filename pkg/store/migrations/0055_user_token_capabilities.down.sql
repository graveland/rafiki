-- Drops the column, so every token returns to full user authority. A narrowed
-- token does not fail loudly after a downgrade; the daemon that reads
-- capabilities is the one this column was added for.
ALTER TABLE conversations.user_token DROP COLUMN capabilities;
