-- Reverses 0077. It deletes EVERY native subagent row, not only the ones the
-- up migration backfilled: the marker (the rafiki/native-subagent label + a NULL
-- pid) is the same for a row 0077 inserted and one the new daemon wrote at birth,
-- and there is no provenance bit to tell them apart. So a rollback DISCARDS the
-- native child rows written while the new binary ran — their lineage, their status
-- and the identity that ATTRIBUTED their spend. The spend itself survives: a
-- thread's branch conversation is still priced through its parent's '<child>:'
-- ref arm. Lost with the row is the closed thread's tombstone too, so a thread an
-- operator had closed comes back live on the next upgrade. There is no lossless
-- inverse for a migration that added a whole kind of row.
--
-- That is the intended cost, and it is why a rollback must run this FIRST (an
-- older daemon resumes every claude row that is not exited, so it would launch a
-- `claude --resume` per idle native row). The conversations themselves are
-- untouched: nothing references conversations.child by foreign key, so the
-- transcripts survive and the next upgraded daemon writes a row again for a
-- thread that speaks.
--
-- Only rows that ARE native and have no pid are removed: a native child always
-- has a NULL pid, while a row the daemon wrote for a real process keeps its pid
-- and is not this migration's to touch.
DELETE FROM conversations.child
 WHERE labels ? 'rafiki/native-subagent'
   AND pid IS NULL;
