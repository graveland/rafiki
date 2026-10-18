-- Reverses 0053. Recreating skills_name_active can fail if the database holds
-- two ENABLED rows under one (namespace, name) in different layers — a state
-- the layer key permits and this index does not.
DROP INDEX conversations.skills_layer_key;

ALTER TABLE conversations.skills ADD COLUMN shadowed_core_version TEXT NULL;

CREATE UNIQUE INDEX skills_name_active
    ON conversations.skills (namespace, name) WHERE enabled;

DROP TABLE conversations.skill_file;
DROP TABLE conversations.skill_blob;
