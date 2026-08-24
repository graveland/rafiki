-- The executor's name is now the operator-written `machine` trust label, not a
-- separate display_name column. One field: display_name was never written by
-- any enrollment path (neither ctrl_executor_enroll nor ctrl_executor_create
-- carried a name), so it was the empty string for every durable executor in
-- existence, while `machine` is the value selection actually matches on.
ALTER TABLE conversations.executors DROP COLUMN IF EXISTS display_name;

-- A name identifies a machine to its OWNER, so uniqueness is scoped to the
-- owner: two operators may each have a `laptop`. Partial, because the name is
-- optional -- a fleet executor selected purely by env=prod needs none, and a
-- k8s Deployment's replicas have no shared filesystem to advertise.
--
-- Same shape as users_username_active: uniqueness over the rows for which the
-- key is meaningful, rather than a table-wide constraint that would forbid the
-- ordinary absent case.
CREATE UNIQUE INDEX IF NOT EXISTS executors_owner_machine_unique
    ON conversations.executors ((labels->>'owner'), (labels->>'machine'))
    WHERE labels ? 'machine';
