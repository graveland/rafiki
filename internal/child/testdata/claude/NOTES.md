# Claude stream-json capture notes

Captured 2026-06-10 from Claude Code **2.1.172** via:

```
printf '%s\n' '{"type":"user","message":{"role":"user","content":"..."}}' \
  | ~/.local/bin/claude -p --input-format stream-json --output-format stream-json \
      --verbose --dangerously-skip-permissions
```

Two fixtures: `startup_and_turn.jsonl` (text-only turn) and `turn_with_tool.jsonl`
(one Bash tool call). These drive `provider_claude_test.go`.

## Answers to the spike questions

1. **Does `system/init` appear before any user message is sent?**
   It appears at process startup, *after* the config dir's hooks run. The first
   lines are `{"type":"system","subtype":"hook_started",...}` and
   `{"type":"system","subtype":"hook_response",...}` (these come from the user's
   `CLAUDE_CONFIG_DIR` hooks), then `{"type":"system","subtype":"init",...}`.
   → `ClaudeProvider.Parse` must gate `FirstResponse` on `type=="system" &&
   subtype=="init"` specifically — **not** any `system` line. (Plan 1 does this.)

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

## Other observed frame types (must be ignored, never a state transition)

- `system/hook_started`, `system/hook_response` — from the config dir's hooks.
- `rate_limit_event` — interleaved telemetry.

`ClaudeProvider.Parse`'s `switch f.Type` has no case for these, so they return a
zero `ParseResult` (no-op). Confirmed correct against both fixtures.

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

- **Plan 1 `ClaudeProvider` needs no change** — the assumed shapes match. The
  golden-transcript test's "exactly 1 FirstResponse" holds (one `init` line).
- **Plan 3 `shapeTranscript`**: a `thinking`-only assistant frame yields an empty
  `turnView`. Skip turns with empty `Text` and no `ToolCalls` to avoid blank
  transcript entries.
