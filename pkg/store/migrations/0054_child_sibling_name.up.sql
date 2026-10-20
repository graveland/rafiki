-- Among an owner's NON-CLOSED children, a non-empty name is unique among
-- siblings — children of the same parent, with top-level agents siblings under
-- the owner. Killing a child keeps its name; closing it frees the name for
-- reuse (closed_at IS NOT NULL).
--
-- The parent's lineage label is coalesced across both spellings because the
-- subtree label was renamed fundi/parent -> rafiki/parent without a backfill,
-- so one parent's children can be split across two keys.
--
-- The first statement renames existing duplicates so the index can build: in
-- each live, non-empty-name sibling group the earliest spawned_at row (ties by
-- child_id) keeps its name and the n-th later row becomes <name>-n, n = 2, 3,
-- …; the conversations linked to a renamed row are renamed with it, by the
-- same three arms as nameConversations (cmd/rafikid/rename.go). Nothing is
-- deleted. The rename only gives each row a name within its own group, so a
-- suffixed name that already exists as a sibling's name makes the index build
-- fail loudly rather than silently pick a winner.
--
-- DEPLOY: apply this migration with the daemon stopped. A running daemon holds
-- the renamed duplicates under their OLD in-memory name, and its next
-- writeRecord would hit the new index. Production had no duplicates when this
-- was written, so this only matters for other deployments.
WITH dupes AS (
    SELECT child_id,
           row_number() OVER (
               PARTITION BY coalesce(owner_user_id::text, ''),
                            coalesce(labels->>'rafiki/parent', labels->>'fundi/parent', ''),
                            name
               ORDER BY spawned_at, child_id
           ) AS rn
      FROM conversations.child
     WHERE closed_at IS NULL AND coalesce(name, '') <> ''
),
renamed AS (
    UPDATE conversations.child c SET name = c.name || '-' || d.rn
      FROM dupes d
     WHERE c.child_id = d.child_id AND d.rn > 1
    RETURNING c.child_id, c.name
)
UPDATE conversations.conversation cv SET name = r.name, updated_at = now()
  FROM renamed r
 WHERE cv.name IS DISTINCT FROM r.name
   AND (cv.id = (SELECT conversation_id FROM conversations.child WHERE child_id = r.child_id)
        OR cv.external_ref = r.child_id
        OR cv.external_ref LIKE replace(replace(r.child_id,'_','\_'),'%','\%') || ':%' ESCAPE '\');

CREATE UNIQUE INDEX child_sibling_name_live ON conversations.child (
    coalesce(owner_user_id::text, ''),
    coalesce(labels->>'rafiki/parent', labels->>'fundi/parent', ''),
    name)
  WHERE closed_at IS NULL AND coalesce(name, '') <> '';
