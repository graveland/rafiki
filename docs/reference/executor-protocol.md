# Executor protocol

The daemon dispatches filesystem and shell tool calls to a `rafiki-executor`
process over Connect RPC on a local unix socket. This document describes the
wire protocol: the seven RPCs, their message shapes, the four failure codes, the
background-handle lifecycle, the workspace lifecycle, and the mtime contract.

## Transport

- **Protocol:** Connect (gRPC-compatible) over HTTP/2 cleartext (h2c).
- **Socket:** Unix domain socket, filesystem mode `0600` — no authentication
  beyond filesystem permissions in this phase.
- **Roles:** `rafikid` (the daemon) is the RPC client; `rafiki-executor` is the
  server. Both binaries run on the same host.
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
capabilities. The `tools[]` list is the authority for which tool names the
parent may route — if a name isn't here, the parent keeps it in-process.

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
then incremental deltas every 200ms, then a final `exit_code`. Output is held
in a 100 KB tail ring: a job that writes more than that loses its OLDEST
bytes, and the first chunk an attacher receives is prefixed with
`... [earlier output dropped: buffer limit reached] ...` when that has
happened. An unknown handle is `CodeNotFound`. Dropping the connection does
not kill the job — reattaching resumes from what the ring still holds.

### Provision

```
Provision(childId, workspaceMode, mounts[], workdir, network, memoryBytes, cpus, env)
  → { workspaceId, roots[], workdir, isolation }
```

Unary. Prepares a workspace for one child and returns a handle every later
`Execute` carries. A **native** executor answers with its pinned root and
does nothing else; a **container** executor starts a container with the
daemon-derived mounts.

- `workspaceMode` is `"ephemeral"` (the executor constructs a reconstructible
  workspace) or `"pinned"` (the executor exposes an existing tree). This
  single field decides the park-vs-fail behaviour when an executor is lost.
- `mounts[]` are bind mounts derived by the daemon from the child's worktree
  assignment. The model never writes a path; the executor never invents one.
- `workdir` must be one of the mounts' container paths — an executor MUST
  reject a workdir outside the mounts rather than silently starting
  somewhere the child cannot write.
- `network` is `"none"` or `"bridge"`. Default: `"none"`.
- Resource caps (`memoryBytes`, `cpus`) are zero-means-executor-default.

### Release

```
Release(workspaceId) → {}
```

Unary. Tears a workspace down. **Idempotent:** a daemon restart that lost
track of a workspace must be able to release it again without error. A
released workspace kills its background jobs — a job in a removed container
is not a job, and reporting it as running is worse than reporting it gone.

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
| `data` | ← | bytes from `since` (or the ring's oldest byte) to `total` |
| `total` | ← | bytes the job has ever written, including any dropped |
| `exited` | ← | whether the process has been reaped |
| `exit_code` | ← | meaningful only when `exited` |
| `found` | ← | false when the handle is unknown or was reaped after 10 minutes |

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
- A job's output ring buffer is capped at 100 KB; once exceeded, the OLDEST
  bytes are dropped so the live tail is always available. Byte offsets are
  tracked against a monotonic total so a re-`Attach` after data has been
  dropped can tell the caller it missed output, rather than silently
  replaying or skipping bytes.
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

Parent-side checking alone would be a TOCTOU — the file can change between
the parent's check and the executor's write. The executor-side check at the
last possible moment closes that window.
