-- A fork borrows its source's history up to fork_from by reference. Set once,
-- when the fork is created, and never changed: the conversation core treats it
-- as the conversation's single legal foreign edge.
ALTER TABLE conversations.conversation
    ADD COLUMN fork_from uuid NULL REFERENCES conversations.conversation_message(id) ON DELETE RESTRICT;

-- A tag names a node others may fork from. Every row is one version of one tag
-- (tag_id): a move supersedes the current row and appends the next, an untag
-- tombstones the current row. Rows are never updated otherwise, never deleted.
CREATE TABLE conversations.agent_tag (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    tag_id           uuid        NOT NULL,
    owner_user_id    uuid        NULL REFERENCES conversations.users(id),
    name             text        NOT NULL,
    scope            text        NOT NULL CHECK (scope IN ('lineage', 'tree', 'owner')),
    scope_root       text        NOT NULL,
    source_child_id  text        NOT NULL,
    conversation_id  uuid        NOT NULL REFERENCES conversations.conversation(id),
    node_id          uuid        NOT NULL REFERENCES conversations.conversation_message(id),
    creator_child_id text        NULL,
    actor_child_id   text        NULL,
    note             text        NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    superseded_at    timestamptz NULL,
    deleted_at       timestamptz NULL
);
CREATE UNIQUE INDEX agent_tag_current
    ON conversations.agent_tag (tag_id) WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX agent_tag_live_name
    ON conversations.agent_tag (owner_user_id, scope, scope_root, name) NULLS NOT DISTINCT
    WHERE superseded_at IS NULL AND deleted_at IS NULL;
CREATE INDEX agent_tag_owner_name
    ON conversations.agent_tag (owner_user_id, name)
    WHERE superseded_at IS NULL AND deleted_at IS NULL;
