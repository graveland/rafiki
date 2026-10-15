-- Agent-facing schedules (CronCreate / ScheduleWakeup). Scoped to the CHILD,
-- not the conversation: a claude child's conversation_id is NULL and /clear
-- changes it. Rows are tombstoned (closed_at), never deleted.
CREATE TABLE conversations.schedule (
    id              TEXT PRIMARY KEY,
    child_id        TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('cron', 'wakeup')),
    cron            TEXT,
    tz              TEXT NOT NULL CHECK (tz <> ''),
    prompt          TEXT NOT NULL,
    recurring       BOOLEAN NOT NULL,
    next_fire_at    TIMESTAMPTZ NOT NULL,
    last_fired_at   TIMESTAMPTZ,
    held_count      INT NOT NULL DEFAULT 0,
    held_first_due  TIMESTAMPTZ,
    held_last_due   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ,
    CHECK ((kind = 'cron') = (cron IS NOT NULL)),
    CHECK ((held_count = 0) = (held_first_due IS NULL AND held_last_due IS NULL))
);

CREATE INDEX schedule_open_by_child ON conversations.schedule (child_id)
    WHERE closed_at IS NULL;

-- One pending wakeup per child, enforced by the database.
CREATE UNIQUE INDEX schedule_one_open_wakeup ON conversations.schedule (child_id)
    WHERE kind = 'wakeup' AND closed_at IS NULL;

-- NULL means unset (the daemon's zone applies), never ''.
ALTER TABLE conversations.users ADD COLUMN timezone TEXT CHECK (timezone <> '');
