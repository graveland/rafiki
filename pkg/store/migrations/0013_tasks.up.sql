-- The task ledger. Replaces tool_state's 'todo' key with real rows so a
-- coordinator can read a subagent's decomposition with one indexed query
-- instead of replaying its transcript.
--
-- parent_id is ON DELETE RESTRICT, deliberately NOT CASCADE: there is no
-- delete verb in the tool surface, so this should never fire. It exists so a
-- future bug becomes an error instead of silently destroying a subtree that
-- another agent is working in.
CREATE TABLE IF NOT EXISTS conversations.tasks (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    conversation_id UUID NOT NULL REFERENCES conversations.conversation(id) ON DELETE CASCADE,
    parent_id       UUID REFERENCES conversations.tasks(id) ON DELETE RESTRICT,
    -- Monotonic per (conversation_id, parent_id). NEVER derived from a count
    -- of live siblings: a reused handle makes a coordinator's stale reference
    -- silently address a different task.
    ordinal         INT  NOT NULL,
    content         TEXT NOT NULL,
    active_form     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','in_progress','blocked',
                                      'completed','failed','orphaned','dropped')),
    -- Required when status = 'dropped'. Three drops are legitimate and one is
    -- not; only the reason separates them.
    drop_reason     TEXT,
    -- childID, not a UUID and not a FK: Controller.Spawn returns a childID
    -- before the child's engine creates its conversation row, so a FK would
    -- make delegation race the child's startup.
    assignee        TEXT,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tasks_drop_reason_required
        CHECK (status <> 'dropped' OR (drop_reason IS NOT NULL AND drop_reason <> '')),
    CONSTRAINT tasks_ordinal_unique UNIQUE (conversation_id, parent_id, ordinal)
);

CREATE INDEX IF NOT EXISTS tasks_conv_idx
    ON conversations.tasks (conversation_id, parent_id, ordinal);
CREATE INDEX IF NOT EXISTS tasks_assignee_idx
    ON conversations.tasks (assignee) WHERE assignee IS NOT NULL;
CREATE INDEX IF NOT EXISTS tasks_metadata_idx
    ON conversations.tasks USING gin (metadata);

-- Where the agent ran. Not derivable at query time: childstore is in-memory
-- only (pkg/childstore/store.go) and conversation had no location column.
-- repo_root groups worktrees of one repo; cwd alone does not.
ALTER TABLE conversations.conversation
    ADD COLUMN IF NOT EXISTS cwd       TEXT,
    ADD COLUMN IF NOT EXISTS repo_root TEXT;

CREATE INDEX IF NOT EXISTS conversation_repo_root_idx
    ON conversations.conversation (repo_root) WHERE repo_root IS NOT NULL;
