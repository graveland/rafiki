# rafiki — long-term backlog

Durable, always-current. Add to it whenever something is deferred rather than
done; delete entries when they land.

**This file is NOT in git — `tasks/` is gitignored, deliberately.** It survives
across sessions and branches because it lives on disk, but it does not survive a
fresh clone, a `git clean -xdf`, or a lost machine. That is a real limitation,
not an oversight: if an entry here matters enough to survive those, promote its
conclusion into `CLAUDE.md`, `README.md` or `docs/reference/`, which are tracked.
(An earlier version of this header claimed the file was committed. It never was.)

---

## Open now

### Code review of the 114-commit platform work — 9 confirmed defects, 31 unconfirmed

Reviewed 2026-08-15 at `f2b5035`. Full detail in
`docs/plans/2026-08-15-review-findings.md` and a fix plan in
`docs/plans/2026-08-15-review-fixes-plan.md` — **both gitignored**, so the
load-bearing content is duplicated here.

**Six critical, three high, all verified against the code.** Eight of nine were
fixed on branch `fix/review-findings-2026-08-15` (2026-08-15), each with a
regression test proven to fail before the fix. **D1 remains open** and is the
only one still live.

- [ ] **D1 · `executor/server.go` — STILL OPEN.** On a container workspace only
      `bash` enters the container; `read`/`write`/`edit`/`glob`/`grep` run in the
      executor process on the HOST. Demonstrated against a real container
      workspace granted only `/work` + `/repo:ro`: `read ~/.ssh/id_rsa` returned
      the host's private key, `read /etc/passwd` succeeded, `grep path=/etc`
      walked the host, and `write` landed outside every mount. `~` is expanded by
      `resolveToolPath` via `os.UserHomeDir()` — the executor user's home.
      It also makes container mode **non-functional**: every legitimate
      container-path call fails (`/work` does not exist on the host), so the only
      calls that succeed today are the escaping ones.

      Pinned by deliberately-skipped docker tests in
      `pkg/executor/workspace_tools_test.go` (loud stderr banner; un-skip to
      reproduce).

      **The review plan's fix is wrong and must not be implemented.** It proposes
      userspace path translation on the host, quoting `08-containers.md`'s "the
      file tools could enforce it in userspace" — but that sentence is about
      NATIVE executors and is an argument *against* path scoping, concluding "a
      scope that holds for `read` and evaporates on the first shell command is
      worse than no scope". For containers the same doc says "docker mounts are
      the grant … enforced by the kernel". The fix is a tool server running
      INSIDE the container, reached over `docker exec -i` stdio (there is no TCP:
      `workspace.Derive` hardcodes `Network: "none"`), reusing `ServeInverted` /
      `ClientForConn`. Planned in
      `docs/plans/2026-08-15-in-container-tool-server-plan.md` (gitignored):
      Task 0 there is a few-line fail-closed refusal that removes the escape
      immediately and costs nothing that currently works.
- [x] **B1** — an omitted executor selector escaped confinement. Fixed by
      `Controller.inheritExecutorGrant`, called from `Controller.Spawn` rather
      than from the spawner adapter, so `ctrl_spawn` straight from a client is
      covered by the same code. `WorkspaceMode` inherits independently.
- [x] **A1** — `p.live` mutated by executor ID with no identity check. Fixed by
      `removeLive` (pointer compare, reports whether it deleted) and
      `installLive` (tears down what it displaces); teardown made idempotent via
      `sync.Once`, since it now arrives from two directions.
- [x] **A2** — no deadline on `Describe`/`Health`, no h2 keepalive. Fixed;
      timeouts are `Pool` fields so the health path is testable in milliseconds.
      The park path is now exercised end to end over a real inverted TLS
      connection, which it never was.
- [x] **D2** — unsynchronized `workspaceRegistry`. Fixed with an `RWMutex`;
      reproduced first as `fatal error: concurrent map writes`.
- [x] **B2** — negative `max_cost` meaning unlimited. Refused in `checkBudget`
      *before* its early returns, so a top-level spawn and an unbudgeted parent
      are covered too.
- [x] **A3** — every `Authenticate` error reported as terminal. Fixed with
      `ExecutorHelloResponse.Retryable` + `executors.IsTerminalAuthError`
      (sentinels moved to `pkg/executors` so the executor binary does not link
      pgx). Also stopped forwarding the store's error text — it was handing a
      DSN to an unauthenticated peer.
- [x] **C1** — `Drop`'s TOCTOU. Fixed with `FOR UPDATE` over the whole subtree,
      with the SQL predicate removed (it left exactly the rows a concurrent
      `Assign` races for unlocked). Deterministic two-transaction test plus a
      deadlock check against `Add`/`Assign`.
- [x] **C2** — `OrphanAssigned("")`. Guarded in both stores; case added to the
      shared conformance suite, which failed on memory and passed on Postgres.

**Three unconfirmed findings worth promoting on their own merits** (B4 has since
been **confirmed**):

- [ ] **B3** — `effectiveExecutorSet` evaluates the `Admits` half against the
      *leaf's* labels only, and at placement against the *parent's* labels
      rather than the child being placed. The phase-07 "Admits is never
      evaluated" fix is therefore half-done.
- [ ] **B4 — CONFIRMED 2026-08-15.** `agent_spawn` exposes a model-facing `cwd`
      ("Absolute working directory. Omit to use your own", `agent_spawn.go:39`).
      It flows `SpawnSpec.Cwd` → `SpawnRequest.Cwd` → `workspace.Derive` with no
      containment check, and Derive turns it into the read-write `/work` mount —
      so `agent_spawn(cwd="/", workspace="ephemeral")` bind-mounts the executor
      host's root filesystem RW. `TestAgentSpawnHasNoPathShapedParameter` does
      not catch it: it blocklists `mount`/`mounts`/`roots`/`path`/`paths`/
      `volume`/`allow`/`deny` and nothing else.
      `grant.go`'s header claim and the `CLAUDE.md` entry have both been
      corrected to stop asserting the opposite; the CODE is unchanged.
      Fix shape: a containment check of the child's cwd against the SPAWNER's at
      admission — the same intersection shape as the executor selector — in the
      caller that knows the lineage, not inside the pure `Derive`.
- [ ] **D3** — a workspace-less `Execute` runs on the host even when
      `Isolation == "container"`, and `executorclient` never sets `WorkspaceId`
      — so this is the **default** for every `--executor-socket` spawn, while
      `Describe` still reports `isolation: "container"`.

**Found while fixing the above, not in the review** (all verified, all small):

- [ ] **`Server.Release` kills background jobs across ALL workspaces.** It calls
      `s.jobs.killAll()`, which has no workspace filter, so releasing one child's
      workspace kills every other child's background jobs on that executor. The
      in-container plan's Task 6 dissolves this by moving the job registry inside
      the container; fix it standalone if that plan slips.
- [ ] **`TestReleaseRemovesTheContainer` passes vacuously.** `containerNameFor`
      in `container_test.go` returns `"rafiki-ws-" + workspaceID`, but the
      workspace id already *is* `"rafiki-ws-" + randomID()`. The double prefix
      matches no container, so the `docker ps` filter is always empty and the
      assertion cannot fail.
- [x] **`newTestClient` could not be used from a subtest.** Its socket path
      embedded `t.Name()` verbatim, so any `t.Run` name put a `/` in the path and
      every subtest died with `bind: no such file or directory` — which reads as
      a permissions problem. Fixed 2026-08-15.

**A load-sensitive test bound, worth knowing before you chase it.**
`pkg/inproc`'s `TestKillReportsTheSameExitShapeAsASignalledSubprocess` has a
hardcoded 5s wait for the turn to go in flight (`runner_test.go:1015`) and
failed once during a `make check` that ran 3× its usual wall time under
concurrent `go test` processes. It failed at the SETUP barrier ("tool never
started"), *before* `Kill` is called — so it is not the kill-path class CLAUDE.md
warns is never a flake. Passes 5/5 standalone and in two clean `make check` runs.
Consider a barrier instead of a timeout if it recurs.

**The other 28 unconfirmed findings.** Reported by review agents, not yet
verified by reading the code — treat each as a lead, not a fact.

*`pkg/execpool` (A):*

- [ ] **A4** `dial.go:106`, `transport.go:44-49` — `ServeInverted` passes
      `context.Background()` to `http2.ServeConnOpts` and never watches the
      caller's ctx, so SIGTERM does not shut the executor down. It sits through
      the whole termination grace period and is SIGKILLed, taking running jobs.
- [ ] **A5 — CONFIRMED 2026-08-15.** `dial.go`'s `writeHello` decodes rafikid's
      hello response through `bufio.NewReaderSize(conn, 4096)` and then calls
      `ServeInverted` on the same conn. That is the exact mirror of the rule the
      daemon side follows byte-at-a-time and CLAUDE.md documents: if the response
      and the HTTP/2 client preface arrive in one segment the buffered reader
      swallows the preface and the connection dies mid-frame. Saved today only by
      TLS record framing. Note the join-path `Describe` timeout added for A2 now
      converts this from an indefinite hang into a bounded, logged failure — an
      improvement, not a fix.
- [ ] **A6** `dial.go:38-55` — reconnect backoff never resets after a successful
      session, so an executor that flapped once waits the full 30s forever after.
- [ ] **A7** `pool.go:482` — `executorID[:12]` slices unconditionally on an error
      path; the guarded `shortID` exists at `executor_select.go:221`. Same raw
      slice at `workspace_wiring.go:120,149,177`.

*`cmd/rafikid` authority (B):*

- [ ] **B5** `limits.go:141-161,281-289` — the concurrency cap binds one
      generation only; a child granted `max_children=5000` is never re-checked
      against its ancestor's cap. Note `05-limits.md`'s own table lists
      concurrency's bound as "—", so this may be a knowingly accepted gap.
- [ ] **B6** `controller.go:688` vs `:833` — TOCTOU between `checkSpawnLimits`
      and `st.Insert`, spanning a process spawn. `agentloop` runs a turn's tool
      calls through `errgroup` with `SetLimit(6)`, so six `agent_spawn` blocks in
      one turn all read `live=0` and all proceed.
- [ ] **B7** `limits.go:88,145,186` — all three limits key off
      `req.ParentChildID`, and the control socket binds no per-connection
      identity, so a child with a shell can run
      `rafiki create --max-children 999` with no `--parent`. **Caveat: if the
      control socket is an operator-only trust boundary this is by design.**
      Settle it by deciding whether children should reach that socket at all.
- [ ] **B8** `agent_runtime.go:166-167` — a failed executor re-selection is
      swallowed and the unscoped client kept, so tools run on the executor
      *outside any workspace* and the child gets no `rafiki/workspace` label.
      Race-only, but the failure direction is "lose confinement silently".
- [ ] **B9** `agent_spawner.go:220-222`, `agent_runtime.go:168-178` — two
      refusal paths leave live state: prompt-delivery failure returns an error
      after the child is registered; and a `child.Spawn` failure after successful
      provisioning leaks a live remote container plus a `wsLabels` entry.
- [ ] **B10** `executor_select.go:180-215` — `explainNoMatch` discloses the full
      executor inventory (ids, `Admits`, labels) to an agent that matched
      nothing, including machines its parent's set excluded.
- [ ] **B-misc** `budget_sweep.go:49` — `budgetBreached` is referenced nowhere.

*`pkg/eventbuf` and `pkg/tasks` (C):*

- [ ] **C3** `memory.go:250` vs `postgres.go:244` — `Assign` returns
      `Task{Handle: ""}` from memory and a populated handle from Postgres. Latent
      (the caller discards it) and untested.
- [ ] **C4** `buffer.go:336` — `redepositLocked` demotes keyed fragments to
      unkeyed, losing last-write-wins and emitting older fragments *after*
      fresher ones. **Reachable only via `PushNow`, which has zero callers
      today**; wiring up one turns it on.
- [ ] **C5** `buffer.go:66,294,338` — `pendingDelivery` can never be set to
      `DeliverSteer` (every steer sets `forced = true`), so the "sticky steer"
      invariant describes behaviour the code cannot produce. Dead invariant plus
      a live footgun for the obvious future change.
- [ ] **C6** `memory.go:124` — the memory store shares `Metadata` maps with
      callers where Postgres hands out fresh ones, so a caller mutating a
      returned `Task.Metadata` permanently edits the in-memory ledger.
- [ ] **C7** `buffer.go:329-334` — `redepositLocked` can resurrect a `bufKey`
      that `Forget` just deleted, stranding it with no timer for the daemon's
      lifetime. Blocked today by `childIsBusy` returning false for exited
      children. Fix as an invariant: do not create a key that does not exist.
- [ ] **C8** `memory.go:177-183` vs `postgres.go:150-152` — `Drop` checks
      `ctx.Err()` and `reason == ""` in opposite orders, so a cancelled context
      with an empty reason yields a different error from each store.

*`pkg/executor` (D):*

- [ ] **D4** `server.go:156`, `jobs.go:204-213` — `Release` calls `killAll()`,
      which takes no workspace argument and kills **every background job on the
      executor across all children**. B's build dies when A's workspace is
      released, and B's `bash_output` returns `Found: false`, indistinguishable
      from "reaped".
- [ ] **D5** `jobs.go:208` — `killAll` signals the process, not the process
      group, though `kill` at `:167` does it correctly. Teardown leaves orphaned
      `node`/worker trees holding ports. Same bug `128cf44` fixed elsewhere.
- [ ] **D6** `container.go:78-80` — the container runs as **root** when
      `user.Current()` fails (empty error branch), reachable with CGO disabled on
      NSS/LDAP hosts. Files in the RW `/work` mount become root-owned on the
      host. `os.Getuid()` cannot fail and is not used.
- [ ] **D7** `container.go:71-76`, `workspace_wiring.go:59-64` —
      `MemoryBytes`/`Cpus` are conditional and never populated, so **no memory or
      CPU cap is ever applied**. `--pids-limit 512` is present.
- [ ] **D8** container orphans, four compounding causes: `Release` is a silent
      success no-op after an executor restart (in-memory registry only); no
      startup reaper exists; `executorID` is regenerated per process so
      label-based reaping could not work anyway; and
      `dev.graveland.rafiki.child` is always empty because
      `workspace_wiring.go:60` hardcodes `ChildId: ""` with `// set by caller`
      and no caller sets it.
- [ ] **D9** `jobs.go:166-175`, `container.go:113-123` — `bash_kill` and the
      `Execute` deadline cannot reach a process inside a container: the
      supervised process is the `docker exec` CLI client, and killing it does not
      stop the workload.
- [ ] **D10** `server.go:200-211` — background jobs return before the semaphore
      is acquired and `jobRegistry` caps nothing, so `bash_start` in a loop is
      unbounded processes plus 100 KB of retained ring each.
- [ ] **D11** `server.go:69-75` — one `FileTracker` shared by every workspace and
      child, so A's read satisfies B's read-before-write interlock and A's write
      makes B's next write report a phantom modification. **Folded into the fix
      plan's task 4.2** (same constructor).
- [ ] **D12** `container.go:65` — `--network` is taken from the request with no
      allowlist; an omitted value normalizes to bridge (full egress) rather than
      failing closed. `req.Mounts.HostPath` is likewise used verbatim.
- [ ] **D13** `server.go:397-422` — jobs are not bound to a workspace, so any
      leaked handle is readable and killable by any caller.
- [ ] **D14** `jobs.go:103-105` — two jobs can collide on a `CallId` handle,
      orphaning the first with its process group unreachable.
- [ ] **D15** `main.go:121-133` — the socket liveness pre-flight is TOCTOU (the
      0600 umask-before-bind half is correct).
- [ ] **D-misc** `server.go:133` `Provision` discards the request deadline;
      `container.go:104` `buildRunArgv` returns nil for an out-of-mount workdir
      so the operator sees docker usage text; `server.go:481-485` both branches
      return `(out, nil)` so a `docker exec` failure reaches the model as
      ordinary command output.

If the findings doc is gone, the method reproduces all of this: four
`code-nitpicker` subagents over `pkg/execpool`,
`cmd/rafikid/{limits,agent_spawner,executor_select}.go`,
`pkg/eventbuf`+`pkg/tasks`, and `pkg/executor` — each briefed with the project's
actual shipped failure modes and told to report only what it can name a line for.

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
