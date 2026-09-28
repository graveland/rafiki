-- Append-only log of routing-policy writes: model_line ('*' = global) -> a
-- routing spec (routing.ParseSpec's grammar). The newest row per model_line is
-- the live policy; a row with deleted = true ends it. Never updated or deleted:
-- the table is also the history of who routed what, when.
CREATE TABLE IF NOT EXISTS openrouter.route_policy (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    model_line TEXT        NOT NULL,
    spec       TEXT        NOT NULL DEFAULT '',
    deleted    BOOLEAN     NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS route_policy_line_idx ON openrouter.route_policy (model_line, id DESC);
