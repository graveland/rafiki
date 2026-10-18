-- Skills carry their supporting files, stored content-addressed, and a skill
-- row is unique within a LAYER rather than across all of them: core, global
-- (owner NULL) and user (owner set) each get one row per (namespace, name),
-- which is what lets a user override a core skill without disabling it first.
--
-- The duplicate check runs before the layer index is created. A database that
-- already holds two rows in one layer cannot be made unique, and a bare index
-- failure names neither the skill nor the fix.
DO $$
DECLARE dup text;
BEGIN
  SELECT string_agg(namespace || ':' || name, ', ') INTO dup FROM (
    SELECT namespace, name FROM conversations.skills
    GROUP BY namespace, name, owner_user_id, (source = 'rafiki-core')
    HAVING count(*) > 1) d;
  IF dup IS NOT NULL THEN
    RAISE EXCEPTION 'skills held twice in one layer: %; remove one of each with "rafiki skills rm" before upgrading', dup;
  END IF;
END $$;

CREATE TABLE conversations.skill_blob (
    sha256  bytea PRIMARY KEY CHECK (length(sha256) = 32),
    content bytea NOT NULL
);
CREATE TABLE conversations.skill_file (
    skill_id uuid NOT NULL REFERENCES conversations.skills(id) ON DELETE CASCADE,
    path     text NOT NULL,
    sha256   bytea NOT NULL REFERENCES conversations.skill_blob(sha256),
    mode     smallint NOT NULL CHECK (mode IN (420, 493)),
    PRIMARY KEY (skill_id, path)
);
CREATE INDEX skill_file_sha256 ON conversations.skill_file (sha256);

DROP INDEX conversations.skills_name_active;
ALTER TABLE conversations.skills DROP COLUMN shadowed_core_version;
CREATE UNIQUE INDEX skills_layer_key
    ON conversations.skills (namespace, name, owner_user_id, (source = 'rafiki-core'))
    NULLS NOT DISTINCT;
