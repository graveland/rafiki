-- Agent presets: named seats (kind, model, tools, prompt, budgets) that
-- agent_spawn resolves by name. Owner-scoped and append-only like
-- conversations.pymodules: a put is an INSERT, "latest" is MAX(id) per
-- (owner_user_id, name), and a delete stamps deleted_at on every live
-- version. Array columns are TRI-STATE: NULL = the kind's default
-- (everything), '{}' = none, a list = exactly those.
CREATE TABLE conversations.presets (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_user_id        uuid NULL REFERENCES conversations.users(id),
    name                 text NOT NULL,
    description          text NOT NULL DEFAULT '',
    kind                 text NOT NULL DEFAULT 'fundi',
    provider             text NULL,
    model                text NULL,
    thinking             text NULL,
    executor             text NULL,
    labels               jsonb NOT NULL DEFAULT '{}',
    tools                text[] NULL,
    skills               text[] NULL,
    mcp_servers          text[] NULL,
    context_files        boolean NULL,
    system_prompt        text NULL,
    append_system_prompt text NULL,
    max_cost             numeric NULL,
    max_depth            int NULL,
    max_children         int NULL,
    written_by_child     text NULL,
    deleted_at           timestamptz NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (kind IN ('fundi', 'claude')),
    -- a claude child honours only model, provider, append_system_prompt,
    -- executor, labels and the budgets (pkg/claudeargv); this CHECK pins the
    -- six fundi-only knobs it must NOT carry -- thinking, tools, skills,
    -- mcp_servers, context_files, system_prompt -- and leaves the honoured
    -- fields unconstrained. pkg/presets.Validate is the enforcement gate:
    -- a database that already ran an older, more permissive 0035 keeps its
    -- CHECK, and Validate still rejects what it must.
    CHECK (kind <> 'claude' OR (thinking IS NULL AND tools IS NULL AND skills IS NULL
           AND mcp_servers IS NULL AND context_files IS NULL AND system_prompt IS NULL)),
    -- mirrors pkg/presets.thinkingLevels (the levels pkg/fundi's
    -- thinkingBudgets can actually honour)
    CHECK (thinking IS NULL OR thinking IN ('off','low','medium','high','xhigh')),
    CHECK (max_cost IS NULL OR max_cost >= 0),
    CHECK (max_depth IS NULL OR max_depth >= 0),
    CHECK (max_children IS NULL OR max_children >= 0)
);

CREATE INDEX presets_owner_name_id_idx
    ON conversations.presets (owner_user_id, name, id DESC);
