-- Revert the script-kind widening. The script arm is dropped outright; a
-- table carrying only the wider kind CHECK reverts to the 0035 pair, which
-- together refuse every script-kind row the wider CHECK admitted.
ALTER TABLE conversations.presets DROP CONSTRAINT presets_script_check;
ALTER TABLE conversations.presets DROP CONSTRAINT presets_check;
ALTER TABLE conversations.presets ADD CONSTRAINT presets_check
    CHECK (kind <> 'claude' OR (thinking IS NULL AND tools IS NULL AND skills IS NULL
           AND mcp_servers IS NULL AND context_files IS NULL AND system_prompt IS NULL));
ALTER TABLE conversations.presets DROP CONSTRAINT presets_kind_check;
ALTER TABLE conversations.presets ADD CONSTRAINT presets_kind_check
    CHECK (kind IN ('fundi', 'claude'));
