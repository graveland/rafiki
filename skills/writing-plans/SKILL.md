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
extracts one task with `sed` and passes the file path, so the task body never
enters the coordinator's context and the implementer never reads the whole
plan. Keep the anchors exact.

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

**Name rungs, never models.** The rung-to-model mapping lives in
`model-selection` so that a delisting, a price move or a provider going bad
does not invalidate a single plan.

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

Every task carries `max_cost`; the header states the total including review
seats. A coordinator cannot spend more than its own grant, so stating the
number makes a shortfall visible before anything runs instead of at task eight.

## Global Constraints

Copy the binding requirements **verbatim** from the design or from CLAUDE.md —
exact values, exact formats, and stated relationships between components. This
block is handed to every implementer and every reviewer as their attention
lens. Summarising it is how a constraint quietly stops binding.

## The plan is scaffolding

Write it to `docs/plans/YYYY-MM-DD-<topic>-plan.md`. **Do not commit it** —
that directory is gitignored on purpose. When the work is done the plan is
deleted; what survives is code, tests, and knowledge graduated into
`docs/reference/` or CLAUDE.md in the same commit as the code it describes.

If a plan ends with knowledge worth keeping and nowhere to put it, it was
written into the wrong file.
