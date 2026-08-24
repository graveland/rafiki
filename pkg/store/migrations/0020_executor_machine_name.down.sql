DROP INDEX IF EXISTS conversations.executors_owner_machine_unique;
ALTER TABLE conversations.executors
    ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';
