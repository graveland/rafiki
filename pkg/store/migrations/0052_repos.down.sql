ALTER TABLE conversations.repos
    DROP COLUMN IF EXISTS refresh_interval,
    DROP COLUMN IF EXISTS commit_sha,
    DROP COLUMN IF EXISTS resolved_at;
ALTER INDEX conversations.repos_owner_name_key RENAME TO pymodule_git_sources_owner_name_key;
ALTER TABLE conversations.repos RENAME TO pymodule_git_sources;
