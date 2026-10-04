-- true: this conversation links to a child whose subtree skips derived indexing; set by WriteWindows at first extraction, read by the indexer's pending queries
ALTER TABLE conversations.conversation ADD COLUMN derived_skip BOOLEAN NOT NULL DEFAULT false;
