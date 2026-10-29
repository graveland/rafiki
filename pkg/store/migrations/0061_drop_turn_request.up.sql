-- The turn row no longer stores the request body. What a request covered is
-- already in the tree: request_head_id names the last path row it read, and
-- [request_trim_from, request_trim_to) names the rows it dropped (0060). The
-- envelope a cache key is computed from is prefix_content. Nothing writes this
-- column any more, but the turns already recorded still hold a body: dropping
-- the column discards it, deliberately and permanently -- nothing reads it, and
-- a later .down does not restore it. Reclaiming its space on existing chunks is
-- a separate manual runbook.
ALTER TABLE conversations.conversation_turn
  DROP COLUMN request;
