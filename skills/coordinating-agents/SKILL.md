---
name: coordinating-agents
description: Use when spawning, steering, budgeting or waiting on subagents - covers agent_spawn/send/view/kill/set_budget, dispatching by preset, the async settle model, worktree isolation, task_* as a ledger, and cost budgets as an instrument.
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

## Dispatching by preset

A dispatch carries `preset: "<group>:<role>"` — a named seat that fixes kind,
model, tools, system prompt and budget. You never choose a model yourself; the
seats live in the daemon's database and are the `managing-presets` skill's
business.

- **Always set the preset explicitly.** An omitted preset inherits the daemon
  default, which silently defeats the seat policy. Record which preset (or its
  absence) every child ran on.
- If no preset fits the work, do not improvise a model: tune or create a seat
  per `managing-presets` — and only when the human asked. Otherwise ask.
- Override `model`/`thinking`/budgets on a dispatch only when the human named
  one; tools/skills/mcp_servers may only narrow what the preset allows.
- Pools are not interchangeable, and only the environment knows which is
  scarce — a rolling-window subscription costs opportunity, prepaid credits go
  unspent, metered spend is money. `quota_status` reports a subscription's
  captured 5h and 7d utilization; consult it **once per plan**, not per
  dispatch. "No data captured yet" is normal and means *no signal*, not
  headroom.

## Pre-filling a worker's files

When a dispatch needs files read before the first turn, pass them as the
`prefill` param of `agent_spawn` — a list of `{path, start, end}` entries
(1-based inclusive line ranges, 0 = open) — instead of pasting the contents
into `prompt`: rafiki reads them through an internal reader before the
worker's first turn and persists them as real history, so the files sit in
the conversation before turn 1 at a fraction of the prompt's token cost.
Ranges work for skill or CLAUDE.md excerpts too (`{"path": "CLAUDE.md",
"start": 1, "end": 60}`). The worker's kind must be fundi; its own tool set
need NOT include `read` — a tool-less seat (`tools: []`, the review
pipeline's batch reviewer) gets its files rendered as one text row under
`=== <path> ===` headers, while a seat that keeps `read` gets ordinary
recorded tool calls. The total read volume is capped at 60% of the model's
context window. Keep the list **identical across a wave** so every worker
shares the same cached prefix.
## On ANY failure: record the evidence, then STOP and ask

A failure is any of: a settle error, a cost-cap hit, a turn error, or a
dispatch naming a missing preset. The response is always the same:

1. Write the evidence with `task_update` (or the plan ledger): the settle
   reason or last error, the preset and model the child ran on, what it had
   done so far.
2. STOP and ask the human. **No retry, no model switch, no budget raise, no
   escalation** — a second attempt is the human's call, made with the evidence
   in front of them.

Model health degrades along two axes — quality (rounds-to-accept, blocked
work, review catches it) and economics (the same work suddenly costing several
times more; a provider-side regression review catches **never**). You cannot
diagnose the second from inside a session, which is exactly why the failure
goes to the human with the numbers instead of being retried.

## Concurrency and seat hygiene

`max_children` defaults to **4** live agents beneath you, and you cannot read
your own value. Dispatch wider work in chunks of four; a refusal past the limit
is a wasted turn.

**A settled child still holds its slot.** Settled-but-unreaped children count
against the cap, so a refusal naming the limit while everything looks finished
is not a reason to wait — it is a reaping problem. `agent_kill` finished
workers as part of handling a settle, not only for runaways. Their branches
survive; only the conversation goes. (Reap after the task's review is
adjudicated — an implementer you may need to resume warm for a fix round is
not reapable until then; if seats are starved, seats go to pending reviews
first.)

Never run two agents concurrently against the same working tree.

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

Worktrees share the object store, cost nothing to create, and integrate with
`git rebase` + `git merge --ff-only` — never a merge commit. The mechanics
live in `subagent-driven-development`'s *Merging a wave*.

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
grant on one confused loop. Size it to what the work *should* cost — a tight
cap on cheap work is how a degraded seat gets caught.

- You may raise or lower a **direct** child's cap with `agent_set_budget`
  (absolute value, not a delta). Not a grandchild's; ask the intermediate
  agent. Not your own — only your parent can raise yours.
- A raise cannot exceed your own remaining budget.
- If your own grant cannot cover the work ahead, say so now and proceed. You
  will be refused at some spawn; discovering that at the start is strictly
  better than at the end.
- **A cap hit is a failure** — see *On ANY failure* above: evidence, then STOP
  and ask. A cap is an instrument, not a licence to retry.

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
  finished and holding a slot. A killed child's work is lost; its branch is not.
  Its task row orphans — reclaim it with `task_update` after the kill.