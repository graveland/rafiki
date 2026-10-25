# Executors, daraja and sandboxes

## Daraja

An ordinary `rafiki create --kind claude` routes through daraja automatically
whenever the daemon has an executor pool configured and one of its executors
declares `claude` in `--launch` — no operator action needed. A daraja-routed
claude child is proxied through the daemon (capture, cost accounting, routing
visibility) while still billing the user's own Claude subscription by default
(`--passthrough-auth auto|on|off`, mirroring `rafiki claude --passthrough-auth`).

`--launch` takes the child protocol the executor will host: `claude` (implying
the skill corpus sync) or `script` (implying the owner's pymodule corpus sync
and repo refreshes, so script children can resolve their pymodules from
that machine's synced cache). They are independent opt-ins; an executor that
declares neither hosts nothing.

`rafiki daraja launch` is the manual entry point — useful for debugging the
launch path without a full spawn. It resolves an executor matching the
selector and supporting claude, mints a one-shot ticket, and waits for the
daraja to reverse-dial back before returning the child id:

```
rafiki daraja launch --cwd <dir> --model <provider/model> [--executor <selector>] [--resume <session-id>]
```

See `docs/reference/control-protocol.md` for the three Connect RPCs behind
this (`DarajaLaunch`, `DarajaSend`, `DarajaWatch`).

## Executor

**`rafiki create` defaults to a workspace that can serve the kind you asked
for.** For `fundi` that's your own machine: the client starts an in-process
executor and points the spawn at it, so `read`/`write`/`bash` run where your
files are. A kind that must be *launched* (`claude`) can't use that
throwaway executor, so with nothing explicit on the command line it's
resolved across every live, admitted executor that declares the kind —
asking you when ambiguous. `--no-local-executor` turns the local offer off;
`--executor <machine-or-id>` / `--executor-selector <labels>` target one.

There are two kinds of executor:

- **Durable** — enrolled with `rafiki executor enroll` or created outright
  with `rafiki executor create`. Has a database row with operator-written
  labels, admission, isolation, and workspace mode; lives until deleted.
  Naming one is two steps on two machines — name the machine where it will
  run, then mint the enrollment token wherever the operator is:

  ```sh
  rafiki executor name laptop                 # on the machine the executor runs on
  rafiki executor enroll --name laptop         # wherever the operator is; carry the token over
  ```
- **Transient** — started automatically by `rafiki create`/`rafiki attach`.
  No database row, authenticated by a one-shot ticket over the already
  authenticated control connection; dies when that connection closes.

If a durable executor already covers this machine and user, the client uses
that instead of starting its own, so an agent keeps working after you detach.

**Without an executor, an agent has no workspace tools at all** —
`read`/`write`/`edit`/`glob`/`grep`/`ls`/`bash`/`lsp_*` simply aren't
registered, never silently running against the daemon's own filesystem. What
remains is the daemon tier: web fetch/search, the task ledger, skills, and the
agent-control verbs. MCP servers are the exception that either tier can serve —
they run on the daemon by default, and on an executor when `mcp.json` says so
(see "MCP servers on an executor").

```bash
go build -o bin/rafiki ./cmd/rafiki
rafiki executor serve --connect-socket "$XDG_RUNTIME_DIR/rafiki/executor.sock" \
  --enroll-token <token> --root "$PWD"
```

**Flags:** `--connect host:port` (remote reverse-dial) / `--connect-socket`
(local reverse-dial) — one is required; `--root` (working directory root,
default cwd); `--concurrency` (default 6); `--proxy name=base_url`
(repeatable — forward an HTTP endpoint only this machine can reach; an LLM
endpoint for "The executor relay" below, or an MCP one, see "MCP servers on an
executor"); `--mcp-command name=argv…` (repeatable — host an MCP server process
by NAME, its argv never leaving this machine); `--launch kind` (repeatable — opt this machine into
hosting a launched child kind, e.g. `claude`); `--lsp-config` / `--no-lsp`
(language servers auto-detect from `PATH` by default; a configured-but-absent
server just drops its `lsp_*` tools rather than advertising broken ones).

No certificate is involved for a local socket. **Socket permissions are the
only access control**: the socket is `0600` from creation (`0177` umask), and
the daemon refuses a second executor on a path already served by a live one.
Anyone who can open the socket gets arbitrary `bash` and filesystem access
inside `--root`.

**Executor owner labels follow the enrolling connection.** A machine enrolled
while its profile carries no token is labelled `owner=<the daemon's OS user>`.
Once the profile has a token, its verbs authenticate as that token's username,
and a spawn selecting `owner=<username>` will not match the machine enrolled
under the OS-user label — admission refuses the spawn. Re-enroll from the
authenticated profile (or relabel `owner=`) so the two agree.

**The executor's environment.** launchd/systemd --user don't inherit a login
shell, so `executor service install` captures the installing shell's
environment into `<config dir>/executor.env` (0600) at install time — a
few classes are skipped (rafiki's own `RAFIKI_*`/provider keys, `PATH`, and
per-session residue: `PWD`/`TERM*`/`DIRENV_*`, `SSH_*` other than
`SSH_AUTH_SOCK` — `SSH_CLIENT` makes bash source `~/.bashrc` in every tool
shell — an enclosing Claude Code session's `CLAUDE_CODE_*`/`ANTHROPIC_*`
routing, and zsh/zplug/starship interactive state). A second,
hand-maintained file, `<config dir>/executor-overrides.env`, sets variables
**unconditionally** — needed for anything the service manager itself seeds
wrong, like launchd's per-session `SSH_AUTH_SOCK`. See `.env.example` and
`RAFIKI_EXECUTOR_ENV_FILE`/`RAFIKI_EXECUTOR_OVERRIDES_FILE`.

**Background execution** is the immediate win over plain `bash`: a long-running
`bash` call is PROMOTED to a background job instead of being killed at its
timeout, so work is not lost to a timeout. An agent can also start a job
explicitly and get a handle immediately:

| Tool | Purpose |
|---|---|
| `bash_start` | start a command in the background, return a handle immediately |
| `bash_output` | read everything the job has printed; reports running/exited and exit code |
| `bash_kill` | stop the job and its whole process group |

Completion is **pushed, not polled** — the executor pushes every job's
lifecycle over one `WatchJobs` stream per connection, and the daemon injects a
fragment into the child's next turn when a job exits, through the same
coalescing path subagent settlements use. An exit is acked only AFTER the
fragment is handed to the event buffer, which persists it to the inbox, and an
unacked exit is replayed on the next subscribe, so a daemon crash does not lose
it — a graceful stop, by contrast, tears the job down and acks it. Output is
written to a file on the executor with a per-workspace byte budget
(`--job-output-budget-mb`, default 256 MB, drop-oldest-finished-first) and
**no time-based expiry** — a turn can end and resume hours later, so a job's
output lives until its workspace is released, not until a wall-clock window
elapses. See `docs/reference/executor-protocol.md` for the full wire protocol.

### The executor relay

A provider in `providers.toml` can be reached through an executor's own
localhost instead of the daemon dialing it directly — for a local inference
server (vmlx, Ollama, …) the daemon usually can't reach:

```toml
[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.via_executor]
selector = "role=workstation"   # which executor(s)
proxy = "vmlx"                  # matches a --proxy name on that executor
```

The executor side is the allowlist (`rafiki executor serve --proxy
vmlx=http://localhost:8005` declares exactly what it forwards to); a request
naming an undeclared name never reaches the network. A `via_executor`
provider with no matching executor is a hard failure on every spawn, never a
silent direct dial (which would have the daemon reach its own localhost
instead). The relay carries request headers over the executor's connection,
so a keyed provider's credential transits whatever machine relays it — treat
`via_executor` like any other tool call an executor already runs on your
behalf.

### MCP servers on an executor

A provider is one thing a `via_executor` entry can name; an MCP server is the
other, and for the same reason: a stdio MCP server that reads
`~/.config/hesabu/config.yaml` exists only on the operator's laptop, and the
cluster's daemon has no route to it.

The executor declares what it will host by NAME, and never accepts an argv from
the daemon:

```bash
rafiki executor serve --connect ... --root ... \
  --mcp-command 'hesabu=/Users/you/go/bin/hesabu mcp --stdio'
```

The value is whitespace-separated argv, with a backslash escaping a space, a
backslash or a quote. **No shell is involved** — a glob or `$(…)` in an argument
is literal text, which is what makes a declaration an allowlist rather than a
promise. The hosted process runs under the EXECUTOR's environment and working
directory, so its credentials never travel to the daemon.

The cluster's `mcp.json` then names the server and the machine:

```json
{
  "mcpServers": {
    "hesabu":   { "via_executor": { "selector": "name=greyshift", "command": "hesabu" } },
    "codescan": { "via_executor": { "selector": "name=greyshift", "proxy":   "codescan" } }
  }
}
```

`command` names a `--mcp-command` on the executor (stdio); `proxy` names a
`--proxy` on it (an HTTP MCP endpoint) — exactly one of the two, and neither
`command`/`args`/`env`/`url` at the entry's own level, because the argv and the
environment live on the executor. A malformed entry is refused at LOAD time, by
name.

**Resolution happens once, at spawn.** Every `via_executor` server is resolved
while the child's runtime is built, against the executors that are live then; a
child's MCP server cannot hop machines mid-life, and every failure is a SPAWN
error naming the real problem rather than a silently toolless child:

| Situation | What you get |
|---|---|
| Selector matches no live executor | spawn refused, naming the selector |
| Executor live, name not declared | spawn refused, telling you to start it with `--mcp-command name=…` |
| Executor live, name declared, more than one matches | the lowest executor ID wins, deterministically |
| The server dies mid-session | the CALL fails with its exit status and stderr tail, and is **not** retried; the next call redials |
| The daemon disconnects, or the child is killed | the executor kills the process group (SIGTERM, then SIGKILL) |
| The hosted process forks a helper and exits | the leftover group is swept too; nothing outlives the stream |

The hosted process runs in the executor's `--root` directory under the
executor's own environment — never the daemon's — so a credentialed server reads
its config on the machine that already holds it.

A crash-looping server (3 failures inside 60 s) is refused for a minute with its
last stderr, so ten children retrying at once cannot spin a shared laptop. Kills
the executor performed itself are not failures: ending a stream is how a child
shuts down normally, and three of those in a minute must not quarantine a
healthy server.

`via_executor` requires the executor plane; a daemon without one (and
`rafikid agent`) refuses to start a child whose `mcp.json` names such a server.

### Container executors

An executor serves whatever filesystem it can see; whether that's a
container is decided by how the operator starts it, not by a flag:

```
docker run -d --name rafiki-executor -v /home/user/worktrees:/work \
  rafiki-executor:latest \
  rafiki executor serve --connect daemon.example.com:443 --enroll-token <token>
```

`--root` sets the working directory — it is **not** a sandbox; the
container's mounts (or the host user's permissions, for a native executor)
are the boundary. The executor asserts nothing about itself: `isolation`,
`workspace_mode`, `roots`, `labels` all live on the database row set at
token-mint time, so the row is where you declare a container:

```
rafiki executor create --isolation container --workspace-mode ephemeral \
  --root /work --label env=ci
```

There is deliberately no path vocabulary narrower than a whole executor:
`docker run -v` expresses the ro/rw model for containers, and for native
executors a userspace path check on the file tools would be fake since
`bash` could still escape it. Native access is gated by **admission**
(label-selection), not by paths.

### Workspace lifecycle

Each child gets a workspace provisioned before it starts and released when it
exits, at the child's cwd.

- **ephemeral** — the operator declares these machines interchangeable; if
  the executor is lost, the child reschedules onto another one.
- **pinned** — the executor exposes an existing tree; if it's lost, the child
  parks until it returns or a timeout expires.

This says nothing about filesystem isolation, which comes from the
operator's composition (containers) or from git worktrees passed as cwd.

### Sandboxes

A **sandbox** is a container the daemon creates on your behalf, through a
**launcher** executor you declare, so an agent's workspace tools run inside a
container instead of on the host. The daemon never touches a container runtime
itself; the launcher is the only process that speaks to Docker.

Declare a launcher where the Docker engine is reachable — a durable executor
started with three flags: a proxy named exactly `docker` pointing at the
(Docker) socket, the host directories a sandbox may bind-mount, and the relay
directory the sandbox uses to reach the daemon:

```sh
rafiki executor serve --connect-socket "$XDG_RUNTIME_DIR/rafiki/executor.sock" \
  --proxy docker=unix:///var/run/docker.sock \
  --sandbox-mount-root /srv/repos \
  --relay-dir "$XDG_RUNTIME_DIR/rafiki/relay"
```

- `--sandbox-mount-root` (repeatable) is the ONLY set of host paths a sandbox
  may bind-mount; a `host_path` mount outside every root is refused, both by
  the daemon (pre-check) and by the launcher (re-check). With no root, no host
  path may be mounted. A root is a read grant of EVERYTHING under it, to every
  child of the owner, so a root must not contain a socket or a credential: a
  bind source that is not a regular file or a directory (a socket, device or
  fifo) is refused by the launcher, because an `ro` bind of a unix socket still
  permits `connect()`.
- `--relay-dir` makes the launcher bind `<dir>/daemon.sock` and splice each
  sandbox's connection to a fresh dial of the daemon (TLS/pin applied when the
  daemon is remote; nothing authenticates the LAUNCHER on the relayed
  connection — the sandbox's own credential is the only auth). It
  requires `--connect`/`--connect-socket` (or a remote `RAFIKI_URL`) — a relay
  with no daemon to dial refuses to start, because a sandbox's only link back
  is that socket. The relay re-exports the executor's own `0177` socket at
  `0666` inside a `0755` directory, so every local user on the launcher host
  (and every container that can see the relay dir) can reach the executor and
  daraja Upgrade routes through it — credential-gated, but a deliberate
  widening worth knowing about. Foothold mode widens it further: the relay is
  then a loopback TCP port every container on the docker host can reach
  through `host.docker.internal` — non-rafiki containers included, not just
  local users.
- The launcher's `docker` proxy inspects every container-create body against a
  fail-closed allowlist (exactly the fields the daemon emits, no duplicate
  keys, bind sources symlink-resolved under a mount root), so a body that asks
  for `Binds`, `Privileged`, a device, etc. is refused. See
  `docs/reference/executor-protocol.md` → "Sandbox launchers".

**The relay.** The sandbox reaches the daemon through a relay socket inside
it. On Linux that socket is a host unix socket the launcher bind-mounts into
the container (`--relay-dir`, defaulting to `<runtime dir>/relay` when the
`docker` proxy is a local unix socket), and the sandbox dials
`/run/rafiki-relay/daemon.sock`. A docker host in **another kernel** — OrbStack
or Docker Desktop on macOS — cannot see a host bind-mount, so select
**foothold mode** instead. It is the default on a non-Linux host with a
`unix://` `docker` proxy and no `--relay-dir`, running the published image
`ghcr.io/graveland/rafiki-sandbox:latest`; `--relay-foothold-image <ref>`
overrides the image (mutually exclusive with `--relay-dir`; it requires a
`unix://` `docker` proxy and a daemon address). Foothold mode works
only where the docker runtime maps `host.docker.internal` to this host's
loopback (Docker Desktop, OrbStack): the foothold's bridge dials its OWN
host's loopback, so a plain lima VM or a remote docker context — which cannot
see the bind-mount either — has no working foothold. The launcher then
runs one **foothold container** per docker host, inside the docker host's own
kernel (`rafiki-foothold-<key>`, volume `rafiki.relay.<key>`), which serves the
relay socket in the volume and splices it to a loopback TCP relay on the
launcher. The sandbox mounts that volume read-only at `/run/rafiki-relay`, so
it still reaches the daemon with **`network: none`**. The foothold is recreated
automatically whenever the container spec changes — the image or the bridge
command (which carries the relay port) — and the change is picked up on the
next sandbox create, so a *fixed tag* needs a manual `docker pull` to take
effect.

Two accepted residuals of foothold mode, not protections: the hop from the
foothold to the launcher's loopback TCP relay is plaintext and carries each
sandbox's durable executor credential, and the foothold keeps running while the
launcher is down (`RestartPolicy` `unless-stopped`) still dialing that
ephemeral port — so a local process that binds the port while the launcher is
stopped would receive those credentials. And the foothold's key is the launcher
host name, so renaming the host orphans the old volume and foothold, stranding
sandboxes already created on the old volume without a relay.

**The image must run `rafiki`.** A sandbox runs `rafiki executor serve
--connect-socket /run/rafiki-relay/daemon.sock` as its entrypoint, so the image
must have the `rafiki` binary on `PATH` (and whatever the agent's tools need);
the daemon pulls the image if it is absent. `RAFIKI_SANDBOX_IMAGE` overrides the default image
(`ghcr.io/graveland/rafiki-sandbox:latest`, the Dockerfile's `sandbox` target,
published by `.github/workflows/images.yml`); a create may still pass `--image`.
The default image is a general-purpose working machine: common process,
network and archive tools, a C toolchain, Python with `uv`, Node, and the
latest stable Go and Rust (Rust and the user-level `PATH` live under the
non-root `rafiki` user's home, so `cargo install`, `go install` and `npm -g`
work without root). It is rebuilt without cache every Monday so Debian security
fixes and new Go/Rust releases land without a merge.

Create, list and remove **named** sandboxes from the CLI (they go through the
daemon — the client never dials a sandbox directly):

```sh
rafiki sandbox create --name work --mount ro:/work=host:/srv/repos/app
rafiki sandbox list
rafiki sandbox rm work
```

A named sandbox has a TTL and always expires. A sandbox bound to a child as a
**spawn block** (`SpawnRequest.sandbox`; fundi/MCP expose it as an
`agent_spawn` field) is created and removed with its child, but it is not
invisible to the CLI: `rafiki sandbox list` shows spawn-block rows too (their
`owner_child` names the child), and `rafiki sandbox rm <id>` removes one (the
spawning child, or any of its ancestors, may also remove it). A spawn block is
unnamed and has no TTL.

**Syncing code in and out.** Move a tree or a committed git branch between
executors (a host executor and a sandbox, or two sandboxes) through the daemon
— the client never dials an executor directly:

```sh
rafiki sandbox sync <executor>:<path> <executor>:<path>
rafiki sandbox sync-repo <executor>:<repo> <executor>:<repo> <branch>
```

`sync` copies a tree (or a single file); `--overwrite` replaces the destination
and is allowed only on a container-isolated destination, and `--max-bytes` caps
the transfer — it counts the bytes of the tar STREAM (headers and padding
included), while the `bytes` reported for a transfer is regular-file content.
Do NOT use `sync` to copy a repository directory: its `.git` carries config,
hooks and alternates the source planted, which would land at the destination —
use `sync-repo` for a repository. `sync-repo` moves only COMMITTED work: it
bundles the branch on the source, relays the bundle and fetches it at the
destination — a checked-out branch and a non-fast-forward update are refused
unless `--force`. A bundle carries objects and refs only, and every git call
the executor makes runs under its git hardening and a fixed environment, so the
source's config and hooks are never read or run. A fetch never touches the
working tree of an existing repository; a repository that does not exist is
created with the branch checked out. No credentials need
enter a sandbox: it authenticates to the daemon with its own executor
credential, and the daemon brokers both ends.

Agents reach the same verbs as tools, on both the fundi and the MCP face:
`sandbox_create`, `sandbox_list`, `sandbox_remove`, and the two transfer tools
— `sandbox_sync` (`src_executor`, `src_path`, `dst_executor`, `dst_path`;
optional `overwrite`, `max_bytes`) copies a file or directory between two
executors, and `sandbox_sync_repo` (`src_executor`, `src_repo`, `dst_executor`,
`dst_repo`, `branch`; optional `force`) fetches one committed git branch
between two repositories. A child reaches only the executors and sandboxes
`resolve` permits, and `overwrite` is allowed only on a container-isolated
destination.

A sandboxed child's working directory is a path **inside the container**: the
executor runs in the container and cannot see a host path, so a spawn block's
`cwd` is rewritten to the sandbox's `workdir` (the container root when the spec
names none), and a NAMED sandbox's `workdir` is resolved from its row. Nothing
else needs to be done; passing a host `cwd` cannot break a sandboxed child.

Defaults and limits come from the daemon's environment — `RAFIKI_SANDBOX_IMAGE`,
`_NETWORK`, `_TTL`, `_MAX_TTL`, `_MAX_PER_OWNER`, `_SWEEP_INTERVAL`, and the
per-child caps `_CHILD_MAX_MEMORY_BYTES`/`_CHILD_MAX_CPUS`/`_CHILD_MAX_PIDS`
(see `.env.example`). `RAFIKI_SANDBOX_MAX_PER_OWNER` caps the live sandboxes one
owner may hold and **counts spawn blocks too** (a block holds a cap slot like a
named sandbox); 0 means unlimited. Mounts, network, resource limits,
`--read-only-rootfs`, user, env and labels are all per-create flags on
`rafiki sandbox create`.

**Development image.** `make sandbox-dev-image` builds `rafiki-sandbox-dev:latest`
(`docker/sandbox-dev.Dockerfile`) on the local docker engine: the published
sandbox image plus `protoc` and `golangci-lint` at pinned versions, and
`GOCACHE`, `GOMODCACHE` and `CARGO_TARGET_DIR` set to fixed paths under `/cache`
so build output has one home. Nothing from the repo is copied in (the build
context is empty), so it cannot go stale against `go.mod`. Pass it per sandbox
(`rafiki sandbox create --image rafiki-sandbox-dev:latest`, or the spawn block's
`image`) — not as `RAFIKI_SANDBOX_IMAGE`, since it exists only in the engine that
built it. The daemon pulls an image only when it is missing locally.
