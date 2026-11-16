-- Executors installed over ssh through a launcher executor. Every
-- access-gating fact is written by the daemon; nothing the target reports
-- gates access. Tombstoned with removed_at, never DELETE'd. executor_id and
-- launcher_executor_id carry no FK because executor rows hard-delete.
CREATE TABLE conversations.executor_install (
    id                   text        PRIMARY KEY,
    owner_user_id        text        NOT NULL DEFAULT '',
    executor_id          text        NOT NULL,
    launcher_executor_id text        NOT NULL,
    target               text        NOT NULL,
    instance             text        NOT NULL,
    root                 text        NOT NULL DEFAULT '',
    installed_by_child   text        NOT NULL DEFAULT '',
    platform             text        NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL DEFAULT now(),
    removed_at           timestamptz NULL
);
CREATE UNIQUE INDEX executor_install_executor_live
    ON conversations.executor_install (executor_id) WHERE removed_at IS NULL;
CREATE INDEX executor_install_owner_live
    ON conversations.executor_install (owner_user_id) WHERE removed_at IS NULL;
