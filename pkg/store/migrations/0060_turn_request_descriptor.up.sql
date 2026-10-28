-- Which path rows an llm.Client request covered, in place of the request
-- body: request_head_id (already present) names the last row, and the
-- half-open range [request_trim_from, request_trim_to) names the rows a
-- trim dropped from between the anchor and the retained tail. Positions
-- are indexes into the working path (root = 0). Both NULL means no trim was
-- recorded -- which is also the state of every turn written before this
-- migration.
ALTER TABLE conversations.conversation_turn
  ADD COLUMN request_trim_from integer,
  ADD COLUMN request_trim_to   integer;
