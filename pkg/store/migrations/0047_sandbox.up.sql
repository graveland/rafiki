-- Sandboxes: containers running `rafiki executor serve` with their own mounts,
-- network and resource limits, either bound to a child (a spawn block) or
-- reached directly (a named sandbox). Every access-gating field here is written
-- by the daemon from a value IT verified; nothing the container reports gates
-- access.
--
-- Tombstoned with removed_at, never DELETE'd: a removed sandbox frees its name
-- while its row survives for lineage. executor_id / launcher_executor_id are
-- plain text with no FK because executor rows hard-delete (executors.Store
-- .Delete has no tombstone), so a sandbox must not hold one open.
--
-- spec is JSONB carrying the resolved SandboxSpec WITHOUT the credential.
CREATE TABLE conversations.sandbox (
    id                   text        PRIMARY KEY,
    owner_user_id        text        NOT NULL DEFAULT '',
    name                 text        NOT NULL DEFAULT '',
    executor_id          text        NOT NULL,
    launcher_executor_id text        NOT NULL,
    container_id         text        NOT NULL DEFAULT '',
    image                text        NOT NULL,
    spec                 jsonb       NOT NULL,
    created_by           text        NOT NULL DEFAULT '',
    owner_child          text        NOT NULL DEFAULT '',
    scope                text        NOT NULL DEFAULT '',
    state                text        NOT NULL CHECK (state IN ('creating','ready','lost','removing')),
    created_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NULL,
    removed_at           timestamptz NULL
);

-- A live NAMED sandbox's name is unique per owner. Spawn blocks (name '') and
-- removed rows are unconstrained, so a name is reusable after a tombstone.
CREATE UNIQUE INDEX sandbox_owner_name_unique
    ON conversations.sandbox (owner_user_id, name)
    WHERE name <> '' AND removed_at IS NULL;

-- The owning child of a spawn block: the lookup the scope resolver makes.
CREATE INDEX sandbox_owner_child_live
    ON conversations.sandbox (owner_child)
    WHERE owner_child <> '' AND removed_at IS NULL;
