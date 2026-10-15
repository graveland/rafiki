# Agent schedules — CronCreate / CronDelete / CronList / ScheduleWakeup

How a rafiki child (or its owner) asks the daemon to run a prompt later — once
or on a recurring schedule. A schedule is a durable row plus a timer in the
daemon's scheduler; the row is the truth and the timer is a hint, so every path
re-reads the row before acting and a stale timer is a wasted wakeup, never a
bug (`cmd/rafikid/schedules.go`; the algebra is `pkg/schedules`).

## Tools

Four agent-facing tools, registered in `pkg/fundi/tools/schedule.go`
(`CronCreateBlueprint` et al., `DefaultBlueprint.Register`) and exposed to
claude children over the MCP face as `mcp__rafiki__CronCreate`,
`mcp__rafiki__CronDelete`, `mcp__rafiki__CronList` and
`mcp__rafiki__ScheduleWakeup` (`mcpBlueprints`, `cmd/rafikid/mcp_face.go`; the
MCP server name is `rafiki`, `pkg/proxyenv/claude.go` `mcpServerName`).

- **`CronCreate{cron, prompt, recurring=true}`** → a job id. A
  `durable` field is accepted and ignored. The cron expression is standard
  5-field (`minute hour day-of-month month day-of-week`), evaluated in the
  zone captured at creation; `seconds`, `@hourly`/`@every` descriptors, and a
  `TZ=`/`CRON_TZ=` prefix in the expression are all refused
  (`pkg/schedules/cron.go` `Parse`, `fiveFieldParser`).
- **`CronDelete{id}`**, **`CronList{}`** — the caller's own jobs only.
- **`ScheduleWakeup{delaySeconds, prompt, reason, noop, stop}`** — a one-shot
  keyed to the child; each call replaces the pending wakeup and `stop: true`
  cancels it. `delaySeconds` is clamped to 300–3600 s (`schedules.ClampWakeup`,
  `MinGap`/`WakeupMax`). `reason` and `noop` are accepted for schema parity and
  not used.

The caller is bound at construction: `newScheduleBinding(ctrl, id.ChildID)`
closes over the child id and no tool method takes one, so a call cannot address
another child's jobs (`cmd/rafikid/schedules.go`, `scheduleBinding`).

Constraints, enforced on every create:

- a recurring expression whose next 32 consecutive occurrences contain any two
  closer than 5 minutes (`schedules.MinGap`) is refused — `CheckMinGap`
  (`pkg/schedules/cron.go`) over `gapSamples = 32`
  (`pkg/schedules/schedules.go`), called from `CronCreate`
  (`cmd/rafikid/schedules.go`) only when `recurring` is true; an expression
  that never fires is `ErrNeverFires`.
- at most 50 open jobs per child of either kind (`schedules.MaxOpenPerChild`),
  count-then-insert atomic per child (`pkg/schedulesdb/postgres.go` `Create`,
  `pg_advisory_xact_lock`). A `ScheduleWakeup` tombstones the child's existing
  open wakeup first and that replaced row does not count toward the cap.
- jobs never expire; they end only through `CronDelete` (a tombstone) or when
  the owning child is closed.

## Timezone

A job's zone is resolved once at `CronCreate` and snapshotted onto the row
(`Job.TZ`), so editing a zone does not shift existing jobs — recreate to move
one. Resolution (`scheduleBinding.zone`, `cmd/rafikid/schedules.go`):

1. the child's owner's `users.timezone`, when the owner row names one
   (`schedules.Store.OwnerTimezone`); else — including when that lookup
   ERRORS (a store outage), not just when the row is absent or its zone NULL,
   the binding silently falls back —
2. the daemon's own zone, resolved once at startup (`schedules.DaemonZone`):
   `$TZ` (with a leading `:` stripped), else the IANA name after `zoneinfo/`
   in the `/etc/localtime` symlink target.

An unresolvable daemon zone is `ErrNoTimezone` — nothing silently falls back to
UTC. `CronCreate` then refuses, naming
`rafikid user set-timezone <name> <iana-zone>`; `ScheduleWakeup` still needs no
zone (it fires at a fixed instant) and degrades the snapshot column to `UTC`
rather than refusing the wakeup.

`users.timezone` is nullable `text`; NULL means unset, never `""`
(`time.LoadLocation("")` is UTC, which would silently give an unset user UTC).
Every stored zone is an IANA name — the empty string and Go's `Local`
pseudo-zone are refused (`schedules.ValidateTimezone`).

DST is pinned in `pkg/schedules/cron.go`: a wall clock skipped by
spring-forward does not fire that day, and a wall clock repeated by fall-back
fires once — `(*Spec).Next` steps past a candidate whose local date + hour +
minute equals `after`'s, the time the occurrence is asked from (`after` is the
spec's own previous occurrence at every re-arm, so the two agree in practice).

## Firing

A due job becomes an inbox fragment from the event-buffer source `schedule`
(`scheduleEventSource`, `cmd/rafikid/schedules.go`). `fireSchedule` re-reads the
row and its child's durable state first, so fire-time checks decide what
happens:

- **tombstoned row** → return, without re-arming (no tombstone call; the row
  is already closed).
- **missing or closed child** → tombstone and return, without re-arming.
- **child exited, or `child.daemon_id` is not this daemon** → return without
  re-arming and without touching the row; registration at the next resume picks
  it up.

Delivery is **push first, then advance**: the fragment goes into the event
buffer under the key `<id>@<due>` (`id + "@" + due.UTC().Format(time.RFC3339)`)
before the row moves. A crash between the two is a replay the key collapses
while the first fragment is still pending; advance-then-push would lose the
fire on the same crash. A recurring job then advances `next_fire_at` and
re-arms; a one-shot stamps `last_fired_at` and tombstones. A failed advance is
retried alone (`retryAdvance`) — never the push, which would deliver twice.

Catch-up is not a separate path: `Spec.CountThrough` counts the occurrences in
`(due, now]`, delivers ONE frame naming how many were missed, and advances to
the first occurrence after now.

## Kill, outage and close

Only live children register jobs, from the rows, at the point a child comes up
on this daemon (`registerSchedules`, `cmd/rafikid/schedules.go`):

| path | recurring | one-shot |
|---|---|---|
| daemon restart, auto-resume (catch-up on) | one wake naming N missed fires, then normal cadence | fires once |
| manual `rafiki resume` (catch-up off) | skips missed fires, advancing to the next FUTURE occurrence | fires once if overdue |
| child killed (status `exited`) | no delivery — Kill cancels no schedule timer, so an armed timer fires, sees the child exited, and returns without re-arming or tombstoning; the next resume re-registers | stays pending |
| killed child auto-closed by the grace window | rows tombstoned | tombstoned |
| child closed | rows tombstoned (`CloseForChild`) | tombstoned |

The grace window defaults to 7 days and is overridden with `RAFIKI_GRACE_HOURS`
(`NewController`, `cmd/rafikid/controller.go`); its timer is `armGrace`
(`cmd/rafikid/timers.go`). Closing a child tombstones its jobs
(`Controller.Close` → `CloseForChild`). The rationale: a daemon outage is not
the user's choice, so it delays the schedule; a kill is, so it pauses it — for
at most the grace window, after which the child closes and its jobs go with it.
A rate-limited child is not killed by the rate-limit path — the daemon
schedules an auto-resume for the reset instead (`cmd/rafikid/ratelimit_resume.go`),
so its fires follow the ordinary live-child rules: delivered while it is live,
and skipped-and-re-registered if its process has DIED (the auto-resume then
relaunches it via `Resume`). An operator Kill is different: it drops the pending
rate-limit watch, so a Killed child is not relaunched this way.

A scheduled fire into a `claude` child's inbox is an ordinary stdin write; a
fire pushed but not yet written when the daemon dies is lost, the same narrow
window every claude prompt already has.

## Budget halt

`budgetHalted` (`cmd/rafikid/budget_halt.go`) is the single over-budget
predicate: true when the child, or any ancestor walking its parent links, is in
the in-memory breach set. The breach set records only the budgeted node that
breached, so the ancestor walk IS the predicate. It is in-memory, populated by
the periodic budget sweep and, at boot, by `seedBudgetBreaches` (which marks
without steering, because the previous incarnation's steer already rode the
durable inbox).

Consumers:

- **Scheduled fires** are HELD when the tree is halted — no delivery, no turn;
  a recurring job still advances and re-arms, and the hold is recorded on the
  row (`RecordHeld`). When the budget is raised,
  `releaseHeldSchedules` delivers ONE frame per held job
  (key `<id>@held@<held_last_due>`) and clears the held state; a released
  one-shot tombstones.
- **A claude child's rafiki MCP tools** are refused while halted, except
  `agent_report` and `agent_result` (`cmd/rafikid/mcp_budget_gate.go`,
  `budgetGateExempt`/`gateToolsOnBudget`).
- **A fundi child's tool loop** wraps up through `EngineConfig.Halted`
  (`pkg/fundi`), consulted by `ShouldStop` before the cost guardrail.

Native claude tools are not gated; the existing over-budget steer covers them.
A child with no budgeted node anywhere in its chain is never held.

## Claude children

Claude Code's own four scheduling tools — `CronCreate`, `CronDelete`,
`CronList`, `ScheduleWakeup` (`claudeargv.NativeSchedulerTools`) — are added to
`--disallowedTools` whenever the child carries the MCP config
(`claudeargv.Build`, gated on `Params.MCPConfig`/`MCPConfigFile`), so a
daemon-managed claude child has exactly one scheduler: rafiki's, which persists
with the conversation. The gate is the MCP config on `claudeargv.Params`, so
both launch paths agree without a proto field. The coordination prompt
(`claudeargv.CoordinationPrompt`) names rafiki's tools. A top-level
`rafiki claude` launcher session is not daemon-managed and keeps the native
four.
