-- OpenRouter list prices, refreshed by store.SyncModelPricing. Keyed by the
-- string you look up — either a model as it appears on a turn ("claude-opus-5",
-- "kimi-k3", "~moonshotai/kimi-latest") or a catalog OR id — with or_id
-- recording what the catalog resolved it to. A row with or_id IS NULL is a
-- model we saw on a turn but could not price; the views surface it as
-- unpriced rather than letting it read as $0 spend.
--
-- Every price column is nullable and NULL means "the source does not price
-- this", never zero. That distinction is load-bearing: a missing cache_read
-- price written as 0 makes cache_saved_usd compute as cache tokens at the FULL
-- prompt price, overstating savings by the entire cache discount.
--
-- There is deliberately no cache_write_1h price: conversation_turn records no
-- cache TTL, so no view can tell a 1h write (2x premium) from a 5m one (1.25x),
-- and a column nothing can correctly consume is worse than no column.
CREATE TABLE conversations.model_pricing (
    model_id        TEXT PRIMARY KEY,
    or_id           TEXT,
    prompt_usd      DOUBLE PRECISION,
    completion_usd  DOUBLE PRECISION,
    cache_read_usd  DOUBLE PRECISION,
    cache_write_usd DOUBLE PRECISION,
    fetched_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- v_conversation and v_turn are the entire FDW surface exposed to a downstream
-- grafana database. Neither selects request, response, prefix_content,
-- cache_breakpoints, or message content: conversation payloads must not be
-- readable from a database every Grafana user can query. Keep it that way.

CREATE VIEW conversations.v_conversation AS
SELECT c.id,
       c.owner,
       c.persona,
       c.model,
       c.origin_entrypoint,
       c.driven_by,
       c.external_ref,
       c.status,
       c.created_at,
       c.updated_at,
       -- Only the dash-for-at typo is corrected. kubecfg-<name> is left alone:
       -- kubecfg-svcaccount would rewrite to svcaccount@example.com, but the
       -- real account is svc@example.com. A wrong merge is worse than two rows.
       --
       -- Deliberately faithful (nullable): a NULL owner stays NULL here. v_turn
       -- is the reporting surface Grafana filters with `col IN ($var)`, which
       -- never matches NULL, so the substitute sentinels belong there — not
       -- here, where a NULL must keep reading as "we do not know".
       k.canonical AS owner_canonical,
       -- An owner that is neither system:-prefixed nor an email address is a
       -- machine identity, not a person: kubecfg-svcaccount is a kubeconfig
       -- service credential, and counting it as human inflated the "distinct
       -- active owners" adoption metric with robots.
       --
       -- Classified on the CANONICAL owner, not the raw one: dev-example.com
       -- is the dash-for-at typo of a real person and carries no "@" until the
       -- rewrite above has been applied.
       CASE WHEN k.canonical IS NULL          THEN NULL
            WHEN k.canonical LIKE 'system:%'  THEN 'system'
            WHEN k.canonical LIKE 'kubecfg-%' THEN 'service'
            WHEN k.canonical LIKE '%@%'       THEN 'human'
            ELSE 'service' END AS owner_kind
FROM conversations.conversation c
-- LATERAL so the rewrite is written once and both columns read the same value.
CROSS JOIN LATERAL (
    SELECT regexp_replace(c.owner, '-example\.com$', '@example.com') AS canonical
) k;

-- Every dimension column here is coalesced to a sentinel, because Grafana
-- filters each panel with `col IN ($var)` and SQL IN never matches NULL: a NULL
-- upstream silently dropped 109 of 113 error turns out of the "top error
-- messages" panel and made "stuck pending" structurally always zero. Two
-- distinct sentinels, because the two causes want different follow-up:
-- '(unset)' means the turn recorded no value for that column, '(unattributed)'
-- means the turn has no conversation row at all.
--
-- '(unset)' rather than 'unknown' on purpose: "unknown" already occurs as a
-- real model string in captured data, and a sentinel that collides with a real
-- value is a sentinel that lies.
--
-- The join to conversation is a LEFT JOIN because conversation_turn carries no
-- foreign key on conversation_id. A turn orphaned by a rolled-back
-- EnsureConversationByExternalRef, or by a manual DELETE FROM conversation
-- (which cascades to conversation_message and conversation_analysis but NOT to
-- the hypertable), would otherwise vanish from the spend surface entirely —
-- tokens spent, no row anywhere.
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
       -- What the cache_read tokens would have cost at full prompt price, minus
       -- what they did cost. Drives the "cache savings" stat tile. NULL, not 0,
       -- when the cache read price is unknown: an unknown saving must not read
       -- as "we saved nothing", and Grafana's sum() skips NULLs.
       CASE WHEN p.cache_read_usd IS NULL THEN NULL
            ELSE coalesce(t.cache_read_tokens, 0) * (coalesce(p.prompt_usd, 0) - p.cache_read_usd)
       END AS cache_saved_usd,
       -- A missing cache price only counts against pricing completeness when
       -- the turn actually has cache tokens: a model that never caches is
       -- fully priced without them.
       (p.model_id IS NULL
        OR p.prompt_usd IS NULL
        OR p.completion_usd IS NULL
        OR (coalesce(t.cache_read_tokens, 0)     > 0 AND p.cache_read_usd  IS NULL)
        OR (coalesce(t.cache_creation_tokens, 0) > 0 AND p.cache_write_usd IS NULL)) AS unpriced
FROM conversations.conversation_turn t
LEFT JOIN conversations.v_conversation c ON c.id = t.conversation_id
-- Joined on the RAW t.model, not the coalesced alias: a NULL model must miss
-- this join and read as unpriced, not match a row keyed '(unset)'.
LEFT JOIN conversations.model_pricing p ON p.model_id = t.model;

-- v_analysis and v_finding extend the same FDW boundary to the analysis
-- tables. conversation_analysis.analysis is a serialized analyze.Analysis
-- whose Outcome field is a one-line natural-language description of the
-- conversation, and .error can carry model output; neither belongs in a
-- database every Grafana user can query. The dashboard needs only the
-- scalar accounting columns.
CREATE VIEW conversations.v_analysis AS
SELECT a.id,
       a.conversation_id,
       a.detector_version,
       a.model,
       a.profile,
       a.prompt_hash,
       a.status,
       a.input_tokens,
       a.output_tokens,
       a.cost_usd,
       a.created_at
FROM conversations.conversation_analysis a;

-- title is excluded for the same reason analysis.Outcome is: it is LLM prose,
-- generated into the same struct as Evidence []TurnCite and drawn from
-- verbatim conversation quotes. The dashboard reads axis, topic_key,
-- skill_name, status and expected_savings_tokens and never reads title, so
-- exposing it buys nothing and widens the payload boundary.
CREATE VIEW conversations.v_finding AS
SELECT f.id,
       f.analysis_id,
       f.axis,
       f.topic_key,
       f.skill_name,
       f.expected_savings_tokens,
       f.status,
       f.created_at
FROM conversations.analysis_finding f;
