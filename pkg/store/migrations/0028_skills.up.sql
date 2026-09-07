-- The authoritative user tier of the skills inventory. Rows here are served
-- inline to fundi agents and (in a later change) rendered onto executors for
-- claude children, so one corpus reaches both kinds on any machine.
--
-- Not a hypertable: low-churn relational content, not time series.
CREATE TABLE conversations.skills (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The render/plugin namespace. 'rafiki' holds core and operator content;
    -- an imported corpus keeps its upstream plugin name so Claude Code can
    -- deduplicate it against a real install of the same plugin.
    namespace     TEXT NOT NULL DEFAULT 'rafiki',
    -- Bare slug, never prefixed. The prefix is applied at render time.
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    -- SKILL.md body with the frontmatter block already stripped.
    body          TEXT NOT NULL,

    -- Provenance AND ownership. 'rafiki-core' is reserved for rows the
    -- daemon's startup sync owns; it must never touch any other value.
    source        TEXT NOT NULL DEFAULT 'manual',

    -- Unused in v1. Present so per-user write scoping is later a policy
    -- change rather than a migration. NULL means global.
    owner_user_id UUID REFERENCES conversations.users(id),

    -- Set when this row replaces a core one: the rafiki version whose core
    -- skill it displaced. The daemon warns when the embedded corpus moves
    -- past it, so an override going stale is announced rather than silent.
    shadowed_core_version TEXT,

    -- Soft disable. A disabled row keeps its content but frees its name.
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Composite because pg's "postgres" and rafiki's "postgres" are different
-- skills. Partial so a disabled row's name can be reclaimed by a replacement,
-- which is how an operator overrides a core skill.
CREATE UNIQUE INDEX skills_name_active
    ON conversations.skills (namespace, name) WHERE enabled;
