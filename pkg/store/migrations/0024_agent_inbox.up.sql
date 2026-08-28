-- 0024_agent_inbox.up.sql
-- The durable inbox: everything destined for an agent's turn.
--
-- Not the event log and not a second copy of it. The log is
-- at-least-once-to-a-cursor, fanned out to N subscribers; this is
-- consume-once-into-a-turn for exactly one consumer. One table serving both
-- gets you neither (design 3.6).
--
-- No foreign key to conversations.child, deliberately: a row is accepted
-- before delivery and must survive the child row being forgotten long enough
-- for the sweep to record what was dropped.
CREATE TABLE IF NOT EXISTS conversations.agent_inbox (
    id           TEXT        PRIMARY KEY,
    child_id     TEXT        NOT NULL,

    -- prompt | steer | abort. Matches inbox.Mode's String().
    mode         TEXT        NOT NULL,

    -- '' for a direct message from a human, a coordinator or an external
    -- service; the eventbuf source name ('subagents', 'executor') for a
    -- coalescing fragment. coalesce_key is last-write-wins WITHIN a source.
    source       TEXT        NOT NULL DEFAULT '',
    coalesce_key TEXT        NOT NULL DEFAULT '',

    body         TEXT        NOT NULL DEFAULT '',

    -- pending -> sent -> consumed, or -> dropped. pending vs sent is what
    -- stops the idle-drain retry from delivering a second copy of something
    -- already queued inside a live child.
    state        TEXT        NOT NULL DEFAULT 'pending',
    drop_reason  TEXT,

    accepted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT agent_inbox_state_check
        CHECK (state IN ('pending', 'sent', 'consumed', 'dropped')),
    CONSTRAINT agent_inbox_mode_check
        CHECK (mode IN ('prompt', 'steer', 'abort'))
);

-- The delivery read: one child's pending rows, oldest first.
CREATE INDEX IF NOT EXISTS agent_inbox_pending
    ON conversations.agent_inbox (child_id, accepted_at, id)
    WHERE state = 'pending';

-- The restart read: one child's unconfirmed rows.
CREATE INDEX IF NOT EXISTS agent_inbox_sent
    ON conversations.agent_inbox (child_id)
    WHERE state = 'sent';

-- The retention sweep.
CREATE INDEX IF NOT EXISTS agent_inbox_terminal_age
    ON conversations.agent_inbox (accepted_at)
    WHERE state IN ('consumed', 'dropped');

COMMENT ON COLUMN conversations.agent_inbox.state IS
    'pending = not yet written to a child; sent = written, awaiting the child''s '
    'consume confirmation; consumed/dropped = terminal. A daemon reverts only ITS '
    'OWN children''s sent rows on restart -- child rows are shared across daemons, '
    'so an unscoped reset double-delivers another daemon''s in-flight messages.';
