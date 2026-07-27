# fundi

A daemon that hosts coding-agent children, multiplexes their event streams to multiple concurrent clients, and exposes a small control plane over a Unix domain socket.

fundi is a fork of pi-controller and speaks the same JSONL wire protocol, so `pic` and the pi TUI work against either. What it adds is a **native agent runtime**: the `agent` child kind (`fundi agent`) drives the Anthropic API directly through [rafiki](https://git.graveland.dev/brent/rafiki) rather than shelling out to Claude Code. That is what makes in-band abort possible — abort arrives as a protocol frame and the process stays resident.

Child kinds:

| Kind | Backend |
|---|---|
| `agent` | native rafiki loop (`fundi agent`) — in-band abort, per-turn token and cost accounting |
| `pi` | a pi process in `--mode rpc` |
| `claude` | Claude Code |

## Layout

- `cmd/fundi` — the daemon, plus `fundi agent` (one agent child on stdio)
- `cmd/pic` — the CLI client
- `internal/agent` — the agent runtime: turn engine, tools, context and skill loading
- `client` — Go client for the daemon's socket

## Paths

fundi follows the XDG base directories, so it coexists with a standalone
pi-controller install instead of competing for its `~/.pi/run` socket:

| | Default | Override |
|---|---|---|
| socket | `~/.local/state/fundi/controller.sock` | `$XDG_RUNTIME_DIR`, or `$PI_CONTROLLER_SOCKET` |
| records | `~/.local/share/fundi/state` | `$XDG_DATA_HOME` |
| logs | `~/.local/state/fundi/logs` | `$XDG_STATE_HOME` |

Its launchd/systemd service identity is `dev.graveland.fundi` / `fundi`, again
distinct from pi-controller's.

## Build

```sh
make build-controller build-pic   # daemon and CLI; needs no submodule
make bootstrap                    # fresh clone: init the pi submodule, build everything
make test-both                    # test under go.work AND against the pinned rafiki (CI path)
```

`fundi -h` and `fundi agent -h` document the two process modes.

See `tasks/pi-controller-protocol.md` for the wire protocol spec and
`docs/plans/2026-07-20-fundi-design.md` for the architecture.
