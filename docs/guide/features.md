# Features

- **Native agent runtime (`fundi`)** — drives the Anthropic, OpenRouter, or any
  OpenAI-compatible provider directly through this repo's own library instead
  of shelling out, with breaker-gated failover. In-band abort: the process
  stays resident and abort arrives as a protocol frame.
- **Subagent trees, either kind.** `fundi` and `--kind claude` agents get the
  *same* rafiki agent-control surface — spawn, steer, budget, and watch real
  subagents, not just Claude Code's own opaque `Task` tool. Kinds mix freely:
  a `fundi` coordinator can spawn `claude` children and a `claude` session can
  spawn `fundi` ones. Depth/cost/concurrency limits are enforced by the
  daemon against stored state, never the caller's own claim, and settlement is
  push-based — a coalesced digest lands in the parent's next turn instead of
  it polling.
- **Cost-tiered fleets** — because kinds and models mix freely under one
  budgeted tree, a coordinator can plan with an expensive model, fan out
  execution across a swarm of cheap OpenRouter subagents, and review with a
  third — plan with Opus, execute with a fleet of $0.10/Mtok workers while
  Sonnet coordinates, review with Fable — all watchable live in one cockpit.
- **Script children (`--kind script`)** — a third child kind whose brain is a
  saved pymodule process instead of an LLM: a workflow driver that gets the
  whole child apparatus — lineage, labels, budget fence, inbox, cockpit row —
  while costing nothing itself, coordinating the `fundi`/`claude` children it
  spawns from Python. Hosted locally or on an executor under daraja; its exit
  is its result, and its `cost_usd` is its subtree's spend.
  [rafiki-review-swarm](https://github.com/graveland/rafiki-review-swarm) is a
  worked example: lens-driven code review where each stage is a script child
  spawning narrow reviewers, then fresh refuters, as one budgeted tree. Its
  "review home" is a git repo registered with `rafiki repo`, so rafiki clones
  it onto executors and builds its venv.
- **Batch transport rides on the model id** — a fundi child on a model id
  ending `:batch` sends its first call through the OpenRouter Batch API
  (parked, hours-scale work — the rail's `⧖` glyph) and goes live on the base
  id for every later call. No flag, no verb: the id is the feature.
- **A searchable model catalog, not a static list.** `agent_models` queries
  the live OpenRouter catalog (300+ entries) plus every locally-configured
  provider — price, context window, tool/vision support, benchmark scores
  where known — so a coordinator can find "cheapest tool-capable model under
  $0.20/Mtok" itself instead of being handed a flat id list or guessing.
- **Ships its own coordination skills, used like Superpowers.** A curated
  skill set — brainstorming an idea into a design, writing a wave-based plan,
  executing it by dispatching concurrent subagents, and the `agent_spawn`/
  `task_*`/budget mechanics underneath — is synced both to a developer's
  local Claude Code and into the daemon's own skill store, so any `fundi` or
  `claude` agent rafiki spawns can pick up the same brainstorm → plan →
  dispatch-a-fleet workflow using rafiki's own feature set, not just a human
  working on this repo.
- **Claude Code's own subagents are captured too.** Claude Code's native
  `Task`-tool subagents get split into their own conversations (and their own
  rail rows in the cockpit) rather than disappearing into one opaque parent
  turn — so a `rafiki claude` session's built-in delegation is just as
  inspectable as a rafiki-spawned one.
- **Executor plane** — a subagent's filesystem/shell/LSP tools run on
  whatever executor it's bound to, not the daemon's own process, and can
  only touch what exists there: install one inside a VM or container and an
  agent using it is confined to that VM/container's filesystem, nothing on
  the host. `rafikid` itself can run in Kubernetes with no local filesystem
  of interest at all — you connect from the cockpit on your laptop and start
  agents against executors elsewhere, isolated per agent if you want it.
  Background jobs survive a 600s `bash` ceiling and notify on completion;
  workspaces can be pinned to a machine or rescheduled across an
  interchangeable pool.
- **A Python library that follows the agent around.** `pymodule_put` saves a
  reusable snippet — a class, a helper, or a runnable script — to the agent's
  own store: versioned, soft-deletable, persisted in the daemon database,
  visible only to its owner. The store syncs to the executors that owner's
  agents land on, and `pymodule_run` executes a saved script straight out of
  that synced cache by name — never a workspace copy, while the script runs
  in the agent's working directory or a `cwd` the call names — with further
  saved modules importable for the run. Pools beyond the personal store:
  `rafiki repo` registers a repo that every
  executor clones, discovers (`scripts/*.py` plus top-level packages) and
  builds one shared venv for, so `pymodule_run` can run a repo's scripts
  against its own dependencies. Code written once in one
  conversation is still runnable in the next one, on another machine.
- **A Python SDK (`sdk/python`).** A hand-written Connect JSON client over
  `httpx` — no grpc, no protobuf runtime — whose message types are generated
  from `control.proto` by `make proto`, so the SDK cannot lag the wire.
  `Client.inside()` dials a script child's own socket (the socket is the
  credential); `Client.from_profile()` connects anywhere else, shelling out
  to `rafiki profile show -o json` so the SDK never parses profile files.
- **Watch or drive the same conversation from anywhere.** A running agent
  isn't tied to one viewer: the cockpit (`rafiki attach`), `rafiki tail`,
  another agent's MCP tools (`agent_view`/`agent_send`), and a script hitting
  the Connect API directly can all observe or steer the same conversation at
  once — the event log fans out to every subscriber, and any number of them
  can replay a child's history from an ordinal.
- **Conversation review** — an LLM-driven pass over your own captured history
  that detects skill gaps and triages findings, on demand
  (`rafiki conversations review`) or automatically on close (`rafiki close
  --review`) — no direct DB access needed on the client.
- **Provider cache guard** — OpenRouter can silently move an unpinned model to
  a colder, more expensive provider mid-conversation. `routing.ProviderGuard`
  watches for the miss pattern and ejects the bad provider automatically.
- **Declared routing, end to end.** An OpenRouter model id can carry a
  bracketed spec (`[sort=price,quant=fp8+,prefer=…,only=…,nodata,zdr]`) that
  any caller who can spawn may set; `rafiki models route` shows where a spec
  would serve a model, `rafiki route set` steers a running child's spec, and
  `rafiki providers route set` stores operator defaults per model line;
  `rafiki providers ban` ejects a bad host by hand at runtime. `nodata`/`zdr`
  are monotone — data policy cannot be routed around by any of it.
- **Model aliasing** — short names for long local/custom model ids, with a
  declared context window so Claude Code doesn't assume 200K against a
  16K-context local model and blow past it.
- **Claude Code integration** — `rafiki claude` launches Claude Code through
  the proxy with full capture, routing, and cost accounting, plus the injected
  agent-control surface described under subagent trees — and can bill your own Claude subscription
  instead of the daemon's API key.
- **The `rafiki` cockpit** — a bubbletea TUI built into the client binary for
  watching and driving a tree of agents live, no separate build step. It draws
  image thumbnails inline on terminals with Kitty Unicode-placeholder support
  (iTerm2 ≥ 3.7.0, ghostty, kitty) and a one-line placeholder elsewhere;
  `--images=auto|kitty|off` overrides detection, `^Y` toggles.
- **Multi-daemon profiles** — one client resolves distinct daemons (local
  socket or remote TLS) by name, each with its own token and model defaults;
  agent presets (named seats: model, tools, prompt, budget) live in each
  daemon's database.
- **Single sign-on** — `rafiki login` authenticates through the daemon's OIDC
  identity provider: the verified email binds the identity once, the minted
  token expires on the daemon's terms, and ownership — presets,
  conversations, executors — follows the user instead of a shared bearer
  token. Minted service tokens cover automation that can't walk the flow.
