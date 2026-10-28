-- Validates the three constraints 0057 added NOT VALID, each in its own
-- statement so a failure names the one that could not be validated. A scan
-- here instead of in 0057 keeps the schema-changing migration short; the
-- constraints were already enforced for new rows, so this only audits the
-- rows that predate them.
ALTER TABLE conversations.conversation_message VALIDATE CONSTRAINT conversation_message_parent_fk;
ALTER TABLE conversations.conversation_message VALIDATE CONSTRAINT conversation_message_derived_fk;
ALTER TABLE conversations.conversation_message VALIDATE CONSTRAINT conversation_message_hash_pair;
