-- The 0013 constraint does not constrain top-level tasks: parent_id is NULL
-- there, and a unique constraint treats NULLs as distinct by default. Two
-- concurrent first task_add calls in one conversation therefore both insert
-- ordinal 1 with nothing to reject either, and resolve() silently picks
-- whichever row comes back first.
--
-- NULLS NOT DISTINCT (PostgreSQL 15+) makes NULL parent_id values compare
-- equal, so the constraint finally covers the top level.
ALTER TABLE conversations.tasks
    DROP CONSTRAINT IF EXISTS tasks_ordinal_unique;

ALTER TABLE conversations.tasks
    ADD CONSTRAINT tasks_ordinal_unique
    UNIQUE NULLS NOT DISTINCT (conversation_id, parent_id, ordinal);
