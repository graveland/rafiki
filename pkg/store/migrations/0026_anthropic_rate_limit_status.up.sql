-- One row per user: the latest anthropic-ratelimit-unified-* snapshot
-- captured off a genuine OAuth-passthrough response to api.anthropic.com.
-- Latest-only, deliberately no history -- Anthropic's own 5h/7d windows are
-- already rolling, so a timeseries here would buy nothing.
--
-- A row exists only once a passthrough call has actually happened for that
-- user; absence is the signal a client gates display on, not a status enum.
CREATE TABLE conversations.anthropic_rate_limit_status (
    user_id             UUID PRIMARY KEY REFERENCES conversations.users(id) ON DELETE CASCADE,
    organization_id     TEXT,

    five_h_utilization  DOUBLE PRECISION,
    five_h_reset_at     TIMESTAMPTZ,
    five_h_status       TEXT,

    seven_d_utilization DOUBLE PRECISION,
    seven_d_reset_at    TIMESTAMPTZ,
    seven_d_status      TEXT,

    overall_status      TEXT,

    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
