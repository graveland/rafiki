-- Git-sourced pymodule scopes: owner-scoped (url, ref) pointers to external
-- git content that the pymodule tool surface addresses as a `repo` scope.
-- Deliberately a separate table from conversations.pymodules: a git source
-- has no Code at all, and there is no versioning here -- a registration is a
-- pointer to repoint, so Put for an existing (owner_user_id, name) updates
-- url/ref in place rather than appending a row. NULL owner means
-- unattributed, same convention as conversations.pymodules.
CREATE TABLE conversations.pymodule_git_sources (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_user_id  uuid NULL REFERENCES conversations.users(id),
    name           text NOT NULL,
    url            text NOT NULL,
    ref            text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Unique per (owner, name), and NULLS NOT DISTINCT so the unattributed
-- bucket (owner_user_id NULL) is one identity: SQL's default treats NULLs
-- as distinct, which would let two unattributed rows for one name coexist
-- and Put's ON CONFLICT would never fire for them.
CREATE UNIQUE INDEX pymodule_git_sources_owner_name_key
    ON conversations.pymodule_git_sources (owner_user_id, name) NULLS NOT DISTINCT;
