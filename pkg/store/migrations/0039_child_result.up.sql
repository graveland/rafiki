-- The script child's final result (Connect SetResult): verbatim JSON, stored
-- as text so the bytes the script wrote are the bytes every reader gets. A
-- JSONB column would normalise them (reorder keys, drop whitespace), which is
-- the wrong guarantee for a value the parent agent is shown verbatim in the
-- settle fragment. NULL until the script sets one; last write wins.
ALTER TABLE conversations.child ADD COLUMN result TEXT;
