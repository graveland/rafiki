-- The conversation tree: a message row gains a parent, a content-addressed
-- path hash and a depth, so a conversation is a tree of nodes addressed by
-- (conversation_id, path_hash) instead of a linear ordinal sequence. The
-- ordinal stays — it is the insertion sequence, gap-free and never reused —
-- but it no longer identifies a position in the history.
--
-- Every column is nullable: rows written before this migration are legacy
-- linear rows with no tree position (path_hash IS NULL), and the hash_version
-- column is what records which hash algorithm produced a path_hash. The pair
-- moves together, hence the CHECK.
--
-- The FKs are added NOT VALID so this migration does not scan the message
-- table; 0058 validates them in its own transaction. ON DELETE RESTRICT is
-- deliberate — a node with children (or a head) cannot be deleted, and
-- messages are never deleted anyway.
ALTER TABLE conversations.conversation_message
  ADD COLUMN parent_id           uuid,
  ADD COLUMN derived_from_id     uuid,
  ADD COLUMN path_hash           bytea,
  ADD COLUMN hash_version        smallint,
  ADD COLUMN depth               integer,
  ADD COLUMN provider_message_id text;

ALTER TABLE conversations.conversation_message
  ADD CONSTRAINT conversation_message_parent_fk
    FOREIGN KEY (parent_id) REFERENCES conversations.conversation_message(id) ON DELETE RESTRICT NOT VALID,
  ADD CONSTRAINT conversation_message_derived_fk
    FOREIGN KEY (derived_from_id) REFERENCES conversations.conversation_message(id) ON DELETE RESTRICT NOT VALID,
  ADD CONSTRAINT conversation_message_hash_pair
    CHECK ((path_hash IS NULL) = (hash_version IS NULL)) NOT VALID;

-- The path index is UNIQUE but not partial: NULL path_hash values are distinct
-- in a unique index, so legacy rows coexist, while two nodes of one
-- conversation can never claim the same path.
CREATE INDEX conversation_message_parent_idx  ON conversations.conversation_message (parent_id)       WHERE parent_id IS NOT NULL;
CREATE INDEX conversation_message_derived_idx ON conversations.conversation_message (derived_from_id) WHERE derived_from_id IS NOT NULL;
-- capture looks a previous_message_id up within ONE conversation, so the
-- conversation is part of the key.
CREATE INDEX conversation_message_provider_idx
  ON conversations.conversation_message (conversation_id, provider_message_id) WHERE provider_message_id IS NOT NULL;
CREATE UNIQUE INDEX conversation_message_path_uniq
  ON conversations.conversation_message (conversation_id, path_hash);

-- head_id is the last node on the working path; next_ordinal is the allocator
-- the append path bumps under this row's lock, so ordinals are gap-free even
-- when two appends race.
ALTER TABLE conversations.conversation
  ADD COLUMN head_id      uuid REFERENCES conversations.conversation_message(id) ON DELETE RESTRICT,
  ADD COLUMN next_ordinal integer NOT NULL DEFAULT 0;

-- The head each turn started from, so a fork can be attributed to the turn
-- that made it. No FK: conversation_turn is a hypertable with columnstore,
-- which rejects ADD COLUMN ... REFERENCES (0030 hit the same wall), and every
-- row written before 0057 has NULL here anyway.
ALTER TABLE conversations.conversation_turn
  ADD COLUMN request_head_id uuid;
