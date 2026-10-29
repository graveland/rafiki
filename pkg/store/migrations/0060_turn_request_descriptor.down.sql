-- Reverses 0060. Dropping the trim range loses only the descriptor -- which
-- rows a request covered, and what a trim dropped -- and no message row. The
-- request body was never restored by this rollback: it is gone with 0061.
ALTER TABLE conversations.conversation_turn
  DROP COLUMN request_trim_to,
  DROP COLUMN request_trim_from;
