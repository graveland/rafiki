-- The transport an installed executor uses to reach the daemon: 'tunnel' is
-- the ssh stdio relay the launcher holds open (every existing row), 'direct'
-- is a detached process on the target that dials the daemon itself. Chosen once
-- at install and never re-probed on relaunch, so the default keeps every
-- pre-existing row on today's behaviour.
ALTER TABLE conversations.executor_install
    ADD COLUMN mode text NOT NULL DEFAULT 'tunnel'
    CHECK (mode IN ('direct', 'tunnel'));
