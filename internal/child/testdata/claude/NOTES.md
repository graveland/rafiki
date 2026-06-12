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
   **No — and the original spike answer here was WRONG.** The capture command
   above *pipes a user message into stdin* (`printf ... | claude`), so the message
   was available immediately and `init` appeared as part of processing it. That
   capture could not observe the truly un-prompted case. In the controller's real
   spawn, stdin is open but **empty** (the prompt is sent later via `subagent_send`):
   claude runs the `CLAUDE_CONFIG_DIR` hooks, emits only
   `{"type":"system","subtype":"hook_started",...}` /
   `{"type":"system","subtype":"hook_response",...}`, then **waits silently for
   input**. `system/init` does NOT arrive until the first turn begins.
   → Gating `FirstResponse` on `subtype=="init"` left a freshly-spawned, un-prompted
   child stuck in `spawning` forever (its `Idle()` never closed → `activateLiveChild`
   timed out → stalled → `subagent_send`, idle-gated, rejected). `ClaudeProvider.Parse`
   now derives readiness from the **first `system` frame of any subtype** (the
   SessionStart hook lifecycle); claude buffers stdin so accepting a send right after
   is safe. Once-ness is enforced downstream (`idleOnce`/`OnFirstResponse`).

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

- `system/hook_started`, `system/hook_response` — from the config dir's hooks.
  These are the **only** stdout an un-prompted child emits, so they are the
  readiness signal (see spike answer #1): the `system` case in `Parse` fires
  `FirstResponse` for any subtype, including these. (They carry `session_id` but
  no `model`; the resolved model still comes from the later `init`.)
- `rate_limit_event` — interleaved telemetry. `Parse`'s `switch f.Type` has no
  case for it, so it returns a zero `ParseResult` (no-op).

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
  Readiness must derive from the first `system` frame (the SessionStart hook
  lifecycle), not `init` — `init` is not emitted un-prompted. The golden-transcript
  test no longer asserts "exactly 1 FirstResponse" (these piped-stdin fixtures
  contain `init`, but a real un-prompted spawn does not); it now asserts readiness
  fires on the first `system` frame and only on `system` frames.
- **Plan 3 `shapeTranscript`**: a `thinking`-only assistant frame yields an empty
  `turnView`. Skip turns with empty `Text` and no `ToolCalls` to avoid blank
  transcript entries.
