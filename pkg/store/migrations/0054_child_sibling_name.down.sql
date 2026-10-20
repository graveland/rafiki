-- Reverses 0054's index. The duplicate renames are NOT reverted: they are a
-- data repair, not a schema change, and the suffixed names have been reported
-- to their operators by the time a rollback runs.
DROP INDEX IF EXISTS conversations.child_sibling_name_live;
