-- parent_conversation_id: the root conversation of the agent that started this
-- one. A thread conversation ('<session>:<thread>') points at its session's
-- root; a spawned child's root conversation points at its parent child's root.
-- NULL is top level. Written once and never updated by the daemon.
--
-- Every arm below points only at a BROWSABLE conversation (node_count > 0 OR
-- fork_from is set), the same predicate ListConversations uses for
-- eligibility: a browser walks down from an eligible row, so parenting a
-- conversation to one the listing excludes would hide it. A script child's
-- task-ledger row is exactly that shape (no messages, no fork_from), so a
-- script coordinator's workers stay top level.
ALTER TABLE conversations.conversation
    ADD COLUMN parent_conversation_id uuid NULL
    REFERENCES conversations.conversation(id) ON DELETE SET NULL;

CREATE INDEX conversation_parent_recent_idx
    ON conversations.conversation (parent_conversation_id, updated_at DESC, id DESC)
    WHERE parent_conversation_id IS NOT NULL;
CREATE INDEX conversation_top_recent_idx
    ON conversations.conversation (updated_at DESC, id DESC)
    WHERE parent_conversation_id IS NULL;
CREATE INDEX conversation_top_created_idx
    ON conversations.conversation (created_at DESC, id DESC)
    WHERE parent_conversation_id IS NULL;

-- Threads: '<session>:<thread>' with exactly one ':' whose '<session>' root
-- exists under the same driven_by and is browsable.
UPDATE conversations.conversation t
   SET parent_conversation_id = r.id
  FROM conversations.conversation r
 WHERE t.parent_conversation_id IS NULL
   AND t.external_ref ~ '^[^:]+:[^:]+$'
   AND r.external_ref = split_part(t.external_ref, ':', 1)
   AND r.driven_by = t.driven_by
   AND (r.node_count > 0 OR r.fork_from IS NOT NULL)
   AND r.id <> t.id;

-- Child roots: a child's root conversation points at its parent child's
-- root, when that root is browsable.
WITH roots AS (
    SELECT ch.child_id,
           COALESCE(ch.labels->>'rafiki/parent', ch.labels->>'fundi/parent') AS parent_id,
           COALESCE(ch.conversation_id,
                    (SELECT v.id FROM conversations.conversation v
                      WHERE v.external_ref = ch.child_id
                      ORDER BY v.created_at LIMIT 1)) AS conv_id
      FROM conversations.child ch
)
UPDATE conversations.conversation c
   SET parent_conversation_id = p.conv_id
  FROM roots me
  JOIN roots p ON p.child_id = me.parent_id
  JOIN conversations.conversation pc ON pc.id = p.conv_id
 WHERE c.id = me.conv_id
   AND c.parent_conversation_id IS NULL
   AND (pc.node_count > 0 OR pc.fork_from IS NOT NULL)
   AND p.conv_id <> c.id;

-- last activity: updated_at becomes the time of the latest stored message,
-- and only moves FORWARD. A row whose updated_at is already later than its
-- newest message keeps that value: before this migration every commit moved
-- it, including one that stored no message (a head retract) and the daemon's
-- bookkeeping writes (status, resume attempts, rename), so a later
-- non-message timestamp is preserved rather than rewound.
UPDATE conversations.conversation c
   SET updated_at = m.last_at
  FROM (SELECT conversation_id, max(created_at) AS last_at
          FROM conversations.conversation_message
         GROUP BY conversation_id) m
 WHERE m.conversation_id = c.id
   AND m.last_at > c.updated_at;
