-- Operator bans ride the provider ejection log (reason 'operator', model_line
-- '*'). A ban may have no expiry — NULL expires_at means "until lifted" — and
-- carries the operator's note. A lift is a superseding row (reason 'lift'),
-- never a delete: the log stays append-only history.
ALTER TABLE openrouter.provider_ejection ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE openrouter.provider_ejection ADD COLUMN IF NOT EXISTS note text NULL;
