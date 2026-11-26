-- 0077 backfills a conversations.child row for every Claude Code Task subagent
-- thread that was captured before the daemon persisted its synthetic children.
--
-- A Task subagent is a real conversation with a real cost that rafiki already
-- modelled as a child in memory (EnsureThreadChild), but the row was missing:
-- the child vanished on every daemon restart, and its lineage disappeared with
-- its parent. From this migration on the daemon writes the row at birth; this
-- is the corpus that predates that.
--
-- WHAT IS A SUBAGENT THREAD, exactly? A conversation whose external_ref is
-- '<parent child id>:<thread id>' and whose turns declare at least one CLIENT
-- tool — an entry in the request's `tools` array carrying an `input_schema`.
-- That is claudethread.DeclaresClientTools' rule, and the predicate below is a
-- term-for-term translation of it:
--
--   * jsonb_typeof(tools) = 'array'          — Go unmarshals into []struct, so
--                                              any other shape fails there too;
--   * every element is an object or null     — a string/number/array element
--                                              makes that unmarshal fail;
--   * some element has 'input_schema' and its
--     value is not JSON null                 — len > 0 && != "null" in Go.
--
-- The rule matters because cc_is_subagent alone is NOT "Task subagent": Claude
-- Code stamps it on the per-tool-call haiku one-shots it fires to summarise a
-- WebFetch page and drive a WebSearch. Those correctly fork their own thread,
-- but they carry no client tools (the WebSearch one carries a SERVER tool), so
-- they are not agents and get no row here either.
--
-- Status: a thread of a LIVE parent is a thread that is still running, so it
-- loads idle; a thread of a parent that is gone ended with it, so it loads
-- exited. The live set is spelled out rather than derived, because "not
-- exited" would also bless the empty status of a degenerate row.
--
-- pid stays NULL: a native child has no process of its own (it is a thread
-- inside the parent's), and every NOT NULL column the table has is either
-- supplied here or carries a default.
--
-- ROLLING BACK: this row shape is understood by the daemon that has a native
-- branch in recovery, and by nothing else. An OLDER binary resumes every claude
-- row whose status is not exited, so starting one against a database that has
-- run this migration launches a `claude --resume` per idle native row, using the
-- thread's branch conversation uuid as the session. Migrate does not refuse a
-- database ahead of the binary, so apply the down migration BEFORE starting an
-- older binary (docs/MIGRATING.md, "Rolling the binary back after 0077").

INSERT INTO conversations.child (
    child_id, conversation_id, owner_user_id, kind, name, status, spawned_at,
    provider, model, thinking, labels, closed_at)
SELECT c.external_ref,
       c.id,
       p.owner_user_id,
       'claude',
       'task:' || split_part(c.external_ref, ':', 2),
       CASE WHEN p.status IN ('spawning', 'idle', 'streaming', 'running',
                              'tool_running', 'compacting', 'batch_wait', 'blocked_ui')
            THEN 'idle' ELSE 'exited' END,
       c.created_at,
       p.provider,
       p.model,
       p.thinking,
       jsonb_build_object(
           'rafiki/parent', p.child_id,
           'rafiki/root', coalesce(p.root_id, p.child_id),
           'rafiki/native-subagent', '1'),
       p.closed_at
  FROM conversations.conversation c
  JOIN conversations.child p
    ON p.child_id = split_part(c.external_ref, ':', 1)
 WHERE c.external_ref LIKE '%:%'
   AND c.external_ref <> p.child_id
   -- EXACTLY TWO SEGMENTS: a thread ref is '<child id>:<thread id>'. A deeper
   -- ref ('root:t1:t2') is not a thread of the child its first segment names,
   -- and taking it would create a row with the wrong parent and a
   -- 'task:<second segment>' name. The daemon's refs are built from real child
   -- ids, so this is a guard against hand-built or foreign refs, not a shape
   -- the proxy produces.
   AND split_part(c.external_ref, ':', 3) = ''
   AND EXISTS (
       SELECT 1
         FROM conversations.conversation_turn t
        WHERE t.conversation_id = c.id
          AND jsonb_typeof(t.prefix_content->'tools') = 'array'
          AND EXISTS (
              SELECT 1
                FROM jsonb_array_elements(t.prefix_content->'tools') AS e
               WHERE e ? 'input_schema'
                 AND e->'input_schema' <> 'null'::jsonb)
          AND NOT EXISTS (
              SELECT 1
                FROM jsonb_array_elements(t.prefix_content->'tools') AS e
               WHERE jsonb_typeof(e) NOT IN ('object', 'null')))
ON CONFLICT (child_id) DO NOTHING;
