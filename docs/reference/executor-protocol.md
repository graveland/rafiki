# Executor protocol

The daemon dispatches filesystem and shell tool calls to an executor process
(`rafiki executor serve`) over Connect RPC. This document describes the wire
protocol: the seven RPCs, their message shapes, the four failure codes, the
background-handle lifecycle, the workspace lifecycle, and the mtime contract.

There are three transports for the same protocol, and they differ only in what
carries the bytes:

| Transport | Command | Used when |
|---|---|---|
| Unix socket | `rafiki executor serve --socket` | executor and daemon on one host |
| Reverse-dialled TLS | `rafiki executor serve --connect` | the daemon cannot reach the executor's host (NAT, a laptop) |
| Unix socket (reverse) | `rafiki executor serve --connect-socket` | executor and daemon on one host, and the executor should be a fully rowed member of the pool |

The two unix-socket forms are not the same thing and the difference is the row.
`--socket` has the executor listen and the daemon dial in with `--executor-socket`,
which produces an executor with **no database row**: no labels, no `admits`, no
`isolation`, no `workspace_mode`, invisible to selectors and never inherited by a child.
`--connect-socket` reverses the direction, so the executor enrolls exactly as a remote one
does and is a full member of the pool. Prefer it; `--executor-socket` remains only until
the workspace path stops having an in-daemon fallback at all.

A container executor uses one of those two like any other host. There was once a
third — `rafiki executor serve-stdio`, spoken over the stdio of a `docker exec
-i` — because rafiki started the container itself and gave it no network. It
does not start containers any more, so the container has whatever network the
operator gave it in `docker run`, and the stdio transport had no callers left.

## Reaching the daemon

The reverse-dial transports are reached at a **path** on the daemon's shared
listener, upgraded out of HTTP/1.1 — the TLS listener and the unix executor
socket mount the same handler:

```
GET /executor/connect HTTP/1.1
Upgrade: rafiki-executor
Connection: Upgrade
        ↓
HTTP/1.1 101 Switching Protocols
        ↓
{"type":"executor_hello",…}\n     then HTTP/2, roles inverted
```

One port and one certificate therefore serve the control plane (`/control`) and
the executor link (`/executor/connect`). When the daemon and executor are on
one host, the executor socket (`executor.sock` in the runtime directory) is the
same handler without TLS. Enable either with `RAFIKI_EXECUTORS_ENABLED=1`; the
executor link has no listener of its own.

Two consequences worth knowing:

- **ALPN is no longer load-bearing.** The outer connection negotiates
  `http/1.1` — net/http can only hijack an HTTP/1.1 connection — and the
  inverted HTTP/2 begins after the 101, directly on the byte stream, where ALPN
  is not consulted. Agreement is the `Upgrade` header's job now, and a mismatch
  fails with a readable HTTP status instead of a TLS alert.
- **An HTTP proxy can carry it.** An Upgrade tunnel is the same machinery as
  WebSocket, so an ingress may terminate TLS and forward rather than requiring
  TCP passthrough. Terminating means the daemon↔executor hop is no longer
  end-to-end encrypted and `--pin-cert` pins the proxy's certificate, so that is
  a deployment choice rather than a default.

## Transport

- **Protocol:** Connect (gRPC-compatible) over HTTP/2 cleartext (h2c).
- **Socket:** Unix domain socket, filesystem mode `0600` — no authentication
  beyond filesystem permissions in this phase.
- **Roles:** `rafikid` (the daemon) is the RPC client; the executor is the
  server. On the reverse-dialled transport the executor DIALS and then serves on
  what it dialled, so the TLS roles and the HTTP roles are inverted relative to
  each other — see pkg/execpool's package comment.
- **Codec:** Binary protobuf by default; JSON also supported (Connect's
  auto-negotiation).

## Enrollment handshake

Over the reverse-dialled transport the executor sends one newline-delimited
JSON frame before any HTTP/2 framing, and rafikid answers with one:

```jsonc
// executor -> rafikid
{"type":"executor_hello","token":"<enrollment token>"}      // first join
{"type":"executor_hello","credential":"<durable credential>"} // thereafter

// rafikid -> executor
{"type":"executor_hello","executorId":"…","credential":"…"}  // success
{"type":"executor_hello","error":"…","retryable":true}       // failure
```

Both sides must read this frame **byte at a time**. A buffered reader that
consumes past the newline swallows the start of the peer's HTTP/2 stream, and
the connection then dies with an unhelpful protocol error rather than a
diagnosable one.

### `retryable`

`retryable` discriminates *"rafikid could not check this credential"* from
*"this credential is not valid"*, and it decides whether the executor exits:

| `retryable` | Meaning | Executor behaviour |
|---|---|---|
| absent / `false` | A decision about the credential — unknown, consumed, expired, disabled, or no such row. | Stop. Retrying cannot un-revoke a row. `Connect` returns `ErrEnrollmentRejected`. |
| `true` | The store could not be reached or read. | Keep reconnecting with backoff. |

Absent means terminal, so an older daemon's responses behave as they always
did.

An unclassified error is reported as **retryable**. The failure directions are
not symmetric: quitting on a genuinely dead credential costs a log line, while
quitting on a transient one takes the machine out of service permanently — and
because executors reconnect together, one database restart would otherwise
take the entire fleet down.

The `error` string for a retryable failure is deliberately generic. The peer
has by definition not proved who it is, and a store error routinely carries a
DSN, a hostname or a query; the real error goes to rafikid's log.

## RPCs

All seven RPCs belong to the `rafiki.executor.v1.ExecutorService` service.

### Describe

```
Describe() → { executorId, platform, roots[], concurrency, isolation,
               workspaceMode, tools[], version, selfReportedLabels }
```

Unary. Called at startup and periodically to discover the executor's
capabilities. `tools[]` is the executor's *actual* served set — the names its
registry materialized, not a static list — and it is what the parent builds the
child's `tools[]` from at spawn, so a name that isn't here never reaches the
child's envelope. It includes the `lsp_*` verbs only when a language server is
installed (or `--lsp-config` names one), and never the parent-side
`bash_start`/`bash_output`/`bash_kill` RPCs, which the daemon implements itself.

### Health

```
Health() → { load, runningHandles[], diskFree, draining }
```

Unary. Liveness and load snapshot. `runningHandles` names every non-exited
background job; the daemon polls this to surface what's long-running.

### Execute

```
Execute(callId, tool, inputJSON, timeoutMs, expectMtime, background, workspaceId)
  → stream { Output(chunk) | Result(content[], isError, observedMtime)
            | Failed(code, msg) | handle }
```

Server-streaming. Dispatches a single tool call. `workspaceId` selects the
workspace this call runs in — empty means the executor's own root, the
pre-phase-08 behaviour kept as a compatibility path so an executor and a
daemon of different vintages still interoperate.

The normal path is:

1. The server checks `expectMtime` — if any path's current on-disk mtime
   does not match, it immediately streams `Failed(CODE_DENIED, …)`.
2. If `background=true` and `tool=bash`, the server starts the command as
   a background job and streams only the handle.
3. Otherwise, the tool runs synchronously; the server streams
   `Result{content[], observedMtime}` on success or `Failed{…}` on error.

**Result carries typed content**, not a bare string. `content[]` holds
`ContentBlock` entries that may be text or (in a future phase) images.

### Attach

```
Attach(handle) → stream { Output(chunk) | exitCode }
```

`Attach` streams a job's output from the beginning of what is still retained,
then incremental deltas every 200ms, then a final `exit_code`. A single reply
carries at most 100 KB, taken from the END — a poll wants the newest output —
and a reply that had to clip is prefixed with a note naming the FILE holding
the fuller record, which the model can `read` or `grep` on the same executor.
An unknown handle is `CodeNotFound`. Dropping the connection does not kill the
job — reattaching resumes from what is still retained.

### Provision

```
Provision(childId, workspaceMode, mounts[], workdir, network, memoryBytes, cpus, env)
  → { workspaceId, roots[], workdir, isolation }
```

Provision prepares a workspace and returns the handle later `Execute` calls
carry. A workspace is a handle bound to the root the executor was started with;
there is nothing else it could be, because rafiki does not start containers — a
container running the executor IS one.

Everything describing a shape the executor might build is therefore dead on this
call. `mounts`, `network`, `workdir` and `workspaceMode` are unset by the daemon
and ignored by the executor; they remain on the wire only until the proto is
next regenerated.

`isolation` in the response is deliberately **empty**, as it is in `Describe`.
An executor does not know whether it is running in a container and must not
guess: the answer gates where other people's children may run, so the
authoritative copy is the `isolation` column on the executor's row, written by
the operator at token-mint time. A daemon that reads it from here gets "none"
for every child on every machine.

### Release

```
Release(workspaceId) → {}
```

Unary. Tears a workspace down. **Idempotent:** a daemon restart that lost
track of a workspace must be able to release it again without error. A
released workspace kills **its own** background jobs — a job in a released
workspace is not a job, and reporting it as running is worse than reporting it
gone. It must not touch any other workspace's jobs.

### Cancel

```
Cancel(callId) → {}
```

Unary. Terminates the background job named by `callId`. Does not wait for
the process to reap.

### `JobOutput`

One-shot poll of a background job. Never blocks.

| Field | Direction | Meaning |
|---|---|---|
| `handle` | → | the handle returned by a background `Execute` |
| `since` | → | byte offset into lifetime output; pass back the previous `total` |
| `data` | ← | bytes from `since` to `total`, capped at 100 KB from the end |
| `total` | ← | bytes the job has ever written, including any not returned |
| `exited` | ← | whether the process has been reaped |
| `exit_code` | ← | meaningful only when `exited` |
| `found` | ← | false when the handle is unknown, or its output was evicted by the workspace's byte budget |

`Attach` remains the streaming path. Use `JobOutput` when you want a snapshot.

## Failure codes

Failures are typed, never collapsed into a bare string. The parent makes a
different retry decision for each code:

| Code | Meaning | Retry? |
|---|---|---|
| `CODE_TOOL_FAILED` (3) | The tool itself errored (missing file, unknown tool name, bash exit ≠ 0 in a *synchronous* call). | Yes — the error is tool-specific. |
| `CODE_DENIED` (1) | Structural refusal (stale mtime, sandbox violation, tool not permitted). | **Never.** The condition is permanent for this request. |
| `CODE_EXECUTOR_LOST` (2) | The executor disappeared mid-call. | Yes — a different executor may still serve the request. |
| `CODE_TIMEOUT` (4) | The tool call exceeded `timeoutMs`. | Yes — a longer timeout may succeed. |

**`bash` exit ≠ 0 in a synchronous call is a `Result` (with `is_error=true`),
not a `Failure`.** That is existing behaviour and the protocol preserves it.
A non-zero exit inside a background job is reported via the exit code in
`AttachResponse`.

## Background-handle lifecycle

```
Start: Execute(background=true, tool=bash) → stream { handle }
Run:   Attach(handle) → stream { Output*, ExitCode }
Kill:  Cancel(callId) → {}
```

- A handle is the `callId` from `ExecuteRequest`. The parent picks it.
- A job's output goes to a FILE on the executor, retained with drop-oldest at
  8 MB, and a single reply returns at most 100 KB of it from the end. Byte
  offsets are tracked against a monotonic total, so a re-`Attach` after data
  has been dropped can tell the caller it missed output rather than silently
  replaying or skipping bytes — and unlike the in-memory ring this replaces,
  what it missed usually still exists, at a path the reply names. That is the
  same "spill, never destroy" rule foreground tool results follow; background
  output was the one place output was both unbounded and thrown away.
- **Retention has no time limit.** A finished job's output lives until its
  workspace is released, which is exactly as long as the agent that might ask
  for it. A wall-clock window cannot work here: an async agent's turn can end
  and resume hours later, so a 10-minute timer expired output while the agent
  that started the job was still alive and idle. Memory is bounded instead by
  a per-workspace BYTE budget (256 MB by default, `--job-output-budget-mb`),
  evicting the oldest FINISHED job first. Running jobs are never evicted:
  their output is a live stream, not an archive.
- Background commands are never rewritten through `rtk`, whatever `--rtk` says.
  The rewrite is only safe because the foreground path can watch stderr after
  the command exits and re-run the original when rtk itself refused; a
  long-running job offers no exit to inspect and no output it could take back.
- A job survives a dropped `Attach` connection — the parent may re-attach
  at any time.
- `Health().runningHandles` lists every handle whose process has not yet
  exited.

## Mtime (TOCTOU) contract

The `FileTracker` splits state from check: the **parent** holds the map
(conversation state that survives executor respawn), and the **executor**
verifies at the moment of the write.

1. After a successful `read` (or `write`/`edit`), the executor populates
   `Result.observedMtime` with the on-disk mtime of each file touched.
2. The parent stores these in its own `FileTracker`.
3. On a subsequent `write` or `edit`, the parent sets
   `ExecuteRequest.expectMtime` to the values it previously recorded.
4. The executor stats each path **before** dispatching the tool. If any
   mtime differs, it streams `Failed(CODE_DENIED, …)` and the write does
   not proceed.

The stat happens in the executor process, which is the only place the file is
visible: if that process is in a container, the path it stats is the container's.

Parent-side checking alone would be a TOCTOU — the file can change between
the parent's check and the executor's write. The executor-side check at the
last possible moment closes that window.
