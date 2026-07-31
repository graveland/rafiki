CREATE TABLE conversations.conversation_analysis (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    conversation_id uuid NOT NULL REFERENCES conversations.conversation(id) ON DELETE CASCADE,
    detector_version int NOT NULL,
    model text NOT NULL,
    profile text,
    status text NOT NULL DEFAULT 'ok',        -- ok | failed
    error text,
    prompt_hash text NOT NULL DEFAULT '',      -- sha256 of effective stage prompts; '' = builtin
    analysis jsonb,                            -- full analyze.Analysis (NULL when failed)
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    cost_usd double precision NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, detector_version, model, prompt_hash)
);

CREATE TABLE conversations.analysis_finding (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    analysis_id uuid NOT NULL REFERENCES conversations.conversation_analysis(id) ON DELETE CASCADE,
    axis text NOT NULL,
    topic_key text NOT NULL,
    skill_name text,
    title text NOT NULL,
    expected_savings_tokens bigint NOT NULL DEFAULT 0,
    status text NOT NULL DEFAULT 'open',       -- open | dismissed | actioned
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON conversations.analysis_finding (analysis_id);
CREATE INDEX ON conversations.analysis_finding (status, axis);
