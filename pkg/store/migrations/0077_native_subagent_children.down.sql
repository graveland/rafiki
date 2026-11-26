-- Reverses 0077's backfill. Only rows that are native subagents AND have no pid
-- are removed: a native child always has a NULL pid, while a row the daemon
-- wrote for a real process keeps its pid and is not this migration's to touch.
DELETE FROM conversations.child
 WHERE labels ? 'rafiki/native-subagent'
   AND pid IS NULL;
