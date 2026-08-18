-- Attribution becomes a foreign key. It was free text stamped from a static
-- token→name map, which produced 'dev', 'configured' and 'rafiki-child' — code
-- artifacts, not people — plus 152 owner-less conversations. None of it is
-- worth migrating, so the columns are DROPPED rather than backfilled.
--
-- The views must go first: v_conversation and v_turn select the columns
-- directly. This is the FOURTH migration to define them (0007, 0008, 0011) —
-- review all of them before touching these bodies again.
--
-- v_analysis and v_finding are deliberately NOT touched. Despite what the
-- design notes assumed, neither depends on v_conversation: v_analysis reads
-- conversation_analysis and v_finding reads analysis_finding, so the CASCADE
-- below reaches only v_turn (verified with pg_depend). Dropping and retyping
-- two views this migration does not change would be pure regression surface.
DROP VIEW IF EXISTS conversations.v_turn;
DROP VIEW IF EXISTS conversations.v_conversation CASCADE;

ALTER TABLE conversations.conversation DROP COLUMN IF EXISTS owner;
ALTER TABLE conversations.conversation
    ADD COLUMN IF NOT EXISTS owner_user_id UUID REFERENCES conversations.users(id);

-- No ON DELETE action, because users are never deleted: `user rm` sets
-- deleted_at. A cascade here would rewrite author_user_id across every
-- compressed chunk of this hypertable.
--
-- FK direction note: conversation_turn is a hypertable and users is a plain
-- table. TimescaleDB supports hypertable→plain; only hypertable→hypertable is
-- rejected. But the column and the constraint must be added in TWO statements:
-- an inline `REFERENCES` fails with "cannot add column with constraints to a
-- hypertable that has columnstore enabled", while ADD COLUMN followed by ADD
-- CONSTRAINT is accepted and propagates to the chunks. It is a genuine,
-- enforced foreign key either way — an app-enforced plain UUID column is not
-- an acceptable substitute, and neither is dropping columnstore.
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS author;
ALTER TABLE conversations.conversation_turn
    ADD COLUMN IF NOT EXISTS author_user_id UUID;

-- ADD CONSTRAINT has no IF NOT EXISTS, and Migrate runs each version once, so
-- the guard exists only to keep this file re-runnable by hand the way every
-- other statement here is.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'conversation_turn_author_user_id_fkey'
           AND conrelid = 'conversations.conversation_turn'::regclass
    ) THEN
        ALTER TABLE conversations.conversation_turn
            ADD CONSTRAINT conversation_turn_author_user_id_fkey
            FOREIGN KEY (author_user_id) REFERENCES conversations.users(id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS conversation_owner_user_idx
    ON conversations.conversation (owner_user_id);

-- owner_canonical and owner_kind are GONE. They existed only to reverse a
-- username out of free text — rewriting 'brent-graveland.net' into an email,
-- then guessing human/service/system from its shape. A users row answers all
-- of that by joining, so the heuristics are deleted rather than ported.
CREATE VIEW conversations.v_conversation AS
SELECT c.id,
       u.username AS owner_username,
       c.persona,
       c.model,
       c.origin_entrypoint,
       c.driven_by,
       c.external_ref,
       c.status,
       c.name,
       c.created_at,
       c.updated_at
FROM conversations.conversation c
-- LEFT JOIN: an unattributed conversation (owner_user_id NULL) must still
-- appear, with a NULL username. And the join deliberately does NOT filter on
-- deleted_at — a removed user's history keeps resolving to their name, which
-- is the whole reason `user rm` tombstones instead of deleting.
LEFT JOIN conversations.users u ON u.id = c.owner_user_id;

-- Body carried over from 0011 unchanged apart from the identity columns:
-- t.author becomes au.username AS author_username, and the owner_canonical /
-- owner_kind pair becomes the single owner_username resolved by
-- v_conversation. Every pricing expression below is verbatim.
--
-- The join to v_conversation stays a LEFT JOIN (0007's reasoning, still
-- current): conversation_turn carries no foreign key on conversation_id, so a
-- turn orphaned by a rolled-back EnsureConversationByExternalRef or a manual
-- DELETE FROM conversation would otherwise vanish from the spend surface
-- entirely — tokens spent, no row anywhere.
CREATE VIEW conversations.v_turn AS
SELECT t.id,
       t.conversation_id,
       t.created_at,
       t.status,
       coalesce(t.model, '(unset)')         AS model,
       coalesce(t.source, '(unset)')        AS source,
       au.username                          AS author_username,
       t.author_kind,
       coalesce(t.upstream, '(unset)')      AS upstream,
       t.protocol,
       t.stop_reason,
       t.latency_ms,
       t.prefix_hash,
       t.error,
       coalesce(t.input_tokens, 0)          AS input_tokens,
       coalesce(t.output_tokens, 0)         AS output_tokens,
       coalesce(t.cache_read_tokens, 0)     AS cache_read_tokens,
       coalesce(t.cache_creation_tokens, 0) AS cache_creation_tokens,
       coalesce(c.owner_username, '(unattributed)')    AS owner_username,
       coalesce(c.origin_entrypoint, '(unattributed)') AS origin_entrypoint,
       coalesce(c.driven_by, '(unattributed)')         AS driven_by,
       coalesce(t.input_tokens, 0)          * coalesce(p.prompt_usd, 0)      AS input_usd,
       coalesce(t.output_tokens, 0)         * coalesce(p.completion_usd, 0)  AS output_usd,
       coalesce(t.cache_read_tokens, 0)     * coalesce(p.cache_read_usd, 0)  AS cache_read_usd,
       coalesce(t.cache_creation_tokens, 0) * coalesce(p.cache_write_usd, 0) AS cache_write_usd,
       CASE WHEN p.cache_read_usd IS NULL THEN NULL
            ELSE coalesce(t.cache_read_tokens, 0) * (coalesce(p.prompt_usd, 0) - p.cache_read_usd)
       END AS cache_saved_usd,
       (p.model_id IS NULL
        OR p.prompt_usd IS NULL
        OR p.completion_usd IS NULL
        OR (coalesce(t.cache_read_tokens, 0)     > 0 AND p.cache_read_usd  IS NULL)
        OR (coalesce(t.cache_creation_tokens, 0) > 0 AND p.cache_write_usd IS NULL)) AS unpriced
FROM conversations.conversation_turn t
LEFT JOIN conversations.v_conversation c ON c.id = t.conversation_id
LEFT JOIN conversations.users au ON au.id = t.author_user_id
LEFT JOIN conversations.model_pricing p ON p.model_id = t.model;
