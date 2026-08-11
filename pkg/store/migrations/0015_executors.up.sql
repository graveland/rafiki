-- The executor registry. The ROW is authoritative for everything that gates
-- access: labels, roots, isolation, enabled state. A credential proves only
-- binding to a row, never what the row says — which is what makes
-- relabelling and revocation row updates needing no reissue, no restart, and
-- no access to the executor's machine.
CREATE TABLE IF NOT EXISTS conversations.executors (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    -- The bcrypt/argon2 digest of the durable credential. NEVER the credential
    -- itself: this table is dumped by ordinary backups and read by anything
    -- with the DSN, and a plaintext bearer token there is a credential store.
    credential_hash TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',

    -- Trust labels, assigned by rafikid and never claimed by the executor.
    -- Split from self_reported deliberately: `arch` and `os` are harmless to
    -- lie about (lying only earns work you cannot run), but `env=work` gates
    -- confidentiality, so anything the executor could set is anything it
    -- could claim. A label which gates access cannot be asserted by the thing
    -- it gates.
    labels          JSONB NOT NULL DEFAULT '{}',
    -- Executor self-report at join. Capability facts only; NEVER consulted
    -- for admission.
    self_reported   JSONB NOT NULL DEFAULT '{}',
    -- Agent-written memoisation of expensive setup ("sentinel is built
    -- here"). Selectable by key-presence and exact value — a deliberate
    -- divergence from k8s, whose no-select rule is an indexing-cost argument
    -- irrelevant at this fleet size. Never referenced by an ADMISSION
    -- selector, or an agent could annotate its way onto a machine.
    annotations     JSONB NOT NULL DEFAULT '{}',

    roots           TEXT[] NOT NULL DEFAULT '{}',
    isolation       TEXT NOT NULL DEFAULT 'none',
    workspace_mode  TEXT NOT NULL DEFAULT 'pinned'
                    CHECK (workspace_mode IN ('ephemeral','pinned')),
    -- The executor-side admission selector, over CHILD labels. Taints and
    -- tolerations expressed symmetrically, without effects.
    admits          TEXT NOT NULL DEFAULT '',

    -- Revocation is a row update, so this is read on EVERY connection.
    enabled         BOOLEAN NOT NULL DEFAULT true,
    enrolled_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS executors_labels_idx
    ON conversations.executors USING gin (labels);
CREATE INDEX IF NOT EXISTS executors_annotations_idx
    ON conversations.executors USING gin (annotations);
CREATE INDEX IF NOT EXISTS executors_enabled_idx
    ON conversations.executors (enabled) WHERE enabled;

-- One-time enrollment tokens. An unknown identity is REJECTED, never
-- auto-enrolled: anyone who can reach the endpoint would otherwise join the
-- pool and start receiving file contents.
CREATE TABLE IF NOT EXISTS conversations.executor_enrollment_token (
    -- The token's digest, for the same reason credential_hash is a digest.
    token_hash   TEXT PRIMARY KEY,
    -- Labels the resulting executor row will carry. Bounded by the MINTER:
    -- an agent may only mint tokens carrying labels it is itself entitled to
    -- grant. Without that, an agent on a personal-machine executor mints a
    -- token claiming rafiki/env=work, provisions a VM with it, and launders
    -- itself a route to work data.
    labels       JSONB NOT NULL DEFAULT '{}',
    roots        TEXT[] NOT NULL DEFAULT '{}',
    isolation    TEXT NOT NULL DEFAULT 'none',
    workspace_mode TEXT NOT NULL DEFAULT 'pinned',
    admits       TEXT NOT NULL DEFAULT '',
    minted_by    TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,

    -- Consumption is ATOMIC: consumed_at is set by a conditional UPDATE
    -- (`WHERE consumed_at IS NULL`) whose RowsAffected is the winner's
    -- receipt, so two concurrent enrollments with the same token cannot both
    -- succeed. A read-then-write would let both read NULL.
    consumed_at  TIMESTAMPTZ,
    executor_id  UUID REFERENCES conversations.executors(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS enrollment_token_unconsumed_idx
    ON conversations.executor_enrollment_token (expires_at)
    WHERE consumed_at IS NULL;