# rafiki — long-term backlog

Durable, always-current. Unlike the ephemeral design/plan docs under `docs/plans/`,
this file is committed and outlives any single piece of work. Add to it whenever
something is deferred rather than done; delete entries when they land.

---

## Architecture

### Split `pkg/routing` — move `CaptureStore` out

`pkg/routing` bundles two unrelated concerns: routing/catalog logic (breaker,
OpenRouter catalog, model resolution, `ClassifyFailure`, `prefix_hash`, SSE
capture parsing) and a postgres-backed turn store (`store.go`, ~443 lines).

Because Go imports whole packages, anything wanting the DB-free catalog helpers
also links pgx. That is how `cmd/rafiki` — a pure socket client that never opens
a database connection — ends up linking 11 pgx packages.

- [x] Move `store.go`'s `CaptureStore` into `pkg/capture`
- [x] (Extension: also moved `RawTraceStore` → `pkg/rawtrace` and
      `EjectionStore` → `pkg/ejection` — both also imported pgx in routing.)
- [x] Update the public signatures that name it:
      `server.NewMessagesProxy`, `server.NewChatCompletionsProxy`, `pkg/llm/client.go`
- [x] Update tests in `pkg/server`, `pkg/llm`
- [x] Split price-sync types (`ModelPrice`, `ModelInfo`, `PriceSource`) into
      `pkg/pricing` so `pkg/routing` no longer imports `pkg/store` (which links
      pgx).

**Result:** `pkg/routing` now links **zero** pgx packages.

---

### Add a model-information RPC to the control protocol

`rafiki claude` currently reads the OpenRouter catalog snapshot off disk itself
(`routing.FileSnapshotStore` + `NewModelCatalog` + `AutoCompactWindow`) purely to
pin `CLAUDE_CODE_AUTO_COMPACT_WINDOW` to the model's real context window. That is
the *only* reason the client imports `pkg/routing` at all.

The daemon already warms and caches that catalog. The client already has a socket
to the daemon. So the client should ask.

- [x] Add a `ctrl_model_info` request/response to `pkg/protocol`
      (context window, max completion tokens, resolved id, auto-compact window)
- [x] Answer it in the daemon from the already-warm catalog (`Controller.ModelInfo`)
- [x] Switch `cmd/rafiki/claude.go` to the RPC and drop its `pkg/routing` import
- [x] Keep the existing graceful degradation: unreachable daemon or unknown model
      returns 0 and leaves Claude Code's default compaction point alone —
      `rafiki claude` must still work with the daemon down
- [x] Update `docs/reference/` for the new verb (added §6.21)

---

### Make `cmd/rafiki` link zero pgx packages (linker-enforced invariant)

`pkg/routing` is now DB-free, and `rafiki claude` asks the daemon over
`ctrl_model_info`. But `cmd/rafiki` still links pgx via:

- `pkg/agentcli` — `cmd_conversations.go` uses shared rendering/types, but
  `agentcli/backend.go` imports `pkg/insights`, `pkg/analyze`, and `pkg/store` —
  all pgx-backed.

Progress so far:

- [x] `pkg/routing`: DB-free (CaptureStore/RawTraceStore/EjectionStore moved out).
- [x] `pkg/executors`: DB-free (`postgres.go` → `pkg/executorsdb`).
- [x] `pkg/insightstypes`: new pgx-free types package (Stats, Transcript,
      ConversationSummary, SearchFilter, Path, Pricer — everything cmd/rafiki
      needs for rendering). `pkg/insights` still imports pgx (query engine stays).
- [ ] `pkg/agentcli`: the last blocker. `backend.go` and `compare.go` must move
      to `pkg/agentcli/local/` (they import insights/analyze/store → pgx). The
      complication: `local/backend.go` already defines a `Backend` struct, and the
      moved `backend.go` defines the `Backend` interface — name conflict. Fix:
      rename the struct to `Impl`, move the interface, update daemon references
      (`agentcli.Backend` → `local.Backend`). Also, `local/` needs access to
      `errWriter`, `newAgentTable`, `dollars`, and `RenderTranscriptMD` (currently
      in the top-level package). Extract the shared helpers to a pgx-free location
      or import them across the package boundary.
- [ ] Once agentcli is done, add the `TestClientDoesNotLinkPostgres` linker guard
      test in `cmd/rafiki/no_postgres_test.go`.

---


## Correctness / consistency

- [ ] **Auto-label prefix migration.** The `fundi/*` → `rafiki/*` reserved
      auto-label prefix rename is a user-visible contract change. Auto-labels are
      **persisted per child**, so a long-running child spawned before the rename
      still carries `fundi/kind=…` while a `--has-label rafiki/kind=…` filter
      written afterwards will not match it. Decide whether to accept the seam
      (and document it in `docs/MIGRATING.md`) or to migrate existing records.
- [ ] **Bare kind literals in tests.** Several pre-existing test files still write
      `Kind: "pi"` / `Kind: "claude"` as bare strings rather than
      `protocol.KindPi` / `protocol.KindClaude`: `cmd/rafikid/controller_streams_test.go`,
      `controller_abort_claude_test.go`, `controller_resume_claude_test.go`,
      `pkg/childstore/session_kind_test.go`, `pkg/control/dispatch_test.go`.
      Correct at runtime — those spellings never changed — but inconsistent with
      the files that were touched during the kind rename.

---

### Cross-language constants are duplicated with nothing enforcing agreement

`APP_NAME` is hardcoded in **three** places that must agree but are checked by
nothing: `pkg/paths/paths.go`'s `appName` (Go), `attach/src/client.ts` (TS), and
`cmd/rafiki/helpersembed/rafiki-helpers/index.ts` (TS). All three derive the XDG
socket/state path independently.

During the rafiki/fundi rename the Go side moved to `"rafiki"` while both TS
sides still said `"fundi"` — a genuine socket-path mismatch that was **masked
only because the daemon injects `RAFIKI_SOCKET` explicitly into every child**.
Remove that injection, or hit any path where it isn't set, and the TUI looks for
a socket that doesn't exist. Nothing would have caught it: not the Go compiler,
not `tsc`, not `make check`.

- [ ] Decide on a single source of truth. Options: have the daemon always pass
      the resolved paths to the TS side (making the TS constants dead), generate
      a small TS constants file from the Go source at build time, or add a test
      that reads both and asserts equality.
- [ ] Same question applies to the `RAFIKI_*` environment variable names, which
      `pkg/paths/envvar.go` documents in a comment block precisely because Go
      cannot enforce what the TS side reads. A comment is not enforcement.

## Test coverage gaps

- [ ] **OTLP exporter is never exercised against a real collector.** Only the
      no-op path (`OTEL_EXPORTER_OTLP_ENDPOINT` unset) is covered. The exporter
      code was moved verbatim from a working binary, so this is low risk, but it
      means an exporter regression would ship silently.
- [ ] **`systemd` service tests are `//go:build linux`-gated** and therefore never
      run on a darwin dev box. They cross-compile, but nothing here executes them.
      Worth a linux CI job — or at minimum, knowing they are unverified locally.

---

## Notes for future work in this repo

- **`go test` caching does not see through to the daemon.** `test/integration`
  builds the daemon binary via a subprocess inside `TestMain`, so its package
  import graph (`go list -json ./test/integration/` → `XTestImports`) contains
  only `pkg/protocol`. A change to `pkg/paths`, `pkg/control`, or anything else
  the daemon reads at startup will NOT invalidate a cached PASS. **Use `-count=1`**
  whenever changing daemon startup behaviour, or a stale `(cached)` result will
  hide a real break.
- **`make check` does not run `make build-attach`.** The TypeScript TUI can be
  broken while the Go gate is fully green. Anything that moves files under `cmd/`
  should check `attach/src/` for relative imports reaching into them.
