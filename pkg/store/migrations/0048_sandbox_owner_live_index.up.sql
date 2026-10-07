-- ListLive(owner) is consulted on EVERY bind: controllerBinder.ChooseFor asks
-- ownedSandbox on every child, sandboxed or not, and the table only ever grows
-- (rows are tombstoned with removed_at, never deleted). Without an index that
-- lookup is a sequential scan over every sandbox row ever created, on every
-- bind. A partial index on the live rows keeps it an indexed read.
CREATE INDEX sandbox_owner_live
    ON conversations.sandbox (owner_user_id)
    WHERE removed_at IS NULL;
