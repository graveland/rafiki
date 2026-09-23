---
name: subagent-driven-development
description: Use when executing a written implementation plan by dispatching subagents - covers wave dispatch in worktrees, settle handling, preset-dispatched tasks, merge-and-gate, and the ledgers that survive compaction.
---

# Executing a plan with subagents

You are the coordinator. You dispatch, review, merge, and decide. You do not
implement dispatched work yourself (except what the plan marks as inline), and
you do not stop to ask permission between tasks.

`coordinating-agents` is what the tools do — spawn semantics, settles, preset
dispatch, worktree isolation, budgets. This skill is the loop.

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
worktrees and the main repo's scratch; your own budget exhausted. **A child's
failure is a fifth stop** — evidence with `task_update`, then ask the human
(no retry, no model switch, no budget raise). See `coordinating-agents`' *On
ANY failure*.

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
   wants is the first thing that breaks a resume. Also check the main
   checkout's own uncommitted tree (`git status --porcelain`) and reconcile
   strays against the ledger before landing.
3. **Budget check, final review reserved first.** Subtract the whole-branch
   final review's cap from your grant *before* summing the plan's task caps
   against what is left. The final review runs last and is the one a shortfall
   silently deletes — and it is the review that catches what every per-task
   review missed. If the remainder cannot cover the plan, say so in the ledger
   and to your human now, and run anyway.
4. **Presets.** The plan names its preset group (its header line
   `Preset group: <name>`). Check the group's seats exist and are sane
   (`rafiki preset list <group>:`) before the first dispatch — a missing preset
   is a failure: record it and ask the human. Dispatches carry
   `preset: "<group>:<role>"`; if no group was declared, ask which one before
   anything runs.
5. **Conflict scan.** Intersect every pair of `touches:` globs within each
   wave. A non-empty intersection is a plan defect — split the wave before
   anything runs. Then one read pass for what globs cannot see: does a later
   wave consume an interface no earlier wave produces? Write what you checked
   into the ledger; "the scan was clean" without the rows is not a scan.
6. **Integration branch, in its own worktree.** One integration branch off
   `main` per plan, and it is checked out in a DEDICATED worktree created at
   setup — never in the main checkout:

   ```
   git worktree add -b <plan>-integration .worktrees/integration-<plan> main
   ```

   The main checkout stays on whatever the operator left it on. Other sessions
   work there: their commits, stashes and uncommitted files must never be
   paved over, and a merge run against the main checkout's HEAD both blocks on
   their dirty tree and exposes the branch to their commits. Every
   integration command names the worktree explicitly (`git -C
   <main-repo>/.worktrees/integration-<plan> …`), so no step depends on where
   HEAD happens to sit. Coordinator-local commits (graduations, doc syncs)
   also land in that worktree — `git -C <integration-wt> branch
   --show-current` must name the integration branch before any commit.
7. `task_add` every task with `metadata: {plan: "<basename>"}`. Metadata is
   write-once — it cannot be added later. Add children one call per parent:
   two parallel `task_add` calls against one parent race and swap content onto
   handles.

## Dispatching a wave

Per task, concurrently, in chunks of at most four:

```
git worktree add -b <plan>-<task> .worktrees/<plan>-<task> \
  $(git -C <main-repo>/.worktrees/integration-<plan> rev-parse HEAD)
<extract the task body> > <main-repo>/tasks/sdd/<plan-basename>/task-<n>-brief.md
agent_spawn(cwd: "<abs worktree>", preset: "<group>:<role>", max_cost: <plan value>,
            task: "1.1", name: "1.1", prompt: <dispatch>)
```

The role per task is the plan writer's placement call — it is written into the
plan, not resolved here. Implementers and reviewers get different roles
(`<group>:implementer` vs `<group>:reviewer`); reviewers are always a fresh
spawn, never the agent that wrote the code.

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
DISPATCH task=1.1 preset=<group>:<role> cap=0.15 base=<sha>
```

**A task the plan marks inline never spawns.** Edit the integration tree, run
`verify`, commit.

Then **stop dispatching and do local work** — the next brief, the ledger, a
returned report. Never poll.

## Handling a settle

- **`settled (idle)`** — the turn ended; not a claim of completion. Read the
  report file: DONE, DONE_WITH_CONCERNS, NEEDS_CONTEXT, or BLOCKED. Concerns
  about correctness or scope get addressed before review; observations get
  noted. NEEDS_CONTEXT gets the missing context and a re-dispatch. BLOCKED
  gets a *change* — more context, a smaller task, a scope ruling, or the plan
  being wrong. Never re-dispatch the same task with nothing changed.
- **`settled — <limit reason>`** (a cost-cap hit) — **evidence, then STOP and
  ask the human.** Record the settle reason, the cap, the spend so far, the
  preset and model. No retry, no raise on your own authority.
- **`settled after a turn error`** — same: evidence, then STOP and ask. A turn
  error is not yours to ride through.

**Reap as you go.** A settled child still holds one of your four slots until it
is killed, so a spawn refused at the cap while everything looks finished is a
reaping problem, not a reason to wait. But an implementer whose review has not
yet adjudicated may need resuming warm for a fix round — seats go to pending
reviews first, and a cold re-dispatch is the accepted cost when they do not.

## Review and fix rounds

Review placement is the plan writer's call, written into the plan; your job is
to execute it. Two standing rules hold everywhere:

- **Reviewers are always a fresh spawn** — a different agent, never the
  implementer's conversation, because same-agent review is correlation rather
  than verification.
- **A fix round is one fix dispatch plus one scoped re-review of the fix
  diff.** Rounds 1 and 2 resume the original implementer — its context is
  intact. Beyond that, dispatch a **fresh** implementer pointed at the report
  file: a task that survives two resumes usually means the implementer cannot
  see its own problem.

When a task's cap is reached, decide each open finding: fix it, rule it out
with a reason, or park it in the ledger for the final review. A task that
cannot converge within its budget is a failure — evidence, then ask.

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

**Order the review dispatch so the file fills itself in.** Rank the sections in
the prompt, highest-value first, and mandate one write per section:

> Resolve question A, write section A to disk, then start question B. Write the
> most valuable section first. For anything you have not settled, write the row
> with `UNDECIDED` and what you would need to check, rather than leaving it
> blank or waiting.

The ranking is the mechanism: a budget death truncates the cheap end of the
file instead of destroying all of it. Do not substitute an exhortation to be
diligent. "Write the skeleton first and fill it in as you go" produces a
skeleton and nothing else, because writing feels like the thing you do once you
know the answer, and warning the reviewer about that does not prevent it.
`UNDECIDED` plus a next step is a finding you can dispatch against; an
UNRESOLVED marker is not.

## Merging a wave

Per task, before its branch lands:

1. `git -C <wt> status --porcelain` — **strays, including untracked files.**
   This is what catches a `docs/` or `tasks/` file written inside a worktree,
   where it will be destroyed with the worktree.
2. `git -C <wt> diff --name-only <BASE>..HEAD` against `touches:`. Anything
   outside the declaration is a plan defect: `PROCESS layer=plan`, a ruling,
   and the wave's remaining branches get read rather than trusted.
3. **Foreign-commit scan.** Before touching the branch:
   `git -C <integration-wt> log <wave-base>..<integration-branch> --oneline`.
   Every commit must be one this plan landed. A parallel session can leave
   commits on the integration branch (this is exactly what happens when the
   branch was ever checked out in the main checkout): keep them — they are
   the operator's property, never silently rebased over, reset or dropped —
   exclude them from the plan's diff scope and review, and record them in the
   ledger before landing anything.
4. Land it linearly. The worktree holds the task branch; the integration
   worktree holds the integration branch. Rebase the task branch, then
   fast-forward the integration branch to it:

   ```
   git -C <wt> rebase <integration-branch>
   git -C <integration-wt> merge --ff-only <task-branch>
   ```

   The rebase replays the task's commits onto the current integration head —
   another task in the wave may have landed first — and `--ff-only` moves the
   branch without a commit of its own. Resolve a conflict during the rebase or
   rule on it; never dissolve one into a merge.

Then run the wave's `gate:` on the **landed** result, with `-count=1`.

**Linear is the rule, not a preference.** Each task's work should be
reviewable and bisectable as its own commits; a merge commit wraps a
subagent's work in a topology nobody reviewed, and undoing one is a history
rewrite and a force-push of `main`. `--no-ff` is never the right flag here.

**A gate outside the main checkout needs its environment carried in.** A fresh
worktree has no `.env` — it is gitignored — so a suite needing a DSN either
skips silently, and green means nothing, or fails every integration test with
`daemon never accepted on …/controller.sock`. That signature reads exactly like
a daemon regression and is not one. Source the main checkout's `.env` before
gating (`set -a; . <main-checkout>/.env; set +a`) and check the skip count, not
just the exit code.

**If the gate fails, do not debug the landed tree.** Reset the integration
branch to the wave's start and re-land one branch at a time, re-running the
gate; the first failure names the culprit. Slow in wall-clock, cheap in
judgement, and impossible to get wrong.

Only then: `git worktree remove` and `git branch -d` for each task worktree.
The integration worktree survives until Finishing. A worktree outlives its
task by exactly one gate. (`git branch -d` checks against the current HEAD —
branches merged only into the integration branch refuse; verify with
`git merge-base --is-ancestor` and delete with `-D`.)

## Finishing

1. One whole-branch review on `<group>:final-reviewer`, pointed at the parked
   minor findings — funded out of the reserve you set aside at Setup. One fix
   dispatch, one scoped re-review, adjudicate what is left.
2. `make check` green, with the evidence in the ledger. Not "should pass".
3. **Knowledge graduates.** Anything learned that would save a future session
   time — a gotcha, an invariant, a footgun — goes into CLAUDE.md or
   `docs/reference/`, in the same commit as the code it describes.
4. Write the verdict block: per preset used, rounds-to-accept and breaches —
   or "insufficient evidence".
5. Append every `layer=plan` and `layer=tool` line to `tasks/lessons.md`.
   `layer=skill` lines are already in `tasks/skill-problems.md`; do not copy
   them again.
6. Remove every worktree and branch. Delete the plan file (unless a later
   step of this plan still needs it — say so in the ledger when keeping it).
7. **Stop.** The integration branch is not landed on `main` without your human
   partner saying so. When it is: re-run the foreign-commit scan first and
   name to the human anything on the branch this plan did not write — landing
   fast-forwards `main` to the integration head, foreign commits included.
   Then fast-forward if `main` has not moved; if it has, rebase the
   integration branch onto `main`, re-run the gate, then fast-forward. A merge
   commit on `main` is a defect. Finally remove the integration worktree
   itself.

## The ledgers

Everything lives in `tasks/sdd/<plan-basename>/` in the **main repo**, never in
a worktree. `ledger.md` is append-only; its first line names its plan.

**Dispatch log** — one line per dispatch, observations only. Dies with the
plan; do not read another plan's, because acting on last week's small sample is
how you refuse a seat that has been fine for a month.

```
DISPATCH task=2.3 preset=default:reviewer outcome=accepted rounds=1 cap=0.40 spend=0.11 note=clean first pass
DISPATCH task=1.2 preset=default:implementer outcome=budget   rounds=0 cap=0.10 spend=0.10 note=blew cap on a 6-line edit
```

`outcome` is one of `accepted`, `rejected`, `blocked`, `budget`, `error`.
`spend` is `unknown` if you cannot observe it. `note` is a few words, not a
place for reasoning — a seat narrating its impression of another seat reads
plausibly and is worthless.

At Finishing this becomes a **verdict per preset**: rounds-to-accept, breaches,
blocked work. **"Insufficient evidence" is a real verdict and often the right
one** — one plan is a small sample. Evidence about a *preset* feeds
`managing-presets`' tuning step; evidence about a model id is recorded where
the preset row can carry it.

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
lines outlive the plan and `DISPATCH` lines do not.