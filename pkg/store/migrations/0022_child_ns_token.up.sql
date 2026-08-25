-- The PID namespace a child's recorded pid belongs to.
--
-- daemon_id cannot answer this: it is pinned across pod restarts on purpose, so
-- a restarted pod has the same daemon id and a fresh PID namespace in which
-- every recorded pid names an unrelated process.
ALTER TABLE conversations.child ADD COLUMN ns_token TEXT;