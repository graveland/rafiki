# rafiki

An LLM proxy and conversation-capture store, an agent library built on it, and
a daemon (`rafikid`) that hosts coding-agent children — driven from a CLI/TUI
client (`rafiki`).

## Quick start

```sh
brew install graveland/tap/rafiki   # macOS; Debian/Ubuntu: see docs/guide/install.md
rafiki setup                        # database, providers, daemon, your user, a local executor
rafiki doctor                       # daemon + executor + one agent, end to end
rafiki create                       # start an agent
rafiki                              # the cockpit
```

`rafiki setup` needs Docker (or OrbStack), a Claude login and `ripgrep`. API
keys are optional. [Installing rafiki](docs/guide/install.md) has the details,
including upgrades. Running a shared daemon on a server instead of a laptop is
[Deploying rafiki for real](docs/guide/deployment.md).

## What it is

- **Native agent runtime (`fundi`)** alongside Claude Code (`--kind claude`) and
  script children (`--kind script`). All three get the same agent-control
  surface, so kinds and models mix freely in one budgeted tree: plan with an
  expensive model, fan out to a fleet of cheap ones, review with a third.
- **Capture everything.** Every request, reply and tool call lands in
  Postgres, searchable through recall, replayable from any ordinal, and
  watchable or drivable from the cockpit, the web UI, the CLI, another agent or
  the Python SDK at once.
- **An executor plane.** An agent's file, shell and LSP tools run on the
  executor it is bound to (your laptop, a VM, a container), never on the
  daemon. Isolation is whatever you compose around the executor.
- **A routing proxy** (Anthropic and OpenAI faces) with breaker-gated
  failover, declarative OpenRouter routing, a provider cache guard, model
  aliases and per-model cost accounting.
- **Multi-user and multi-daemon.** Per-user tokens or OIDC single sign-on;
  one client reaches many daemons through named profiles.

The long version is [Features](docs/guide/features.md).

## Naming

Three Swahili words, three roles:

- **rafiki** ("friend") — the project as a whole: the client/TUI binary
  (`rafiki`), the daemon (`rafikid`), and the library underneath both.
- **fundi** ("craftsman") — the native agent runtime, one of three child
  kinds (`--kind fundi`, the default). It does the work in-process, through
  this repo's own `pkg/llm`/`pkg/agentloop` rather than shelling out to another
  agent binary.
- **daraja** ("bridge") — the mechanism that lets a `claude`-kind child run on
  a remote executor while still looking, to the daemon, like a normal managed
  child. See [Executors](docs/guide/executors.md#daraja).

## Binaries

| Binary | Role |
|---|---|
| `rafikid` | the daemon. Runs `fundi` children as goroutines inside itself; `claude` children route through daraja on an executor when one is configured for it, else run as local subprocesses |
| `rafiki` | the CLI client, the cockpit, the web UI (`rafiki web`), and the executor (`rafiki executor serve`). Links no Postgres driver |
| `rafiki-hooks` | the webhook gateway, the one process meant to face the internet. Holds no database credential. See [Webhook gateway](docs/guide/webhooks.md) |

## Documentation

**Using rafiki**

| | |
|---|---|
| [Installing rafiki](docs/guide/install.md) | `rafiki setup`, upgrades, what it configures |
| [The cockpit and web UI](docs/guide/cockpit.md) | watching and driving agents; keys |
| [Agents and subagents](docs/guide/agents.md) | child kinds, script children, subagent trees, limits, presets |
| [Executors, daraja and sandboxes](docs/guide/executors.md) | where agents run and what they can touch |
| [Repos, skills and the Python SDK](docs/guide/repos-and-skills.md) | shared code and skills; scripting rafiki |
| [Recall and memory](docs/guide/recall.md) | searching past conversations |
| [The proxy and library](docs/guide/proxy.md) | model selection, routing, the cache guard, aliases |
| [Claude Code and subscription billing](docs/guide/claude-code.md) | `rafiki claude` |
| [Features](docs/guide/features.md) | the full list |

**Running rafiki**

| | |
|---|---|
| [Deploying rafiki for real](docs/guide/deployment.md) | a shared daemon on a server or Kubernetes |
| [Authentication](docs/guide/authentication.md) | first user, SSO, service tokens |
| [Configuration](docs/guide/configuration.md) | paths, profiles, environment variables |
| [Webhook gateway](docs/guide/webhooks.md) | third-party events waking agents |
| [Telemetry](docs/reference/telemetry.md) | OpenTelemetry and metrics |

**Reference and development**

| | |
|---|---|
| [Development](docs/guide/development.md) | building from source, running locally, tests |
| [`rafikid agent` CLI](docs/agent-cli.md) | every verb and flag |
| [Control protocol](docs/reference/control-protocol.md) | the Connect API |
| [Executor protocol](docs/reference/executor-protocol.md) | the daemon-executor wire format |
| [Vision and architecture](docs/rafiki.md) | where this is going |
| [Migrating](docs/MIGRATING.md) | changes that need you to do something |

## License

See [LICENSE](LICENSE) and [NOTICE](NOTICE).
