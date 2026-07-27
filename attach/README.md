# fundi-attach

A thin TUI client for fundi-managed children. Bundles pi's native
`InteractiveMode` with a `RemoteAgentSessionRuntime` that proxies all agent
operations to the `fundid` daemon over its UDS socket.

Normally spawned by `fundi create` / `fundi attach` rather than run directly.
`fundi` injects the resolved socket path when it spawns this, so `$FUNDI_SOCKET`
only matters for direct invocation against a non-default socket.

## Build

Requires [bun](https://bun.sh) (`brew install oven-sh/bun/bun`).

From the repo root:

```bash
make build-attach
```

Or directly:

```bash
cd attach
bun install
bun run build
```

Produces `bin/fundi-attach`.

## Usage

```bash
fundi create my-session --cwd /path/to/project
# (auto-attaches; Ctrl+D detaches)

fundi attach my-session
# (reattach later)
```

## Scrollback

On attach, fundi-attach replays the child's retained history into the TUI so you
see the prior transcript, then follows live. Control how much is replayed with
`-n/--tail` (default `-1` = all retained, `0` = none):

```bash
fundi attach my-session -n 0    # no scrollback, live only (old behaviour)
fundi attach my-session -n 50   # last 50 retained events, then live
```

Scrollback renders the conversation for both pi and claude children: the daemon
captures the normalized (pi-vocabulary) bus stream into a per-child render-ring
(persisted to `render.jsonl.gz` so it survives a daemon restart), and the TUI
primes from that. The raw backend stream is still available via `fundi logs --raw`.

## Slash-command autocomplete

For both pi and claude children, typing `/` offers the child's slash commands.
pi commands come from a live `get_commands` RPC; claude commands are captured
from its init frame and served from the daemon's store.

## Exit semantics

By default, exiting the TUI (Ctrl+D, /quit, terminal close) **detaches** —
the daemon's pi child keeps running. Use `fundi attach <name>` to reopen the
TUI, or `fundi kill <name>` from any shell to terminate the session.

Pass `--kill-on-exit` to fundi create / fundi attach for native-pi exit
semantics (quitting the TUI terminates the session).

## Debugging

Set `FUNDI_ATTACH_DEBUG=1` to log every event the TUI receives to stderr:

```bash
FUNDI_ATTACH_DEBUG=1 fundi attach my-session
```

This prints the event type, listener count, and full stack traces for any
event-processing errors — useful for diagnosing events that appear in
`fundi tail` but don't render in the TUI.
