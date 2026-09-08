---
name: coordinating-agents
description: Use when spawning, steering, budgeting or waiting on subagents - covers agent_spawn/send/view/kill/set_budget, choosing a model from the live catalog, the async settle model, worktree isolation, task_* as a ledger, and cost budgets as an instrument.
---

# Coordinating agents

You spawn subagents; they run in their own processes, possibly on other
machines, spending real money. This is what the tools actually do and how to
compose a dispatch that works.

## When to spawn at all

`agent_spawn` creates a separate, cross-process, potentially cross-machine,
dollar-metered agent. It outlives your conversation, appears in `rafiki list`,
and is budget-, depth- and executor-constrained.

Reach for it when the work should survive independently, run on different
hardware, use a different model, or be watched from outside your session. For a
lightweight helper scoped to just this conversation, use your harness's own
subagent mechanism — it costs nothing to set up and nothing to reap.

A child's depth, cost and children limits are carved out of yours. **A spawn you
cannot justify in one sentence is usually work you should do yourself.**

## Spawning returns immediately. It does not wait.

`agent_spawn` hands back an id the moment the child registers. The work has not
started. You will be **notified** when it settles, and pinged periodically
while it is still working.

**Never poll.** `agent_list` in a loop costs a turn each time and tells you
nothing sooner than the notification will. A progress ping is not a settle.

While a child works, do local work: package the next brief, write the ledger,
read a returned report. If you have genuinely nothing to do, say one line and
wait — do not manufacture tool calls to fill the time.

Use `agent_list`/`agent_view` when you need to *look something up* — which
agent is which, what one has said — never as a waiting loop.

## Three settle reasons, three different responses

The notification names one:

- **`settled (idle)`** — the turn ended. **This is not a claim of
  completion.** The agent may have finished, or stopped, or asked you
  something. Read its report file before deciding anything.
- **`settled — <limit reason>`** — it hit its cost budget. See *Budgets* below;
  the right response depends on how expensive the work should have been.
- **`settled after a turn error: <err>`** — resume once on the same model. A
  second identical error is not transient: change something.

## Concurrency and seat hygiene

`max_children` defaults to **4** live agents beneath you, and you cannot read
your own value. Dispatch wider work in chunks of four; a refusal past the limit
is a wasted turn.

**A settled child still holds its seat.** Settled-but-unreaped children count
against the cap, so a refusal naming the limit while everything looks finished
is not a reason to wait — it is a reaping problem. `agent_kill` finished workers
as part of handling a settle, not only for runaways. Their branches survive;
only the conversation goes.

Never run two agents concurrently against the same working tree.

## Choosing the model

**Never carry a model id in memory or in a habit.** Prices, availability and
provider health move week to week, and an id you remember is stale on a schedule
you do not control. Resolve a query at dispatch time instead:

- `agent_models` with **no arguments** returns a distribution — how many models,
  price range and median, context range, tool and vision counts, how many carry
  benchmark scores. Use it to aim the second call. It does not return every row
  and you should not want it to.
- Then narrow, and ask for a handful rather than a page. The response always
  states how many matched before the cap.
- A model the catalog cannot answer for — no price, no context, no score — is
  **kept**, not excluded. Locally-served models look exactly like that.

**If the project declares seats, use them.** A repo's CLAUDE.md or AGENTS.md is
where provider availability, bans and concrete model choices belong: they are
facts about one environment, and this skill is not. Absent a declaration,
resolve a query yourself and record what you chose in the ledger, so the next
session inherits evidence rather than a guess.

### Qualities, and when each is load-bearing

- **Tool calling** is non-negotiable for any agent that must act, and the
  catalog reports it as a **tri-state**: "unknown" is not "no". A model that
  cannot call tools spawns, attaches, and does nothing.
- **Context** measured against the brief plus everything it will read, not
  against the brief alone.
- **Benchmark scores** are weak third-party evidence. Absent means *unscored*,
  never zero — an unbenchmarked model is not a bad one, and every locally-served
  model is unbenchmarked.
- **Turn count beats token price.** The cheapest tier routinely takes two or
  three times the turns on multi-step work and costs more overall. It is right
  when the brief contains the code to write — transcription plus tests. For
  anything that must be inferred from prose, start mid.
- **Kind scoping is a mechanism, not a policy.** A `claude` child can only
  resolve Anthropic ids; a `fundi` child needs a provider-qualified one. Hand
  either the wrong shape and it spawns, attaches, and never answers.

**Always set the model explicitly.** An omitted model inherits the daemon
default, which silently defeats every choice above.

### Pools are not interchangeable, and only the project knows which is scarce

Where the money comes from changes what "expensive" means:

- A **rolling-window subscription** costs opportunity rather than dollars.
  Trivial work that eats the window costs you the real work that cannot run at
  hour four.
- **Prepaid credits** cost you by going unspent.
- **Metered spend** is money.

Find out which pools this environment has and which one is constrained — that is
a CLAUDE.md/AGENTS.md fact, not something to infer. `quota_status` reports a
subscription's captured 5h and 7d utilization; consult it **once per plan**, not
per dispatch, because it is a rolling window that does not move between two
spawns and a call per dispatch is a turn per dispatch. "No data captured yet" is
normal and means *no signal*, not headroom.

## The dispatch prompt

A subagent starts with **none of your context** and cannot ask you a follow-up
mid-turn. Its prompt must stand alone.

Put in it, and nothing else:

1. One line on where this work fits.
2. The **absolute path** to its brief — "read this first; it is your
   requirements, and its exact values are to be used verbatim."
3. The **absolute path** to its report file, and what to write there.
4. Interfaces and decisions from earlier work the brief cannot know.
5. The binding constraints, quoted, not summarised.
6. The isolation block, if the work runs in a worktree (see below).
7. The commit convention: `area: what changed`, imperative, describing the
   change and never the plan or task that produced it. No task numbers, no
   `Co-Authored-By`.

Never paste accumulated history ("state after tasks 1-3") into a dispatch. A
fresh agent needs its task, its interfaces, and its constraints.

**Every path you hand a subagent is absolute.** It has its own cwd and will
resolve a relative path against it, which is how reports end up lost inside
disposable directories.

**Subagents do not spawn reviewers.** Review comes from you, after the report.
An implementer that reviews itself buys you a duplicate seat and no
independence.

## Isolation is git worktrees, and `cwd:` alone does not deliver it

`workspace: "ephemeral"` does **not** give a subagent its own checkout. It is a
*reschedulability* flag — whether the daemon may re-bind the child to a
different executor. `Provision` serves the executor's single root, so on one
executor two "ephemeral" siblings share a working tree and overwrite each other.

Make the isolation yourself:

```
git worktree add -b <branch> .worktrees/<name> <base>
agent_spawn(cwd: "<absolute path to .worktrees/<name>>", ...)
```

Worktrees share the object store, cost nothing to create, and merge with
ordinary `git merge`.

**Then do not trust `cwd:` to enforce it.** Probed and confirmed: a child
spawned with `cwd: <worktree>` still ran `pwd` in the daemon's cwd, and the
failure is worse than it sounds — it is SPLIT-BRAIN. A real implementer's file
tools wrote into the worktree while its bash verified and committed in the main
repo, which it misread as the edit tool silently losing its writes; the net
effect was a commit on the branch the work promised never to touch.

So every dispatch that needs isolation carries this block in its prompt:

- `cd <absolute worktree path> &&` first on **every** bash invocation.
- Absolute paths under the worktree for every read, write and edit.
- Before the first edit: `pwd`, `git rev-parse --show-toplevel`, and
  `git branch --show-current`. Report **BLOCKED** on any mismatch; do not
  improvise around it.
- Before committing, re-check — the commit output must name the worktree's
  branch.

And verify from outside rather than believing the report: a reviewer diffs **in
the worktree**, never trusting a claim about where the work landed.

**Worktrees hold code and nothing else.** Plans, ledgers, briefs and reports
live in the main repo at absolute paths. A worktree is deleted when its work
merges; anything written inside it that you still need is gone, and `git
worktree remove` refuses outright while untracked files are present.

## Budgets

`max_cost` is a hard fence on a child **and everything it spawns**, in USD.
`0` means unlimited — it is not "spend nothing". Always set one when
coordinating: an unbudgeted subagent is the failure that spends your whole
grant on one confused loop.

- You may raise or lower a **direct** child's cap with `agent_set_budget`
  (absolute value, not a delta). Not a grandchild's; ask the intermediate
  agent. Not your own — only your parent can raise yours.
- A raise cannot exceed your own remaining budget.
- If your own grant cannot cover the work ahead, say so now and proceed. You
  will be refused at some spawn; discovering that at the start is strictly
  better than at the end.

### The budget is an instrument, not just a fence

A cap sized to what the work *should* cost turns a breach into a signal. When a
child breaches:

- **Cheap, mechanical work** → suspect the model, not the estimate. The same
  work used to fit. Switch models and re-dispatch; do not raise the cap.
- **Hard, judgement-heavy work** → suspect the estimate. `agent_set_budget` and
  resume.
- **In between** → one `agent_view`, then decide.

Record the breach either way. It is evidence about the model regardless of how
you rule.

### Model health degrades along two axes. Review sees one.

- **Quality** — rounds-to-accept, blocked work, specs missed. Review catches
  this.
- **Economics** — the same work suddenly costing several times more. Review
  catches this **never**, and it is usually a *provider-side* regression rather
  than anything about the model: a cache layer stops hitting, and output quality
  is unchanged while cost multiplies.

The second is the one that actually happens, and nothing but the budget will
tell you. **On an economic anomaly, switch models. Do not diagnose.** Switching
costs one spawn; debugging someone else's cache layer costs an afternoon and you
do not own the fix.

## `task_*` is a durable ledger, not a scratchpad

Tasks live in the database, survive restarts, and are queryable.

- `task_add` takes **`metadata`**, which is write-once at creation. Put
  anything you will want to select on later in it at add time — it cannot be
  added afterwards. `task_update` changes status only.
- `agent_spawn(task: "2.1")` assigns that row to the new agent **atomically**,
  so `agent_list` and `task_list` agree on who owns what.
- Statuses mean what they say: `blocked` when you are waiting on something
  nameable, `failed` when you tried and could not, `task_drop` (with a reason)
  only when the work should not happen at all.
- Resolve or drop every task before considering yourself done. The daemon
  checks this and will escalate residue.

## Steering and stopping

- `agent_send` delivers a prompt to a running child — this is how you send
  review findings, extra context, or a correction.
- `agent_kill` shuts one down. Use it for a child that is looping, working from
  a premise you have since ruled against, duplicating another's work, or simply
  finished and holding a seat. A killed child's work is lost; its branch is not.
