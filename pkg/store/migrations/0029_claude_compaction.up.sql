-- Claude Code compaction support: an append-only rebase + resume horizon.
-- See docs/plans/2026-09-09-claude-compaction-design.md.
--
-- resume_from_ordinal is the conversation's horizon: the ordinal at and after
-- which the current working context begins. NULL means 0 (full replay) --
-- correct for every conversation that predates this column. Only the
-- claude-compaction boundary-write path ever sets it non-zero (see the design
-- doc §5 for why no other path may).
ALTER TABLE conversations.conversation ADD COLUMN resume_from_ordinal INT;

-- kind tags a conversation_message row as something other than an ordinary
-- turn message. The only value written today is 'compaction_summary', on the
-- one row inserted at a rebase boundary (its role is 'user' -- it is Claude
-- Code's own summary message, kept API-valid for a future replay). NULL on
-- every ordinary row; no default is set.
ALTER TABLE conversations.conversation_message ADD COLUMN kind TEXT;
