-- Reverses 0058. The counters are derived from the message rows, so dropping
-- them loses nothing that cannot be recomputed; fillfactor returns to the
-- table default.
ALTER TABLE conversations.conversation RESET (fillfactor);

ALTER TABLE conversations.conversation
  DROP COLUMN payload_bytes,
  DROP COLUMN fallback_count,
  DROP COLUMN fork_count,
  DROP COLUMN node_count;
