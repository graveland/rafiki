---
name: coordinating-agents
description: Use when spawning, steering, budgeting or waiting on subagents - covers agent_spawn/send/view/kill/set_budget, the async settle model, task_* as a ledger, worktree isolation, and cost budgets as an instrument.
---

# Coordinating agents

You spawn subagents; they run in their own processes, possibly on other
machines, spending real money. This is what the tools actually do.

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
6. The commit convention: `area: what changed`, imperative, describing the
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

## Isolation is git worktrees, not `workspace:`

`workspace: "ephemeral"` does **not** give a subagent its own checkout. It is a
*reschedulability* flag — whether the daemon may re-bind the child to a
different executor. `Provision` serves the executor's single root, so on one
executor two "ephemeral" siblings share a working tree and will overwrite each
other.

To isolate concurrent work, make the isolation yourself:

```
git worktree add -b <branch> .worktrees/<name> <base>
agent_spawn(cwd: "<absolute path to .worktrees/<name>>", ...)
```

Worktrees share the object store, cost nothing to create, and merge with
ordinary `git merge`.

**Worktrees hold code and nothing else.** Plans, ledgers, briefs and reports
live in the main repo at absolute paths. A worktree is deleted when its work
merges; anything written inside it that you still need is gone, and `git
worktree remove` refuses outright while untracked files are present.

## Concurrency

`max_children` defaults to **4** live agents beneath you, and you cannot read
your own value. Dispatch wider work in chunks of four; a refusal past the limit
is a wasted turn.

Never run two agents concurrently against the same working tree.

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
  a premise you have since ruled against, or duplicating another's work. A
  killed child's work is lost; its branch is not.
