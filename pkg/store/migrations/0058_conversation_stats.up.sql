-- Denormalised tree statistics on the conversation row, so listing
-- conversations does not aggregate the message table. The tree write path
-- maintains them in the same transaction as the node it describes; until that
-- path lands these are seeded once here and not updated.
--
-- fillfactor 85 leaves room for the in-place updates these counters get on
-- every append (a full page would force a new tuple per append and bloat the
-- table), at the cost of a little space per row.
ALTER TABLE conversations.conversation
  ADD COLUMN node_count     integer NOT NULL DEFAULT 0,
  ADD COLUMN fork_count     integer NOT NULL DEFAULT 0,
  ADD COLUMN fallback_count integer NOT NULL DEFAULT 0,
  ADD COLUMN payload_bytes  bigint  NOT NULL DEFAULT 0;
ALTER TABLE conversations.conversation SET (fillfactor = 85);

-- Only nodes (path_hash IS NOT NULL) count: legacy linear rows have no tree
-- position, and counting them would make node_count disagree with the number
-- of nodes a fork can reach. fork_count is the number of parents in this
-- conversation with more than one child.
UPDATE conversations.conversation c SET
  node_count    = s.n,
  payload_bytes = s.bytes,
  fork_count    = s.forks
FROM (
  SELECT m.conversation_id,
         count(*) FILTER (WHERE m.path_hash IS NOT NULL)                              AS n,
         coalesce(sum(octet_length(m.content::text)) FILTER (WHERE m.path_hash IS NOT NULL), 0) AS bytes,
         (SELECT count(*) FROM (SELECT parent_id FROM conversations.conversation_message x
                                 WHERE x.conversation_id = m.conversation_id AND x.parent_id IS NOT NULL
                                 GROUP BY parent_id HAVING count(*) > 1) f)          AS forks
    FROM conversations.conversation_message m
   GROUP BY m.conversation_id) s
WHERE c.id = s.conversation_id;
