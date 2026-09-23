---
name: writing-plans
description: Use when turning an agreed design into an implementation plan for subagents to execute - defines the wave/worktree plan format, how to batch work at authoring time, where reviews go, and how to write task bodies an implementer seat can execute.
---

# Writing an implementation plan

The plan is the only thing the coordinator reads, and one task body is the only
thing its implementer reads. Its job is to be executable by the seat it will be
dispatched to, which is a property you can check rather than hope for.

**Ask for the preset group up front.** Every dispatch names a preset
(`<group>:<role>`), so a plan needs one. If the design (or the human) did not
name a group, ask before writing — and record the answer in the header:

```markdown
# <topic> — implementation plan
Spec: docs/plans/2026-09-07-<topic>-design.md
Preset group: default
Budget: $2.40 across 9 tasks in 3 waves
```

**Name the group, never models.** Which model fills each seat is decided by the
`managing-presets` skill against the live catalog — a delisting, a price move
or a provider going bad does not invalidate a single plan.

## Document shape

```markdown
# <topic> — implementation plan

Spec: docs/plans/2026-09-07-<topic>-design.md
Preset group: default
Budget: $2.40 across 9 tasks in 3 waves

## Coverage
<One row per item in the design's scoped slice: covered here, deliberately
excluded, or missing. See the design pass below.>

## Global Constraints
<Binding requirements, verbatim. Exact values, exact formats, and the CLAUDE.md
invariants this plan touches — quoted, not summarised.>

## Wave 1 — <name>
gate: make check

### Task 1.1 — Add cost_usd to AgentInfo
touches: pkg/fundi/tools/agent.go, pkg/fundi/tools/agent_steer.go
worktree: .worktrees/<plan>-1.1
max_cost: 0.15
verify: go test ./pkg/fundi/tools/ -run TestAgentList -count=1

<body>
```

`### Task <wave>.<n> — <title>` is a **machine anchor**. The coordinator
extracts one task and passes the file path, so the task body never enters the
coordinator's context and the implementer never reads the whole plan. Keep the
anchors exact, and keep them uniform: extraction runs heading-to-heading, so one
task titled differently from its siblings arrives truncated with no error.

**No worker-invocation boilerplate.** The plan is read by a coordinator that has
already loaded its skill; a header telling an "agentic worker" which skill to use
only preserves a prefix until it goes stale, and an agent obeying a dead prefix
improvises. Older plans under `docs/plans/complete/` carry exactly that header
alongside checkbox task lists this format does not use — do not copy one as a
template.

## The five task fields

- **`touches`** — the files this task may modify. Checked for disjointness
  within a wave when the plan is written, and against the real diff before the
  branch merges.
- **`worktree`** — `.worktrees/<plan>-<task>`. Stated, not derived, so a
  coordinator that lost its context finds the same path. Omit for an inline
  task.
- **`max_cost`** — USD, passed straight to `agent_spawn`. **Size it to what the
  work should cost**, not generously: a tight cap on cheap work is how a
  degraded seat gets caught.
- **`verify`** — the exact command that proves this task. Not "run the tests".
  Required for every task, including inline ones, because inline is the mode
  most often tagged wrong.
- **body** — the work.

**Inline tasks exist.** A one-line change whose body contains the exact text
gains nothing from a spawn — mark it for the coordinator to edit itself, run
its `verify`, and commit. Tag honestly: an inflated dispatch costs a spawn and
a review cycle on a one-line change; a deflated one costs a bug.

## Where reviews go

Review placement is yours to decide, and the plan states it — the coordinator
does not infer ceremony. The rules:

- **A task that touches a CLAUDE.md invariant, concurrency, a wire format, or
  a security boundary gets its own reviewer checkpoint** (a `<group>:reviewer`
  dispatch after it lands). This is the one hard rule — these are the tasks a
  cheap first pass quietly gets wrong.
- Everything else: batch reviews at **wave boundaries** by default. One review
  over a wave's whole diff is cheaper than one per task and still catches
  cross-task drift; split a wave's review into per-task reviews only when a
  task carries a plan invariant or lands in a request path.
- **Reviewers are always a fresh spawn** — a different agent, never the
  implementer's conversation.
- **The whole-change `:final-reviewer` review is mandatory and budgeted
  first** — it is the seat that catches what every per-task review missed, and
  it is the one a budget shortfall silently deletes.

## Waves

A wave is a **barrier**. Every task inside it runs concurrently in its own
worktree; the next wave starts only after the previous one merges and passes
its `gate:`.

- **Within a wave, no two tasks' `touches:` may intersect.** That is the whole
  contract, and it is mechanically checkable — check it while writing.
- **Every wave has a `gate:`** — the command run on the *merged* result,
  usually `make check`. Per-task `verify` proves the task; the gate proves the
  merge, because two disjoint changes still break each other's tests.
- **Shared files get special handling.** A registry, CLAUDE.md, a docs page —
  anything several tasks would touch. Collapse every edit into one task, or
  give that task its own wave. Never two tasks in one wave editing one file.
- Order waves by dependency. Do not build a general graph; a barrier is enough
  and is impossible to get wrong.
- Prefer a review checkpoint on a wave boundary: it sees the wave's whole diff
  after the gate, where per-task context is fresh in the reviewer's brief.

## Writing a body a seat can execute

Three rules, each of which a reviewer can fail a plan on:

1. **An inline or mechanical body must be transcribable.** Could a model with
   zero knowledge of this repo produce the diff from this body alone? If not,
   either write the body properly or mark it for a review checkpoint.
2. **Every exact value appears in the body** — constants, strings, signatures,
   test names, error text. Never "as described in the design doc". The
   implementer receives the brief and nothing else.
3. **No task says "see Task N."** Restate cross-task interfaces in every task
   that consumes them. Duplication in a discarded scaffold is free; a dangling
   reference costs a wasted dispatch.

**Batch here, not at execution.** Six one-line edits across six files are
**one task with six bullets**, not six tasks. A task is a unit of dispatch, not
a unit of thought. Getting this wrong is the single largest source of waste,
because every task carries a spawn and possibly a review.

## Budget

Every task carries `max_cost`; the header states the total. **That total is not
the sum of the implementer caps** — it is the implementers, plus the review
checkpoints the plan places, plus the fix rounds they allow, plus the
whole-branch `:final-reviewer` review, named and budgeted first. Stating only
the implementer caps understates a plan by roughly half, and the review a
shortfall silently deletes runs last, after everything else has consumed the
margin.

A coordinator cannot spend more than its own grant, so stating the number
honestly makes a shortfall visible before anything runs instead of at task
eight.

## Global Constraints

Copy the binding requirements **verbatim** from the design or from CLAUDE.md —
exact values, exact formats, and stated relationships between components. This
block is handed to every implementer and every reviewer as their attention
lens. Summarising it is how a constraint quietly stops binding.

## Read the plan as one document, before anyone runs it

Every check above is local: a body, a field, a wave. A plan's worst defects are
not local. They live between two task bodies, where no implementer and no
reviewer will ever see both, and a Global Constraints block quoting the violated
rule correctly does not catch them.

So make one pass over the finished plan, reading it as a single document. Seven
questions, roughly in order of what they cost when missed:

1. **Does each prescribed mechanism deliver the property its own prose claims?**
   Check the mechanism, not the sentence. A body prescribing a two-arm `select`
   and asserting the first arm is preferred is wrong, because Go picks uniformly
   at random — the prose is fine and the code holds ~50% of the time. A body
   claiming a second range "terminates immediately" over code that closes an
   already-closed channel describes a panic. Both read as correct prose.
2. **Do the Global Constraints actually bind the code?** Same move, one level
   up. Re-read each quoted constraint against every body it touches, as a
   mechanism. A plan that quotes "nothing blocks without a `ctx.Done()` arm"
   verbatim and then prescribes a `select` without one has a constraints block
   that is decorative. Copying the rule is not the check; applying it is.
3. **Do two tasks assert incompatible things?** Each is self-consistent, so
   nothing catches this but reading both. A package doc promising "cancellation
   propagates and the run-level error reports `context.Canceled`" is a defect if
   another task's dispatcher is specified to prevent exactly that. Doc comments
   are the usual site, because they are where a plan states guarantees far from
   the code that implements them.
4. **Would each prescribed test fail against the bug it is named for?** Write
   the test's failure, not its success. A cancellation test specified over a
   *bounded* input passes against the unbounded-input livelock it is named
   after, and it will pass on day one and stay green forever.
5. **Is every guard pinned by a test that dies without it?** Specify tests per
   production **line**, not per behaviour: for each guard, clause, and early
   return, name the test that fails when it is deleted. "Behaviour X is tested"
   routinely means a happy path that a mutated guard sails straight through.
6. **Does every file named in a body appear in exactly one `touches:`?** The
   disjointness check under *Waves* catches two tasks declaring one file. This
   catches its mirror image: a file specified in two bodies and declared in
   neither, where both implementers must report BLOCKED to obey their own
   isolation rules. Walk the bodies, not the `touches:` lines.
7. **Is every `max_cost` priced for what the task actually reads?** A cap is a
   number about a seat and everything it will read — the brief plus the files.
   When a review checkpoint is described as reading a whole wave's diff but
   priced at a per-task rate, the coordinator discovers it mid-flight and raises
   budgets under time pressure.

This pass is cheap and it is not optional. It is the only place these defects
are visible at all, and each one that survives it costs a dispatch, a review
round, or a shipped bug.

## Then read the plan against the design

Everything above reads the plan against itself. None of it can catch the plan
disagreeing with the thing it was written from, because the task bodies
involved are each perfectly self-consistent, which is what makes this failure
convincing rather than obvious. Internal consistency and fidelity to the spec
are different properties, and a plan is at its most persuasive exactly where it
is wrong in the second way.

So make a second pass with the design open, in **both directions**. They find
different defects and neither substitutes for the other.

**Design → plan: walk the scoped slice item by item.** Mark each item covered,
deliberately excluded, or missing, and put that table in the plan. This is the
only direction that finds a *dropped* requirement, because a requirement nobody
wrote down leaves no trace in the plan to notice, and re-reading the plan more
carefully will never surface it. The table is not bookkeeping: without it the
coordinator infers the scope, and a deliberate partial is indistinguishable
from an oversight. State each partial as one, with its reason and with what it
leaves unproven. "This lands the credential plugin and not the file that calls
it, because the naming source is still open" is a decision the plan hands on;
silence is a bug someone finds at merge.

**Plan → design: re-read every guarantee the plan prescribes against the design
section it touches.** Doc comments first. They are where a plan states a promise
furthest from the code that implements it, and an implementer copies them
verbatim into the codebase having never read the design. A helper documented as
producing "a stable context name" is a defect when the design says context names
come from topology rather than from string surgery on an API response: correct
implementation, good test table, wrong claim, and nothing inside the plan
disagrees with it.

The design is the binding authority. Where the plan is right and the design is
wrong, say so in the plan explicitly with the command or measurement that
settles it, **and amend the design in the same pass**. A correction recorded in
both places survives; one that lives only in a plan dies with the scaffold.

## The plan is scaffolding

Write it to `docs/plans/YYYY-MM-DD-<topic>-plan.md`. **Do not commit it** —
that directory is gitignored on purpose. When the work is done the plan is
deleted; what survives is code, tests, and knowledge graduated into
`docs/reference/` or CLAUDE.md in the same commit as the code it describes.

If a plan ends with knowledge worth keeping and nowhere to put it, it was
written into the wrong file.