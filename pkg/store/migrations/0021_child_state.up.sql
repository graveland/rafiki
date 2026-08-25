-- Child state moves off the local filesystem. <stateDir>/<childId>.json is
-- replaced by a row here, so a daemon restarting on a different host recovers
-- the same children.
CREATE TABLE conversations.child (
    child_id          TEXT PRIMARY KEY,

    -- Correlation, filled in when known. NULL at spawn (the conversation row
    -- does not exist yet) and forever for a pi/claude child whose proxy never
    -- created one. Deliberately NOT the primary key: an FK on the PK fails on
    -- the child's first write, which happens before any conversation exists.
    conversation_id   UUID REFERENCES conversations.conversation(id),
    owner_user_id     UUID REFERENCES conversations.users(id),

    kind              TEXT NOT NULL,
    name              TEXT,
    cwd               TEXT,
    config_dir        TEXT,
    pid               INT,
    -- Whose pid namespace the pid column belongs to. NOT a recovery filter:
    -- the lease decides who resumes. This exists so a daemon never signals a
    -- pid that belongs to a process on another host.
    daemon_id         TEXT,

    provider          TEXT,
    model             TEXT,
    thinking          TEXT,

    session_file      TEXT,
    session_dir       TEXT,
    session_id        TEXT,
    no_session        BOOLEAN NOT NULL DEFAULT false,

    -- status is the CURRENT state. last_status is the state the child was in
    -- before it exited, and it is what the recovery predicate reads. They are
    -- different questions; collapsing them inverts auto-resume.
    status            TEXT NOT NULL,
    last_status       TEXT,
    spawned_at        TIMESTAMPTZ NOT NULL,
    last_activity     TIMESTAMPTZ,
    exited_at         TIMESTAMPTZ,
    exit_code         INT,
    exit_signal       TEXT,

    executor_selector TEXT,
    workspace_mode    TEXT,

    max_depth         INT NOT NULL DEFAULT 0,
    max_cost          DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_children      INT NOT NULL DEFAULT 0,

    config            JSONB NOT NULL DEFAULT '{}',
    labels            JSONB NOT NULL DEFAULT '{}',

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX child_recoverable_idx ON conversations.child (kind, last_status);
CREATE INDEX child_labels_idx ON conversations.child USING gin (labels);
CREATE INDEX child_conversation_idx ON conversations.child (conversation_id);
CREATE INDEX child_owner_idx ON conversations.child (owner_user_id);

-- One writer per conversation. State on local disk used to provide daemon
-- isolation for free; a shared child table removes it, and this restores it.
--
-- Readers are never gated: every daemon may read every conversation, which is
-- what makes search across a laptop and a cluster work.
CREATE TABLE conversations.conversation_lease (
    conversation_id UUID PRIMARY KEY REFERENCES conversations.conversation(id) ON DELETE CASCADE,
    holder          TEXT NOT NULL,
    token           UUID NOT NULL,
    acquired_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX conversation_lease_holder_idx ON conversations.conversation_lease (holder);