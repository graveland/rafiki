-- A capability set narrows what a credential may do. NULL is full user
-- authority — every token minted before this column existed keeps exactly the
-- access it had — '{}' grants nothing, and '{notify}' is Notify only.
--
-- The column stays nullable on purpose: NULL and '{}' are DIFFERENT answers
-- and neither may be normalized into the other (users.Token.Capabilities is
-- nil for the first and a non-nil empty slice for the second). No CHECK
-- constraint: the allowed set is validated in users.ValidCapabilities, so a
-- later capability does not need a migration to become legal.
ALTER TABLE conversations.user_token ADD COLUMN capabilities TEXT[];
