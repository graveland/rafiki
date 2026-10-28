-- Reverses 0057. Order is the reverse of the up migration: the child-side
-- columns and indexes first, then the constraints, then the columns they
-- constrain. Dropping head_id and next_ordinal makes the tree unreachable but
-- does NOT delete any node — the message rows survive as legacy linear rows,
-- and re-applying the up migration loses their tree positions.
ALTER TABLE conversations.conversation_turn DROP COLUMN request_head_id;

ALTER TABLE conversations.conversation
  DROP COLUMN next_ordinal,
  DROP COLUMN head_id;

DROP INDEX conversations.conversation_message_path_uniq;
DROP INDEX conversations.conversation_message_provider_idx;
DROP INDEX conversations.conversation_message_derived_idx;
DROP INDEX conversations.conversation_message_parent_idx;

ALTER TABLE conversations.conversation_message
  DROP CONSTRAINT conversation_message_hash_pair,
  DROP CONSTRAINT conversation_message_derived_fk,
  DROP CONSTRAINT conversation_message_parent_fk;

ALTER TABLE conversations.conversation_message
  DROP COLUMN provider_message_id,
  DROP COLUMN depth,
  DROP COLUMN hash_version,
  DROP COLUMN path_hash,
  DROP COLUMN derived_from_id,
  DROP COLUMN parent_id;
