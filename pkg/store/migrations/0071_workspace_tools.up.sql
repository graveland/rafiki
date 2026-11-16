-- workspace_tools: '' = native (Claude Code's built-in file and shell tools on
-- the launch host, the default); 'rafiki' = those built-ins disallowed and
-- rafiki's executor-routed tools served over MCP instead. Resolved once at
-- spawn, so a resumed child never regains its native tools. On a preset the
-- column is NULLable: NULL means the preset leaves the choice to the spawn.
ALTER TABLE conversations.child
    ADD COLUMN workspace_tools text NOT NULL DEFAULT ''
    CHECK (workspace_tools IN ('', 'rafiki'));
ALTER TABLE conversations.presets
    ADD COLUMN workspace_tools text NULL
    CHECK (workspace_tools IS NULL OR workspace_tools IN ('native', 'rafiki'));