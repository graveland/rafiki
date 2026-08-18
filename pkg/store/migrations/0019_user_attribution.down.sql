-- Restore the free-text identity columns and the 0011 views. The DATA is not
-- restored: a down migration restores shape, not history — the usernames lived
-- in conversations.users, which this file must not read from.

DROP VIEW IF EXISTS conversations.v_turn;
DROP VIEW IF EXISTS conversations.v_conversation CASCADE;

DROP INDEX IF EXISTS conversations.conversation_owner_user_idx;
ALTER TABLE conversations.conversation      DROP COLUMN IF EXISTS owner_user_id;
ALTER TABLE conversations.conversation_turn DROP COLUMN IF EXISTS author_user_id;
ALTER TABLE conversations.conversation      ADD COLUMN IF NOT EXISTS owner  TEXT;
ALTER TABLE conversations.conversation_turn ADD COLUMN IF NOT EXISTS author TEXT;

-- Both view bodies below are copied verbatim from
-- 0011_owner_canonical_dash_domain.up.sql.

CREATE VIEW conversations.v_conversation AS
SELECT c.id,
       c.owner,
       c.persona,
       c.model,
       c.origin_entrypoint,
       c.driven_by,
       c.external_ref,
       c.status,
       c.name,
       c.created_at,
       c.updated_at,
       k.canonical AS owner_canonical,
       -- Classified on the CANONICAL owner, not the raw one: a dash-typo
       -- address carries no '@' until the rewrite below has been applied.
       -- This still works for kubecfg- owners because the rewrite below
       -- leaves them untouched (canonical == raw owner), so 'kubecfg-%'
       -- keeps matching even for a dotted host like
       -- kubecfg-deploy.prod.internal — see 0011's header for why.
       CASE WHEN k.canonical IS NULL          THEN NULL
            WHEN k.canonical LIKE 'system:%'  THEN 'system'
            WHEN k.canonical LIKE 'kubecfg-%' THEN 'service'
            WHEN k.canonical LIKE '%@%'       THEN 'human'
            ELSE 'service' END AS owner_kind
FROM conversations.conversation c
-- LEFT JOIN LATERAL (not CROSS JOIN, as in 0007) so a NULL owner still
-- produces a row with a NULL canonical rather than dropping the
-- conversation from the view entirely.
LEFT JOIN LATERAL (
    SELECT CASE
               -- kubecfg- identities are never rewritten, even when they
               -- carry a dotted hostname: see 0011's header.
               WHEN c.owner LIKE 'kubecfg-%' THEN c.owner
               ELSE regexp_replace(
                        CASE WHEN c.owner LIKE '%-at-%'
                             THEN regexp_replace(c.owner, '-at-', '@', 'g')
                             ELSE c.owner END,
                        '-([^-@]+\.[A-Za-z]{2,})$', '@\1'
                    )
           END AS canonical
) k ON true;

CREATE VIEW conversations.v_turn AS
SELECT t.id,
       t.conversation_id,
       t.created_at,
       t.status,
       coalesce(t.model, '(unset)')         AS model,
       coalesce(t.source, '(unset)')        AS source,
       t.author,
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
       coalesce(c.owner_canonical, '(unattributed)')   AS owner_canonical,
       coalesce(c.owner_kind, '(unattributed)')        AS owner_kind,
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
LEFT JOIN conversations.model_pricing p ON p.model_id = t.model;
