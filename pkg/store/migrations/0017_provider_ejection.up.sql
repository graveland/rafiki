-- Append-only log of providers ejected from OpenRouter routing by the provider
-- cache guard.  Rows are never updated or deleted: the table doubles as
-- breakage history ("when did this provider last go bad, and how often?"), and
-- the daemon reseeds its in-memory blacklist at startup from the rows that have
-- not yet expired.  In-memory state is authoritative; this is durability, not
-- coordination, so no uniqueness constraint is wanted here -- a provider that
-- breaks twice should leave two rows.
CREATE TABLE IF NOT EXISTS openrouter.provider_ejection (
    id         BIGINT GENERATED ALWAYS AS IDENTITY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider   TEXT        NOT NULL,
    model_line TEXT        NOT NULL,
    reason     TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    evidence   JSONB,
    PRIMARY KEY (id, created_at)
);

CREATE INDEX IF NOT EXISTS provider_ejection_expires_idx
    ON openrouter.provider_ejection (expires_at DESC);
