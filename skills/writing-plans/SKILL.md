---
name: writing-plans
description: Use when turning an agreed design into an implementation plan for subagents to execute - defines the wave/worktree/rung plan format, how to batch work at authoring time, and how to write task bodies a cheap model can execute.
---

# Writing an implementation plan

The plan is the only thing the coordinator reads, and one task body is the only
thing its implementer reads. Its job is to be executable by the **cheapest
model that can do the work**, which is a property you can check rather than
hope for.

## Document shape

```markdown
# <topic> — implementation plan

Spec: docs/plans/2026-09-07-<topic>-design.md
Budget: $2.40 across 9 tasks in 3 waves

## Global Constraints
<Binding requirements, verbatim. Exact values, exact formats, and the CLAUDE.md
invariants this plan touches — quoted, not summarised.>

## Wave 1 — <name>
gate: make check

### Task 1.1 — Add cost_usd to AgentInfo
rung: 1
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

## The six task fields

- **`rung`** — how much ceremony this earns. See below.
- **`touches`** — the files this task may modify. Checked for disjointness
  within a wave when the plan is written, and against the real diff before the
  branch merges.
- **`worktree`** — `.worktrees/<plan>-<task>`. Stated, not derived, so a
  coordinator that lost its context finds the same path. Omit at rung 0.
- **`max_cost`** — USD, passed straight to `agent_spawn`. **Size it to what the
  work should cost**, not generously: a tight cap on cheap work is how a
  degraded model gets caught.
- **`verify`** — the exact command that proves this task. Not "run the tests".
  Required at every rung, including 0, because rung 0 is the rung most often
  tagged wrong.
- **body** — the work.

**Name rungs, never models.** The rung-to-seat mapping lives in
`subagent-driven-development`, and which model fills a seat is a fact about the
environment that belongs in the project's CLAUDE.md/AGENTS.md — so a delisting,
a price move or a provider going bad does not invalidate a single plan.

## The rungs

| Rung | Work | What it costs to run |
|---|---|---|
| **0** | one line, one constant; the body contains the exact text | coordinator edits inline. No subagent, no review |
| **1** | several same-shape mechanical edits | one subagent; the coordinator reads the diff itself |
| **2** | an ordinary task with its own tests | implementer plus a reviewer |
| **3** | a CLAUDE.md invariant, concurrency, a security boundary, a wire format | implementer plus a reviewer on the strongest seat |

Tag honestly. A rung inflated "to be safe" costs two spawns and a review cycle
on a one-line change; a rung deflated costs a bug. The coordinator may raise a
rung on evidence and may never lower one.

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

## Writing a body a cheap model can execute

Three rules, each of which a reviewer can fail a plan on:

1. **A rung-0 or rung-1 body must be transcribable.** Could a model with zero
   knowledge of this repo produce the diff from this body alone? If not, it is
   not rung 1 — either write the body properly or raise the rung.
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
the sum of the implementer caps** — it is the implementers, plus a reviewer seat
for every rung-2 and rung-3 task, plus the fix rounds those rungs allow, plus a
named reserve for the whole-branch final review. Stating only the implementer
caps understates a plan by roughly half, and the seat a shortfall silently
deletes is the final review: it runs last, and it is the one that catches what
every per-task review missed.

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
7. **Is every `max_cost` priced for the seat that fills it, and everything that
   seat reads?** A cap is a number about a model, and the plan does not name
   models. When a review seat is described as an expensive model in one
   paragraph and priced at a flat rung rate in another, the coordinator
   discovers it mid-flight and raises budgets under time pressure.

This pass is cheap and it is not optional. It is the only place these defects
are visible at all, and each one that survives it costs a dispatch, a review
round, or a shipped bug.

## The plan is scaffolding

Write it to `docs/plans/YYYY-MM-DD-<topic>-plan.md`. **Do not commit it** —
that directory is gitignored on purpose. When the work is done the plan is
deleted; what survives is code, tests, and knowledge graduated into
`docs/reference/` or CLAUDE.md in the same commit as the code it describes.

If a plan ends with knowledge worth keeping and nowhere to put it, it was
written into the wrong file.
