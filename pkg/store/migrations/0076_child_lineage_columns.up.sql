-- 0076 canonicalises the two lineage labels and promotes them to generated
-- columns, so every reader stops repeating
-- coalesce(labels->>'rafiki/parent', labels->>'fundi/parent').
--
-- DEPLOY: apply this migration with EVERY daemon stopped (the 0054 note's rule).
-- The rewrite below runs once; a daemon still running the previous binary keeps
-- writing fundi/parent and fundi/root, and a row written after this migration
-- would have NULL parent_id/root_id — invisible to the lineage and subtree-spend
-- queries and grouped as a top-level sibling by the name index.

-- 1. Keys. The subtree labels were renamed fundi/parent, fundi/root -> rafiki/*
--    without a backfill, so rows written before the rename carry the old
--    spelling. Every fundi/<k> becomes rafiki/<k>; when rafiki/<k> is ALSO
--    present its value wins and the fundi/<k> key is dropped, so one parent can
--    never be spelled two ways again. Rows with no fundi/ key are untouched,
--    and unrelated keys are carried through verbatim.
UPDATE conversations.child c
   SET labels = (
       SELECT jsonb_object_agg(t.nk, t.v)
         FROM (SELECT CASE WHEN e.k LIKE 'fundi/%'
                           THEN 'rafiki/' || substr(e.k, 7)
                           ELSE e.k END AS nk,
                      e.v
                 FROM jsonb_each(c.labels) AS e(k, v)
                WHERE NOT (e.k LIKE 'fundi/%'
                           AND c.labels ? ('rafiki/' || substr(e.k, 7)))) t)
 WHERE jsonb_typeof(c.labels) = 'object'
   AND EXISTS (SELECT 1 FROM jsonb_object_keys(c.labels) AS k WHERE k LIKE 'fundi/%');

-- 2. The two labels the tree walks read, generated so they cannot drift from
--    labels. jsonb ->> is immutable, which is what a STORED generated column
--    requires.
ALTER TABLE conversations.child
    ADD COLUMN parent_id text GENERATED ALWAYS AS (labels->>'rafiki/parent') STORED,
    ADD COLUMN root_id   text GENERATED ALWAYS AS (labels->>'rafiki/root') STORED;

-- 3. Lineage reads select the subtree by root_id and the sibling-name index
--    groups by parent_id; both are plain btree columns now.
CREATE INDEX child_root_idx ON conversations.child (root_id);
CREATE INDEX child_parent_idx ON conversations.child (parent_id);

-- 4. A non-object labels value, or a parent/root key that is not a string, has
--    no determinable lineage: the walk would either read it as absent (a
--    descendant silently dropped from a budget rollup) or fail. Refuse it at the
--    write instead, so the readers never meet one. NOT VALID + VALIDATE so the
--    table is checked once here rather than locked against writes per row.
ALTER TABLE conversations.child ADD CONSTRAINT child_labels_lineage_shape CHECK (
    jsonb_typeof(labels) = 'object'
    AND coalesce(jsonb_typeof(labels->'rafiki/parent'), 'string') = 'string'
    AND coalesce(jsonb_typeof(labels->'rafiki/root'), 'string') = 'string') NOT VALID;
ALTER TABLE conversations.child VALIDATE CONSTRAINT child_labels_lineage_shape;

-- 5. The sibling-name index now groups by the generated parent_id. Same name
--    (childstoredb.isSiblingNameViolation matches on it) and the same predicate;
--    the parent term is the only change, and it is the REBUILT index that makes
--    nameHolderSQL's coalesce(parent_id, '') term-for-term mirror of it.
DROP INDEX conversations.child_sibling_name_live;
CREATE UNIQUE INDEX child_sibling_name_live ON conversations.child (
    coalesce(owner_user_id::text, ''),
    coalesce(parent_id, ''),
    name)
  WHERE closed_at IS NULL AND coalesce(name, '') <> '';
