-- Per-owner model aliases: a slash-free name standing for a full model string
-- (provider/id, optionally with a [routing spec]). One value per (owner, name):
-- a put updates in place and a delete deletes -- no history is kept, by
-- design. owner_user_id NULL is the unattributed (local-socket) bucket.
CREATE TABLE conversations.model_alias (
    owner_user_id uuid        NULL REFERENCES conversations.users(id),
    name          text        NOT NULL,
    target        text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX model_alias_owner_name
    ON conversations.model_alias (owner_user_id, name) NULLS NOT DISTINCT;
