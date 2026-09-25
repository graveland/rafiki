-- RESERVED for 0038_operator_provider_ban (branch :batch), which claimed the
-- number while this branch was in flight: the shared databases recorded
-- version 38 under that name first. Migrate skips by NUMBER, not name, so a
-- second 0038 here would be silently skipped on every database that already
-- ran :batch's — and would duplicate 0038 in the merged chain, which
-- loadMigrations refuses. This no-op keeps this branch's chain contiguous
-- 1..N until the merge, where it is simply dropped in favour of :batch's
-- file. See 0039_child_result for this branch's actual migration.
SELECT 1;
