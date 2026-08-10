# Executor protocol

The daemon dispatches filesystem and shell tool calls to a `rafiki-executor`
process over Connect RPC on a local unix socket. This document describes the
wire protocol: the five RPCs, their message shapes, the four failure codes, the
background-handle lifecycle, and the mtime contract.

## Transport

- **Protocol:** Connect (gRPC-compatible) over HTTP/2 cleartext (h2c).
- **Socket:** Unix domain socket, filesystem mode `0600` — no authentication
  beyond filesystem permissions in this phase.
- **Roles:** `rafikid` (the daemon) is the RPC client; `rafiki-executor` is the
  server. Both binaries run on the same host.
- **Codec:** Binary protobuf by default; JSON also supported (Connect's
  auto-negotiation).

## RPCs

All five RPCs belong to the `rafiki.executor.v1.ExecutorService` service.

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
Execute(callId, tool, inputJSON, timeoutMs, expectMtime, background)
  → stream { Output(chunk) | Result(content[], isError, observedMtime)
            | Failed(code, msg) | handle }
```

Server-streaming. Dispatches a single tool call. The normal path is:

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

Server-streaming. Attaches to a background job identified by handle. The
server streams accumulated output, then incremental deltas, and finally the
exit code when the process exits. A dropped connection does **not** kill the
job — that is the entire point of handles.

### Cancel

```
Cancel(callId) → {}
```

Unary. Terminates the background job named by `callId`. Does not wait for
the process to reap.

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
- A job's output ring buffer is capped at 100 KB; head and tail are kept,
  the middle elided.
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
