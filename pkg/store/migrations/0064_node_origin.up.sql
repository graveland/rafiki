-- origin is the harness that wrote a message row (pkg/conv Origin):
-- NULL = unclassified, 'claude-code' = captured by the Anthropic proxy face,
-- 'fundi' = written by pkg/llm. It is outside the path hash: identity does
-- not change.
ALTER TABLE conversations.conversation_message ADD COLUMN origin TEXT;

-- Only the Anthropic proxy face writes nodes into client-driven
-- conversations, so every one of them is Claude Code's.
UPDATE conversations.conversation_message m
   SET origin = 'claude-code'
  FROM conversations.conversation c
 WHERE m.conversation_id = c.id
   AND c.driven_by = 'client';

ALTER TABLE conversations.conversation_message
  ADD CONSTRAINT conversation_message_origin_check
  CHECK (origin IN ('claude-code', 'fundi'));
