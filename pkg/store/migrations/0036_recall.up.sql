-- Recall schema: the memory tree, conversation windows and summaries, the
-- recall failure ledger and the recall state table (plan task 1.1). Extensions
-- are created here, not by the operator: ltree gives memory paths their Gist
-- index, vector gives windows/summaries/memories their embedding column type,
-- and pg_textsearch registers the bm25 access method every recall search uses.

CREATE EXTENSION IF NOT EXISTS ltree;
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_textsearch;

-- The child close tombstone: "deleted" was always a lie for a child row — Close
-- is a lifecycle close (the transcript is kept and the child is never
-- resumed), not a removal. Renamed so the column reads the way every reader
-- treats it. No index or constraint on this table carries the old name.
ALTER TABLE conversations.child RENAME COLUMN deleted_at TO closed_at;

-- Conversations close when their linked child is closed (stamped by
-- Controller.Close); the column lives here rather than on the child so a
-- conversation with no child row at all can still be closed later.
ALTER TABLE conversations.conversation ADD COLUMN closed_at timestamptz NULL;

CREATE TABLE conversations.memory (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_user_id   uuid NOT NULL REFERENCES conversations.users(id),
    path            ltree NOT NULL,
    name            text NOT NULL,
    body            text NOT NULL,
    meta            jsonb NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz NULL,
    embedding       vector NULL,
    embedding_model text NULL
);
CREATE UNIQUE INDEX memory_live_key ON conversations.memory (owner_user_id, path, name) WHERE deleted_at IS NULL;
CREATE INDEX memory_path_gist ON conversations.memory USING gist (path);
CREATE INDEX memory_bm25 ON conversations.memory USING bm25 (body) WITH (text_config='english');

CREATE TABLE conversations.conversation_window (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    conversation_id   uuid NOT NULL REFERENCES conversations.conversation(id),
    owner_user_id     uuid NULL,
    seq               int NOT NULL,
    ordinal_from      int NOT NULL,
    ordinal_to        int NOT NULL,
    text              text NOT NULL,
    sealed            boolean NOT NULL DEFAULT false,
    extractor_version int NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    embedding         vector NULL,
    embedding_model   text NULL,
    UNIQUE (conversation_id, seq)
);
CREATE INDEX conversation_window_conv_ord ON conversations.conversation_window (conversation_id, ordinal_from);
CREATE INDEX conversation_window_bm25 ON conversations.conversation_window USING bm25 (text) WITH (text_config='english');

CREATE TABLE conversations.conversation_summary (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    conversation_id uuid NOT NULL REFERENCES conversations.conversation(id),
    owner_user_id   uuid NULL,
    level           text NOT NULL CHECK (level IN ('segment','conversation')),
    seq             int NOT NULL,
    ordinal_from    int NOT NULL,
    ordinal_to      int NOT NULL,
    title           text NOT NULL,
    summary         text NOT NULL,
    prompt_version  int NOT NULL,
    model           text NOT NULL,
    input_tokens    bigint NOT NULL DEFAULT 0,
    output_tokens   bigint NOT NULL DEFAULT 0,
    total_cost_usd  double precision NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    embedding       vector NULL,
    embedding_model text NULL,
    UNIQUE (conversation_id, level, seq)
);
CREATE INDEX conversation_summary_bm25 ON conversations.conversation_summary USING bm25 ((title || ' ' || summary)) WITH (text_config='english');

CREATE TABLE conversations.recall_summary_failure (
    conversation_id uuid PRIMARY KEY REFERENCES conversations.conversation(id),
    prompt_version  int NOT NULL,
    model           text NOT NULL,
    attempts        int NOT NULL DEFAULT 0,
    last_error      text NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE conversations.recall_state (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
