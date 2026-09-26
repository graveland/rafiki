-- One row per parked first call of a :batch model (pkg/batch). Tombstoned,
-- never deleted: a failed row is tombstoned and a fresh row inserted when the
-- same call re-parks, so custom_id is unique only among live rows.
--
-- request/response are TEXT, not JSONB: the conformance contract (pkg/batch
-- batchtest) is byte-exact round-tripping of the caller's JSON, and JSONB
-- normalises keys and whitespace. Same guarantee as conversations.child.result.
CREATE TABLE conversations.batch_call (
	id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	custom_id         text        NOT NULL,
	model             text        NOT NULL,
	state             text        NOT NULL CHECK (state IN ('queued','submitting','submitted','completed','failed')),
	provider_batch_id text        NULL,
	request           text        NOT NULL,
	response          text        NULL,
	error             text        NULL,
	created_at        timestamptz NOT NULL DEFAULT now(),
	updated_at        timestamptz NOT NULL DEFAULT now(),
	deleted_at        timestamptz NULL
);
CREATE UNIQUE INDEX batch_call_custom_id_live ON conversations.batch_call (custom_id) WHERE deleted_at IS NULL;
CREATE INDEX batch_call_pending ON conversations.batch_call (state)
	WHERE deleted_at IS NULL AND state IN ('queued','submitting','submitted');
