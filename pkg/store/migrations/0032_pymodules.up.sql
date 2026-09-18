-- Reusable Python snippets, owner-scoped: "my scripts are mine, not other
-- users." Versioning is the identity column itself — nothing is ever
-- updated or deleted, a new save is a new row, and "latest" is MAX(id) per
-- (owner_user_id, name). NULL owner means unattributed (matches
-- pkg/childstore/pkg/capture's OwnerUserID-empty convention), never
-- "global" the way it does on conversations.skills.
CREATE TABLE conversations.pymodules (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_user_id  uuid NULL REFERENCES conversations.users(id),
    name           text NOT NULL,
    code           text NOT NULL,
    description    text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX pymodules_owner_name_id_idx
    ON conversations.pymodules (owner_user_id, name, id DESC);
