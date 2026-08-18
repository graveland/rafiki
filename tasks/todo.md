# rafiki — long-term backlog

Durable, always-current. Add to it whenever something is deferred rather than
done; delete entries when they land.

**This file IS tracked in git, despite `.gitignore` listing `/tasks/`.** It
predates that rule, and gitignore does not affect an already-tracked file — see
the comment at `.gitignore:46`, which says so deliberately. Verify rather than
trust either claim: `git ls-files tasks/`. So this file survives a fresh clone
and a `git clean -xdf`, it shows up in diffs, and it should be committed
alongside the work it describes. (Two earlier versions of this header got this
backwards in both directions; checked again 2026-08-17.)

`docs/plans/` is genuinely ignored. Anything load-bearing in a plan doc — a
design decision, a supersession — must be duplicated here, or promoted into
`CLAUDE.md`, `README.md` or `docs/reference/`.

---

## Open now

### Executor model: rafiki no longer launches containers (2026-08-17)

A container running `rafiki executor serve` IS a container executor. Every fact
that gates access comes from the row, set at mint time. `--isolation`,
`--workspace-mode` and `--image` are gone; `--root` is a working directory, not
a sandbox. Container detection must never be added — it is self-assertion of a
gating fact.

Deleted: containerBackend, the in-container tool server, pkg/workspace, the
image contract, all docker-gated tests. B4 (model-chosen cwd becoming a RW bind
mount) died structurally: nothing model-facing becomes a mount. D1's bug class
died too — there is no host/container split inside an executor any more.

Accepted cost: isolation granularity equals executor granularity. Children on
one container executor are isolated from the host but not from each other; per
child isolation means one executor per child, which is a `docker run` or k8s Job
concern.

Also deleted: `rafiki executor serve-stdio` and `execpool.NewStdioConn`. The
stdio transport existed because rafiki started a network-less container and had
to reach a tool server inside it; with no container to start, it had no callers.

**The row is authoritative only if something READS it, and the first cut of this
work read the self-report everywhere.** Three consequences, all fixed, all
invisible to a green test run:

- `provisionWorkspace` built the child's `WorkspaceInfo` from `resp.Isolation`,
  which is now empty for every executor, so `BuildSystemPrompt` dropped the
  "Your machine" block for every sandboxed worker. It reads the row through
  `workspaceInfoFromRow` now.
- The `rafiki/workspace-mode` label was a hardcoded `"pinned"`, which made
  `HandleExecutorLost` fail every child and left `tryReschedule` unreachable.
  The label comes from the row's `workspace_mode`.
- Nothing matched a child's requested mode against the executor it landed on —
  the Provision-side check was deleted with the backend and not replaced — so an
  inherited `ephemeral` silently accepted a pinned machine. `narrowByWorkspaceMode`
  enforces it during selection, where the rest of the grant is enforced.

- [ ] Follow-up: remove `Mount`, `mounts`, `network` and `workspace_mode` from
      `proto/rafiki/executor/v1/executor.proto`, and `isolation` from
      `ProvisionResponse`, then run `make proto`. All are unset by the daemon
      and ignored by the executor. Left on the wire because regenerating needs
      buf/protoc.

### ▶ WORKING PLAN: background bash on the executor (agreed 2026-08-18)

`bash_start` had a confirmed hang and three divergences from foreground `bash`.

- [x] A1. `jobRegistry.start` sets no `WaitDelay`, so `cmd.Wait()` blocks forever
      on any command leaving a grandchild on the pipe (`npm run dev &`). The job
      never reports exited, `RunningHandles` counts it forever, `purge` is never
      scheduled, and the goroutine leaks. Verified with a standalone repro.
- [x] A2. Exit bookkeeping unwraps `*exec.ExitError`, but a job that exits 0
      with a lingering grandchild returns `exec.ErrWaitDelay` instead — recorded
      as exit code -1 for a job that succeeded. `ProcessState.ExitCode()` is
      correct in all three cases (verified) and simpler.
- [x] B. Background output is a 100 KB in-memory ring that drops oldest bytes
      permanently, violating "spill, never destroy" on the one path where output
      is unbounded. Replace with a per-job file, drop-oldest at 8 MB, whose path
      the reader is told when bytes were dropped — the same shape
      `OutputPolicy.Clip` already uses for foreground results, and reachable by
      the model through `read`/`grep` on the same executor.
- [x] B2. Retention is a 10-minute timer from job exit, which is meaningless for
      an async agent: a turn can end and resume hours later. No timers. Output
      lives until the workspace is released, bounded by a per-workspace BYTE
      budget (256 MB) with oldest-finished evicted first.
- [x] C. The executor's registry passes no `RTK` mode, and `RTKMode("")` is not
      `RTKOff`, so every executor silently rewrites through rtk with nobody
      having chosen it. Make it an explicit `Options.RTK` + `--rtk`, default
      `auto` (today's effective behaviour, now a decision).
- [x] C2. Background never uses rtk and must not: the refusal fallback works by
      inspecting stderr after exit and re-running, which a long-running job
      cannot offer. Document at the site.

**Not doing:** capping concurrent job count; routing background through
`bashTool`; SIGTERM before SIGKILL.

**Done 2026-08-18.** The memory ring is gone rather than supplemented: with
reads capped at 100 KB either way, one file with drop-oldest does everything the
ring plus a spill copy would have, at less code and near-zero memory. The
registry now builds the command itself, so the background path cannot drift from
the foreground one again — it had been running `sh -c` for a workspaced job and
`bash -c` for a bare one. Also removed three dead `--isolation`/`--workspace-mode`/
`--image` flag registrations left on `executor service install` when `serve` lost
them.

- [ ] Follow-up: an unreleased workspace retains its jobs until the executor
      restarts. That is inherited, not new — a leaked workspace already leaks its
      `wsReg` entry — and the byte budget bounds what it costs. Fix leaked
      workspaces as their own thing rather than adding a timer back here.

### ▶ WORKING PLAN: make the executor functional (agreed 2026-08-17)

**Goal:** a container-isolated executor that actually works, and the binary shape
settled as one `rafiki` (client + executor) and one `rafikid` (server).

**Settled design decisions.** These **supersede** parts of
`docs/plans/2026-08-15-in-container-tool-server-plan.md`, which is gitignored. If
that file is gone, these are the decisions:

- **The workspace image BAKES `rafiki`. Nothing is copied into a container.**
  The plan's **D-2** (`docker cp` a statically-linked linux binary in at
  Provision) and its entire **Task 2** (`make build-executor-linux`, a
  `--container-executor-binary` flag, startup location logic, a
  missing-artifact refusal, and the "can't be `/proc/self/exe` on a macOS
  host" cross-compile problem) are **deleted**.

  The repo's existing `./Dockerfile` already *is* the reference image:
  `debian:trixie-slim` with `ripgrep`, `rtk` pinned to a version and
  checksum-verified, a non-root uid-1000 `rafiki` user, multi-arch via
  `TARGETARCH`, and both binaries built in-image with `golang:1.26` — so the
  cross-compile problem never arises. D-2's justification was preserving
  arbitrary operator-supplied images, but **D-3 already mandates ripgrep and
  validates it at Provision**, so that freedom was already spent; paying for it
  again with a per-provision copy plus ~30 MB in every container's writable
  layer (versus one shared read-only image layer) is strictly worse.
- **One `rafiki`.** `cmd/rafiki-executor` folds into `rafiki executor serve`
  (host-side reverse-dial) and `rafiki executor serve-stdio` (in-container). A
  third `cmd/rafiki-toolserver` was considered and **rejected**: it existed only
  to keep a copied payload small, and there is no payload now. Baking `rafiki`
  also makes it available interactively inside the container for debugging.
- **Two things baking must get right**, both silent when wrong:
  1. *Version skew.* `docker cp` accidentally guaranteed the inner server
     matched the host executor; baking does not. Provision must run the inner
     hello/`Describe` and refuse on protocol mismatch, **naming the image**.
     This replaces D-2's missing-artifact refusal rather than adding work.
  2. *Never rely on the image's `USER`.* `/work` is a host path owned by the
     host user, so the container uid must match or every write fails.
     `docker run --user` (already derived from `user.Current()` at
     `container.go:78`) overrides the image's `USER 1000`, and the baked
     binaries must stay world-executable. Worth a test.
- **D-4 still holds, and is the single most likely thing to be undone later:**
  on a container workspace there is **no host fallback, ever**. If the inner
  server cannot start or has died, `Execute` fails.

**Steps, in order. Each is independently committable and `make check`-green.**

- [x] **0.** DONE 2026-08-17. `make check` on `fix/review-findings-2026-08-15`, then
      `git merge --ff-only` into `main`. Confirmed linear
      (`git merge-base --is-ancestor main HEAD`). **No push.** Eight finished
      review fixes; every step below builds on them.
- [x] **1.** DONE 2026-08-17. Reproduced first: the un-skipped test read this
      machine's real `~/.ssh/id_rsa` (`-----BEGIN OPENSSH PRIVATE KEY-----`),
      `/etc/passwd`, an out-of-mount secret, and wrote a file outside every
      mount — 10/10 escape cases plus the symlink. All refused now; `bash` and
      its three container tests unaffected.
      In-container plan **Task 0 — fail closed.** A few lines in
      `pkg/executor/server.go`: on a container workspace the five non-`bash`
      tools REFUSE rather than running on the host. Prove it by un-skipping
      `TestFileToolsCannotEscapeTheWorkspace` and
      `TestASymlinkOutOfTheWorkspaceIsRefused` in
      `pkg/executor/workspace_tools_test.go`. Breaks nothing that works: the
      only calls that currently succeed on that path are the escaping ones.
- [x] **2.** DONE 2026-08-17. Dropped the tag rather than teaching the Makefile
      `-tags` (the tag bought no isolation — both files call `bootDaemon`, same
      as the untagged tests). `go vet ./...` then failed for the first time on
      `grant_test.go:200`, which is the gate starting to tell the truth. Both
      formerly-invisible tests — `TestGrant_NativeNarrowing` and
      `TestSubagentLineagePersistence`, the only two in those files — now
      compile and PASS, so nothing had regressed behind the tag. `make
      build-linux` cross-vets them too. No `go:build integration` remains in
      the repo.
      Unbreak the build-tag gate — task 1 of
      `docs/plans/2026-08-14-executor-e2e-and-tag-gate-plan.md`.
      `test/integration/grant_test.go` has not compiled since `0acadf2` moved
      `executors.NewPostgresStore` to `pkg/executorsdb`, and
      `//go:build integration` hides that from both `go vet ./...` and
      `go test ./...` (`subagent_test.go` shares the package, so phase 04's
      e2e has not run either). Convert to a loud runtime skip. **Before** the
      container work: it is the only thing that will notice a break in the code
      steps 4–5 rewrite.
- [x] **3.** DONE 2026-08-17 (bca0a41). Split `pkg/tasks` → `pkg/tasksdb` (postgres store out; the
      interface and memory store left DB-free), then add
      `TestExecutorDoesNotLinkPostgres` mirroring
      `cmd/rafiki/no_postgres_test.go`.

      Today `cmd/rafiki-executor` links pgx via
      `pkg/executor` → `pkg/fundi/tools` → `pkg/tasks`, and `pkg/execpool`
      reaches it the same way — dragging in `pkg/agentloop`, `pkg/capture`,
      `pkg/llm`, `pkg/store` and `pkg/rawtrace` too, none of them reachable
      (the executor registers only `ExecutorLocalTools()` via
      `MaterializeOnly`). Exactly the failure mode the client's own guard
      describes: *"Go imports whole packages."* Not a security issue — nothing
      calls it and there is no DSN on that host — but it is 12 MB of dead
      weight, it couples the executor's build to pgx, and it is a **hard
      prerequisite for step 4**: `rafiki` linking `pkg/executor` would trip
      `TestClientDoesNotLinkPostgres`.

      Also fix the now-false CLAUDE.md claim that `cmd/rafiki-executor`
      "must not link pgx" — the sentinel move it justifies is still right on
      its own merits, but the stated reason does not currently hold.

      **SCOPE CORRECTED 2026-08-17 — this is not one file move, it is four
      edges, and the estimate of "mechanical" was wrong.** Measured with
      `go list`, the packages that import pgx *directly* are: `cmd/rafikid`,
      `pkg/agentcli/local`, `pkg/capture`, `pkg/ejection`, `pkg/executorsdb`,
      `pkg/fundi`, `pkg/insights`, `pkg/llm`, `pkg/rawtrace`, `pkg/server`,
      `pkg/store`, `pkg/tasks` (and, until `0454bf2`, `pkg/fundi/tools`). The
      executor reaches pgx by four routes, not one:

      1. ~~`ToolOpts.Pool` in `registry.go` — a dead field, declared and read
         by nothing.~~ **DONE `0454bf2`**, 2 lines. `pkg/fundi/tools` no longer
         imports pgx directly.
      2. `pkg/tasks/postgres.go` → `pkg/tasksdb`. Mechanical, but harder than
         the `executors`/`executorsdb` precedent: `pkg/tasks` has BOTH a memory
         and a postgres store plus a shared `conformance_test.go` that runs
         against both, so the suite must be extracted into an importable
         `pkg/tasks/tasktest` helper package first. (`pkg/executors` had no
         second implementation, so its split needed none of this.)
      3. `pkg/agentloop` → `pkg/llm`, which holds a `*pgxpool.Pool` field with
         `WithStore(pool)` and a `Pool()` accessor. This is a dependency
         inversion, not a move: `pkg/llm` needs to take a capture interface it
         defines rather than a pool. 2 production call sites
         (`cmd/rafikid/proxy.go:174`, `pkg/fundi/config.go:160`) + 5 test sites.
      4. `pkg/agentloop` → `pkg/store` directly. Another DB edge.

      **The structural answer is probably not any of these individually.** It
      is that `pkg/executor` should not import `pkg/fundi/tools` at all —
      `ToolOpts` is one struct carrying every tool's dependencies and
      `DefaultBlueprint` references every blueprint, so importing the package
      for `read`/`write`/`edit`/`glob`/`grep`/`bash` drags in the entire agent
      runtime. Splitting the six executor-local tools + the Registry/blueprint
      machinery into a package free of the parent-side dependencies is the same
      "split by dependency" move as `rafiki`/`rafikid`,
      `executors`/`executorsdb` and the `pkg/routing` store extraction. It is
      also the single biggest piece of work in this plan, and it is better
      informed AFTER step 5, when it is clear what the executor-local tool
      package actually has to contain.
- [x] **4.** DONE 2026-08-17 (a50c87d). Fold `cmd/rafiki-executor` into `rafiki executor serve` /
      `rafiki executor serve-stdio`, and add it to `./Dockerfile`. Before the
      container work, so the image contract and the `docker exec` argv name the
      binary exactly once. `rafiki executor` already hosts the operator verbs
      (`enroll`/`list`/`label`/`disable`/`enable`); `serve` does not collide
      with them.
- [x] **5.** DONE 2026-08-17. **Finding D1 is closed.** Every tool on a
      container workspace now runs inside it, through `rafiki executor
      serve-stdio` reached over `docker exec -i` stdio. The acceptance test
      that had been skipped since the review
      (`TestFileToolsWorkOnWorkspacePaths`) passes, and `pkg/executor` runs
      26 pass / 0 skip / 0 fail under `-race`.

      Commits: ea7e424 (Task 1, stdioConn + serve-stdio), ad2efcd (Task 3,
      baked image + contract validation), 54342d2 (Tasks 4-5, inner server at
      Provision + route every tool), fda516d (Task 6, background jobs inside +
      finding D4), 451d0cd (Task 7, D11 + no-fallback + docs).

      Three things worth keeping:
      - The plan's "biggest risk" (whether HTTP/2 needs connection deadlines,
        which a pipe pair cannot provide) is closed by reading x/net/http2
        v0.55.0 call site by call site rather than guessing: every conn
        deadline call is guarded by a timeout we never set, and the Transport
        never touches them at all. The net.Pipe fallback is unnecessary.
      - `read /etc/passwd` on a container workspace now SUCCEEDS and is NOT an
        escape — it is the container's own. Several cases in the escape test
        stopped being escapes and had to be rewritten rather than re-run; the
        strongest replacement is positive proof of placement (bash creates a
        file outside every mount, read sees it, the host does not).
      - Refusals are now kernel errno text, not policy messages. Asserting on
        their wording is asserting on libc.

      Superseded plan text: Task 1 DONE
      (ea7e424: `stdioConn` + `serve-stdio`; the deadline risk the plan called
      its biggest is closed — x/net/http2 never sets conn deadlines with our
      config, verified call site by call site, so the net.Pipe fallback is
      unnecessary). Task 3 DONE (ad2efcd: baked reference image + Provision-time
      contract validation). **Tasks 4-7 remain: start the inner server at
      Provision, route every tool through it, background jobs inside, then D11
      and docs.** against the baked
      image (Task 2 deleted per above): `stdioConn` as a `net.Conn` over
      `docker exec -i`; a workspace stage in the Dockerfile plus Provision-time
      image-contract validation (`rg` present, inner protocol version matches);
      start the inner server at Provision; route **every** tool through it,
      retiring `executeBashInWorkspace` and the `docker exec` path with it —
      one route, not two; background jobs inside the container (which also
      dissolves **D4**, `Release` killing every job on the executor); and
      **D11** dissolves for free, since each container gets its own
      `FileTracker`. Fold in **D6** while in `container.go:78` — it runs the
      container as **root** when `user.Current()` fails.
- [ ] **6. NEXT.** Fill `TestExecutorPool_FullLifecycle` and
      `TestExecutorPool_Narrowing` in `test/integration/executor_pool_test.go`
      — empty bodies carrying stale `t.Skip("not yet implemented (plan-07
      scope)")` reasons, though the listener has been wired since
      `cmd/rafikid/main.go:441`.

**Deliberately out of scope:** **B3** below stays open; it is real and does not
block a functional container executor. B4 and D3 are closed — the executor-model
change deleted the mechanism each one exploited.

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

**Three unconfirmed findings worth promoting on their own merits** (B4 was
confirmed, then closed structurally — see below):

- [ ] **B3** — `effectiveExecutorSet` evaluates the `Admits` half against the
      *leaf's* labels only, and at placement against the *parent's* labels
      rather than the child being placed. The phase-07 "Admits is never
      evaluated" fix is therefore half-done.
- [x] **B4 — CLOSED 2026-08-17, structurally.** `agent_spawn`'s model-facing
      `cwd` flowed `SpawnSpec.Cwd` → `SpawnRequest.Cwd` → `workspace.Derive`
      with no containment check, and Derive turned it into the read-write
      `/work` mount — so `agent_spawn(cwd="/", workspace="ephemeral")`
      bind-mounted the executor host's root filesystem RW. The planned fix was a
      containment check against the spawner's cwd. It was not needed: rafiki no
      longer derives mounts at all, `workspace.Derive` is deleted, and the
      daemon does not send a workdir. Nothing model-facing reaches a mount,
      because nothing composes one.
- [x] **D3 — CLOSED 2026-08-17, structurally.** A workspace-less `Execute` ran
      on the host even when `Isolation == "container"`, and `executorclient`
      never set `WorkspaceId`, making that the default for every
      `--executor-socket` spawn. There is no host/container split inside an
      executor any more: every call runs in the executor process, wherever the
      operator started it, workspace or not.

**Found while fixing the above, not in the review** (all verified, all small):

- [x] **`Server.Release` kills background jobs across ALL workspaces.** Fixed:
      `releaseWorkspace(id)` replaced `killAll()`, and it signals the process GROUP.
- [x] **`TestReleaseRemovesTheContainer` passes vacuously.** Moot: the test and
      the container backend it exercised are both deleted. The double-prefix
      trap is worth remembering if a `docker ps` filter is ever written again —
      `containerNameFor` returned `"rafiki-ws-" + workspaceID` when the
      workspace id already WAS `"rafiki-ws-" + randomID()`, so the filter
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

~~Related and worse: `test/integration/grant_test.go` has not compiled since
`0acadf2`~~ — **fixed 2026-08-17** (task 1 of the plan below). The tag is gone
from both files, the call points at `executorsdb.NewPostgresStore`, and
`TestGrant_NativeNarrowing` + `TestSubagentLineagePersistence` both pass. They
had not regressed; they had merely stopped being compiled.

- [ ] Plan: `docs/plans/2026-08-14-executor-e2e-and-tag-gate-plan.md`. Task 1
      done; **tasks 2 and 3 remain** — fill the two empty pool tests. That is
      step 6 of the working plan at the top of this file.

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
