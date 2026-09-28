-- The parked call's provider pin: the batch envelope's narrowed provider
-- object ({"only": [...]}) this row parked with, NULL when it carried none.
-- pkg/batch's Batcher groups queued rows by (model, provider) and submits the
-- object at the envelope's TOP level; without the column the production store
-- dropped the only-list on the first round-trip (a silent fallback to
-- unrouted). Never updated: it is read back at submit time.
ALTER TABLE conversations.batch_call ADD COLUMN provider JSONB NULL;
