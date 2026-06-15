# pic-attach

A thin TUI client for pi-controller-managed children. Bundles pi's native
`InteractiveMode` with a `RemoteAgentSessionRuntime` that proxies all agent
operations to the pi-controller daemon over its UDS socket.

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

Produces `bin/pic-attach`.

## Usage

```bash
pic create my-session --cwd /path/to/project
# (auto-attaches; Ctrl+D detaches)

pic attach my-session
# (reattach later)
```

## Scrollback

On attach, pic-attach replays the child's retained history into the TUI so you
see the prior transcript, then follows live. Control how much is replayed with
`-n/--tail` (default `-1` = all retained, `0` = none):

```bash
pic attach my-session -n 0    # no scrollback, live only (old behaviour)
pic attach my-session -n 50   # last 50 retained events, then live
```

Scrollback is faithful for pi children; for claude children the retained frames
render best-effort (the normalized view is produced live-only).

## Exit semantics

By default, exiting the TUI (Ctrl+D, /quit, terminal close) **detaches** —
the daemon's pi child keeps running. Use `pic attach <name>` to reopen the
TUI, or `pic kill <name>` from any shell to terminate the session.

Pass `--kill-on-exit` to pic create / pic attach for native-pi exit
semantics (quitting the TUI terminates the session).

## Debugging

Set `PIC_ATTACH_DEBUG=1` to log every event the TUI receives to stderr:

```bash
PIC_ATTACH_DEBUG=1 pic attach my-session
```

This prints the event type, listener count, and full stack traces for any
event-processing errors — useful for diagnosing events that appear in
`pic tail` but don't render in the TUI.
