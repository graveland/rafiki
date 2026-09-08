---
name: subagent-driven-development
description: Use when executing a written implementation plan by dispatching subagents - covers wave dispatch in worktrees, settle handling, ceremony and seats by rung, merge-and-gate, and the ledgers that survive compaction.
---

# Executing a plan with subagents

You are the coordinator. You dispatch, review, merge, and decide. You do not
implement rung-2 or rung-3 work yourself, and you do not stop to ask permission
between tasks.

`coordinating-agents` is what the tools do — spawn semantics, settles, model
selection, worktree isolation, budgets. This skill is the loop.

## Rulings, not stalls

A running plan does not wait on a human. Conflicts, ambiguities, plan defects,
a cap you would have asked to exceed — **decide them.** The design is the
binding authority, the plan is its argument, and your judgement settles what
neither answers. Record every decision:

```
RULING: <what you decided> — <why> — <what it costs if wrong>
```

A wrong ruling costs rework your human partner can see and undo. A session
parked on a question costs their whole day and buys nothing.

**Four things stop you, and only these:** an irreversible or destructive
operation; a security-sensitive action; a side effect outside this plan's
worktrees and the main repo's scratch; your own budget exhausted.

## Setup

You run **in the main repo**. You never `cd` into a worktree.

1. **Resume check.** `tasks/sdd/<plan-basename>/ledger.md` — its first line
   names its plan. Tasks with a `SETTLE … outcome=accepted` line are done; do
   not re-dispatch them. A ledger naming a different plan is someone else's;
   leave it and start your own. **After a compaction, trust the ledger and
   `git log` over your own recollection** — re-dispatching completed work is
   the most expensive mistake available to you.
2. **Worktree hygiene.** `git worktree list`, prune leftovers from a killed run
   *before* creating any. A stale worktree holding a branch name this plan
   wants is the first thing that breaks a resume.
3. **Budget check, final review reserved first.** Subtract the whole-branch
   final review's cap from your grant *before* summing the plan's task caps
   against what is left. The final review is the last seat to run and therefore
   the one a shortfall silently deletes — and it is the seat that catches what
   every per-task review missed. If the remainder cannot cover the plan, say so
   in the ledger and to your human now, and run anyway.
4. **Seats.** Read the project's CLAUDE.md/AGENTS.md for declared seats,
   provider availability and bans. Absent a declaration, resolve seats per the
   rung table below and record what you chose. Never carry a model id in from a
   previous plan.
5. **Conflict scan.** Intersect every pair of `touches:` globs within each
   wave. A non-empty intersection is a plan defect — split the wave before
   anything runs. Then one read pass for what globs cannot see: does a later
   wave consume an interface no earlier wave produces? Write what you checked
   into the ledger; "the scan was clean" without the rows is not a scan.
6. **Integration branch** off `main`, one per plan. Every wave merges into it.
   `main` is never touched.
7. `task_add` every task with `metadata: {rung: "<n>", plan: "<basename>"}`.
   Metadata is write-once — it cannot be added later.

## Dispatching a wave

Per task, concurrently, in chunks of at most four:

```
git worktree add -b <plan>-<task> .worktrees/<plan>-<task> <integration-head>
<extract the task body> > <main-repo>/tasks/sdd/<plan-basename>/task-<n>-brief.md
agent_spawn(cwd: "<abs worktree>", model: <seat for rung>, max_cost: <plan value>,
            task: "1.1", name: "1.1", prompt: <dispatch>)
```

Extraction runs from each `^### ` heading to the next `^### ` or a `^---$`
separator — derive the boundary from the plan in front of you rather than
assuming a heading format, because a hardcoded pattern sends a truncated brief
with no error anywhere. Whatever the shape, **you never read the task body into
your own context**, and the implementer never reads the whole plan. Everything
you paste into a prompt stays resident and is re-read on every later turn.

Every dispatch carries the isolation block from `coordinating-agents` verbatim.
`cwd:` does not enforce the worktree on its own, and the failure is split-brain
rather than loud.

Record before each spawn — `base` is not recoverable afterwards:

```
DISPATCH task=1.1 model=<id> cap=0.15 base=<sha>
```

**Rung 0 never spawns.** Edit the integration tree, run `verify`, commit.

Then **stop dispatching and do local work** — the next brief, the ledger, a
returned report. Never poll.

## Handling a settle

- **`settled (idle)`** — the turn ended; not a claim of completion. Read the
  report file: DONE, DONE_WITH_CONCERNS, NEEDS_CONTEXT, or BLOCKED. Concerns
  about correctness or scope get addressed before review; observations get
  noted. NEEDS_CONTEXT gets the missing context and a re-dispatch. BLOCKED
  gets a *change* — more context, a stronger model, a smaller task, or a
  ruling that the plan was wrong. Never re-dispatch the same model at the same
  task with nothing changed.
- **`settled — <limit reason>`** — a budget breach. **Rung 0/1: suspect the
  model** — switch seats and re-dispatch; do not raise the cap. **Rung 3:
  suspect the estimate** — `agent_set_budget` and resume. **Rung 2:** one
  `agent_view`, then rule. Write a `MODEL … outcome=budget` line either way.
- **`settled after a turn error`** — resume once on the same seat. Twice is not
  transient; escalate the seat and record it.

**Reap as you go.** A settled child still holds one of your four seats until it
is killed, so a spawn refused at the cap while everything looks finished is a
reaping problem, not a reason to wait.

## Ceremony and seats by rung

| Rung | Implementer | Reviewer | Fix rounds |
|---|---|---|---|
| 0 | you, inline — no spawn | none | — |
| 1 | cheapest tool-capable seat, tight `max_cost` | **you read the diff yourself** | 1, then escalate the seat |
| 2 | mid seat | one tier above the implementer | 2 |
| 3 | strongest seat available | strongest available, always separate, fresh each round | 4 |

Two rules the table encodes. **A reviewer sits one tier above what it reviews**,
because same-model review is correlation rather than verification — it shares
the implementer's blind spots. At rung 3 there is no tier above, so the rule
degrades to its weaker half: a *different* seat, fresh each round, never the
model that wrote the code. And **err on the side of the cheaper seat**: an
inflated seat is invisible waste, while a deflated one announces itself in
rounds-to-accept and you can escalate on that evidence.

A fix round is one fix dispatch plus one scoped re-review of the fix diff.
Rounds 1 and 2 resume the original implementer — its context is intact. Beyond
that, dispatch a **fresh** implementer one tier up, pointed at the report file:
a task that survives two resumes usually means the implementer cannot see its
own problem.

When the cap is reached, decide each open finding: fix it, rule it out with a
reason, or park it in the ledger for the final review. Do not spend a fifth
round on a model that has failed four times.

**Reviewing well:**
- Hand the reviewer the diff as a **file** (`git diff -U10 <base>..HEAD`
  redirected), plus the brief path, the report path, and the Global Constraints
  block verbatim. Never dispatch a reviewer without a diff file.
- Do not ask it to re-run tests the implementer already ran; the report carries
  that evidence.
- **Never pre-judge.** If your prompt contains "do not flag", "at most minor",
  or "the plan chose this", stop — you are trying to spare yourself a review
  loop. Let the finding be raised and adjudicate it.
- A finding that conflicts with what the plan mandates is **yours to rule on**,
  with the design as the binding authority. Do not dismiss it because the plan
  says so, and do not fix against the plan without recording the ruling.
- Minor findings go in the ledger, never into the fix loop, and get handed to
  the final review to triage.

## Merging a wave

Per task, before its branch merges:

1. `git -C <wt> status --porcelain` — **strays, including untracked files.**
   This is what catches a `docs/` or `tasks/` file written inside a worktree,
   where it will be destroyed with the worktree.
2. `git -C <wt> diff --name-only <BASE>..HEAD` against `touches:`. Anything
   outside the declaration is a plan defect: `PROCESS layer=plan`, a ruling,
   and the wave's remaining merges get read rather than trusted.
3. Merge into the integration branch.

Then run the wave's `gate:` on the **merged** result, with `-count=1`.

**A gate outside the main checkout needs its environment carried in.** A fresh
worktree has no `.env` — it is gitignored — so a suite needing a DSN either
skips silently, and green means nothing, or fails every integration test with
`daemon never accepted on …/controller.sock`. That signature reads exactly like
a daemon regression and is not one. Source the main checkout's `.env` before
gating (`set -a; . <main-checkout>/.env; set +a`) and check the skip count, not
just the exit code.

**If the gate fails, do not debug the merged tree.** Reset the integration
branch to the wave's start and re-merge one branch at a time, re-running the
gate; the first failure names the culprit. Slow in wall-clock, cheap in
judgement, and impossible to get wrong.

Only then: `git worktree remove` and `git branch -d`. A worktree outlives its
task by exactly one gate.

## Finishing

1. One whole-branch review on the strongest seat, pointed at the parked minor
   findings — funded out of the reserve you set aside at Setup. One fix
   dispatch, one scoped re-review, adjudicate what is left.
2. `make check` green, with the evidence in the ledger. Not "should pass".
3. **Knowledge graduates.** Anything learned that would save a future session
   time — a gotcha, an invariant, a footgun — goes into CLAUDE.md or
   `docs/reference/`, in the same commit as the code it describes.
4. Write the verdict block: per model used, a **rung ceiling**, or
   "insufficient evidence".
5. Append every `layer=plan` and `layer=tool` line to `tasks/lessons.md`.
   `layer=skill` lines are already in `tasks/skill-problems.md`; do not copy
   them again.
6. Remove every worktree and branch. Delete the plan file.
7. **Stop.** The integration branch is not merged to `main` without your human
   partner saying so.

## The ledgers

Everything lives in `tasks/sdd/<plan-basename>/` in the **main repo**, never in
a worktree. `ledger.md` is append-only; its first line names its plan.

**Model log** — one line per dispatch, observations only. Dies with the plan;
do not read another plan's, because acting on last week's small sample is how
you refuse a model that has been fine for a month.

```
MODEL <model-id> rung=2 task=2.3 outcome=accepted rounds=1 cap=0.40 spend=0.11 note=clean first pass
MODEL <model-id> rung=1 task=1.2 outcome=budget   rounds=0 cap=0.10 spend=0.10 note=blew cap on a 6-line edit
```

`outcome` is one of `accepted`, `rejected`, `blocked`, `budget`, `error`.
`spend` is `unknown` if you cannot observe it. `note` is a few words, not a
place for reasoning — a model narrating its impression of another model reads
plausibly and is worthless.

At Finishing this becomes a **verdict per model used**, and the recommendation
is a **rung ceiling**: "fine at rung <=1, three budget breaches at rung 2" is
actionable; "good" is not. **"Insufficient evidence" is a real verdict and
often the right one** — one plan is a small sample, and a model that failed
twice may simply have drawn the two hard tasks. A verdict needs three dispatches
at a rung, or one strong single-shot signal; a cheap task breaching a tight cap
is strong.

**Process log** — defects in the *workflow*, and it names which layer it
indicts, because generalising from one instance edits the wrong thing:

- `layer=plan` — this plan's author erred. Fix is nothing; the plan is
  discarded. Goes to `tasks/lessons.md` at Finishing.
- `layer=skill` — this skill is wrong, ambiguous, or silent where it should
  rule. Fix is a commit to `skills/<name>/SKILL.md`, which you may not make
  mid-plan: it is a side effect outside this plan's worktrees. So **append the
  line to `tasks/skill-problems.md` as you observe it**, not at Finishing. That
  file is a drainable queue against `skills/`, emptied when the skills are
  edited; empty is its correct steady state.
- `layer=tool` — rafiki itself: a drifted description, a missing field, a tool
  that cannot express what you needed. Fix is a design doc or a code change.
  Goes to `tasks/lessons.md` at Finishing.

```
PROCESS layer=tool task=1.2 cost=one-wasted-spawn obs=agent_spawn workspace:ephemeral gave the same dir as pinned
```

**Record nothing that did not cost something.** An entry needs an observable
cost — a burnt round, a wrong dispatch, a stall, a ruling this skill should
have answered — and the line states it. Absent that you are writing
self-critique to look diligent, which is worse than silence.

One occurrence is an observation; a pattern is a defect. That is why `PROCESS`
lines outlive the plan and `MODEL` lines do not.
