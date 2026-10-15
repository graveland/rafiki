-- Operator attention state on the child row. Both columns hold the daemon's
-- last observation of an in-flight turn; NULL means unset and both are cleared
-- when the next turn starts, so a value here always describes the turn that is
-- current or just ended, never a stale one.
--
-- pending_question is set by a top-level agent_report kind=question: the
-- operator-facing question the child is blocked on.
-- last_turn_error is written from fundi.TurnOutcome at settle: why the child's
-- last turn failed, empty when it did not.
ALTER TABLE conversations.child ADD COLUMN pending_question TEXT;
ALTER TABLE conversations.child ADD COLUMN last_turn_error TEXT;
