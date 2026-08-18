-- Identity. Until now attribution was a free-text username stamped from a
-- static config map; this table makes it a row, so a name change or a
-- revocation is an UPDATE rather than a redeploy.
CREATE TABLE IF NOT EXISTS conversations.users (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    username      TEXT NOT NULL,

    -- base64url(sha256(token)) — NEVER the token. Same scheme and same
    -- reasoning as executors.credential_hash: this table is dumped by
    -- ordinary backups and readable by anything with the DSN.
    --
    -- SHA-256 rather than bcrypt, deliberately. The token is 256 bits of
    -- daemon-generated randomness, so a work factor buys nothing against
    -- brute force — and bcrypt salts per row, which would make every
    -- authentication a full-table scan of bcrypt comparisons. The proxy face
    -- authenticates per HTTP request, so that cost would be paid per LLM
    -- call. A UNIQUE index on the digest makes it one lookup.
    token_sha256  TEXT NOT NULL UNIQUE,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Tombstone, never a row delete. Hard-deleting would force
    -- ON DELETE SET NULL to rewrite author_user_id on every historical turn
    -- the user ever wrote, inside compressed chunks of a hypertable — the
    -- operation columnstore is worst at. It also keeps history resolvable:
    -- a deleted user's turns still show their name instead of decaying to
    -- anonymous.
    deleted_at    TIMESTAMPTZ
);

-- Unique among ACTIVE users only, so `user rm` frees the name for reuse.
CREATE UNIQUE INDEX IF NOT EXISTS users_username_active
    ON conversations.users (username) WHERE deleted_at IS NULL;
