ALTER TABLE conversations.tasks
    DROP CONSTRAINT IF EXISTS tasks_ordinal_unique;

ALTER TABLE conversations.tasks
    ADD CONSTRAINT tasks_ordinal_unique
    UNIQUE (conversation_id, parent_id, ordinal);
