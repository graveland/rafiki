ALTER TABLE conversations.users ADD COLUMN email TEXT;
CREATE UNIQUE INDEX users_email_active ON conversations.users (email)
    WHERE deleted_at IS NULL AND email IS NOT NULL;
ALTER TABLE conversations.users ALTER COLUMN token_sha256 DROP NOT NULL;

-- One row per credential. users.token_sha256 is no longer read; the INSERT
-- below carries every existing credential over.
CREATE TABLE conversations.user_token (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id       UUID NOT NULL REFERENCES conversations.users(id),
    token_sha256  TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    origin        TEXT NOT NULL CHECK (origin IN ('service', 'oidc')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);
CREATE INDEX user_token_user_idx ON conversations.user_token (user_id);

INSERT INTO conversations.user_token (user_id, token_sha256, name, origin, created_at)
SELECT id, token_sha256, 'initial', 'service', created_at
  FROM conversations.users
 WHERE token_sha256 IS NOT NULL;

-- (issuer, subject) -> user. Lookups are always scoped to the configured
-- issuer, so rows under a previous issuer are inert.
CREATE TABLE conversations.user_identity (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id     UUID NOT NULL REFERENCES conversations.users(id),
    issuer      TEXT NOT NULL,
    subject     TEXT NOT NULL,
    email       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX user_identity_active ON conversations.user_identity (issuer, subject)
    WHERE revoked_at IS NULL;
CREATE INDEX user_identity_user_idx ON conversations.user_identity (user_id);

ALTER TABLE conversations.executors
    ADD COLUMN owner_user_id UUID REFERENCES conversations.users(id);
ALTER TABLE conversations.executor_enrollment_token
    ADD COLUMN owner_user_id UUID REFERENCES conversations.users(id);

UPDATE conversations.executors e
   SET owner_user_id = u.id
  FROM conversations.users u
 WHERE u.deleted_at IS NULL AND u.username = e.labels->>'owner';
UPDATE conversations.executor_enrollment_token t
   SET owner_user_id = u.id
  FROM conversations.users u
 WHERE u.deleted_at IS NULL AND u.username = t.labels->>'owner';
