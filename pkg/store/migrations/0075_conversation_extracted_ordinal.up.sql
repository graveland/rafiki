-- extracted_ordinal: the highest message ordinal the recall indexer has read
-- for this conversation. The indexer's "has work" test compares it with the
-- conversation's last ordinal. The window tail cannot serve for that: it ends
-- at the last message with extractable text, so a conversation whose last
-- messages carry only tool results looked behind forever and was re-read on
-- every pass. NULL means never read.
--
-- Backfill takes the last window's end, which is exactly what the previous test
-- compared against, so the first pass over a conversation that was stuck reads
-- it once more and then stamps the real value.
ALTER TABLE conversations.conversation
    ADD COLUMN extracted_ordinal integer NULL;

UPDATE conversations.conversation c
   SET extracted_ordinal = lw.ordinal_to
  FROM (SELECT DISTINCT ON (conversation_id) conversation_id, ordinal_to
          FROM conversations.conversation_window
         ORDER BY conversation_id, seq DESC) lw
 WHERE lw.conversation_id = c.id;
