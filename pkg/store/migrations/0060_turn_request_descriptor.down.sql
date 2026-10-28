-- Reverses 0060. The trim range is a description of a request the turn body
-- already records, so dropping it loses no message row.
ALTER TABLE conversations.conversation_turn
  DROP COLUMN request_trim_to,
  DROP COLUMN request_trim_from;
