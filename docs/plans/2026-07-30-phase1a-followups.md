# Phase 1a follow-ups

Everything the phase-1a branch (`phase1a-inproc`) knowingly left undone. Recorded because
the reasoning is worth more than the list: each item says why it was deferred, so a future
reader can disagree with the judgement rather than rediscover the problem.

Nothing here blocks the merge. The two items under "Needs a decision" are not defects —
they are choices nobody has made yet.

## Needs a decision (not mine to make)

### 1. `Child.Shutdown`'s terminal rung is no longer a guaranteed reap

`internal/child/child.go` does an unbounded `<-c.done` after `runner.Kill()`. For a
subprocess that is safe: SIGKILL always lands. For an in-process child, `Kill()` cancels a
context and closes two pipe ends, which is sufficient for the wedged-*write* case it was
written for — but `eng.Wait()` can still block on a genuinely wedged turn. So `fundi kill`
can hang, `ShutdownAllChildren` returns at its 180s bound with the goroutine still live, and
because `pool.Close()` waits for every acquired connection, a straggler doing non-`ctx`-derived
work can extend daemon exit until the service manager SIGKILLs it.

`main.go` already mitigates by calling `baseCancel()` before `pool.Close()`, but that is only
as good as the straggler's context discipline.

**The decision:** bounding the wait means choosing to leak a goroutine rather than hang the
daemon. That is a real trade, not a cleanup, which is why it was left out of the fix wave.
Whoever decides should also decide what `ShutdownResult` reports when the bound expires.

### 2. `internal/agent/faketurns.go`'s fake sender ignores its context

`fakeSender.New(_ context.Context, ...)` discards the context, and rafiki's `agentloop.drive`
never checks `ctx.Err()` between iterations. After an abort, the loop therefore runs one more
context-blind iteration, the fake sender answers with the next scripted message, and the turn
completes with `err == nil`.

Two consequences:

- **`Engine.runTurn`'s abort branch is never executed by any test** — including
  `RepairOrphans`, whose whole job is to stop the *next* API call being rejected for a
  dangling `tool_use`. This is the one subsystem whose own comment calls in-band abort "this
  project's entire reason for existing."
- `TestIntegration_AgentKind_AbortPreservesProcess` flakes as a side effect. Measured over 10
  runs each: **4/10 fail on this branch, 6/10 on the merge base** — pre-existing, marginally
  improved here, and nowhere near "intermittent". CI is red about half the time, which means
  a genuine regression in the next branch would be indistinguishable from noise.

**The fix is three lines and fundi-side, not upstream:** have `New` take the `ctx` and return
`nil, err` when `ctx.Err() != nil`, mirroring what a real HTTP sender does. `agentloop.Run`
then returns an error on the second iteration, the abort arm fires, `RepairOrphans` runs, the
scripted message is not consumed, and the abort path becomes testable for the first time. The
`drive`-checks-`ctx.Err()` variant is more correct but lives in rafiki, and is not needed for
either benefit.

## Real, one line each, deferred only because there was no second fix wave

### 3. `c.stdin` is still never closed on the ordinary self-exit path

`handleChildExit` does not call `Child.Shutdown` — the only callers are `Controller.Kill` and
`ShutdownAllChildren`. So a child that ends on its own (build error, engine-fatal, frontend
EOF) has stdout and stderr closed by `supervise` but leaves the stdin write end to an
`os.File` finalizer. This is precisely the case the FD fix's own rationale named as newly
common for in-process children.

`supervise`'s `cleanup:` block is the symmetric home for it — it is the only writer to
`c.stdin` and has already left the write loop. A third `closeStream(c.ID, "stdin", c.stdin)`
there closes the class for both runners. Note FD exhaustion does not trigger a GC, so
finalizers are not a backstop under real pressure.

### 4. Standalone `fundid agent` keeps the wedge the daemon now rejects

`cmd/fundid/agent.go` builds `RuntimeOptions` without `OnFatal`, so a turn panic marks the
engine dead, logs, and leaves the process answering `get_state` forever while every prompt is
silently dropped — the exact silently-stopped-queue shape the daemon path was fixed to avoid.
A nil `OnFatal` is documented as acceptable, but here it is a choice rather than a constraint.

### 5. `fatal()` emits `agent_error` before calling `OnFatal`

If the daemon has stopped reading stdout, `Frontend.Emit` blocks inside the very path whose
job is to end the child, so the child wedges until an external `Kill` closes `stdoutR`. Narrow
in practice — 64 KB of kernel buffer plus a continuously-reading daemon — but the ordering is
the reverse of the safe one.

## Deferred, low value

- The dropped-forwarded-env `WARN` lists the caller's near-entire environment on every agent
  spawn (`--forward-env` defaults on), so a ~100-name line at WARN will bury real warnings
  from day one. One line with a count at WARN and the names at Debug is the better shape.
- `agentSpawnHasExplicitDB` matches `strings.TrimLeft(tok, "-")` against `db`, so a literal
  `db` in a *value* slot (`--name db`) trips the rejection with a misleading message. Fails
  loud, so it is safe; include the offending token in the error when touching it.
- `Interrupt()` before the engine is built silently no-ops, and the window includes
  `ConnectMCP`, so it can be seconds rather than microseconds. An `slog.Info` would leave a
  trace for a dropped abort.
- `SpawnSpec.EnvOverride` is inert for agent children, and `RespawnChild`'s spec is the only
  one of the three that omits the field — currently harmless, but a latent divergence in the
  "all three spawn paths agree" property.
- `processRunner.Wait()` uses `cmd.Process.Wait()` rather than `cmd.Wait()`, so exec's
  parent-side pipes are also finalizer-reclaimed. Deliberately unchanged: switching would
  perturb the `ExitCode`/`Signal` contract this branch pinned.
- Two stale test comments (`resume_test.go` still says the second pool is "independent";
  `agent_kind_test.go`'s docstring opens loosely) and a dead second return value in
  `orphans_db_test.go`'s `dbTestPool`.
- The spawn-during-shutdown window: the UDS server keeps serving `ctrl_spawn` until
  `srv.Close()`, i.e. after `ShutdownAllChildren` has already enumerated live children. A
  `shuttingDown` flag checked in `Spawn` closes it.
- `Engine.worker()`'s loop body outside `runTurnGuarded`, and the daemon-side
  `supervise`/`readStdout` goroutines (whose `provider.Parse` runs over child-supplied JSON),
  have no recover. The latter is pre-existing for pi and claude.

## Pre-existing, surfaced by this work, not caused by it

- **`forget --help` and `kill --help` both claim disk artifacts are untouched**, while
  `forgetOne` calls `persist.DeleteRecord`, `deleteLogDump` and `deleteSpillDir`. Reproduced
  live: `fundi kill` deletes a child's log dump before `fundi logs --err` can read it, so you
  cannot read a child's stderr after a normal kill. Either the help text or the behaviour is
  wrong; someone should decide which.
- **`switch_session` on a claude child passes a pi session *path* to `--resume`**, which
  expects a claude session id. Unchanged by this branch, but newly reachable now that
  `RespawnChild` resolves the kind correctly.

## Worth keeping from how this was built

- **Prove a guard fails before trusting it.** Five tests in this plan asserted nothing until
  someone deliberately broke the code and watched: two written wrong, one silently defanged by
  the change itself (`0 != 0` after `PID()` started returning 0), one passing on ambient
  environment, and one measuring GC timing instead of the behaviour it named. Every fix round
  that mattered ended with an invert-then-revert transcript.
- **Audit the channel, not the symptom.** The dropped-API-key Critical was fixed once, then
  the same class was found again in `req.Env`. Enumerating all four of `buildEnv`'s payloads
  closed it properly.
- **A wrong comment near subtle lifecycle code is a live hazard here.** One convinced an
  implementer that a task was impossible and cost an abandoned attempt; another would have led
  a reader to reintroduce an FD leak on the panic path. Both were treated as must-fix, and
  should be again.
