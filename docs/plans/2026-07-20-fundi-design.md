# Fundi: Direct-API Agent Platform (pi-controller successor)

**Status:** design approved via brainstorm, not yet implemented
**Date:** 2026-07-20
**Sources:**
- `/tmp/ai.md` — "Task-Shape Coordinator: Harness-Driven Decomposition" (Brent's synthesis)
- Alex L. Zhang, "Language model harnesses are compositional generalizers" (July 2026)
- Wilson Lin / Cursor, "Agent swarms and the new model economics" (2026-07-20)

## Summary

pi-controller is forked and renamed **fundi** (Swahili: craftsman/skilled tradesman — sibling to
*rafiki*, friend). It grows a native, direct-API agent runtime (`fundi agent`) built on the rafiki
LLM substrate, replacing Claude Code as the default child kind. The daemon's supervision plane
(spawn, attach/detach/steer, labels, rings, search, multi-client subscriptions) is unchanged and
works identically for the new kind. Sentinel's `subagent_*` tools keep working because fundi
children are protocol-identical to pi children.

A **coordinator** is not a component: it is a fundi agent spawned with a planner-tier model, a
decomposition skill (the task-shape catalog), and the spawn tool. Domain work is always a
sub-call; the planner writes specs at a density matched to the worker tier it spawns (the Cursor
"Fable anti-pattern" lesson: a lazy planner makes cheap workers thrash and the savings evaporate).

## Why

- **Claude Code is the painful provider.** No in-band abort (SIGINT + respawn + `--resume`),
  silent-until-prompted readiness, synthesized prompt echo, coarse cost data
  (`total_cost_usd` only), `AskUserQuestion` disabled. Nearly all recent pi-controller churn is
  Claude-integration stabilization.
- **pi is a fast-moving target.** We don't want to rebase or fork it; Go is preferred. pi and
  claude kinds remain as legacy providers but the platform stops depending on either.
- **Model economics.** Cursor's SQLite-swarm data: planner tokens are the per-unit cost center,
  workers are the token bulk; opus-planner + cheap-worker beat frontier-solo ~8x on cost at equal
  or better quality — but only when the harness keeps planner and worker contexts separate and the
  spec density matches worker capability. Owning the loop lets us route workers to
  DeepSeek/Composer-class models via OpenRouter while reserving frontier models for planning.
- **Harness as generalizer (Zhang).** Keep every model call locally in-distribution: the
  coordinator sees task *shape*, never domain content; workers see one narrow leaf, never the
  goal. Spawn is the context-isolation primitive.

## Repos and dependency arrow

- **`~/home/rafiki`** — thin fork of `github.com/timescale/rafiki`, gitea repo at
  `git.graveland.dev/brent/rafiki` acting as the branch. The fork **renames the module** to
  `git.graveland.dev/brent/rafiki` (one mechanical commit: `go mod edit -module` + self-import
  rewrite, with the rewrite script kept in the repo so upstream merges re-apply it
  deterministically; upstreaming a patch reverses the rename on that diff). The fork stays
  structurally identical to upstream; home-specific needs (streaming sender, configurable
  truncation backstop) are upstream candidates, not divergence.
- **`~/home/fundi`** — fork of pi-controller, renamed. The platform repo: daemon, `pic`, attach
  TUI, and the new agent runtime. Keeps pi-controller's history and its ~3,500 lines of
  supervision tests.
- **Dependency arrow:** fundi → rafiki (library). Never the reverse. rafiki stays reusable by
  other consumers (e.g. Sentinel could later front Zoe's LLM traffic with rafiki's proxy faces).
- **Build wiring:** a normal dependency. fundi's go.mod does
  `require git.graveland.dev/brent/rafiki vX.Y.Z`; CI sets `GOPRIVATE=git.graveland.dev` and
  fetches from gitea like any other module. No replace, no submodule, no workspace requirement in
  CI. `go.work` (uncommitted) remains purely a local-dev convenience for live-editing both
  checkouts.

## Architecture

### Components

1. **rafiki (library):** senders (Anthropic SDK native + OpenRouter Anthropic-compatible),
   routing (circuit breakers, OpenRouter model catalog, model resolution, effort map), `agentloop`
   (tool-use loop: bounded concurrency 6, per-result backstop truncation, `is_error` tool
   failures, iteration cap, context trim, resume with synthetic errors for orphaned tool_use),
   durable DB-backed `Conversation` store (write-ahead messages + turn evidence), prompt-cache
   breakpoint management, `prefix_hash` cache-drift detection.
2. **fundi daemon (existing pi-controller core, unchanged):** child supervision, event bus, ring
   buffers, state machine, label-filtered dynamic subscriptions, store/search, UDS + TCP+token
   control plane.
3. **`fundi agent` (new):** a subcommand of the same multi-call binary. Three layers:
   - *Front-end:* pi rpc protocol over stdio — emits `AgentSessionEvent` frames, accepts
     prompt/steer/abort frames. Protocol types stay internal to the module (shared code, no
     export needed).
   - *Engine:* rafiki `Conversation` + `agentloop`, model pinned per spawn.
   - *Toolset:* core tools + skills + MCP + spawn (below).
4. **New child `Kind` ("agent"):** identity-provider semantics (no translator files).
   `resolveSpawnPlan` execs `os.Executable()` with the `agent` subcommand and a config blob.
   **The process boundary is kept deliberately:** a worker OOMing or panicking must not take down
   the supervision plane; process groups, kill semantics, and per-child cwd/worktrees assume real
   processes.
5. **Sentinel/Zoe (consumer):** no M1 changes beyond passing `kind=agent` + model through the pi
   plugin's spawn path (`SpawnRequest.Kind` already exists) and a config default preferring fundi
   children. `_fleet` tier, signal batching, labels: unchanged.

### Transport and launcher seams (designed in now, remote later)

The daemon↔child link is a **framed bidirectional stream**: stdio pipes locally (v1), TCP/TLS
remotely (later). Remote children **reverse-dial**: the launcher starts the agent with
`--connect <controller-addr> --token <t> --child-id <id>`; the agent dials home, authenticates,
and streams the same frames it would have written to stdout. Reverse-dial avoids requiring
network reachability into pods/VMs.

**Launcher abstraction:** local exec (v1), k8s Job/pod (later), VM/ssh (later). Lifecycle is
protocol-level for the agent kind (abort/steer/shutdown are in-band frames), so remote mapping is
clean: kill = in-band shutdown + launcher teardown; crash = connection drop + launcher status.
Only the agent kind is transportable; pi and claude kinds stay local-exec legacy.

Deliberately punted to the launcher milestone: remote workspace access (clone-in-pod vs shared
volume vs artifact-only tasks) and remote resource limits.

## Agent-loop internals

### Turn engine

One inbound `prompt` frame = one `agentloop.Run` over a rafiki `Conversation`. Fundi wraps the
loop and emits the pi event sequence: `agent_start` → `message_start`/`message_update` →
`tool_execution_start`/`end` per tool → `agent_end` (full message list) → `agent_settled`, plus
usage frames from rafiki's turn capture (real per-turn tokens/cost).

- **Steer:** queue mid-turn text; inject as a user block alongside the next tool-result message.
- **Abort:** cancel the in-flight request via context; synthesize error results for orphaned
  `tool_use` blocks (rafiki's resume mechanism); emit `agent_end`; process stays resident and
  idle. No SIGINT, no respawn.
- **Streaming gap:** rafiki's library sender is non-streaming (`Messages.New`); only its proxy
  faces parse SSE. v1 emits message-granularity events (attach shows tool activity live, not
  token-by-token text). M3 adds a streaming sender to the rafiki fork (upstream candidate — the
  SSE parsing already exists in `routing/`). Design event emission so the streaming sender slots
  in without protocol changes.

### Tools

- **Bash:** per-call process in the child's cwd, timeout + output policy (below). No persistent
  shell in v1.
- **Read/Write/Edit/Grep/Glob:** standard semantics; read-before-write tracking so Edit refuses
  stale writes. Read pages (offset/limit) instead of truncating.
- **Skill:** inventory (name + description) rendered into the system prompt; invocation loads the
  SKILL.md body into the conversation. Discovery from the same directories Claude Code uses, so
  existing skills work as-is.
- **Spawn:** client of the controller socket — spawn/send/watch/collect/kill with `model` and
  `kind` passthrough. Children are auto-labeled with the parent's child-id so the task tree is
  queryable and label-subscribable.
- **MCP:** `.mcp.json`-compatible config; stdio + HTTP servers; tools exposed as
  `mcp__server__tool` through the ToolSet seam.

Out of scope for v1: hooks, slash commands, interactive permission prompts.

### Tool-output policy (spill, never destroy)

- Every over-budget tool result is written **in full** to a per-turn scratch file
  (`~/.fundi/run/<child>/out/<tool-call-id>`); the truncated result includes the path + total
  size so the agent can Grep/Read the remainder.
- Clipping keeps head **and** tail (~20%/80% of budget, tail-weighted — test/build verdicts are
  at the end) with an explicit `[... N lines elided, full output at <path> ...]` marker.
- Per-tool budgets, configurable per spawn (cheap small-context workers get tighter budgets than
  planners — part of the economics story).
- rafiki's per-result backstop stays as the loop-level guard; fundi's policy fires first so the
  backstop should never trigger.
- **Lifecycle:** spill files live under the child's run dir, are snapshotted/retained on exit
  like ring snapshots, and are **deleted when the child record is forgotten** — one retention
  story for everything a child leaves behind.

### Context assembly

System prompt = fundi base prompt + CLAUDE.md/AGENTS.md chain (user-global, project,
@-includes) + skills inventory + environment block (cwd, platform, model). Static content ordered
for cache stability; rafiki's `prefix_hash` detects cache breakage.

### Model routing

Per-spawn `--model`: family aliases (`haiku-latest`, `sonnet-latest`, `opus-latest`) or explicit
OpenRouter IDs (DeepSeek, Composer-class) resolved through rafiki's catalog, pinned per
conversation; fallback chains and circuit breakers inherited. Reasoning effort is a second
per-spawn knob (rafiki effortmap).

### Persistence and resume

With a DB configured: write-ahead conversation persistence; `ctrl_resume` re-execs
`fundi agent --resume <conversation-id>`; rafiki `Resume` reconstructs state (orphaned tool calls
get synthetic errors, never re-executed; resume-attempt cap). Without a DB: in-memory; daemon
rings still provide post-mortem scrollback.

### Permissions

Parity with today: no interactive permission gate (current children run with permissions skipped
and `AskUserQuestion` disabled). Isolation = per-child cwd/worktrees now, launcher-level
sandboxes (k8s) later. A `blocked_ui`-style approval gate remains possible through the existing
state machine; v1 does not build it.

## Coordinator (Approach A: prompt, not code — evolve toward scaffolds)

A coordinator is a fundi agent spawned with:

- **Planner-tier model** (opus-family).
- **Decomposition skill** (SKILL.md): the task-shape catalog (investigate→synthesize,
  design→implement→verify, compare→decide, monitor→alert, fan-out→reduce, plan→execute→verify,
  converse→capture), shape triggers and execution structures, and **per-worker-tier spec-density
  rules** (DeepSeek-tier workers get exhaustive specs — file paths, acceptance criteria;
  sonnet-tier workers get goals).
- **System prompt hard rules:** (1) never implement — domain work is always a sub-call;
  (2) spec density must match the worker tier being spawned.
- **Spawn tool.** Worker subtrees carry the parent's label; any node is watchable/steerable via
  `pic` or Zoe.
- **Stacked review as prompt policy:** the skill instructs the coordinator to spawn a
  different-model reviewer for each coding leaf before accepting it (decorrelated lenses).

Shapes are priors, not enforced structure (Zhang's bitter-lesson warning about hand-crafted
task-specific structure). Once a shape has proven out in real use, it may be hardened into a
deterministic scaffold tool (`run_fanout(items, worker_spec, model_tier)`) — the A→C evolution.
Nothing in v1 commits to scaffolds.

## Milestones

- **M0 — forks.** rafiki → `~/home/rafiki` (+ gitea); pi-controller → `~/home/fundi` (rename);
  go.work + pinned replace.
- **M1 — the flip.** `fundi agent`: pi-rpc front-end, rafiki engine, core tools with
  spill/elision policy, CLAUDE.md + skills + MCP loading, per-spawn model routing, resume; new
  child kind via multi-call exec; Sentinel pi-plugin passes kind+model. *Exit criterion: Zoe and
  `pic` run real tasks on fundi children with in-band abort and per-turn cost; claude kind demoted
  to fallback.*
- **M2 — the economics.** Spawn tool + decomposition skill + review-lens policy
  (coordinator-as-prompt). Per-subtree cost instrumentation from rafiki's turn store. Run the
  spec-density experiment: same task, DeepSeek-tier vs sonnet-tier workers, measure thrash
  (worker tokens per accepted leaf).
- **M3 — proven-out hardening.** Streaming sender in the rafiki fork (token-level attach);
  scaffold tools for shapes that earned it; TCP reverse-dial transport + k8s launcher.

## Testing

1. **Loop tests** against a fake rafiki `Sender`: scripted model turns exercising steer, abort,
   orphaned-tool synthesis, resume, and truncation/spill paths deterministically.
2. **Protocol golden-frame tests** reusing pi-controller's existing dispatch/integration harness:
   fundi children must be indistinguishable from pi children at the frame level.
3. **Live smoke task** on a cheap model as the M1 acceptance gate: spawn → skill invocation →
   edit → bash → settle, attached throughout.

## Open questions (carried forward, not blockers)

- How tight do DeepSeek-tier specs need to be vs sonnet-tier? (M2 experiment.)
- Can local Qwen handle routine worker tasks at zero marginal cost?
- Shape switching mid-task: re-prompt suffices under Approach A; revisit if shapes get scaffolds.
- Field-Guide-style shared memory (agent-curated, line-budgeted) — substrate could be the
  memory engine; deferred past M3.
- Remote workspace strategy for k8s workers (clone-in-pod vs shared volume vs artifact-only).
