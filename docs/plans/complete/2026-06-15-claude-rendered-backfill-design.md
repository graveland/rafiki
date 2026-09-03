# Claude rendered backfill + slash_commands completion — design

Date: 2026-06-15
Status: approved (design)

## Problem

`pic logs`/`pic tail` and `pic attach` scrollback show **raw claude JSON**, not the
rendered conversation view, for claude-backed children. Root cause (verified
against the code):

- The per-child **out-ring** (`internal/child/child.go`) stores the **raw backend
  stdout** verbatim. For a **pi** child that stdout is already pi-vocabulary
  (`message_start`/`message_end`/`agent_*`/`tool_execution_*`) — identical to what
  the renderer and TUI consume. For a **claude** child it is claude's native
  stream-json (`system`/`assistant`/`result`/…).
- The **bus** (live fan-out to `pic tail` / the TUI) carries pi-vocabulary. For
  claude that is produced live by a **stateful, order-sensitive translator**:
  `claudeProvider.BusFrames` (stdout → pi-vocab) **and** `OutboundEcho` (synthesizes
  the user turn from stdin, since claude never echoes the prompt on stdout). The
  bus stream is **never persisted**.
- Backfill reads the out-ring → for claude that is claude-native → the renderer
  (`renderPiEvent`, pi-vocabulary only) doesn't understand it → dumps raw.

Replaying the out-ring through the translator does **not** work: (a) the out-ring
has **no user turns** (they're injected from the stdin side via `OutboundEcho`,
never on stdout), and (b) the translator is stateful, so a partial/suffix replay
is wrong and the bounded ring loses early frames on long sessions.

Separately, claude's `system`/init frame carries a **`slash_commands`** list (names)
that would make excellent TUI autocomplete — the same way pic-attach already feeds
pi-backend commands into completion — but it is currently ignored.

## Principle

The daemon should **persist and expose the pi-vocabulary stream it serves to
clients**, captured at the one moment it is produced correctly (live, with full
translator state and both input halves in order) — not re-derive it from partial
raw inputs. The raw ring remains the forensic/`--raw` under-layer.

A single provider capability gates the claude-only behavior:

**`ProtocolProvider.Normalizes() bool`** — `PiProvider` → `false` (stdout == bus),
`claudeProvider` → `true`. Avoids scattering `kind == "claude"` string checks.

## Sub-project A: claude rendered backfill (render-ring)

### A1. Render-ring capture (Child)

For providers where `Normalizes()` is true, the Child keeps a second bounded
`ring.Ring` — the **render-ring** — capturing the **bus output** (pi-vocabulary).

- Add `c.publishBus(f []byte, ts int64)`: if `c.provider.Normalizes()`, append `f`
  to `c.renderRing`; then `c.bus.Publish(f)`.
- Route **both** existing publish sites through it:
  - `readStdout`: `for _, f := range c.provider.BusFrames(line, ts) { c.publishBus(f, ts) }`
  - supervise loop: `for _, f := range c.provider.OutboundEcho(frame, ts) { c.publishBus(f, ts) }`
- pi children: `Normalizes()` false → no render-ring allocated, no extra memory.
- Render-ring uses the same `ring.Options` defaults as the raw ring.
- Add `c.RenderRingSnapshot() [][]byte` (mirrors `RingSnapshot`), returning nil when
  the provider doesn't normalize.

### A2. Exit + disk persistence

- At exit, snapshot the render-ring into the store session as **`ExitedRenderRing`**
  (mirrors the existing `ExitedRing`), so an exited child in the same daemon
  session still renders. `MarkExited` (or the exit path) captures both.
- `LogDumper.Dump` also writes **`render.jsonl.gz`** for normalizing children
  (alongside the raw `out.jsonl.gz`), gated by the same emission mode. So rendered
  backfill survives a daemon restart (orphan reload). If mode is `never`,
  rendered-for-orphans is unavailable — same limit as raw.

### A3. `GetRecent` raw vs rendered selector

`protocol.GetRecentRequest` gains `Rendered bool` (default `false` = today's raw
behavior — `pic recent` and `--raw` are unchanged).

`Controller.GetRecent(childID, q)` where `q.Rendered`:
- **`Rendered == false`** (raw): unchanged. Live → raw ring; exited → `ExitedRing`;
  orphan → `out.jsonl.gz`.
- **`Rendered == true`, normalizing child**: live → render-ring; exited →
  `ExitedRenderRing`; orphan-after-restart → `render.jsonl.gz` on disk.
- **`Rendered == true`, non-normalizing (pi) child**: read the raw ring / `ExitedRing`
  / `out.jsonl.gz` — identical to rendered, no render-ring exists.

`server.RecentQuery` gains the matching `Rendered bool`; the dispatch handler
copies it from the request. `Limit`/`Since`/`Include`/`Exclude` apply to the
selected source as today.

### A4. CLI + bun wiring

- `cmd/pic/history.go`: the rendered path (`runHistoryOut` when not `opts.raw`)
  sets `Rendered: true` in its `GetRecentRequest`; `--raw` sets `Rendered: false`.
  For claude the rendered frames are now pi-vocab → `renderPiEvent` renders them.
  The inner-bytes dedup now matches for claude too (backfill and live are both
  pi-vocab).
- `pic recent` stays `Rendered: false` (raw debug dump).
- bun `attach/src/client.ts` `getRecent` sends `rendered: true`; `primeHistory`
  then receives pi-vocab frames → existing `translate`/`updateCacheFromEvent`
  reconstruct the transcript → claude attach scrollback works.

## Sub-project B: claude slash_commands → TUI completion

### B1. Capture (daemon)

claude's `system` frame with `subtype:"init"` carries `slash_commands: []string`
(names only; see fixture `internal/child/testdata/claude/startup_and_turn.jsonl`).

- Extend `internal/child/sniff.go` `SnifferMetadata` + `ExtractMetadata` to pull
  `slash_commands` from that frame.
- Store the captured list on the Child and propagate to the store session, so it
  appears in `store.Snapshot`.
- Surface as `SlashCommands []string` on `protocol.ChildSummary` (the `ctrl_get`
  response), populated by `snapshotToSummary`.

### B2. Serve to the TUI autocomplete

pic-attach's `setupTuiAutocomplete` (`cmd/pic/picembed/pic-helpers/index.ts`)
currently fetches pi commands via a `get_commands` round-trip, gated on
`kind === "pi"`. Extend it:

- For `kind === "claude"`: issue a one-shot `ctrl_get` for the child and read
  `slashCommands` (names only, no descriptions); feed them into the **same**
  autocomplete provider via `filterCommandSuggestions` (already handles name-only
  `CommandInfo`). pi path unchanged.
- The `refresh()` mechanism stays; claude commands are captured once at init and
  are effectively static for the session.

## Testing

**Go:**
- `Normalizes()` returns false for pi, true for claude.
- Render-ring captures both publish sites in order: drive a child (or the publish
  helper) with a stdout `BusFrames` emission and an `OutboundEcho` user turn;
  assert the render-ring equals the bus sequence including the user turn.
- `GetRecent` `Rendered` selector: claude live → render-ring; claude exited →
  `ExitedRenderRing`; claude orphan → `render.jsonl.gz`; pi rendered → raw ring;
  raw path unchanged. Cover with a fake controller/store as the existing
  `GetRecent`/dispatch tests do.
- `LogDumper` writes `render.jsonl.gz` for a normalizing child and not for pi.
- `ExtractMetadata` extracts `slash_commands` from the test fixture frame;
  `snapshotToSummary` carries `SlashCommands`.

**bun:**
- `primeHistory` reconstructs a claude transcript from rendered (pi-vocab) frames.
- `setupTuiAutocomplete` serves claude `slashCommands` (via a fake `ctrl_get`) into
  the completion provider; pi path still uses `get_commands`.

## Out of scope

- Re-deriving rendered frames by replaying raw I/O (rejected: incomplete input +
  stateful translator).
- Normalizing the `in`/`err` raw streams' rendering (they stay raw).
- Descriptions for claude slash_commands (claude emits names only).
- The deferred follow-ups from the prior feature (backfill `-n` filter-after-limit,
  `--profile` not reaching backfill) — tracked in the prior plan's open items.
