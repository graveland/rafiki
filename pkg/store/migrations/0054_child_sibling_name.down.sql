-- Reverses 0054's index only. The duplicate renames are NOT reverted: they are
-- a data repair, not a schema change, and nothing records which rows were
-- renamed. A child renamed to <name>-2 by the up migration keeps that name, and
-- so do its conversations; rename it back by hand if you need the old one.
DROP INDEX IF EXISTS conversations.child_sibling_name_live;
