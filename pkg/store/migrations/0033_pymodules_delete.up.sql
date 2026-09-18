-- Soft delete for pymodules: a tombstone is an INSERT (deleted_at set), never
-- an UPDATE -- the append-only invariant survives. "Latest per name" resolves
-- to the tombstone, so a deleted name leaves List; a later Put appends a live
-- row and the name returns. NULL deleted_at means live.
ALTER TABLE conversations.pymodules ADD COLUMN deleted_at timestamptz;
