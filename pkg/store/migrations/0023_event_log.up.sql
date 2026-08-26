-- 0023_event_log.up.sql
-- The durable event log: what a subscriber cursors over.
--
-- NOT a hypertable, deliberately. TimescaleDB requires a UNIQUE index to
-- include the partitioning column, which would make UNIQUE (child_id, ordinal)
-- inexpressible -- and that constraint is what keeps per-child ordinals
-- gap-free and makes a concurrent double-append fail loudly rather than
-- duplicate. The tier split (design §3.1) already keeps content deltas out, so
-- the volume is tens of rows per turn, not thousands.
CREATE TABLE IF NOT EXISTS conversations.event_log (
    child_id   TEXT        NOT NULL,
    ordinal    INTEGER     NOT NULL,
    type       TEXT        NOT NULL,
    payload    JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (child_id, ordinal)
);

-- Retention scans and operational "what happened around 14:05" queries.
CREATE INDEX IF NOT EXISTS event_log_created_at ON conversations.event_log (created_at DESC);

-- The rail's "everything of this type recently" read.
CREATE INDEX IF NOT EXISTS event_log_type_created_at ON conversations.event_log (type, created_at DESC);

COMMENT ON COLUMN conversations.event_log.ordinal IS
    'Per-child, gap-free, starts at 0. NOT conversation_message.ordinal -- that is a '
    'separate space keyed by conversation, and comparing the two mis-seeks every consumer.';
