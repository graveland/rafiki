-- Baseline: the initial conversations schema. Assembled from three
-- originally separate migrations, kept as delimited sections below so each is
-- still readable on its own.

-- >>> conversations schema and core tables
CREATE SCHEMA IF NOT EXISTS conversations;

-- A conversation is a first-class, entrypoint-agnostic entity (design §9).
-- driven_by is stamped at creation and never changes; it scopes the future
-- orphan-reissue sweep to server-driven conversations only.
CREATE TABLE IF NOT EXISTS conversations.conversation (
	id                UUID PRIMARY KEY DEFAULT uuidv7(),
	owner             TEXT,                 -- who-is identity (nullable for now)
	persona           TEXT,
	model             TEXT,
	origin_entrypoint TEXT NOT NULL,        -- 'diagnose' | 'claude' | 'slack' | ...
	driven_by         TEXT NOT NULL,        -- 'server' | 'client' (immutable)
	external_ref      TEXT,                 -- X-Rafiki-Session, slack thread ts, ...
	status            TEXT NOT NULL DEFAULT 'active',
	created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS conversation_driven_by_idx ON conversations.conversation (driven_by, status);
-- Atomic correlation key for EnsureConversationByExternalRef: one conversation
-- per (external_ref, driven_by), enabling INSERT ... ON CONFLICT DO NOTHING.
-- This partial unique index also serves the non-null external_ref lookup, so no
-- separate plain index on (external_ref) is needed.
CREATE UNIQUE INDEX IF NOT EXISTS conversation_external_ref_uniq
	ON conversations.conversation (external_ref, driven_by) WHERE external_ref IS NOT NULL;

-- One row per API turn. Ordered for replay by created_at (id is uuidv7, so
-- time-ordered); ordinal is a decorative hint only, NOT a uniqueness key — the
-- client/proxy path writes 0 every turn. Write-ahead: request + status='pending'
-- inserted before the call, response + usage filled after.
CREATE TABLE IF NOT EXISTS conversations.conversation_turn (
	id                    UUID NOT NULL DEFAULT uuidv7(),
	conversation_id       UUID NOT NULL,
	ordinal               INT NOT NULL,
	status                TEXT NOT NULL DEFAULT 'pending',   -- 'pending' | 'complete' | 'error'
	model                 TEXT,
	request               JSONB NOT NULL,
	response              JSONB,
	stop_reason           TEXT,
	input_tokens          BIGINT,
	output_tokens         BIGINT,
	cache_read_tokens     BIGINT,
	cache_creation_tokens BIGINT,
	upstream              TEXT,              -- 'anthropic' | 'openrouter'
	error                 TEXT,
	latency_ms            INT,
	created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- id is uuidv7 (time-ordered); created_at is the partition column, so the
	-- composite is hypertable-legal and lets CompleteTurn/FailTurn prune to one
	-- chunk. ordinal is retained only as a decorative ordering hint.
	PRIMARY KEY (id, created_at)
) WITH (
	tsdb.hypertable,
	tsdb.partition_column = 'created_at',
	tsdb.enable_columnstore = true,
	tsdb.segmentby = 'conversation_id',
	tsdb.orderby = 'created_at DESC'
);
CREATE INDEX IF NOT EXISTS conversation_turn_conv_idx ON conversations.conversation_turn (conversation_id, created_at DESC);
-- tsdb.enable_columnstore=true (above) auto-creates a 7-day columnstore policy
-- on TimescaleDB 2.23+ (see 0001_fleet_initial.up.sql), so no explicit
-- add_columnstore_policy call is needed. Retention is intentionally deferred
-- (no drop policy) pending a data decision.

-- Schema-only in this increment: multi-entrypoint attach/detach (design §9).
CREATE TABLE IF NOT EXISTS conversations.conversation_attachment (
	id              UUID NOT NULL DEFAULT uuidv7() PRIMARY KEY,
	conversation_id UUID NOT NULL REFERENCES conversations.conversation(id),
	entrypoint      TEXT NOT NULL,
	external_ref    TEXT,
	attached_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	detached_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS conversation_attachment_active_uniq
	ON conversations.conversation_attachment (conversation_id, entrypoint) WHERE detached_at IS NULL;

-- >>> turn provenance columns
-- Per-turn provenance: a conversation can interleave inputs from multiple
-- entrypoints (claude, tui, slack, diagnose) and multiple users, but the turn
-- row previously carried no origin/author — only conversation.owner, a single
-- conversation-level identity. These nullable columns record, per turn, where
-- the inbound message came from and who authored it.
ALTER TABLE conversations.conversation_turn
	ADD COLUMN IF NOT EXISTS source      TEXT,  -- 'claude' | 'tui' | 'slack' | 'diagnose' | ...
	ADD COLUMN IF NOT EXISTS author      TEXT,  -- who-is username / slack user id (nullable)
	ADD COLUMN IF NOT EXISTS author_kind TEXT;  -- 'human' | 'agent' | 'system'

-- >>> turn prefix_hash column
-- prefix_hash: sha256 of the request's static cache-prefix — the request with the
-- volatile "messages" list removed and keys canonicalized, i.e. model + system
-- prompt + tools. Lets us verify the prefix stays byte-identical across a
-- conversation's turns: a change flags dynamic content leaking into the
-- Anthropic-cached prefix (or a non-deterministically-ordered tools array), and
-- it correlates with cache_read/cache_creation tokens to explain why a cache broke.
ALTER TABLE conversations.conversation_turn
	ADD COLUMN IF NOT EXISTS prefix_hash TEXT;
