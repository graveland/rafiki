-- Reverses 0076's schema only. The fundi/* -> rafiki/* key rewrite is NOT
-- reversed: it is a data repair (the spellings mean the same thing), and
-- nothing records which keys were rewritten. A row migrated to rafiki/parent
-- keeps that key, which is what every binary since the rename reads.
ALTER TABLE conversations.child DROP CONSTRAINT IF EXISTS child_labels_lineage_shape;

-- The rebuilt sibling-name index names parent_id, so it has to go before the
-- column does.
DROP INDEX IF EXISTS conversations.child_sibling_name_live;
DROP INDEX IF EXISTS conversations.child_root_idx;
DROP INDEX IF EXISTS conversations.child_parent_idx;

ALTER TABLE conversations.child
    DROP COLUMN IF EXISTS parent_id,
    DROP COLUMN IF EXISTS root_id;

-- 0054's definition, verbatim.
CREATE UNIQUE INDEX child_sibling_name_live ON conversations.child (
    coalesce(owner_user_id::text, ''),
    coalesce(labels->>'rafiki/parent', labels->>'fundi/parent', ''),
    name)
  WHERE closed_at IS NULL AND coalesce(name, '') <> '';
