-- Daemon-brokered OAuth credentials for remote MCP servers.
--
-- Two tables, deliberately separate:
--
--   conversations.mcp_credential -- one grant per (owner, resource). Tokens and
--     the client secret live ONLY inside credential_sealed (AES-256-GCM under
--     RAFIKI_SECRETS_KEY, pkg/secretbox), bound by associated data to
--     lowercase-owner|mcp|canonical-resource|credential. This table is dumped
--     by ordinary backups, so no token or secret has its own column.
--
--   conversations.mcp_endpoint -- one row per (owner, canonical MCP endpoint).
--     It maps the configured endpoint to the resource+issuer it was validated
--     for, and carries the durable generation that fences logins. It exists
--     even before a grant, so logging out an endpoint nothing is bound to still
--     stops an in-flight first login.
--
-- Both carry generations, not just rows: an explicit login advances the grant
-- generation, and a logout advances every endpoint/grant fence it touches. A
-- pending login commits only under the generations it captured, so neither a
-- logout nor an older login can be overwritten or undone.

CREATE TABLE IF NOT EXISTS conversations.mcp_credential (
    owner_user_id          UUID   NOT NULL REFERENCES conversations.users(id),
    -- The canonical RFC 8707 resource this grant is for, NEVER the endpoint URL.
    resource               TEXT   NOT NULL,
    -- The exact issuer string the SDK validated, not a canonical equivalent.
    issuer                 TEXT   NOT NULL,
    -- Durable grant fence: advanced by every successful explicit login and
    -- every invalidation. A login commits only against the generation it
    -- captured; the row is kept as a tombstone after logout.
    generation             BIGINT NOT NULL,
    key_id                 TEXT   NOT NULL DEFAULT '',
    credential_sealed      BYTEA  NOT NULL,
    -- Non-secret registration and token metadata. client_id is public; the
    -- client secret is inside credential_sealed.
    client_id              TEXT   NOT NULL DEFAULT '',
    auth_style             TEXT   NOT NULL DEFAULT 'auto'
                                CHECK (auth_style IN ('auto', 'in_params', 'in_header')),
    authorization_endpoint TEXT   NOT NULL DEFAULT '',
    token_endpoint         TEXT   NOT NULL DEFAULT '',
    -- Empty when the issuer advertised no revocation endpoint; revocation is
    -- then skipped rather than guessed.
    revocation_endpoint    TEXT   NOT NULL DEFAULT '',
    scopes                 TEXT[] NOT NULL DEFAULT '{}',
    redirect_url           TEXT   NOT NULL DEFAULT '',
    token_type             TEXT   NOT NULL DEFAULT '',
    -- NULL means the issuer reported no expiry; such a token never refreshes
    -- on expiry alone.
    access_expiry          TIMESTAMPTZ,
    -- Durable refresh intent. 'refreshing' is committed BEFORE the upstream
    -- request and refuses every other acquisition until it is finalized. A
    -- failed, ambiguous or uncommitted refresh leaves it set, so the generation
    -- needs a fresh explicit login: a single-use refresh token is never replayed.
    refresh_state          TEXT   NOT NULL DEFAULT 'idle'
                                CHECK (refresh_state IN ('idle', 'refreshing')),
    refresh_version        BIGINT NOT NULL DEFAULT 0,
    -- Logout tombstone. The row and its generation outlive it; the sealed blob
    -- is wiped.
    invalidated_at         TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_user_id, resource)
);

CREATE TABLE IF NOT EXISTS conversations.mcp_endpoint (
    owner_user_id            UUID   NOT NULL REFERENCES conversations.users(id),
    -- The canonical configured endpoint URL, with path and query preserved. It
    -- is frequently NOT the resource string (e.g. endpoint /mcp, resource /).
    endpoint                 TEXT   NOT NULL,
    -- Durable endpoint fence: advanced by every logout on this endpoint or on
    -- the grant it is bound to. A login begun before a logout cannot commit.
    generation               BIGINT NOT NULL DEFAULT 1,
    -- Live binding: the validated resource+issuer this endpoint authenticates
    -- against, and the grant generation it was installed under. All three are
    -- set together or all are NULL.
    bound_resource           TEXT,
    bound_issuer             TEXT,
    bound_grant_generation   BIGINT,
    -- Pending login: the resource, exact issuer and grant generation a login in
    -- progress is fenced to, recorded before the browser URL is published. All
    -- three are set together or all are NULL.
    pending_resource         TEXT,
    pending_issuer           TEXT,
    pending_grant_generation BIGINT,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_user_id, endpoint),
    CHECK ((bound_resource IS NULL) = (bound_issuer IS NULL)
       AND (bound_resource IS NULL) = (bound_grant_generation IS NULL)),
    CHECK ((pending_resource IS NULL) = (pending_issuer IS NULL)
       AND (pending_resource IS NULL) = (pending_grant_generation IS NULL))
);

-- A logout by resource walks the endpoints bound or pending to it; these keep
-- that from scanning an owner's whole endpoint set.
CREATE INDEX IF NOT EXISTS mcp_endpoint_bound_resource
    ON conversations.mcp_endpoint (owner_user_id, bound_resource)
    WHERE bound_resource IS NOT NULL;
CREATE INDEX IF NOT EXISTS mcp_endpoint_pending_resource
    ON conversations.mcp_endpoint (owner_user_id, pending_resource)
    WHERE pending_resource IS NOT NULL;
