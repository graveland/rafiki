---
name: sandboxed-execution
description: Use when planning, dispatching, reviewing or finishing work that will run inside a rafiki sandbox (a container-backed executor) - covers which preset seats a sandbox can run at all (claude-kind children cannot), what the mount decides about extracting work, what a sandbox cannot see, and why verification and finishing cross back to the host.
---

# Running work in a sandbox

A **sandbox** is a container a launcher creates on request; it runs `rafiki executor
serve` and enrolls as an ordinary executor. It is therefore a **durable executor row**,
not a disposable process: `sandbox_list` reports it, its row is the authority on it, and
it outlives any one child. For the mechanism — creation, the create-body allowlist, the
relay, ownership lookup, lifetime — read `executor-plane`. This skill is the
practitioner's view of *running work inside one*.

## Decide the seats before you plan

An executor advertises the launch kinds it can run. A sandbox's create body fixes its
command to `rafiki executor serve --connect-socket … --launch script`, and the
create-body allowlist admits nothing else, so **a sandbox hosts fundi and script children
but never `claude`** (the image carries no claude binary). A claude-kind spawn into one is refused outright (`spawn
refused: no executor can launch a "claude" child …`), and there is **no fallback**: a
sandboxed child never forks locally (`localForkRefused`, see `executor-plane`), a child
narrowed to a sandbox cannot widen to another executor, and overriding a preset's kind is
refused (`kind "fundi" conflicts with preset "…" (kind "claude")`).

Consequence: **a preset group whose value is a claude reviewer or final-reviewer cannot
run in a sandbox at all**. Choose the group and the execution venue *together, at plan
time*. Discovering it mid-run costs the review checkpoints, and the coordinator is right
to refuse a substitute seat. Sandboxes are for fundi-kind work.

## The mount decides whether anything has to be extracted

- **Mounted at the same path** — `host_path` and `target` identical — the plan's absolute
  paths (worktree paths, doc paths) stay valid with no rewriting, and the container's
  writes are *real*: commits, worktrees and merges land in the host checkout. Nothing
  needs pulling out. The trade is that a bind mount is not file isolation.
- **Unmounted** (the container's own filesystem) — the work is trapped until you move it.
  `sandbox_sync` copies a file or directory between executors; `sandbox_sync_repo` fetches
  **one committed branch**, and *only committed state travels*. So **a worker in an
  unmounted sandbox must commit**, or its work cannot be reached at all. Put the
  extraction step in the plan — a review that needs the changes must not be the first
  place anyone finds out they are unreachable.
- `overwrite` is permitted only when the destination executor's row says
  `isolation=container`.
- **Read-only mounts are live, not snapshots.** Editing a mounted doc on the host changes
  what a running agent reads at its next read — handing a dispatched worker an amended
  spec costs one file write, no restart and no copy.
- Mounts are bounded by the launcher's declared mount roots; a path outside them is
  refused at create time. Choose roots deliberately: a root is a grant of everything under
  it (a `ro` mount reads it, an `rw` mount writes it) to any sandbox that can be created
  on that launcher.

## What a sandbox cannot see

Live or production state that is not mounted, credentials, and any container-runtime
socket. Anything needing them runs on the host — a real store, a push, a credential'd
mirror refresh. Treat this as the point of the exercise rather than an obstacle: a
sandbox that cannot reach production is one that cannot damage it. Do not mount production
state to make a step "work".

## Python and script children

A sandbox's executor serves `--launch script`, so `pymodule_start` and `kind=script`
spawns can land in it, using the image's `python3`/`uv`. Launching it also turns on the
owner's pymodule syncs into the container:

- **Saved modules** (`repo: "local"`) are pushed over the relay and work under any
  network setting.
- **Git sources** are cloned by the *container's own git*, so they need `network: egress`.
  Under `network: none` the refresh fails for that executor only — it is logged, the
  other executors' snapshot stays, and `local` modules are unaffected — but a script whose
  `repo` names a git source cannot resolve in that sandbox. Choose the network with the
  repo kind in mind.
- A git source URL that embeds credentials is delivered to every sandbox the owner holds,
  contradicting "a sandbox cannot see credentials". Use credential-free URLs for sources a
  sandbox will sync.

## Verification inside is evidence, not proof

The container's runtime is not the host's, and a container has tools the host may not.
A gate that passes in-container proves the change against *the container's* runtime. When
the artifact will run on the host, **re-run the gate there before anything real is
touched** — the host run is cheap and it is the one that counts. Make that an explicit
step in the plan, not an intention.

## Reviews and finishing cross the boundary

A sandboxed coordinator cannot dispatch claude seats, so the review checkpoints and every
host-only finishing step land outside it. Expect a sandboxed plan to have a **host-side
tail** — reviews, landing, live-state migration, push, mirror refresh — and write it into
the plan. Two rules keep that tail honest:

- The coordinator must **refuse** to improvise a substitute seat (a bare model, a changed
  kind) and report blocked. A review run on the wrong seat is worse than one not run.
- The dispatcher owns the tail. A coordinator that parks with the change gated but
  unlanded has done its job correctly; landing it is not a loose end to improvise.

## Scope and lifetime

Two shapes, with disjoint fields. A **spawn block** (`sandbox` on `agent_spawn`) takes
`scope` — `self` (the new agent only) or `subtree` (its descendants too); a coordinator
that will spawn workers needs `subtree`, or its children are not in the container. It has
no `ttl`: it is removed when its owning child closes, so closing a `subtree` owner pulls
the container out from under its live descendants. A **named sandbox** (`sandbox_create`)
takes `ttl` (default 7 days, capped by the daemon) and no `scope`; the sweep removes it on
expiry. Everything in a `subtree` block shares one container, so a **shared `/tmp`** is how
one worker's artifact (a generated script, a fixture) reaches another's step. Because a
sandbox is a durable row, `sandbox_list` shows every sandbox your owner holds, including
ones other children created — never assume a lone result is yours.

## Before dispatching a sandboxed plan

1. Are any seats claude-kind? If so those checkpoints run on the host — plan for it.
2. Is the target path mounted at its own path, or will the worker's output need
   extracting? If extracting, the worker must commit.
3. Does any step need live state, credentials or a runtime socket? That step is host-side.
4. Does the plan say who runs the host-side tail, and that the coordinator must not
   improvise a seat it cannot dispatch?
5. Is there an explicit step to re-run the gate on the host before anything real is
   touched?
