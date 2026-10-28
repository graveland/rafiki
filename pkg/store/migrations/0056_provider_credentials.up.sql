-- One ChatGPT (or future OAuth) login per rafiki user and provider. Token
-- columns are AES-256-GCM ciphertext (pkg/secretbox) under RAFIKI_SECRETS_KEY,
-- never plaintext: this table is dumped by ordinary backups.
CREATE TABLE IF NOT EXISTS conversations.provider_credential (
    owner_user_id   UUID NOT NULL REFERENCES conversations.users(id),
    provider        TEXT NOT NULL,
    key_id          TEXT NOT NULL,
    refresh_sealed  BYTEA NOT NULL,
    access_sealed   BYTEA NOT NULL,
    access_expiry   TIMESTAMPTZ NOT NULL,
    account_id      TEXT NOT NULL DEFAULT '',
    models_json     JSONB,
    models_etag     TEXT NOT NULL DEFAULT '',
    dead_at         TIMESTAMPTZ,
    last_refresh_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_user_id, provider)
);
