-- Kind=script presets (wave 3 of script children): a script child is a saved
-- pymodule process, so its preset honours only executor, labels, budgets,
-- description and append_system_prompt. This widens the kind CHECK and adds
-- a script arm alongside 0035's claude arm, pinning the LLM-shaping knobs a
-- script preset must NOT carry -- thinking, tools, skills, mcp_servers,
-- context_files, system_prompt, and (stricter than the claude arm, because a
-- script has no model to point them at) provider and model.
-- pkg/presets.Validate is the enforcement gate: a database that already ran
-- an older, more permissive chain keeps whatever CHECKs it has, and Validate
-- still rejects what it must.
ALTER TABLE conversations.presets DROP CONSTRAINT presets_kind_check;
ALTER TABLE conversations.presets ADD CONSTRAINT presets_kind_check
    CHECK (kind IN ('fundi', 'claude', 'script'));
ALTER TABLE conversations.presets DROP CONSTRAINT presets_check;
ALTER TABLE conversations.presets ADD CONSTRAINT presets_check
    CHECK (kind <> 'claude' OR (thinking IS NULL AND tools IS NULL AND skills IS NULL
           AND mcp_servers IS NULL AND context_files IS NULL AND system_prompt IS NULL));
ALTER TABLE conversations.presets ADD CONSTRAINT presets_script_check
    CHECK (kind <> 'script' OR (thinking IS NULL AND tools IS NULL AND skills IS NULL
           AND mcp_servers IS NULL AND context_files IS NULL AND system_prompt IS NULL
           AND model IS NULL AND provider IS NULL));
