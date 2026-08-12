-- Add extracted columns from OpenRouter OTLP span attributes that were
-- previously only available via raw_payload JSONB: split cost components,
-- token breakdowns.  The new columns are NULL-able so the migration is
-- safe against existing rows (they simply stay NULL).
ALTER TABLE openrouter.broadcast
    ADD COLUMN IF NOT EXISTS total_tokens       BIGINT,
    ADD COLUMN IF NOT EXISTS reasoning_tokens   BIGINT,
    ADD COLUMN IF NOT EXISTS input_cost_usd     DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS output_cost_usd    DOUBLE PRECISION;
