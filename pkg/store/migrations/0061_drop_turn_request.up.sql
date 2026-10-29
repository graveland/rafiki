-- The turn row no longer stores the request body. What a request covered is
-- already in the tree: request_head_id names the last path row it read, and
-- [request_trim_from, request_trim_to) names the rows it dropped (0060). The
-- envelope a cache key is computed from is prefix_content. Nothing wrote this
-- column after the body moved to conversation_message, so dropping it loses no
-- data; reclaiming its space on existing chunks is a separate manual runbook.
ALTER TABLE conversations.conversation_turn
  DROP COLUMN request;
