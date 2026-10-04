-- true: the recall indexer skips embedding and summarising this child's conversations; inherited down the subtree at spawn
ALTER TABLE conversations.child ADD COLUMN skip_derived_index BOOLEAN NOT NULL DEFAULT false;
