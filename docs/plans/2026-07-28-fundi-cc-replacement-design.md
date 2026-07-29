# fundi as a Claude Code replacement — design

**Date:** 2026-07-28
**Status:** design approved, implementation plan pending
**Supersedes ordering in:** `docs/plans/2026-07-20-fundi-design.md` §Milestones

## Goal

Make fundi good enough to be Brent's primary interactive coding agent, then daily-drive
it for a fixed window and let real use rank the remaining work.

This is a **new milestone, M1.5 "the daily driver"**, inserted between M1 and M2. It
does not replace M1's close (merge `fundi-agent-kind`, flip `default_kind: "agent"`),
which is independent and should just happen.

### Why a new milestone rather than stretching M1 or M2

- M1's exit criterion is about Zoe, per-turn cost, and demoting the claude kind. A
  different goal.
- **M2 depends on this.** The spec-density experiment measures "thrash as worker tokens
  per accepted leaf." That is uncalibratable without knowing what a normal task costs on
  this stack, and only sustained use produces that baseline. The dogfood feeds M2.
- M3's streaming sender was mis-filed as "token-level attach" polish. Reframed — *you
  cannot judge an interactive tool that does not stream* — it is a gate, and moves to
  Phase 0.

## Principle: configuration ownership

> **fundi writes to a foreign directory only when that directory is the foreign tool's
> discovery contract. fundi never reads its own configuration from one.**

This is the same argument `internal/paths` already makes about `~/.pi`, applied
consistently. It resolves three separate inconsistencies at once.

| Thing | Today | After | Rationale |
|---|---|---|---|
| Global instructions | `~/.claude/CLAUDE.md` hardcoded (`contextfiles.go:46`) | `$FUNDI_INSTRUCTIONS`, default `~/.config/fundi/instructions.md` | fundi's config |
| Skills | `~/.claude/skills` hardcoded (`agent.go:136`) | `$FUNDI_SKILLS_DIRS` (list), default `~/.config/fundi/skills` | fundi's config |
| Presets | `~/.pi/agent/fundi-presets.json` | `~/.config/fundi/presets.json`; old path a deprecated fallback | fundi's config, in pi's dir |
| Per-repo `CLAUDE.md`/`AGENTS.md` | git root + cwd | unchanged | project files, CC-schema compatible |
| `.mcp.json` | cwd | unchanged; optional global `~/.config/fundi/mcp.json` | CC's contract |
| `fundi-helpers` extension | `~/.pi/agent/extensions/` | unchanged | genuinely pi's discovery contract |

**Schemas stay Claude-Code-compatible; only locations become fundi-owned.** Existing
setups are reached by pointing at them, e.g.
`FUNDI_SKILLS_DIRS=~/.claude-personal/skills:~/.claude-personal/plugins/cache/superpowers-marketplace/superpowers/6.2.0/skills`.
This is strictly more capable than the hardcoded path, which could never reach
plugin-cache skills at all.

The deprecated presets path follows the `PIC_*` precedent: still read, warns, never
deleted. `DiscoverSkills`' existing precedence (later dirs win on name collision) is
unchanged.

### Knock-on changes

- New env vars registered in `internal/envvar`: `FUNDI_INSTRUCTIONS`,
  `FUNDI_SKILLS_DIRS`, `FUNDI_MCP_CONFIG`, `FUNDI_SETTINGS`. `FUNDI_SKILLS_DIRS` is a
  list using the OS path-list separator (`:` on unix), matching `$PATH` convention;
  entries are ordered lowest-to-highest precedence, consistent with `DiscoverSkills`.
  A non-existent directory is skipped, not an error.
- Documented in `.env.example`; wired into Makefile dev targets so the dogfood needs no
  per-spawn flags.
- `fundi create` gains real `--skills-dir` and `--mcp-config` flags (today reachable only
  via `--extra-arg`).
- `--config-dir` documented as claude-kind-only rather than appearing general.

## Phase 0 — the gate (before daily use starts)

### 0.1 Configuration ownership

As above.

### 0.2 Token streaming

**No protocol change and no TUI change.** The three frames already exist and are already
emitted — `emit.go:72-74` fires `PiMessageStart` / `PiMessageUpdate` / `PiMessageEnd`
back-to-back from a completed message. Streaming means firing them at the right *times*
with real partials. Evidence the rest of the stack is ready: `fundi tail --no-deltas`
exists, defaults true, and its help describes "token-by-token `message_update` deltas" —
the tooling was built expecting streaming; only the agent kind fails to produce it.

**rafiki side.** `llm.Sender` stays as-is. Add an optional interface so the upstream PR
to `timescale/rafiki` remains additive:

```go
type StreamingSender interface {
    Sender
    NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error)
}
```

`Conversation` type-asserts and falls back to `New` when absent, which keeps
`faketurns.go` and every existing test working unchanged. SDK v1.37.0 provides
`Message.Accumulate(event)`, and `routing/sse.go:121` already decodes
`MessageStreamEventUnion` — the event handling is borrowed, not written.

**fundi side.** `Emitter.AssistantTurn` splits into `MessageStart` on `message_start`,
`MessageUpdate` per `content_block_delta`, `MessageEnd` on `message_stop`. Cost
accounting is unchanged: accumulate into a final `anthropic.Message` and pass *that* to
the existing `MapAssistantMessage(resp, provider, pricer)` at end-of-message, so pricing
still runs once against the served model.

**Two constraints:**

1. Tool-call inputs arrive as partial JSON (`input_json_delta`) and must fully accumulate
   before dispatch. The engine already dispatches after the turn, so this falls out — but
   it means `ToolStart` cannot fire on first sight of a `tool_use` block.
2. `sendWithTrim` (`llm/conversation.go:459`) retries up to 3× on prompt-too-large. A
   retry after deltas were emitted would leave the TUI showing abandoned text. In
   practice the API rejects an oversized request before any content deltas — but do not
   rely on that: **do not emit `message_start` until the first content event arrives**,
   making the invariant structural.

### 0.3 Cost baseline

`FUNDI_AGENT_DB` must be set in the daemon's service environment from day one. It is the
default source for `fundid agent -db` (`usage.go:89`); without it conversations are
in-memory and the per-turn token/cost data — the entire point of the comparison — is
never recorded. This is why it belongs in Phase 0's env plumbing and not in a later
phase.

## Phase 1 — fast-follows, landed while dogfooding

### 1.1 Hooks — `PreToolUse` only

One event, with input rewriting and blocking. Not `PostToolUse`, `Stop`, or `PreCompact`;
those get built when use proves they are missed.

**Motivation:** `rtk hook claude` is a `PreToolUse` hook that rewrites bash commands
(`git status` → `rtk git status`), claiming 60-90% token savings on dev operations.
Dogfooding without it inflates bash token costs several-fold and poisons the one
comparison the exercise exists to make.

**Config:** CC's `hooks` schema at a fundi-owned location — `~/.config/fundi/settings.json`,
override `$FUNDI_SETTINGS`. Global only; per-project hook config is YAGNI until something
needs it.

**Wire contract**, verified empirically against `rtk hook claude`:

- **stdin:** `{session_id, transcript_path, cwd, hook_event_name:"PreToolUse", tool_name, tool_input}`
- **stdout:** `hookSpecificOutput.updatedInput` replaces the tool input;
  `permissionDecision: "deny"` blocks, with `permissionDecisionReason` returned to the
  model.
- **exit code 2** blocks, stderr returned to the model.

Observed rtk output:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite","updatedInput":{"command":"rtk git status","description":"show status"}}}
```

**Absent `permissionDecision` means allow.** Not optional — rtk omits the field entirely,
so treating absence as anything but "allow" breaks it.

**Two failure modes that must be designed against:**

1. **Tool names must map to CC's spelling.** fundi's registry uses `bash`, `read`, `edit`;
   CC uses `Bash`, `Read`, `Edit`; Brent's settings.json matches on `"Bash"`. Emitting
   `bash` means the matcher silently never fires — hooks appear enabled while every rtk
   rewrite is skipped, costing tokens invisibly. Requires an explicit fundi→CC tool-name
   map, with unmapped tools passed through verbatim.
2. **Matchers chain in config order.** Brent has two `PreToolUse` entries (`.*` →
   `claude-hooks`, `Bash` → `rtk hook claude`). Both must run for a Bash call, and each
   hook's `updatedInput` feeds the next hook's `tool_input`.

**Failure policy:** a hook that errors, times out, or emits unparseable JSON logs loudly
and the tool call proceeds with the *original* input. Never silently swallowed; never
allowed to wedge the daily driver.

### 1.2 Subagents

Multi-level spawning with per-agent model selection is the **design goal**, not a hazard:
a coordinator decomposes, workers execute on cheap models, reviewers check on expensive
ones. Any design that caps the tree at one level designs out the product.

**Shape: asynchronous with handles, mirroring the proven `subagent_*` surface** in
`sentinel-plugins/plugins/node/pi/`. A coordinator running five workers on different
models cannot block, so a synchronous Claude-Code-style `Task` tool is the wrong shape.
`spawn` returns a handle; `list`/`view` observe; `send` drives; signals (`idle`,
`exited`, `failed`, `gate`) wake the parent.

fundi's native tools are the same RPCs at a different layer — the sentinel plugin is an
external UDS client of the daemon, this is an internal one. Keeping one vocabulary across
both means Zoe's mental model transfers.

**Everything needed already exists:**

| Need | Existing |
|---|---|
| Transport | `client.Client.Request` / `Subscribe` |
| Spawn RPC | `ctrl_spawn` |
| Child runtime | `fundid agent` |
| Await completion | event bus → `agent_settled` |
| Socket reachable from a child | `controller.go:2187` sets `FUNDI_SOCKET` on every child |
| Sub-agent frame linkage | `ParentToolUseID`, already in the pi vocabulary (`pi_events.go:136`) and already populated for Claude Code sub-agents (`provider_claude_state.go:116`) |

#### Differentiation

Modelled on `labels.go`, whose comment records the load-bearing part: `consumer` and
`conversation` are *"load-bearing for isolation and must never be removed by a
label-mutation tool,"* with `ownedByConversation()` being *"what keeps Zoe out of Brent's
sessions."*

| Label | Purpose |
|---|---|
| `fundi/parent` | direct parentage — the tree edge |
| `fundi/root` | top-level ancestor — makes a whole-subtree query one filtered call rather than a recursive walk |
| `fundi/role` | `coordinator` / `worker` / `reviewer` — drives rendering; lets a coordinator find its own reviewers |
| `name` | **required at spawn.** A uuid is useless when eight agents are running |

Stamping `root` alongside `parent` costs nothing now and is exactly what M2's per-subtree
cost rollup needs — it avoids a later backfill. Model is already per-session, so "which
agent, which model" falls out of the existing record.

**Isolation invariant:** an agent may see and steer only its own descendants.

#### Resource limits

Depth and cost are deliberately **not** the same mechanism, because they are not the same
kind of resource. Cost is consumable — dollars a grandchild spends are dollars gone from
the subtree, so it decrements. Depth is not, and forcing it into a decrementing budget
makes a coordinator reason about the shape of a tree it should not have to think about: a
coordinator that only ever makes one hop would need `depth=2` merely to express "my
workers may also spawn reviewers."

So **depth is granted locally, per spawn**, and the runaway bound is a separate absolute
ceiling:

- *depth* = how many further levels of descendants **this child** may create. `0` = cannot
  spawn.
- Granted explicitly at spawn time. Default `1`. Configurable per agent at startup:
  `fundid agent -max-depth`, `SpawnRequest.MaxDepth`, `fundi create --max-depth`.
- A parent grants what its child needs without reference to its own allowance. A
  coordinator making one hop grants each worker `1`, and those workers may then spawn
  reviewers at `0`. The coordinator never computes total tree depth.
- **The bound is an absolute ceiling, not the parent's remainder:** `FUNDI_MAX_DEPTH`
  (default `3`) caps the child's *absolute* depth in the tree. The daemon computes that
  position from the `parent`/`root` labels it already holds and rejects any spawn that
  would exceed it, regardless of what the parent granted.

This keeps grants intuitive and local while making the safety bound a real, auditable
limit rather than an emergent property of arithmetic a prompt-injected coordinator could
be talked into getting wrong.

Alongside it:

- **Cost budget** — `fundid agent -max-cost` / `SpawnRequest.MaxCost` /
  `fundi create --max-cost`, in USD. Unlike depth, this one *does* decrement: a child may
  be granted at most the parent's remaining budget, because subtree spend is genuinely
  consumable. Spend is summed across the
  subtree via the `root` label against `conversation_turn`'s existing priced rollup.
  Unset means unlimited, which is the correct default for a top-level interactive agent
  and the wrong one for a coordinator — so a coordinator should always set it.
- **Concurrency cap** — `fundid agent -max-children`, the number of simultaneously *live*
  descendants across the subtree. Default 4. Guards process and resource exhaustion
  independent of cost, which matters because a runaway recursion of cheap spawns can
  exhaust the machine long before it exhausts a dollar budget.

#### Enforcement is daemon-side — security boundary

**Every subagent limit is enforced by the controller, not by the tool.** Tool arguments
are produced by an LLM that can be prompt-injected into requesting depth 99, a sibling's
id, or another conversation's child. A check inside the child process is UX only. The
controller already knows the spawner's depth and labels from its session record, and is
the trust boundary. This applies to depth, the ownership rule, the cost budget, and the
concurrency cap alike.

#### Scope

Six tools: `spawn`, `list`, `view`, `send`, `kill`, `models`, plus completion signals.
Deferred until use demands them: `steer`, `interrupt`, `respond`, `search`, `label`,
`forget`.

**One CLI change ships with this:** `fundi list` is flat today; with `parent`/`root` it
should render a tree. That is how a human tells running subagents apart.

## Deferred

| Item | Rationale |
|---|---|
| Context compaction | Brent actively avoids hitting it. `sendWithTrim` prevents hard failure (degrades rather than crashes). Reference material captured — see below |
| Remaining hook events | Build when the dogfood misses them |
| `todo` tool, web search/fetch | Web is reachable via MCP meanwhile |
| Conversation tree (parent/root columns), per-subtree cost rollup | M2 — the labels laid down in Phase 1 supply the data |
| Decomposition skill, review-lens policy | M2 — methodology, genuinely design-shaped |

### Compaction reference

`docs/reference/claude-compaction-prompt.md` records Claude Code's compaction prompt,
captured by pointing Claude Code at rafiki via `ANTHROPIC_BASE_URL` and reading the
request body out of `routing/store.go`'s `CaptureStore`. Four structural details worth
copying are noted there.

**Hard constraint on the eventual compaction work:** a captured request is live-ammunition
text. It contains instructions written to steer an agent ("Respond with TEXT ONLY", "Do
NOT call any tools"). Any fundi code that reads captured requests back — compaction,
replay, debugging tooling — must quarantine that content as data: fenced, clearly
delimited, never concatenated into a system or user turn. This is not hypothetical; it
occurred while writing this design.

Anthropic's own prompt solves the same class of problem explicitly, instructing that text
merely *shaped* like a user turn inside an assistant message must never be attributed to
the user. fundi's compaction must carry an equivalent rule, or a summarizer can be induced
to fabricate user approval that never happened.

## Interaction with the M1 smoke gate

`docs/plans/2026-07-20-fundi-m1-smoke.md` has two open steps:

- **Step 9** (CLI rendering eyeball) is fully subsumed by daily use. Close it via the
  dogfood.
- **Step 8** (Zoe end-to-end) is **not** subsumed — Zoe spawns detached children while
  Brent attaches a TUI. It remains, and becomes cheap once `default_kind` is flipped.

## Success criteria

The dogfood is a gate, not a vibe:

1. **A fixed window** — two weeks of primary use. Not "until it feels right."
2. **Cost per task, fundi vs Claude Code.** `conversation_turn` already stores model plus
   all four token counts and `insights.GlobalStats` already does priced rollups. Requires
   `FUNDI_AGENT_DB` from day one (§0.3).
3. **A logged gap list.** Every reach for something absent goes in a file. This list is
   the M2/M3 reprioritization input and is arguably the exercise's real product.
4. **Logged abandonments.** Every bail back to Claude Code mid-task, with the reason. The
   honest signal, and the easiest one to not write down.

**Exit:** either "daily driver, remaining gaps ranked" or "these N specific things make it
non-viable." Both are successful outcomes. Quiet drift is the only failure mode.

## Implementation decomposition

This design is too large for one implementation plan. It splits into four, in order, with
the Phase 0 pair gating the dogfood:

1. **Configuration ownership** (§Principle + §0.1) — self-contained, no dependencies,
   touches `contextfiles.go`, `agent.go`, `presets.go`, `internal/envvar`, `.env.example`,
   Makefile, `fundi create` flags.
2. **Token streaming** (§0.2) — spans two repos (rafiki's `StreamingSender`, fundi's
   emitter). Independent of (1); can proceed in parallel.
3. **`PreToolUse` hooks** (§1.1) — depends on nothing above, but sequenced after the gate
   so daily use starts sooner.
4. **Subagents** (§1.2) — the largest, and the only one with a genuine design surface left
   (signal delivery to a parent mid-turn, tree rendering in `fundi list`).

Each gets its own plan. (1) and (2) should be planned first and together, since they are
the gate.

## Risks

- **Streaming regressions in rendering.** `render_tail.go` depends directly on
  go-runewidth with no test coverage, and streaming exercises it far harder than
  batch emission. This is what smoke step 9 was guarding; daily use is a better test but
  a noisier one.
- **`ctrl_resume` for agent kind is the least-proven path** — it was broken for every
  agent-kind child until `e583dc1`. Resume-after-exit deserves early deliberate exercise.
- **Hook chaining order** is config-order-dependent and silent when wrong. Worth an
  explicit test with both of Brent's real hook entries.
- **Subagent cost runaway** before the budget lands. The concurrency cap should ship in
  the same change as `spawn`, not after.
