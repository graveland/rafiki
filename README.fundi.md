# fundi

A daemon that hosts coding-agent children, multiplexes their event streams to multiple concurrent clients, and exposes a small control plane over a Unix domain socket.

fundi is a fork of pi-controller and speaks the same JSONL wire protocol, so pi-controller's `pic` client and the pi TUI work against either. What it adds is a **native agent runtime**: the `agent` child kind (`fundid agent`) drives the Anthropic API directly through [rafiki](https://git.graveland.dev/brent/rafiki) rather than shelling out to Claude Code. That is what makes in-band abort possible — abort arrives as a protocol frame and the process stays resident.

Child kinds:

| Kind | Backend |
|---|---|
| `agent` | native rafiki loop (`fundid agent`) — in-band abort, per-turn token and cost accounting |
| `pi` | a pi process in `--mode rpc` |
| `claude` | Claude Code |

## Binaries

The usual daemon/client split, as with `dockerd`/`docker`:

| Binary | Role |
|---|---|
| `fundid` | the daemon. It runs `agent`-kind children as goroutines inside itself; `pi` and `claude` children remain subprocesses. `fundid agent` still exists as a standalone one-child-on-stdio mode, but the daemon no longer re-execs itself to spawn one |
| `fundi` | the CLI client — the one you type |
| `fundi-attach` | the TUI, spawned by `fundi create` / `fundi attach` |

Note that a `pic` on your `$PATH` is *pi-controller's* client, not fundi's.

## Layout

- `cmd/fundid` — the daemon (which hosts `agent`-kind children in-process), plus the standalone `fundid agent` mode (one agent child on stdio)
- `internal/inproc` — the `child.Runner` that runs an agent child as a goroutine over a pair of OS pipes
- `cmd/fundi` — the CLI client
- `internal/agent` — the agent runtime: turn engine, tools, context and skill loading
- `client` — Go client for the daemon's socket

## Paths

fundi follows the XDG base directories, so it coexists with a standalone
pi-controller install instead of competing for its `~/.pi/run` socket:

| | Default | Override |
|---|---|---|
| socket | `~/.local/state/fundi/controller.sock` | `$XDG_RUNTIME_DIR`, or `$FUNDI_SOCKET` |
| records | `~/.local/share/fundi/state` | `$XDG_DATA_HOME` |
| logs | `~/.local/state/fundi/logs` | `$XDG_STATE_HOME` |
| config | `~/.config/fundi` (instructions, skills, `mcp.json`, `presets.json`) | `$XDG_CONFIG_HOME` |

Its launchd/systemd service identity is `dev.graveland.fundi` / `fundi`, again
distinct from pi-controller's.

The one thing fundi writes outside its own directories is the `fundi-helpers`
pi extension, into `~/.pi/agent/extensions/` — that is pi's contract, and how
pi discovers extensions.

## Environment

fundi's variables are `FUNDI_`-prefixed. The pre-rename `PIC_*` and
`PI_CONTROLLER_*` spellings are still accepted, with a deprecation warning:

| | |
|---|---|
| `FUNDI_SOCKET` | override the controller socket path |
| `FUNDI_DEFAULT_MODEL` | model used when `fundi create` gets no `--model` |
| `FUNDI_DEFAULT_PRESET` | preset used when `--preset` is not given |
| `FUNDI_DEFAULT_LABELS` | comma-separated `k=v` label defaults |
| `FUNDI_NO_AUTO_INSTALL_HELPERS` | skip the `fundi-helpers` auto-install |
| `FUNDI_ATTACH_TAIL` | scrollback the TUI replays (`-1` all, `0` none) |
| `FUNDI_ATTACH_DEBUG` | `1` logs every event the TUI receives to stderr |
| `FUNDI_KILL_ON_EXIT` | `1` terminates the child when a directly-invoked TUI quits |
| `FUNDI_INSTRUCTIONS` | user-global instruction file (default `~/.config/fundi/instructions.md`) |
| `FUNDI_SKILLS_DIRS` | skill directories, path-list separated (default `~/.config/fundi/skills`) |
| `FUNDI_MCP_CONFIG` | global `.mcp.json` (default `~/.config/fundi/mcp.json`) |
| `FUNDI_AGENT_DB` | postgres URL for conversation persistence; **required for cost accounting** |

## Build

```sh
make build-daemon build-cli   # daemon and CLI; needs no submodule
make build                    # both, plus the fundi-attach TUI (needs bun + submodule)
make install                  # copy the binaries to ~/.local/bin (override with DESTDIR=)
make bootstrap                # fresh clone: init the pi submodule, build everything
make test-both                # test under go.work AND against the pinned rafiki (CI path)
```

`fundid -h` and `fundid agent -h` document the two daemon process modes;
`fundi --help` covers the client.

See `tasks/pi-controller-protocol.md` for the wire protocol spec and
`docs/plans/2026-07-20-fundi-design.md` for the architecture.
