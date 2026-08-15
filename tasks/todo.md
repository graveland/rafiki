# rafiki — long-term backlog

Durable, always-current. Unlike the ephemeral design/plan docs under `docs/plans/`,
this file is committed and outlives any single piece of work. Add to it whenever
something is deferred rather than done; delete entries when they land.

---

## Open now

### Code review of the 114-commit platform work — 9 confirmed defects, 31 unconfirmed

Reviewed 2026-08-15 at `f2b5035`. Full detail in
`docs/plans/2026-08-15-review-findings.md` and a fix plan in
`docs/plans/2026-08-15-review-fixes-plan.md` — **both gitignored**, so the
load-bearing content is duplicated here.

**Six critical, three high, all verified against the code:**

- [ ] **D1 · `executor/server.go:259`** — the container grant covers `bash` and
      nothing else. `read`/`write`/`edit`/`glob`/`grep` go to a registry built
      with `Cwd: opts.Root` on the host, and `resolveToolPath` passes absolute
      paths through untouched and expands `~` to the executor user's home. A
      containerised agent can `read ~/.aws/credentials` and `write ~/.zshenv`.
      Also makes container mode non-functional for file tools (`/work` does not
      exist on the host). `container_test.go` only exercises `execBash`.
- [ ] **B1 · `rafikid/executor_wiring.go:24`** — omitting the executor selector
      escapes confinement. `resolveExecutor` narrows only when a selector is
      present, and `agent_spawner.go` never falls back to the spawner's stored
      one, so the child runs in the daemon process — and since its stored
      selector is `""` and `lineageChain` skips empty links, the whole subtree
      inherits the escape. Fix: inherit the spawner's stored selector.
- [ ] **A1 · `execpool/pool.go:139,152,311`** — `p.live` is mutated by executor
      ID with no identity check, so after a restart a stale health loop deletes
      its own replacement, parks a healthy executor, and 5 min later `onLost`
      tears down its children. Compare the pointer before deleting; close any
      displaced `liveConn`. The comment at `:306` describes a re-check its own
      ordering defeats — fix it too.
- [ ] **A2 · `execpool/pool.go:76,84,145`** — no deadline on `Describe`/`Health`
      and no `ReadIdleTimeout`/`PingTimeout`, so a black-holed executor is never
      parked. That is the sleeping-laptop case `departure.go` exists for.
- [ ] **D2 · `executor/workspace.go:67`** — `workspaceRegistry` is an
      unsynchronized map across concurrent RPC handlers. Concurrent map
      read/write is a Go *fatal error*: the executor dies with every child on it.
- [ ] **B2 · `rafikid/limits.go:275`** — a negative `max_cost` collapses to 0,
      which means *unlimited*, for the child and its whole subtree.
      `grantedDepth`/`grantedChildren` fail closed; only cost inverts.
- [ ] **A3 · `execpool/pool.go:99`** — any `Authenticate` error is reported to
      the executor as terminal, so a 5-second DB blip permanently downs every
      executor host. Needs a retryable/terminal discriminator on the wire.
- [ ] **C1 · `tasks/postgres.go:174`** — `Drop`'s live-assignee check has no
      `FOR UPDATE` and the `UPDATE` never re-checks it, so a concurrent `Assign`
      lets an assigned task be dropped. **The memory store is atomic under its
      own mutex, so the conformance suite structurally cannot catch this** — the
      test belongs in `postgres_test.go`.
- [ ] **C2 · `tasks/memory.go:263`** — `OrphanAssigned("")` orphans every
      unassigned row in memory (Go has no NULL) and no rows in Postgres
      (`assignee IS NULL`). Latent, but total data loss one empty string away.

**Three unconfirmed findings worth promoting on their own merits:**

- [ ] **B3** — `effectiveExecutorSet` evaluates the `Admits` half against the
      *leaf's* labels only, and at placement against the *parent's* labels
      rather than the child being placed. The phase-07 "Admits is never
      evaluated" fix is therefore half-done.
- [ ] **B4** — `workspace.Derive` receives a model-chosen `cwd` with no
      containment check, so `agent_spawn(cwd="/", workspace="ephemeral")`
      bind-mounts the executor host's root filesystem read-write at `/work`.
      `grant.go:1-2` asserts the opposite invariant.
- [ ] **D3** — a workspace-less `Execute` runs on the host even when
      `Isolation == "container"`, and `executorclient` never sets `WorkspaceId`
      — so this is the **default** for every `--executor-socket` spawn, while
      `Describe` still reports `isolation: "container"`.

The remaining 28 unconfirmed findings are in the findings doc. If that file is
gone, the review method reproduces them: four `code-nitpicker` subagents over
`pkg/execpool`, `cmd/rafikid/{limits,agent_spawner,executor_select}.go`,
`pkg/eventbuf`+`pkg/tasks`, and `pkg/executor`, each briefed with the project's
actual shipped failure modes.

**Two lessons worth more than any single finding:**

1. **Search for the bypass, not the flaw in the guard.** B1 and D1 are both
   cases where the confinement mechanism is correct and complete, and simply not
   on the path taken.
2. **A stale comment is worse than no comment.** Three comments actively
   misled — `pool.go:306`, `grant.go:1-2`, and the `agentloop_test.go` skip
   reason. A1 survived two reviews behind one of them.

### Executor path has never been exercised end to end

The remote executor transport — reverse-dial, enrollment, pool membership,
selection, reconnect — has unit tests at every layer and **no test that assembles
them**. `TestExecutorPool_FullLifecycle` and `TestExecutorPool_Narrowing` are
empty bodies carrying stale `t.Skip("not yet implemented (plan-07 scope)")`
reasons, though the listener has been wired since `cmd/rafikid/main.go:445`.

Related and worse: `test/integration/grant_test.go` has not compiled since
`0acadf2` moved `executors.NewPostgresStore` to `pkg/executorsdb`, and nothing
noticed because the file carries `//go:build integration` — a tag `go vet ./...`
and `go test ./...` both omit. `subagent_test.go` shares the package, so phase
04's e2e has not run either.

- [ ] Plan written: `docs/plans/2026-08-14-executor-e2e-and-tag-gate-plan.md`.
      Fix the gate first (task 1), then fill the two tests.

**Why this is the top entry:** every other item here is tidiness. This one is an
untested transport in production code.

---

## Deferred by design — revisit when there is a reason

These were considered and deliberately not built. Each records *why*, so it is
not re-litigated from scratch.

- [ ] **Subagent verbs `steer`, `interrupt`, `respond`, `search`, `label`,
      `forget`.** Phase 04 shipped `agent_spawn/list/view/send/kill/models` and
      deferred the rest until actual use demands them. Adding a verb is cheap;
      removing one from a model's tool surface is not.
- [ ] **mTLS for executor connections.** Deferred as an *upgrade*, not a
      rejection: a bearer credential over an authenticated TLS channel never
      crosses cleartext, and a private key at rest on an executor is no better
      protected than a token at rest on the same disk. It is the only reason to
      introduce a rafiki-owned CA, so it stays deferred until something needs
      one.
- [ ] **Landlock / Seatbelt native scoping.** Docker mounts cover the container
      case entirely and native executors are admission-gated. If real native
      scoping is ever wanted, the shape is multiple native executors with
      different roots, each self-applying a ruleset at startup so every `bash`
      it forks inherits it. Note the deferral also avoids a test burden that
      would never run on a darwin dev box.
- [ ] **Gate MCP at server granularity.** Any agent may use any MCP tool, so a
      worker sandboxed to its worktree still reaches whatever the MCP surface
      reaches. Correct today — an MCP server's containment is its operator's job
      — but a filesystem- or kubectl-shaped server added later silently widens
      every worker in the fleet. The vocabulary leaves room for this without
      redesign.

---

## Correctness / consistency

- [ ] **Auto-label prefix migration.** The `fundi/*` → `rafiki/*` reserved
      auto-label prefix rename is a user-visible contract change. Auto-labels are
      **persisted per child**, so a long-running child spawned before the rename
      still carries `fundi/kind=…` while a `--has-label rafiki/kind=…` filter
      written afterwards will not match it. Decide whether to accept the seam
      (and document it in `docs/MIGRATING.md`) or to migrate existing records.
      `pkg/childstore/tree.go` already tolerates both spellings on read, so the
      lineage path is safe either way; label *filters* are the exposed edge.

### Cross-language constants are duplicated with nothing enforcing agreement

`APP_NAME` is hardcoded in **three** places that must agree but are checked by
nothing: `pkg/paths/paths.go`'s `appName` (Go), `attach/src/client.ts:20` (TS),
and `cmd/rafiki/helpersembed/rafiki-helpers/index.ts:152` (TS). All three derive
the XDG socket/state path independently. **All three currently say `"rafiki"`**,
so this is latent rather than broken.

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
      no-op path (`cmd/rafikid/tracing.go:29`, both endpoint vars unset) is
      covered. The exporter code was moved verbatim from a working binary, so
      this is low risk, but an exporter regression would ship silently.
- [ ] **`systemd` service tests are `//go:build linux`-gated**
      (`cmd/rafiki/service_linux_test.go`) and therefore never run on a darwin
      dev box. They cross-compile, but nothing here executes them. Worth a linux
      CI job — or at minimum, knowing they are unverified locally. Note this is
      the *legitimate* use of a build tag (the file genuinely cannot compile
      elsewhere), unlike the `integration` tag above.
- [ ] **Flow control under a genuinely large executor stream is untested.** The
      phase-07 spike's payloads were small, so "a 40-minute build streaming from
      a docker host would otherwise buffer unboundedly" rests on an
      incremental-flush assertion rather than a load test.

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
- **`make check` does not compile build-tagged Go files either.** Neither
  `go vet ./...` nor `go test ./...` passes `-tags`, so a tagged file can stop
  compiling with the gate staying green — which is exactly what happened to
  `grant_test.go`. Prefer a loud runtime skip (`requireDocker` in
  `test/integration/container_test.go`) over a build tag: it still compiles,
  still gets vetted, and still says so on stderr where `rtk` cannot swallow it.
- **Three gate blind spots, one shape.** The TUI, the daemon's import graph, and
  build-tagged files are all things `make check` cannot see. When adding a new
  kind of artifact, ask what compiles it before asking what tests it.
