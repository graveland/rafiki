-- OpenRouter broadcast receiver: per-generation metadata from OTLP webhook.
-- Lives in its own schema — this data originates from OpenRouter, not rafiki,
-- and a cross-schema join to conversations.conversation_turn is read-only.
--
-- Privacy mode is expected (no request/response content — only token counts,
-- cost, timing, model, and correlation ids). The raw_payload JSONB stores the
-- full OTLP span for schema evolution without re-migration.
CREATE SCHEMA IF NOT EXISTS openrouter;
CREATE TABLE openrouter.broadcast (
    id              BIGINT GENERATED ALWAYS AS IDENTITY,
    session_id      TEXT,
    generation_id   TEXT,
    trace_id        TEXT,
    span_id         TEXT,
    model           TEXT,
    input_tokens    BIGINT,
    output_tokens   BIGINT,
    cache_read_tokens  BIGINT,
    cost_usd        DOUBLE PRECISION,
    latency_ms      INT,
    provider        TEXT,
    finish_reason   TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw_payload     JSONB NOT NULL,
    PRIMARY KEY (id, created_at)
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'created_at',
    tsdb.enable_columnstore = true,
    tsdb.segmentby = 'session_id',
    tsdb.orderby = 'created_at DESC'
);
CREATE INDEX IF NOT EXISTS broadcast_session_idx ON openrouter.broadcast (session_id, created_at DESC);