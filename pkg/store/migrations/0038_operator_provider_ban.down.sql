-- Inverse of 0038_operator_provider_ban.up.sql.
UPDATE openrouter.provider_ejection SET expires_at = 'infinity' WHERE expires_at IS NULL;
ALTER TABLE openrouter.provider_ejection ALTER COLUMN expires_at SET NOT NULL;
ALTER TABLE openrouter.provider_ejection DROP COLUMN IF EXISTS note;
