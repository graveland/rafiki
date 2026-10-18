-- Repos: the pymodule git-source table under its own name. A repo is still
-- an owner-scoped (url, ref) pointer to external git content, so the rename
-- carries the registration semantics unchanged. The three added columns are
-- the resolved-commit state a refresh writes: refresh_interval opts a repo
-- into periodic refresh (never on by default, one-minute floor), commit_sha
-- is the resolved tip a refresh fanned out, and resolved_at says when.
ALTER TABLE conversations.pymodule_git_sources RENAME TO repos;
ALTER INDEX conversations.pymodule_git_sources_owner_name_key RENAME TO repos_owner_name_key;
ALTER TABLE conversations.repos
    ADD COLUMN refresh_interval interval NULL
        CHECK (refresh_interval IS NULL OR refresh_interval >= interval '1 minute'),
    ADD COLUMN commit_sha  text NULL,
    ADD COLUMN resolved_at timestamptz NULL;
