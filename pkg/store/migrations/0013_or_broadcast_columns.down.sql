ALTER TABLE openrouter.broadcast
    DROP COLUMN IF EXISTS total_tokens,
    DROP COLUMN IF EXISTS reasoning_tokens,
    DROP COLUMN IF EXISTS input_cost_usd,
    DROP COLUMN IF EXISTS output_cost_usd;
