-- Restore the dash-for-at owner rewrite that 0008 dropped.
--
-- 0007 canonicalized an owner with `regexp_replace(c.owner,
-- '-example\.com$', '@example.com')`, and its own comment (still present in
-- that file) explains why: "dev-example.com is the dash-for-at typo of a
-- real person and carries no @ until the rewrite above has been applied".
-- The owner_kind CASE classifies on the canonical value precisely so that
-- typo lands in 'human' rather than falling through to 'service'.
--
-- 0008 replaced that expression with a `-at-` → `@` rewrite while carrying
-- 0007's comment forward unchanged, so the documented behaviour and the
-- implementation disagreed from that point on. Nothing flagged it because
-- the only test covering it (TestOwnerCanonicalRewritesDashDomain) is
-- DB-backed and skips without RAFIKI_TEST_DSN, so a green `go test ./...`
-- said nothing. The visible effect is an under-count: a dash-typo owner
-- canonicalizes to itself, carries no '@', and is classified 'service' —
-- dropping a real person out of the "distinct active owners" adoption
-- metric, the mirror image of the robots-inflating-it problem the
-- kubecfg- rule was added to fix.
--
-- This migration applies BOTH rewrites. 0008's `-at-` form is kept because
-- it is a real spelling that postdates 0007 and nothing says it was
-- mistaken; only the regression is undone.
--
-- The domain half is generalized off 0007's hardcoded 'example.com', which
-- only ever fixed the one domain that appears in the tests. The rule is
-- "a trailing -<something>.<tld>": the dot is what distinguishes a typo'd
-- address from an ordinary hyphenated machine name, which is exactly the
-- distinction the test fixtures encode —
--   dev-example.com              -> dev@example.com      (dot: a typo'd address)
--   dev-mail.example.com         -> dev@mail.example.com (subdomains still work)
--   foo-bar-example.com          -> foo-bar@example.com  (splits at the last dash)
--   kubecfg-svcaccount           -> unchanged             (no dot: a service account)
--   ci-runner                    -> unchanged             (no dot: a machine identity)
--   kubecfg-deploy.prod.internal -> unchanged             (dotted host, but still kubecfg-)
-- [^-@]+ is what makes it split at the last dash and refuse a string that
-- already contains an '@'.
--
-- That last row is the fix this migration makes over its first version: the
-- generalized "-<label>.<tld>" rule fires on ANY dotted suffix, including a
-- service identity that happens to carry a dotted hostname
-- (kubecfg-deploy.prod.internal). Left alone, the rewrite turns it into
-- kubecfg@deploy.prod.internal, which no longer matches the `LIKE
-- 'kubecfg-%'` classification arm below and — now that it contains an '@' —
-- falls into 'human', inflating the adoption metric with a robot. That is
-- the exact bug the 'kubecfg-' rule exists to prevent, just approached from
-- the rewrite side instead of the classification side.
--
-- The fix skips the rewrite entirely for any raw owner already starting
-- with 'kubecfg-': canonical is left equal to the raw owner, so the
-- classification arm (which still reads the canonical value, see below)
-- keeps matching. This is preferred over classifying the 'kubecfg-' prefix
-- on the raw owner instead of canonical: it keeps the CASE below untouched
-- and — more importantly — keeps owner_canonical itself trustworthy for a
-- service account. Rewriting it to kubecfg@deploy.prod.internal would still
-- be wrong even if owner_kind correctly said 'service', because anything
-- reading owner_canonical directly (not just owner_kind) would see what
-- looks like a real email address for a robot.

DROP VIEW IF EXISTS conversations.v_conversation CASCADE;

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
       -- kubecfg-deploy.prod.internal — see the migration header for why.
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
               -- carry a dotted hostname: see the migration header.
               WHEN c.owner LIKE 'kubecfg-%' THEN c.owner
               ELSE regexp_replace(
                        CASE WHEN c.owner LIKE '%-at-%'
                             THEN regexp_replace(c.owner, '-at-', '@', 'g')
                             ELSE c.owner END,
                        '-([^-@]+\.[A-Za-z]{2,})$', '@\1'
                    )
           END AS canonical
) k ON true;

-- Recreate v_turn unchanged from 0008. This duplication is intentional —
-- 0011 is the third of exactly three migrations that define this view, and
-- any future migration touching it must review all three.
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
