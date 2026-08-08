CREATE TABLE conversations.raw_http_request (
    id              UUID NOT NULL DEFAULT uuidv7(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    conversation_id UUID,
    turn_id         UUID,
    model           TEXT,
    upstream        TEXT,
    source          TEXT NOT NULL,  -- 'proxy' | 'fundi'

    -- Request
    req_method      TEXT NOT NULL,
    req_path        TEXT NOT NULL,
    req_headers     JSONB,
    req_body        JSONB,

    -- Response
    resp_status     INT,
    resp_headers    JSONB,
    resp_body       JSONB,

    latency_ms      INT,
    error           TEXT,

    PRIMARY KEY (id, created_at)
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'created_at',
    tsdb.enable_columnstore = true,
    tsdb.segmentby = 'source',
    tsdb.orderby = 'created_at DESC'
);
CREATE INDEX ON conversations.raw_http_request (conversation_id, created_at DESC);
