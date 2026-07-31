# Claude stream-json capture notes

Captured 2026-06-10 from Claude Code **2.1.172** via:

```
printf '%s\n' '{"type":"user","message":{"role":"user","content":"..."}}' \
  | claude -p --input-format stream-json --output-format stream-json \
      --verbose --dangerously-skip-permissions
```

Two fixtures: `startup_and_turn.jsonl` (text-only turn) and `turn_with_tool.jsonl`
(one Bash tool call). These drive `provider_claude_test.go`.

**The committed fixtures are scrubbed** — see "Scrubbing" at the bottom before you
regenerate them.

## Answers to the spike questions

1. **Does `system/init` appear before any user message is sent?**
   **No — the original spike answer here was WRONG, and so was a first patch that
   tried to read the hooks.** The capture command above *pipes a user message into
   stdin* (`printf ... | claude`), so the message was available immediately and the
   whole stream (hooks, `init`, the turn) appeared as part of processing it. That
   capture could not observe the truly un-prompted case. Verified directly against
   `claude 2.1.172` with **stdin held open and no input**: claude emits **ZERO bytes**
   on stdout (and stderr) — no hooks, no `init`, nothing — until the first user
   message arrives. (The "only hooks at startup" observation came from an *already
   steered* child, i.e. after input.) In the controller's real spawn, stdin is open
   but empty (the prompt is sent later via `subagent_send`), so the child is silent.
   → There is **no stdout signal** that can drive `spawning→idle`. Gating
   `FirstResponse` on `subtype=="init"` (or on any `system` frame) left a freshly-
   spawned, un-prompted child stuck in `spawning` forever (its `Idle()` never closed
   → `activateLiveChild` timed out → stalled → `subagent_send`, idle-gated, rejected).
   The fix is **process-up readiness**: `ClaudeProvider.ReadyOnSpawn()` returns true
   and the `Child` fires `spawning→idle` the instant the process launches (claude
   buffers stdin, so accepting a send immediately is safe; hooks still run when the
   message is processed). `Parse` keeps `FirstResponse` on `init` only — it no longer
   drives initial readiness, but the `init` line still carries the session id/model.

2. **Session id path:** top-level `session_id` on `system/init`, on every
   `assistant`/`user` frame, and on `result`. (Plan 1 reads it from init + result.)

3. **Model path on init:** top-level `model` (string). Observed value:
   `"claude-fable-5[1m]"`. Stored verbatim as `SnifferMetadata.Model`.

4. **`assistant` shape:** `message.content` is an array of typed blocks:
   `{"type":"thinking"|"text"|"tool_use", ...}`. `text` blocks carry `text`;
   `tool_use` blocks carry `name` (e.g. `"Bash"`), `id` (`toolu_...`), `input`,
   `caller`. A turn can begin with a `thinking`-only assistant frame (no text).

5. **`tool_result` shape:** `{"type":"user","message":{"content":[{"type":
   "tool_result","tool_use_id":"toolu_...","content":...,"is_error":bool}]}}`.

6. **`result` shape:** `{"type":"result","subtype":"success","session_id":...,
   "is_error":false,...}` (also `total_cost_usd`, `usage`, `stop_reason`, etc.).
   Marks turn completion → `agent_end` → idle.

7. **Input envelope that worked:** `{"type":"user","message":{"role":"user",
   "content":"<plain string>"}}` — **string content is accepted** (no block array
   needed). `ClaudeProvider.EncodeOutbound` emits exactly this shape.

## Other observed frame types

- `system/hook_started`, `system/hook_response` — from whatever `SessionStart`
  hooks the capturing machine's config dir has installed.
  These appear **only after** a turn begins (input arrived), interleaved with the
  turn — NOT un-prompted (see spike answer #1). `Parse` ignores them (no case);
  readiness is process-up, not derived from these.
- `rate_limit_event` — interleaved telemetry. `Parse`'s `switch f.Type` has no
  case for it, so it returns a zero `ParseResult` (no-op).

The fixtures (`startup_and_turn.jsonl`, `turn_with_tool.jsonl`) were captured with
a piped prompt, so they show hooks→init→turn back-to-back. A real un-prompted spawn
shows **none** of this until the first `subagent_send`.

## Process lifecycle

The process **exits cleanly (code 0) on stdin EOF** after the turn completes. The
daemon keeps the child's stdin open, so a controller-driven child stays alive
across turns and exits only when stdin is closed (Shutdown) — matching pi.

## Full sequences

- `startup_and_turn.jsonl`: hook_started, hook_response, init, assistant(thinking),
  assistant(text "pong"), result(success). 6 lines.
- `turn_with_tool.jsonl`: hook_started, hook_response, init, assistant(tool_use Bash),
  rate_limit_event, user(tool_result), assistant(text), result(success). 8 lines.

## Plan refinements surfaced by this capture (apply during implementation)

- **Plan 1 `ClaudeProvider` readiness was wrong** (see spike answer #1, corrected).
  claude emits nothing on stdout un-prompted, so initial readiness is **process-up**
  (`ReadyOnSpawn()` → the `Child` fires `spawning→idle` on launch), not derived from
  any stdout frame. `Parse` still maps `init`→session-id/model and `result`→agent_end.
  The golden-transcript test keeps "exactly 1 FirstResponse" (the one `init` line);
  the live readiness path is covered by `TestClaudeChild_IdleOnSpawnWhenSilent` (a
  fake-claude that is silent until input).
- **Plan 3 `shapeTranscript`**: a `thinking`-only assistant frame yields an empty
  `turnView`. Skip turns with empty `Text` and no `ToolCalls` to avoid blank
  transcript entries.

## Scrubbing

A raw capture describes the capturing machine in detail, so the committed
fixtures have every environment-identifying **value** replaced with a neutral
placeholder. Only values changed — key order, nesting, types and array lengths
are byte-for-byte those of the real capture, because the tests parse these files
and assert on their shape.

What was replaced, and with what:

| Field | Placeholder |
|---|---|
| `system/init.cwd` | `/home/user/project` |
| `system/init.tools[]` (`mcp__*` entries only) | `mcp__example-mcp-N__tool_NN` |
| `system/init.mcp_servers[].name` | `example-mcp-N`, `plugin:example-plugin-N:example-mcp-N` |
| `system/init.plugins[].{name,path,source}` | `example-plugin-N` under `/home/user/.config/example/plugins/…` |
| `system/init.{skills,slash_commands}[]` | `example-skill-N`, `example-plugin-N:skill-N` (Claude Code builtins left alone) |
| `system/init.agents[]` | `example-agent-N` (builtins left alone) |
| `system/init.memory_paths.auto` | `/home/user/.config/example/projects/-home-user-project/memory/` |
| `hook_response.{output,stdout}` | a stub `SessionStart` `hookSpecificOutput` payload |
| the Bash `tool_result` content | a generic Go project `ls` listing |

Session ids, uuids, request ids, token counts, costs and the thinking-block
signature are capture artefacts, not environment data, and were left verbatim.

**If you regenerate these fixtures, scrub them again.** Two constraints are easy
to miss:

- `tools[]` is sorted, and `-` sorts before `_`, so placeholder MCP tool names
  need zero-padded indices (`tool_01`, not `tool_1`) or the list stops being
  plausibly sorted.
- `TestClaudeBusFrames_ToolTurn` asserts the tool output contains `go.mod`, so
  whatever you substitute for the `ls` result must keep it.
