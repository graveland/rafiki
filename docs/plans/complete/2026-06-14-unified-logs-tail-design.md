# Unified `logs`/`tail` + history backfill — design

Date: 2026-06-14
Status: approved (design)

## Problem

Three commands sit on top of one data source (the per-child event ring) and
each makes different, unprincipled choices:

| cmd | source | rendering | follow | availability |
|-----|--------|-----------|--------|--------------|
| `pic tail` | ring (live) | rendered (`render_tail.go`) | yes | live only, **no backfill** |
| `pic logs` | on-disk gz dump | raw JSONL | no | **exited only** |
| `pic recent` | ring / `ExitedRing` | raw JSON | no | live + exited |

Concrete pain:

1. `pic attach` and `pic tail` start blank — they don't show prior history even
   though the daemon retains it.
2. `pic tail` should catch you up on the last N events, then follow.
3. `pic logs` only works after the child exits; it should work while running.
4. `logs` and `tail` are "completely different" when they should be the same
   thing with following as the only difference (as every other tool on the
   planet does it).

## Model: one history engine, follow is the only difference

A child's event history lives in exactly one logical place:

- **alive** → the in-memory ring (`internal/ring`, default 5000 events / 64 MiB)
  plus the in-memory `in`/`err` snapshots on the `Child`.
- **exited** → `ExitedRing` (snapshot taken at exit, held in the store) and, if
  the persistence mode dumped them, the on-disk `in.jsonl.gz` / `err.log.gz`.

All commands read that one source. The source is **invisible plumbing** —
identical content and identical rendering before or after exit. `logs` and
`tail` become two presets over a single engine, differing only in defaults:

| | `--follow` | default `-n` | rendering |
|---|---|---|---|
| `pic tail [id]` | on | `20` | rendered |
| `pic logs [id]` | off | `all` | rendered |

Invariants:

- `pic tail` ≡ `pic logs -f`.
- Both render by default. `--raw` is the escape hatch (raw JSONL), required for
  dev/grep/pipe workflows but expected to be the 1% path.
- The `-n` default keys off **follow mode**, not the command name: following →
  short catch-up (`20`); one-shot → full dump (`all`). Always overridable.
- `-n 0` = no history (pure live; today's `tail` behavior). `-n all` (or `-1`) =
  everything available in the source.

## Components

### 1. Daemon (Go) — minimal additions

`Controller.GetRecent(childID, RecentQuery{Limit, Since, Include, Exclude})`
already serves the **`out` event stream** for both live (ring) and exited
(`ExitedRing`) children, returning `[]json.RawMessage`. Reuse unchanged. This
covers the rendered default and `--raw` for the `out` stream, live and exited.

**New RPC `ctrl_get_streams`** (isolated, lowest-value, deferrable): returns the
in-memory `in` and `err` snapshots for a **live** child (raw escape-hatch
streams via `Child.InSnapshot()` / `Child.StderrSnapshot()`). Exited children
keep using the existing on-disk zcat path in `cmd_logs.go`.

- Shape (proposed): request `{type:"ctrl_get_streams", childId, which:"in"|"err"|"all"}`;
  response carries raw bytes per requested stream.
- This is what makes `--in`/`--err`/`--all` return "the same thing" while
  running as after exit. If the persistence mode is `never`, post-exit `in`/`err`
  are unavailable — pre-existing limitation, surfaced as a clear message, not
  changed here.

### 2. cmd/pic (Go) — the unification

New shared `cmd/pic/history.go` with a single `runHistory(opts)`:

1. Resolve target child (`resolveTarget`), update active marker (best-effort).
2. If `-n != 0`: fetch backfill.
   - rendered/raw **out**: `GetRecent` with `Limit` derived from `-n`
     (`all` → no limit), honoring `--include/--exclude/--profile`, `--no-deltas`.
   - raw **in/err/all**: `ctrl_get_streams` (live) or on-disk zcat (exited).
3. Emit backfill through the chosen sink:
   - rendered → existing `render_tail.go` renderer.
   - raw → frame bytes verbatim (JSONL).
4. If `--follow`: subscribe and stream live (existing `tail` loop), applying
   the dedup rule below. Per-child follow auto-exits on `ctrl_child_exited`
   (today's behavior); label-filtered follow exits only on SIGINT/SIGTERM.
   If not `--follow`: stop after backfill.

`cmd_logs.go` and `cmd_tail.go` become thin frontends that set preset defaults
and delegate to `runHistory`. Flag surface (union, all preserved for
back-compat):

- shared: `-n/--tail N` (depth; `0`/`all`/`-1` semantics above), `-f/--follow`,
  `--raw`, `--include`, `--exclude`, `--profile`, `--no-deltas` (default true),
  `-v/--verbose`, `--label`, `--has-label`.
- raw stream selectors (imply `--raw`): `--in`, `--err`, `--all`, `--path`.

Caveat: only the **`out` event stream** is on the daemon bus and can be
*followed*. `in`/`err` are snapshot-only — `--in`/`--err` (and the `in`/`err`
portion of `--all`) ignore `--follow` and print the current snapshot. `--all -f`
follows `out` and snapshots `in`/`err` at start.

`pic recent` is left as-is for now; it is effectively `logs --raw -n N` with
pretty-printed JSON and is a candidate for later deprecation (out of scope).

### 3. pic-attach (bun) — scrollback, zero pi changes

The pi TUI reads `RemoteAgentSession.messages` (backed by `_messages`), which
starts empty — that is exactly why attach shows no scrollback today. Fix lives
entirely in pic-attach (controller code), not pi.

- `attach/src/client.ts`: add `getRecent(childId, {limit})` issuing
  `ctrl_get_recent` and returning the event frames.
- `attach/src/session.ts` (`RemoteAgentSession`): before `consumeEvents()` /
  before the TUI runs, prime `_messages` from retained history by replaying
  events through the **existing** `translate → updateCacheFromEvent` path. This
  reuses the exact accumulation logic the live path uses; no new message-
  building code. `message_update` deltas (transient streaming state) are
  skipped for history priming.
- `attach/src/main.ts`: ensure priming completes before `tui.run()` so the
  initial paint includes scrollback.
- `cmd/pic/cmd_attach.go`: add `-n` and pass scrollback depth to `pic-attach`
  via an env var (e.g. `PIC_ATTACH_TAIL`); default = full ring.

**Open decision (resolve in implementation):** seed source for `_messages`.
- (a) daemon ring via `getRecent` — consistent with `tail`/`logs`, but bounded
  (~5000 events / 64 MiB).
- (b) pi's session file via the already-built `sessionManager` — canonical and
  unbounded, but couples to pi's session format.
Default lean: (a) for consistency; switch to (b) only if ring-bounded history
proves insufficient in practice.

### 4. Backfill + follow dedup/ordering

To avoid missing or duplicating events at the catch-up→live boundary:

1. Subscribe first (so events between fetch and stream are buffered, not lost).
2. Fetch backfill (`GetRecent`, depth `-n`); note the newest backfilled
   timestamp `T`.
3. Render backfill.
4. Drain/stream live frames, dropping any with timestamp `< T`, and for frames
   at exactly `T` (ms collision), drop those byte-identical to a backfilled
   frame in that boundary millisecond.

Ring `Event` carries only `{Bytes, Timestamp}` (unix ms), so the watermark is
timestamp-based; the byte-compare within the boundary ms makes it exact without
needing a sequence number. (If frames turn out to carry a monotonic id, prefer
that during implementation.)

## Testing

- Go:
  - `history_test.go` — `runHistory` fetch + dedup + rendered/raw sink.
  - logs-while-running: assert live child serves the same content the on-disk
    dump would after exit (out via `GetRecent`; in/err via `ctrl_get_streams`).
  - backfill ordering: boundary watermark drops duplicates, loses nothing.
  - extend `cmd_tail_test.go`, `render_tail_test.go`; daemon RPC test alongside
    `internal/server/dispatch_test.go`.
- bun:
  - `attach/src/session.test.ts` — priming `_messages` from a recorded
    `getRecent` reply reconstructs the expected transcript; `message_update`
    deltas are ignored for priming.

## Out of scope

- Deprecating/removing `pic recent`.
- Changing persistence modes or on-disk dump format.
- Seeding attach scrollback from the session file unless (a) proves insufficient.
