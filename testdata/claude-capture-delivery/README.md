# A captured Claude Code session: message delivery

One real Claude Code session recorded through `rafikid proxy record` — the raw
HTTP exchanges between Claude Code and the Anthropic API, as
`NNNNNN-request.http` / `NNNNNN-response.http` pairs in arrival order.

It is the evidence for the delivery acknowledgement
(`docs/reference/control-protocol.md`, "`message_delivered`: a UI acceptance
receipt"), because the acknowledgement's premise is a claim about what a
captured request actually contains:

- a message sent **while a turn is running** does not appear in the next
  request as the user typed it. Claude Code folds it into the running turn
  wrapped in its own prose — "The user sent a new message while you were
  working: …" — **inside the next tool result**. Request 2 carries it; requests
  3 and 4 keep it as history;
- **two deliberate sends of the same text** are two messages. Requests 3 and 4
  each add one `Reply with exactly one word: twice` user message, and nothing
  deduplicates them: the uuid, not the text, is the identity.

`TestDeliveryCorpusKeepsTheMidTurnWrapperAndBothIdenticalSends` (`pkg/capture`)
replays it through capture and asserts both, on the stored tree.

## Provenance

| | |
|---|---|
| Recorded | 2026-11-05 |
| Claude Code | **2.1.290** (`claude --version`) |
| Model | `haiku` (`claude-haiku-4-5-20251001`) |
| Recorder | `rafikid proxy record --listen 127.0.0.1:0 --upstream https://api.anthropic.com --record-dir testdata/claude-capture-delivery --exec …` |
| Driver | `test/integration/fixtures/claudedriver` — it writes each turn with its own uuid, sends one message MID-TURN (on the tool call, while `sleep 3` runs), then two identical sends. `rafiki claude --turn-file` cannot produce this: it sends its turns strictly one after another. |

The session was:

```
turn 1   Use the Bash tool to run `sleep 3`. Then reply with exactly one word: ready
mid-turn Reply with exactly one word: also          (sent while turn 1's tool call ran)
turn 3   Reply with exactly one word: twice
turn 4   Reply with exactly one word: twice         (the same text, a new uuid)
```

Prompts were dull on purpose: they are recorded verbatim.

## What was scrubbed, and why

**Values only.** Every substitution below replaces a value and nothing else: the
request bodies keep the recording's structure, block counts, key order and
ordering, because the tests parse them and the delivery shapes are the point.
This follows the same rule `pkg/child/testdata/claude/NOTES.md` documents for
its own recordings, and the stdout half of this session
(`pkg/child/testdata/claude/delivery_echo.jsonl`) is **verbatim** — nothing in
it was touched, because those four lines carry no personal data to remove.

| Field | Placeholder |
|---|---|
| the `SessionStart` hook's `additionalContext` (first text block of the first user message) | an `<example-context>` stub |
| the "available agent types" reminder block | the Claude Code builtins only |
| the "skills available" reminder block | two `example-skill-N` entries |
| the "Codebase and user instructions" block (the operator's global `CLAUDE.md`) | `<example-instructions>` |
| the `userEmail` reminder block | `user@example.com` |
| the auto-memory path inside the system prompt | `/home/user/.config/example/…` |

What is NOT scrubbed, and is safe to commit: the prompts and completions
(deliberately dull), Claude Code's own tool schemas and system prompt,
`metadata.user_id` (already digested to a sha256 by the recorder, as are
`Authorization` and `X-Api-Key`), session uuids and timestamps (capture
artefacts).

`MCP servers were disabled for the recording` (`--strict-mcp-config
--mcp-config '{"mcpServers":{}}'`): the operator's MCP inventory is personal,
and it is 90 tool schemas that have nothing to do with message delivery.

## Regenerating it

```sh
go build -o bin/rafikid ./cmd/rafikid
go build -o /tmp/claudedriver ./test/integration/fixtures/claudedriver
cat > /tmp/inner.sh <<'EOF'
#!/bin/sh
cd /tmp && exec env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN /tmp/claudedriver \
  --claude "$(command -v claude)" --model haiku --timeout 240s \
  --extra '--strict-mcp-config --mcp-config {"mcpServers":{}}'
EOF
chmod 0700 /tmp/inner.sh
bin/rafikid proxy record --listen 127.0.0.1:0 --upstream https://api.anthropic.com \
  --record-dir testdata/claude-capture-delivery --exec /tmp/inner.sh
```

**Scrub it again afterwards** — the recording is made with a real Claude Code
login and a real config directory, so the hook, skills, agent and `CLAUDE.md`
reminder blocks all come back. The table above is the complete list; check the
result with:

```sh
grep -o "/Users/[^\"']*\|@[a-z]*\.[a-z]*\.[a-z]*" testdata/claude-capture-delivery/*-request.http | sort -u
```

The matching stdout half of the same session — the four acceptance echoes — is
`pkg/child/testdata/claude/delivery_echo.jsonl`.
