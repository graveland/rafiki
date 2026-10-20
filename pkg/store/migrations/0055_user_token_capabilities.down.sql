-- Drops the column, so every token returns to full user authority. A narrowed
-- token does not fail loudly after a downgrade — it WIDENS: revoke every
-- notify-scoped token BEFORE this runs (or before rolling the daemon binary
-- back without running it), or the gateway's internet-facing credential
-- becomes an ordinary full-authority user token.
ALTER TABLE conversations.user_token DROP COLUMN capabilities;
