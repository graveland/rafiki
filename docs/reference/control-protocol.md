# pi-controller wire protocol — v1 draft

A coordinator daemon that hosts pi children running in `--mode rpc`, multiplexes
their event streams to multiple concurrent clients, and exposes a small control
plane over a Unix domain socket (or optionally localhost TCP).

This document is the contract between the Go controller, the `pi-ctl` CLI, any
headless drivers, and any eventual thin TUI client.

## 1. Goals

- One always-running daemon. Pi children are subprocesses it owns.
- Many concurrent clients per daemon. Drivers, watchers, CLI commands all attach
  freely. No exclusive-control semantics in v1.
- Forever decoupled from pi's RPC schema. The controller never introspects or
  validates pi RPC frames; they flow through transparently inside an envelope.
  Pi can evolve its own protocol without changing the controller.
- Coordinator-friendly. Subscriptions support filtering so a high-level agent
  can listen for "interesting moments" without eating the token-by-token
  firehose, and pull details on demand.
- Survives controller restarts. Pi children that died are listed and explicitly
  resumable; pi's own session.jsonl is the durable conversation state.
- Forensics by default during development. Each child's three streams (`in`,
  `out`, `err`) are buffered in memory and dumped compressed to disk on exit.

## 2. Transport

Two transports, identical framing and protocol.

### 2.1 Unix domain socket (default)

- Path: the daemon always binds `<RuntimeDir>/controller.sock` — `$XDG_RUNTIME_DIR/rafiki/` if that's set to an absolute path, else `$XDG_STATE_HOME/rafiki/` (default `~/.local/state/rafiki/`); see `pkg/paths.SocketPath`. `$RAFIKI_SOCKET` does **not** change where the daemon binds — it is a client-side override (`pkg/client.DefaultSocketPath`, what a spawned child is handed to find its own daemon) that must agree with the daemon's own path or a client dials a socket nobody is listening on. (`$PI_CONTROLLER_SOCKET` / `~/.pi/run/controller.sock` were the pre-rename names; nothing reads them any more.)
- Mode: socket `0600`, parent directory `0700`. Filesystem permissions are
  the only authentication.
- Stream-oriented (`SOCK_STREAM`). Multiple concurrent client connections.

### 2.2 Remote TCP/TLS (optional)

- Address: `RAFIKI_CONTROL_LISTEN` (e.g. `tcp:8036`). Unset → TCP disabled;
  the daemon listens on the UDS only.
- TLS is **mandatory** on TCP — the daemon refuses to start without
  `RAFIKI_CONTROL_TLS_CERT` and `RAFIKI_CONTROL_TLS_KEY`. No plaintext TCP
  control plane, ever.
- `RAFIKI_CONTROL_LISTEN` also **requires `RAFIKI_DB`**: control-plane
  identity is row-backed and there is nothing to degrade to. Without a
  database the daemon exits at startup rather than serving a listener stuck
  in permanent bootstrap mode.
- Certificates are re-read from disk per handshake via
  `tls.Config.GetCertificate`, so cert-manager rotation needs no pod restart.
  Minimum TLS version: 1.2.
- Authentication: **per-user tokens**, mandatory. The first frame on a TCP
  connection must be `ctrl_auth` (see §6.6), carrying a token minted by
  `ctrl_user_create` and stored as a digest in `conversations.users`. The
  resolved identity is attached to the connection for its lifetime.
- Failure modes are distinct and must stay distinct: an unknown or
  tombstoned token answers `auth_invalid`; an identity store that cannot be
  reached answers `internal` with a fixed message ("identity store
  unavailable") and never the store's own error text. Reporting an outage as
  an invalid credential makes clients discard working tokens.
- **Bootstrap mode.** While zero active users exist, a TCP connection is
  admitted with no `ctrl_auth` frame at all, and the first frame it sent is
  dispatched as an ordinary request — but the connection is *restricted*:
  every command other than `ctrl_user_create` is refused with
  `auth_required`. The first `ctrl_user_create` therefore claims the daemon,
  and closes the window for everyone else. The daemon logs a warning on each
  such connection and once a minute for as long as the window is open.
- **The window is re-checked on every restricted request, not just at
  admission.** Admission is decided once, when the connection is accepted,
  and is never revisited; so `ctrl_user_create` on a restricted connection
  re-reads the active user count from the store and answers `auth_required`
  if it is non-zero. Without that, a peer that connected during the window
  and simply held the socket open would keep minting users indefinitely.
  Two callers racing for the very first user is a different matter and is
  accepted: the database decides, first writer wins.
- **An unauthenticated peer never receives an underlying error's text.** On a
  restricted connection an `internal` response carries a fixed message
  ("identity store unavailable") and the real error goes to the daemon log.
  This mirrors the handshake rule above and exists for the same reason: a pgx
  connection error names the host, the user and the database.
- UDS connections skip auth entirely — local filesystem permissions
  (0600 socket, 0700 directory) are the only trust mechanism. They are never
  bootstrap-restricted and carry no user identity.

## 2.3 Connect control plane (HTTP)

A second, additive control face served by Connect (`connectrpc.com/connect`) on
the same HTTP listener as the proxy faces. The JSON-Lines frame protocol in the
rest of this document is unchanged and remains the path `rafiki attach` and the
existing CLI use.

In this phase, the Connect plane is reached over the **local proxy listener
only** — the same base URL `/v1/messages` already uses — not the separate
remote-daemon TLS control listener an `https://` `RAFIKI_URL` names. `rafiki
history` detects that case and says so rather than failing with an opaque
transport error.

- **Path prefix:** `/rafiki.v1.Control/`
- **Schema:** `proto/rafiki/v1/*.proto`; generated Go in `pkg/gen/rafiki/v1/`.
- **Auth:** the same per-user bearer credential as the proxy faces
  (`Authorization: Bearer`, `x-api-key`, or `X-Rafiki-Token`), resolved
  against the users store by `server.UserTokenAuth` — the Connect plane is
  mounted under the identical middleware that protects `/v1/messages` and
  `/v1/chat/completions` (`server.Handler.Mount`), not a separate
  control-plane-specific credential. There is deliberately no second auth
  scheme here: CLAUDE.md documents one credential scheme for both users and
  executors, and a parallel mechanism for this face would drift from it.
- **Encoding:** protobuf or JSON. A unary call is an ordinary HTTP POST:

```bash
curl -H 'Content-Type: application/json' \
     -H "Authorization: Bearer $RAFIKI_TOKEN" \
     -d '{"childId":"c_01HX..."}' \
     http://127.0.0.1:8035/rafiki.v1.Control/GetHistory
```

### Two event tiers

| Tier | Contents | Cursor | Resumable |
|---|---|---|---|
| Durable | Persisted messages | `conversation_message.ordinal` | Yes, exactly |
| Ephemeral | Live content deltas | `turn_id` only | No, by design |

`GetHistory` and `StreamEvents` both accept `afterOrdinal`. Ordinal is used
rather than a timestamp because postgres `now()` is transaction-start time, so
every message written in one transaction shares a byte-identical `created_at` —
making any timestamp cursor either re-deliver or drop.

### Verbs

| RPC | Kind | Purpose |
|---|---|---|
| `GetHistory` | unary | Durable events for one child, after an optional ordinal |
| `StreamEvents` | server-streaming | Durable replay, then live follow |

## 3. Framing

JSON Lines (`application/jsonl`).

- **Record delimiter is `\n` (LF) and only `\n`.** Strip an optional trailing
  `\r` on input. Do **not** split on `U+2028` or `U+2029`; these are valid
  inside JSON strings. Generic line readers (e.g. Node `readline`) that split
  on Unicode separators must not be used. Go implementers must not rely on
  `bufio.Scanner` with `ScanLines` if the buffer-size default is small — use
  a custom splitter or raise `MaxScanTokenSize` well above the largest
  expected pi event (≥16MB).
- Each frame is a single, complete JSON object on one line.
- Encoding is UTF-8. No BOM.
- Each frame has a `type` field (string). Frames without `type` are rejected.

## 4. Identifiers

Two unrelated identifier concepts. Worth being explicit about the
distinction because their names collide naturally and conflating them is a
common implementer mistake.

### 4.1 `childId` — resource identity

Opaque ULID-shaped string assigned by the controller when a child is
spawned. Stable for the lifetime of the persistent record — it survives
`ctrl_resume`, so the same `childId` spans multiple underlying pi processes.
Never reused.

**Always controller-assigned**. Clients cannot supply a desired `childId` on
spawn. The only commands that *take* a `childId` are those acting on an
existing resource: `ctrl_resume`, `ctrl_kill`, `ctrl_forget`, `ctrl_subscribe`,
`ctrl_send`, etc. For these, the `childId` references something the
controller has already minted.

If clients want a stable user-visible identifier, that's what `name` is for.

### 4.2 `id` — request/response correlation

Optional, client-supplied. Echoed by the controller in the matching
`ctrl_response`. Plays the same role as `id` in JSON-RPC: lets clients
correlate responses to outstanding requests when they have several
in flight.

**`id` is not the identity of any resource.** It does not name the
`childId` being created (`ctrl_spawn`), or the operation outcome, or
anything else. Set it to whatever you want for tracking; it's opaque to the
controller.

Independent of any pi-RPC `id` field inside frames forwarded via
`ctrl_send`. Pi has its own request-correlation `id` field; the controller
neither inspects nor modifies it.

### 4.3 Worked example

```jsonc
// client sends a spawn request
{ "type": "ctrl_spawn", "id": "req-42", "cwd": "/...", "name": "afk" }

// controller responds; "id" echoes "req-42", "childId" is the new resource
{ "type": "ctrl_response", "command": "ctrl_spawn", "id": "req-42",
  "success": true,
  "data": { "childId": "c_01HX..." } }

// later: client kills the spawned child
{ "type": "ctrl_kill", "id": "req-43", "childId": "c_01HX..." }
//                          ^^^^ correlation       ^^^^^^^^^^^^ resource
```

## 5. The two namespaces

Every frame's `type` field falls in exactly one of two namespaces:

1. **`ctrl_*`** — controller verbs (commands, responses, events). The
   controller handles these.
2. **Anything else** — opaque pi-RPC frames. Only legal inside the `frame`
   field of `ctrl_send` (client → controller direction) or inside the `event`
   field of `ctrl_event` (controller → client direction). The controller is
   transparent for *routing and content* of these frames — except for a
   small, documented set of intercepted commands (§5.1). For non-intercepted
   frames the controller forwards verbatim. For intercepted frames the
   controller takes a defined action and synthesizes a response event so the
   client experience matches pi's native semantics.

Additionally, the controller opportunistically *sniffs* response and event
payloads for known metadata fields (`sessionFile`, `sessionId`, `sessionName`,
`model`) to keep its in-memory cache and the on-disk state record current.
This is observation only — never routing or rewriting.

This is the load-bearing decoupling: as long as the controller's envelope
shape is stable and the intercepted set stays narrow, pi can add, remove, or
modify its own RPC commands and events without breaking the controller or
its clients.

### 5.1 Intercepted pi-RPC commands

These commands are recognized inside `ctrl_send.frame` and *not* forwarded
to pi. The controller performs the documented action and synthesizes the
appropriate pi-RPC response event to subscribers, matching pi's native
response shape.

| Pi command        | Action                                                       |
|-------------------|--------------------------------------------------------------|
| `new_session`     | Graceful shutdown of current child, respawn with no `--session` (fresh session, same `childId`). |
| `switch_session`  | Graceful shutdown, respawn with `--session <targetPath>` (same `childId`). |

**Why interception**: pi's RPC mode emits no event for session changes, and
the responses to these commands don't carry the new `sessionId`/`sessionFile`.
Without interception the controller's cached metadata and on-disk state
record can go stale, breaking `ctrl_resume` and `ctrl_list`. Interception
guarantees the controller always knows the active session.

**Timing**: interception uses the same graceful shutdown timeouts as
`ctrl_kill` (§6.5). A `/new` or `/resume` via the controller may take up to
~3 minutes in pathological cases (shutdown handlers running LLM calls with
retries). Clients should not assume sub-second response.

**Pass-through with caveat**: `fork`, `clone`, `import_session`, and any
other pi command that may change the active session under the process are
*not* intercepted. They flow through verbatim. The controller's metadata
cache will go stale until the client (or a periodic refresh, see §11.5)
sends `{"type":"get_state"}` via `ctrl_send`; the controller's response
sniff picks up the new fields.

**Synthesized response shape**: when interception completes successfully,
the controller emits a `ctrl_event` carrying:

```jsonc
{ "type": "response", "command": "new_session",
  "id": "<original pi-RPC id from frame>",
  "success": true, "data": { "cancelled": false } }
```

This matches what pi would have returned for a native non-cancelled
`new_session`. The new `sessionId`/`sessionFile` are not in pi's native
response either; clients that need them call `get_state` against the
respawned child (or call `ctrl_get` to query the controller's freshly
populated cache).

## 6. Client → controller commands

All commands respond with a single `ctrl_response` carrying the same `id`
(if provided). Events from subscribed children flow asynchronously and are
not paired with any specific command.

### 6.1 `ctrl_list`

List children known to the controller, including those in the exit grace
window.

```jsonc
{
  "type": "ctrl_list",
  "id": "1",
  "filter": {                       // all fields optional
    "status": "streaming",          // exact match
    "name": "afk-impl",             // exact match
    "nameContains": "afk",
    "cwdContains": "/savannah",
    "since": 1716000000             // unix ms; lastActivity >= this
  }
}
```

Response:

```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_list",
  "id": "1",
  "success": true,
  "data": {
    "children": [
      {
        "childId":            "c_01HX...",
        "pid":                12345,             // null when status == "exited"
        "cwd":                "/Users/.../dev",
        "name":               "afk-impl",        // user-visible label, mutable
        "model":              "anthropic/claude-sonnet-4",
        "sessionId":          "abc123",          // pi internal
        "sessionFile":        "/Users/.../session-abc.jsonl",
        "status":             "streaming",       // see §11
        "startedAt":          1716636789,
        "lastActivity":       1716636890,
        "exitCode":           null,              // populated when exited
        "exitSignal":         null,
        "contextWindow":      200000,            // omitted when the daemon's
        "maxCompletionTokens":64000               // model catalog has no entry for "model"
      }
    ]
  }
}
```

`contextWindow`/`maxCompletionTokens` come from the daemon's own shared
model catalog (`routing.ModelCatalog.ContextWindow`, backed by OpenRouter's
live model list — the same data source pricing already uses), independent
of whatever static model list a client-side TUI carries. Both are omitted
(zero value, `omitempty`) when the catalog has no entry for the child's
`model` — no catalog configured, a model the catalog hasn't seen, or a
cold/stale cache.

Default sort: `startedAt` desc. Entries in the grace window (status
`exited`) are included by default; filter on `status` to narrow.

### 6.2 `ctrl_get`

Snapshot one child by id. Same shape as a single `ctrl_list` entry.

```jsonc
{ "type": "ctrl_get", "id": "2", "childId": "c_01HX..." }
```

### 6.3 `ctrl_spawn`

Start a new pi child. The controller assigns a fresh `childId` and returns
it in the response. The spawn does not return until pi has started, replied
to an initial `get_state`, and (if `name` was set) been renamed — so when
the client sees the response, the child is fully ready for `ctrl_send`.

```jsonc
{
  "type":                "ctrl_spawn",

  // Identity
  "name":                "afk-impl",       // optional; if set, controller
                                            // sends set_session_name after spawn
  "parentChildId":       null,             // optional; child id of spawning parent
                                            // recorded as rafiki/parent and rafiki/root
                                            // labels; rejected with child_not_found if
                                            // no such child exists
  "task":                null,             // optional; handle ("2.1") in spawner's
                                            // task ledger to assign to new child
                                            // Honoured only with parentChildId and
                                            // spawnerConversationId. Assigned after
                                            // admission; a refused spawn assigns nothing.
  "spawnerConversationId": null,           // optional; the conversation task resolves
                                            // against. DAEMON-SET ONLY. A client setting
                                            // it is naming somebody else's ledger.

  // Working directory (required, absolute)
  "cwd":                 "/Users/.../dev",

  // Model + auth (all optional; pi resolves from its own config if omitted)
  "provider":            "anthropic",
  "model":               "claude-sonnet-4",
  "thinking":            "medium",         // off|minimal|low|medium|high|xhigh
  "apiKey":              "sk-...",         // not persisted in state record

  // Session (all optional)
  "noSession":           false,            // --no-session
  "sessionDir":          null,             // --session-dir
  "resumeSession":       null,             // --session <path>
  "forkSession":         null,             // --fork <path>

  // Tool / extension / skill scoping (all optional)
  "tools":               null,             // --tools <comma-joined>
  "noTools":             false,
  "noBuiltinTools":      false,
  "extensions":          [],               // -e <src> per entry
  "noExtensions":        false,
  "skills":              [],               // --skill <path> per entry
  "noSkills":            false,
  "skillsDirs":          [],               // --skills-dir <dir> per entry; kind=fundi only
  "mcpConfig":           null,             // --mcp-config <path>; kind=fundi only
  "mcpServers":          [],               // --mcp-servers <comma-joined>; kind=fundi only
  "noMcp":               false,            // --no-mcp; kind=fundi only
  "promptTemplates":     [],               // --prompt-template <path>
  "noPromptTemplates":   false,
  "themes":              [],               // --theme <path>
  "noThemes":            false,
  "noContextFiles":      false,            // --no-context-files

  // System prompt (all optional)
  "systemPrompt":        null,             // --system-prompt
  "appendSystemPrompt":  null,             // --append-system-prompt

  // Verbosity
  "verbose":             false,

  // Process control
  "piBinary":            null,             // absolute path; overrides PATH lookup
  "env":                 {},                // additions/overrides merged into inherited
  "envOverride":         false,            // replace inherited env entirely (rare)

  // Escape hatch
  "extraArgs":           []                // appended last; wins by last-flag-wins

  // Resource grants (pointer fields — "unset" ≠ "zero")
  "maxDepth":            1,                // levels this child may further delegate
                                            // 0 = cannot spawn. Default 1. Bounded by
                                            // RAFIKI_MAX_DEPTH (default 3), an absolute
                                            // ceiling on position in the tree.
  "maxCost":             null,             // USD budget for this child's subtree.
                                            // Nil = unlimited. A child gets at most its
                                            // parent's remaining budget. Refused with
                                            // invalid_args when over or unreadable.
  "maxChildren":         4,                // live descendant cap; default 4. Zero blocks
                                            // every spawn beneath this child.

  // Executor grant (the entire model-facing grant — a selector, not a path)
  "executorSelector":    null,             // label selector over executor labels,
                                            // e.g. "env=work,os=linux". Intersected with
                                            // the parent's effective executor set, so it
                                            // can only ever narrow. A spawn matching
                                            // nothing is refused naming the excluding
                                            // predicate per candidate; it does not queue.
                                            //
                                            // nil means NO workspace tier: the child is
                                            // registered with no read, write, edit, glob,
                                            // grep, ls, bash or lsp_* tools at all. That
                                            // is an answer, not an error — a caller that
                                            // wants a pure reasoning agent over the
                                            // daemon tier gets exactly that.
  "workspaceMode":       null,             // "ephemeral" or "pinned". Empty = pinned.
                                            // Decides whether the child can be rescheduled
                                            // when its executor goes away. The resolved
                                            // assignment (executor, isolation, roots,
                                            // read-only roots) is written into the
                                            // child's system prompt at spawn.
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_spawn", "id": "req-42",
  "success": true,
  "data": {
    "childId":     "c_01HX...",
    "sessionId":   "abc123",
    "sessionFile": "/Users/.../session-abc.jsonl",
    "model":       "anthropic/claude-sonnet-4",  // pi's resolved choice
    "stalled":     false                          // see below
  }
}
```

`stalled: true` indicates the child started successfully but did not respond
to the initial `get_state` within the kickstart timeout (default 5s). The
child is still alive in `spawning` state; the caller can `ctrl_kill` it or
wait. The other response fields will be empty/null in this case.

Errors: `spawn_failed`, `invalid_args`.

#### 6.3.1 Spawn defaults and policy

| Concern | Policy |
|---------|--------|
| **Pi binary location** | `piBinary` field → `$PI_BINARY` env → `exec.LookPath("pi")`. Resolved per-spawn. |
| **Working directory** | `cwd` required, absolute, exists, readable. Set via `cmd.Dir`. Pi has no `--cwd` flag. |
| **Kind may not widen confinement** | A `ctrl_spawn` whose `parentChildId` names a child carrying an executor grant (`executorSelector`) is refused with `invalid_args` unless `kind` is `fundi`. `claude` and `pi` are forked on the daemon's own host and ignore executors entirely, so allowing one would let a confined agent spawn an unconfined sibling. Applies identically to `ctrl_resume` and to the `agent_spawn` tool, because the check reads the stored parent row rather than anything the caller supplied. Top-level spawns have no parent and are unaffected. |
| **Model / thinking / API key** | Never defaulted by the controller. Omitted fields are passed to pi as missing flags; pi resolves from `auth.json`, `settings.json`, env, etc. State record stores requested values; response reports pi's resolved choice. |
| **API key persistence** | `apiKey` is **never** written to the on-disk state record. `ctrl_resume` of a child originally spawned with a per-call `apiKey` requires re-supplying it (see §6.4). |
| **Environment** | Full inherit from the controller's `os.Environ()` by default. `env` field merges additions/overrides. `envOverride: true` replaces entirely. Controller always injects `PI_CONTROLLER_CHILD_ID=<id>` and `PI_CONTROLLER_SOCKET=<path>`; these are reserved and silently override any user value. |
| **Extensions / skills** | Pi's auto-discovery runs against the child's `cwd`. Project-local `.pi/extensions/` and `.pi/skills/` are picked up automatically. The various `no*` flags and `extensions` / `skills` arrays let callers further scope. |
| **Session handling** | Omitted session fields → pi creates a fresh jsonl in its default location. `resumeSession` triggers `--session <path>`; `forkSession` triggers `--fork <path>`. |
| **Spawn blocks until ready** | Caller waits ~500ms–1s for pi to start, return its first `get_state`, and (optionally) be renamed. Trade: caller code stays simple; controller guarantees a known-good child on success. |
| **Naming race** | `set_session_name` is sent after the initial `get_state` returns. If the rename round-trip fails (rare), the spawn still succeeds, the name stays at pi's default, and a warning event is emitted. |

#### 6.3.2 Argv assembly order

```
pi --mode rpc
   [--no-session]
   [--session <path>]      |   [--fork <path>]
   [--session-dir <path>]
   [--provider <p>]   [--model <m>]   [--thinking <t>]   [--api-key <k>]
   [--no-builtin-tools]   [--no-tools]   [--tools <list>]
   [--no-extensions]       [-e <src>]*
   [--no-skills]           [--skill <path>]*
   [--no-prompt-templates] [--prompt-template <path>]*
   [--no-themes]           [--theme <path>]*
   [--no-context-files]
   [--system-prompt <text>] [--append-system-prompt <text>]
   [--verbose]
   [...extraArgs]
```

Fields are added only if non-zero/non-empty. `extraArgs` are appended last
and always win pi's flag parsing (last-wins). The controller logs the full
assembled argv (with `--api-key` redacted) at spawn time.

#### 6.3.3 Spawn sequence on the wire

```
1. Validate ctrl_spawn fields; reject early on invalid_args.
2. Resolve pi binary path.
3. Assemble argv.
4. exec.Command(pi, argv...).Start()
5. Status = spawning; persist state record; emit ctrl_child_spawned.
6. Send {"type":"get_state"} on pi's stdin.
7. Read first pi response. On timeout (5s), set stalled=true; skip 8–10.
8. From response: extract sessionId, sessionFile, sessionName, model.
   Update Session entry + state record.
9. Status = idle; emit ctrl_child_status.
10. If spawn.name was set and differs from response sessionName:
    send {"type":"set_session_name","name":"..."}; update on response.
11. Send ctrl_response acknowledging the spawn, with full metadata in data.
```

#### 6.3.4 Spawn failure modes

| Failure | Error | Cleanup |
|---------|-------|---------|
| `cwd` invalid or absent | `invalid_args` | none; no child ever started |
| Pi binary not found | `spawn_failed` | none |
| `cmd.Start()` fails | `spawn_failed` | none |
| Pi exits before first response | `spawn_failed` | reap; capture last stderr into error message; no state record persists |
| Initial `get_state` 5s timeout | success with `stalled: true` | child alive, status stays `spawning` |
| `set_session_name` round-trip fails | success | name stays as pi's default; emit warning event |

### 6.4 `ctrl_resume`

Re-spawn a pi child against its persisted state record. The child must be in
`exited` status. Inherits the existing `childId`; the new pi process runs with
`--session <recordedSessionFile>` plus the original spawn arguments.

```jsonc
{
  "type":    "ctrl_resume",
  "childId": "c_01HX...",
  "apiKey":  "sk-..."      // optional; required if the original spawn used
                            // a per-call apiKey not in env
}
```

Response is identical to `ctrl_spawn`'s, returning the same `childId` with
refreshed metadata.

`apiKey` provided here is used at spawn time only and is *not* persisted to
the state record, same as in `ctrl_spawn`.

Errors: `not_found`, `not_resumable` (child is not in `exited`),
`session_file_missing`, `spawn_failed`.

### 6.5 `ctrl_kill`

Stop a running child gracefully, escalating to SIGKILL only if necessary.

```jsonc
{
  "type":               "ctrl_kill",
  "id":                 "5",
  "childId":            "c_01HX...",
  "shutdownTimeoutMs":  180000,   // optional; default 180000 (3 min)
  "killTimeoutMs":      30000     // optional; default 30000 (30 s)
}
```

Three-stage sequence:

1. **Close child's stdin.** Pi sees EOF and runs `session_shutdown`
   extension handlers. This is the path that lets memory engines persist
   state, summarizers write their final turn, etc. — including handlers that
   make LLM calls with retries.
2. **Wait up to `shutdownTimeoutMs`.** If the child exits in this window,
   we're done.
3. **SIGTERM.** Pi handles this and gets another opportunity to run cleanup.
4. **Wait up to `killTimeoutMs`.** If still alive, **SIGKILL**.
5. `cmd.Wait()` to reap.

Status transitions on entry to step 1: any → `shutting_down`. On reap:
`shutting_down → exited`. Subscribers see both transitions via
`ctrl_child_status`.

During `shutting_down`, the child's stdin is closed; any further `ctrl_send`
against this child returns `child_shutting_down` (§8).

Why the long defaults: shutdown handlers that call LLMs are realistic
(memory engines doing embeddings, summarization extensions, classifiers).
Auto-retry on transient errors can add 30-90s by itself. Truncating an
in-progress LLM call risks corrupting persisted state. Configurable globally
via `PI_CONTROLLER_SHUTDOWN_TIMEOUT` and `PI_CONTROLLER_KILL_TIMEOUT`.

Response is acknowledged after reap.

Errors: `child_not_found`, `child_exited` (already gone).

```jsonc
{ "type": "ctrl_response", "command": "ctrl_kill", "id": "5",
  "success": true,
  "data": { "exitCode": 0, "signal": null, "durationMs": 1247,
            "escalated": false } }
```

`escalated: true` if SIGTERM or SIGKILL was needed; `false` if pi exited
from stdin EOF alone.

### 6.6 `ctrl_auth` (TCP/TLS only)

```jsonc
{ "type": "ctrl_auth", "id": "0", "token": "..." }
```

Must be the first frame on a TCP connection, except in bootstrap mode
(§2.2), where the first frame may instead be a `ctrl_user_create`. TCP
connections run over mandatory TLS (see §2.2); there is no plaintext TCP
control plane.

The token is a per-user token minted by `ctrl_user_create`. On success, the
connection proceeds normally — no explicit success response is written, and
the resolved identity is attached to the connection for its lifetime. On
failure the controller sends an error response and closes the connection:
`auth_invalid` for a token that names no active user, `auth_required` for a
non-`ctrl_auth` first frame once a user exists, and `internal` ("identity
store unavailable") when the store could not be consulted at all — a
distinction clients depend on, since `auth_invalid` means "discard this
credential". UDS connections skip auth entirely.

### 6.7 `ctrl_subscribe`

Subscribe to events from a single child. Multiple `ctrl_subscribe` calls on
the same connection (to different children, or with different filters) are
independent — events are tagged with `childId` in the wrapping frame.

```jsonc
{
  "type":    "ctrl_subscribe",
  "id":      "6",
  "childId": "c_01HX...",
  "filter": {                      // all optional
    "profile": "coarse",           // see §6.8 for the profile table
    "include": ["turn_end"],       // event types to add
    "exclude": ["message_update"]  // event types to remove
  }
}
```

Filter resolution: `(profile members) ∪ include − exclude`. Default profile
when omitted: `firehose` (everything).

Once acknowledged, events from this child are delivered to this connection
as `ctrl_event` frames (§7.1).

### 6.8 Profile registry

Named filter sets defined by the controller. The controller's set is the
authoritative source; new pi event types added later get folded into
existing profiles where appropriate so clients using `profile: "coarse"`
remain durable across pi version changes.

| Profile     | Events forwarded                                                                  |
|-------------|-----------------------------------------------------------------------------------|
| `firehose`  | All pi events + all `ctrl_child_*` events. The default.                           |
| `results`   | `agent_start`, `agent_end`, `turn_end`, `extension_ui_request`, `auto_retry_*`, `compaction_*`, all `ctrl_child_*` |
| `coarse`    | `agent_end`, `extension_ui_request`, `ctrl_child_status`, `ctrl_child_exited`, `auto_retry_end` (on failure)        |
| `lifecycle` | Only `ctrl_child_*` events                                                        |

Recommendation: coordinator agents use `coarse` plus `ctrl_get_recent`
queries (§6.11) on `agent_end` to inspect details on demand.

### 6.9 `ctrl_unsubscribe`

```jsonc
{ "type": "ctrl_unsubscribe", "id": "7", "childId": "c_01HX..." }
```

Removes the subscription for this connection. Events already in the
client's receive buffer may still arrive after the response.

### 6.10 `ctrl_global_subscribe` / `ctrl_global_unsubscribe`

Subscribe to controller-wide lifecycle events (`ctrl_child_spawned`,
`ctrl_child_exited`, `ctrl_child_renamed`, `ctrl_child_status` for any
child). Useful for monitoring UIs.

```jsonc
{ "type": "ctrl_global_subscribe", "id": "8" }
{ "type": "ctrl_global_unsubscribe", "id": "9" }
```

Global subscribers see only `ctrl_child_*` events, never `ctrl_event`
(per-child pi event frames).

### 6.11 `ctrl_get_recent`

Query the per-child replay buffer for recent events without subscribing.
This is the primary mechanism for coordinator agents to pull details after
a coarse-subscription notification.

```jsonc
{
  "type":    "ctrl_get_recent",
  "id":      "10",
  "childId": "c_01HX...",
  "limit":   100,                                  // optional; default 100
  "since":   1716636789,                           // optional; ms; events with timestamp >= since (inclusive)
  "include": ["turn_end", "tool_execution_end"],   // optional; filter
  "exclude": ["message_update"]                    // optional
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_get_recent", "id": "10",
  "success": true,
  "data": {
    "events":           [ /* verbatim pi events, in publish order */ ],
    "totalInBuffer":    2347,
    "oldestTimestamp":  1716636000,
    "truncatedByLimit": false
  }
}
```

`totalInBuffer` and `oldestTimestamp` let the caller detect that the ring
has dropped events earlier than what was returned (i.e., the caller's
`since` is older than `oldestTimestamp`).

### 6.12 `ctrl_send`

Forward a pi-RPC frame to a child's stdin. The controller does not
introspect or modify the `frame` field; it is re-serialized to one JSONL
line and written verbatim.

```jsonc
{
  "type":    "ctrl_send",
  "id":      "11",
  "childId": "c_01HX...",
  "frame":   { "type": "prompt", "message": "Hello", "id": "p1" }
}
```

Response acknowledges the *forward*, not pi's response to the frame:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_send", "id": "11",
  "success": true
}
```

Pi's response (if any — for synchronous commands like `set_model`) and
subsequent streaming events flow to this connection only if it is
subscribed to the child. Clients that want to confirm pi acted on a
command should correlate using pi's own `id` field inside the `frame`
and watch for the matching `response` event.

Errors: `child_not_found`, `child_exited`, `child_in_grace`, `backpressure`.

### 6.13 `ctrl_forget`

Drop an exited child from the controller's in-memory store. Does not
remove disk artifacts (logs or state record); the user manages those
out-of-band.

```jsonc
{ "type": "ctrl_forget", "id": "12", "childId": "c_01HX..." }
```

Only valid in `exited` status. Errors: `not_found`, `not_exited`.

### 6.14 `ctrl_forget_all_exited`

```jsonc
{
  "type":         "ctrl_forget_all_exited",
  "id":           "13",
  "olderThanMs":  3600000     // optional; default = all in grace window
}
```

Response includes `{ "count": N }` indicating how many entries were
removed.

### 6.15 `ctrl_search`

Live-only content search across in-memory state. Walks each entry's
replay ring and the live tail of its open session.jsonl.

```jsonc
{
  "type":  "ctrl_search",
  "id":    "14",
  "query": "ublk_register",
  "regex": false,
  "limit": 50,
  "context": 2,                          // lines around each hit
  "sessionFilter": {                     // narrows the children scanned
    "cwdContains":  "/savannah",
    "nameContains": "afk",
    "since":        1716000000
  }
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_search", "id": "14",
  "success": true,
  "data": {
    "hits": [
      {
        "childId":     "c_01HX...",
        "sessionFile": "/Users/.../session-abc.jsonl",
        "sessionId":   "abc",
        "sessionName": "savannah-impl",
        "entryId":     "msg-42",
        "timestamp":   1716636789,
        "role":        "assistant",
        "snippet":     "...calling ublk_register on the device...",
        "matchStart":  12, "matchEnd": 25
      }
    ],
    "totalHits": 137,
    "scanned":   12,         // number of children scanned
    "elapsed":   42          // milliseconds
  }
}
```

Out of scope for v1: scanning historical session files on disk that are
not currently owned by any live child. Users grep
`~/.pi/agent/sessions/` directly when they need that.

### 6.16 `ctrl_status`

Controller daemon health and stats.

```jsonc
{ "type": "ctrl_status", "id": "15" }
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_status", "id": "15",
  "success": true,
  "data": {
    "version":      "0.1.0",
    "startedAt":    1716000000,
    "children":     { "live": 3, "exited": 2 },
    "memoryBytes":  157286400,
    "socket":       "/Users/.../controller.sock",
    "logsDir":      "/Users/.../logs"
  }
}
```

### 6.17 `ctrl_conversation_stats`

**fundi-specific.** Unlike every other command in this document, this is not answerable by a
stock pi-controller daemon — it requires the `fundi` child kind's database
(`RAFIKI_DB`), which pi-controller has no concept of. `pic` sending this to a real
pi-controller daemon gets `unknown command`.

Global stats over persisted conversation history, or stats for one conversation when
`conversationId` is given (filter fields are then ignored). `sinceUnix`/`untilUnix` are Unix
seconds (0/absent = unbounded). `owner` is a **username** from `conversations.users` — the
daemon resolves it to a user id server-side; rows store the id, never the name.

```jsonc
{
  "type":      "ctrl_conversation_stats",
  "id":        "30",
  "owner":     "brent",
  "sinceUnix": 1716000000
}
```

Response `data` is the same JSON shape `rafikid agent stats -j` prints (`pkg/insights.Stats`):
volume, adoption, token, cost, failure, latency, cache-waste, and prefix-reuse facets. See
`docs/agent-cli.md` for the field-level description; not duplicated here since it's the exact
same struct.

Errors: `no_agent_db` (§8) means the daemon has no database configured; `not_found` (§8) means
`conversationId` was given but no such conversation exists.

### 6.18 `ctrl_conversation_search`

**fundi-specific** (see §6.17). Searches persisted conversation history — the opposite
population from `ctrl_search` (§6.15), which is live-only and explicitly does not scan
historical sessions.

```jsonc
{
  "type":      "ctrl_conversation_search",
  "id":        "33",
  "text":      "skill gap",
  "status":    "failed",
  "minTokens": 5000,
  "limit":     20
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_conversation_search", "id": "33",
  "success": true,
  "data": { "rows": [ /* insights.ConversationSummary, same shape rafikid agent search -j prints */ ] }
}
```

Results are capped at 500 rows per request regardless of the requested `limit`.

Error `no_agent_db` (§8) means the daemon has no database configured.

### 6.19 `ctrl_conversation_export`

**fundi-specific** (see §6.17). Fetches one persisted conversation's full transcript.

```jsonc
{ "type": "ctrl_conversation_export", "id": "35", "conversationId": "conv-abc" }
```

Response `data` is `insights.Transcript` (same shape `rafikid agent export -j` prints):
ordered turns with role, content, per-turn token/latency/model metrics, and the recovered
skill catalog.

Errors: `invalid_args` when `conversationId` is missing; `not_found` when no such conversation
exists; `payload_too_large` (§8) when the transcript exceeds the maximum response size — export
it via `rafikid agent export` instead; `no_agent_db` (§8) when the daemon has no database
configured.

### 6.20 `ctrl_task_list`

Query the task ledger. Returns tasks as a bare array of `tasks.Task` objects (not wrapped in
`{"rows": [...]}`).

```jsonc
{
  "type":    "ctrl_task_list",
  "id":      "37",
  "childId": "c_abc",        // optional: filter by assignee
  "status":  "orphaned",     // optional: filter by status
  "limit":   500,            // optional: max rows; clamped to 2000
  "all":     true            // optional: include dropped tasks
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_task_list", "id": "37",
  "success": true,
  "data": [ /* tasks.Task objects */ ]
}
```

The query spans every conversation — this verb answers "what is every agent
doing", so there is no conversation scope. `limit` is clamped to 2000 rows
server-side and applied after ordering, which is what keeps the response
inside the 16 MiB frame ceiling. Errors: `no_agent_db` when the daemon has no
database configured.

CLI: `rafiki tasks [--child <id>] [--status <s>] [--limit <n>] [--all]`.

### 6.21 `ctrl_model_info`

Answers what the daemon's warm OpenRouter catalog knows about a model, so a
client never has to read that catalog off disk itself (which is what made
`cmd/rafiki` link postgres — it imported `pkg/routing` for `ModelCatalog`).

```jsonc
{ "type": "ctrl_model_info", "id": "38", "model": "anthropic/claude-opus-5" }
```

Response `data` is bare (not wrapped in a `{"rows":[...]}` envelope, unlike
`ctrl_conversation_search` — the same shape `ctrl_conversation_stats` and
`ctrl_conversation_export` use):

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_model_info", "id": "38",
  "success": true,
  "data": {
    "model":               "anthropic/claude-opus-5",
    "resolvedId":          "anthropic/claude-opus-5",  // omitempty; absent when unknown
    "contextWindow":       200000,
    "maxCompletionTokens": 8192,
    "autoCompactWindow":   190000,
    "known":               true
  }
}
```

`known == false` is an ordinary answer, not an error: an unknown model and an
unconfigured catalog (proxy disabled) both return zeroes with `known: false`.
`autoCompactWindow` is computed daemon-side (`contextWindow` minus a reply
reserve clamped to 5%–10%) so the formula does not live in two binaries. The
raw numbers ride along for future callers; the `rafiki claude` client reads only
`autoCompactWindow` and does no maths. There is no error code — an unreachable
daemon is a transport failure the client handles by leaving the model's defaults
alone, and an unknown model is `known: false`.

## 7. Controller → client events

### 7.1 `ctrl_event`

A pi-RPC event from a subscribed child, wrapped with the source `childId`.

```jsonc
{
  "type":    "ctrl_event",
  "childId": "c_01HX...",
  "event":   { "type": "turn_end", "message": { /* ... */ }, "toolResults": [] }
}
```

The inner `event` object is verbatim from the child's stdout. The
controller does not modify, filter (beyond the subscription filter), or
rewrite it. New pi event types pass through transparently.

### 7.2 `ctrl_child_spawned`

```jsonc
{
  "type":    "ctrl_child_spawned",
  "childId": "c_01HX...",
  "name":    "afk-impl",
  "cwd":     "/Users/.../dev",
  "pid":     12345,
  "model":   "anthropic/claude-sonnet-4",
  "at":      1716636789
}
```

Delivered to global subscribers and to per-child subscribers of this
child (i.e., a client that subscribed to a not-yet-existing child via
some race; see §11 for the resume case where this fires on a known
`childId`).

### 7.3 `ctrl_child_exited`

```jsonc
{
  "type":       "ctrl_child_exited",
  "childId":    "c_01HX...",
  "exitCode":   0,
  "signal":     null,
  "lastStatus": "streaming",
  "duration":   124.5,         // seconds
  "at":         1716636913
}
```

### 7.4 `ctrl_child_status`

Status transitions derived from pi events.

```jsonc
{
  "type":     "ctrl_child_status",
  "childId":  "c_01HX...",
  "status":   "streaming",
  "previous": "idle",
  "at":       1716636890
}
```

### 7.5 `ctrl_child_renamed`

```jsonc
{
  "type":     "ctrl_child_renamed",
  "childId":  "c_01HX...",
  "name":     "afk-impl-v2",
  "previous": "afk-impl",
  "at":       1716636900
}
```

## 8. Errors

All command failures use the same envelope:

```jsonc
{
  "type":    "ctrl_response",
  "command": "ctrl_spawn",
  "id":      "3",
  "success": false,
  "error":   {
    "code":    "spawn_failed",
    "message": "pi exited immediately: bad model anthropic/claude-foo"
  }
}
```

Defined error codes:

| Code                    | Meaning                                                            |
|-------------------------|--------------------------------------------------------------------|
| `child_not_found`       | No child with the given `childId` exists.                          |
| `child_exited`          | Child has exited; only `ctrl_get`, `ctrl_get_recent`, `ctrl_forget`, `ctrl_resume` apply. |
| `child_in_grace`        | Equivalent state to `child_exited`; explicit for clarity in errors.|
| `child_shutting_down`   | Child is mid-graceful-shutdown; stdin is closed. Send commands rejected. |
| `not_resumable`         | Child is not in `exited` status; cannot resume.                    |
| `not_exited`            | `ctrl_forget` against a still-live child.                          |
| `session_file_missing`  | `ctrl_resume` cannot find the recorded session.jsonl.              |
| `backpressure`          | The child's command channel is full; client should retry.          |
| `invalid_args`          | Request fields failed validation.                                  |
| `spawn_failed`          | `pi` subprocess failed to start or exited immediately.             |
| `auth_required`         | TCP connection sent a non-`ctrl_auth` frame first, or sent a command other than `ctrl_user_create` on a bootstrap connection. |
| `auth_invalid`          | TCP auth token names no active user.                               |
| `not_found`             | Generic; e.g., `ctrl_resume` against unknown id.                   |
| `internal`              | Unexpected controller-side error. The message carries details **only for an authenticated connection**; on an unauthenticated (bootstrap) one it is a fixed string and the real error is logged server-side only — a pgx failure names the host, user and database. |
| `no_agent_db`           | `ctrl_conversation_*`: no agent database configured (`RAFIKI_DB` unset). |
| `payload_too_large`     | `ctrl_conversation_export`: transcript exceeds the maximum response frame size. |

## 9. Multi-client semantics

- **Sends.** Any client may `ctrl_send` to any child. Frames interleave at the
  child's command channel in arrival order at the controller. No exclusive
  control mechanism in v1.
- **Subscribes.** Any number of independent subscribers per child. Each
  receives events in publish order. A slow subscriber may drop events (see
  §11 on the Bus) but never backpressures the child or other subscribers.
- **Extension UI.** Pi's `extension_ui_request` events are forwarded to all
  matching subscribers. The first client to send a matching
  `extension_ui_response` (wrapped in `ctrl_send`) wins; pi's RPC mode drops
  subsequent responses by `id` mismatch. A child agent can therefore "ask
  its supervisor" by having an extension call `ctx.ui.confirm()` and a
  coordinator subscribed to the child can answer via `ctrl_send`.

## 10. State machine

Eight states. Transitions are driven by pi RPC events (forwarded by the
supervise goroutine) and controller lifecycle actions. The state machine is
maintained per-child by the supervise goroutine.

```
              ┌──────────┐
              │ spawning │
              └────┬─────┘
                   │ first response received (typically to get_state)
                   ▼
              ┌──────────┐    agent_start          ┌────────────┐
              │   idle   │ ──────────────────────► │ streaming  │
              └──────────┘ ◄──────────────────────  └─────┬──────┘
                   ▲       agent_end                      │
                   │                                      │ tool_execution_start
                   │                                      │ (activeTools 0→1)
                   │                                      ▼
                   │                                ┌────────────┐
                   │                                │ tool_running│
                   │                                └─────┬──────┘
                   │                                      │ tool_execution_end
                   │                                      │ (activeTools 1→0)
                   │                                      │
  modal (stack):                                          ▼
    compacting ─── on compaction_start, push; on compaction_end, pop.
    blocked_ui ─── on extension_ui_request (dialog method), push;
                   on matching extension_ui_response, pop.

              shutting_down ──── on ctrl_kill or interception. Closes stdin,
                                 then waits, escalates SIGTERM/SIGKILL.
                                 → exited on reap.

              exited ──── terminal except for ctrl_resume → spawning.
```

### 10.1 Transition table

| From                  | Trigger                                                     | To              |
|-----------------------|-------------------------------------------------------------|-----------------|
| (none)                | `ctrl_spawn` or `ctrl_resume`                               | `spawning`      |
| `spawning`            | First pi response received (initial `get_state`)            | `idle`          |
| `spawning`            | Pi process exits before any response                        | `exited`        |
| `spawning`            | 5s elapsed without response (warning only)                  | `spawning`      |
| `idle`                | `agent_start`                                               | `streaming`     |
| `streaming`           | `tool_execution_start`, activeTools 0→1                     | `tool_running`  |
| `tool_running`        | `tool_execution_start` (parallel), activeTools++            | `tool_running`  |
| `tool_running`        | `tool_execution_end`, activeTools 1→0                       | `streaming`     |
| `streaming`           | `agent_end`                                                 | `idle`          |
| `streaming` or `tool_running` or `idle` | `compaction_start` (push)                | `compacting`    |
| `compacting`          | `compaction_end` (pop)                                      | previous state  |
| `streaming` or `tool_running` | `extension_ui_request` (dialog only) (push)         | `blocked_ui`    |
| `blocked_ui`          | matching `extension_ui_response` forwarded by controller (pop) | previous state |
| any except `exited` and `shutting_down` | `ctrl_kill` or interception starts        | `shutting_down` |
| `shutting_down`       | Pi process exits (reap)                                     | `exited`        |
| `shutting_down`       | `ctrl_send` from a client                                   | rejected (`child_shutting_down`) |
| any non-exited        | Pi process exits unexpectedly                               | `exited`        |
| `exited`              | `ctrl_resume`                                               | `spawning`      |

`ctrl_child_status` is emitted on every transition, carrying both the new
and previous state.

### 10.2 Informational events (no transition)

These pi events update counters on the Session entry but do not change
status. They are observable via `ctrl_get` / `ctrl_list`.

| Pi event              | Effect                                                            |
|-----------------------|-------------------------------------------------------------------|
| `extension_error`     | `extensionErrors++`                                               |
| `auto_retry_start`    | `autoRetries++`; `lastRetryError = ev.errorMessage`               |
| `auto_retry_end` (failure) | `lastRetryFinal = ev.finalError`                             |
| `queue_update`        | Cached for `ctrl_get` (`pendingSteer`, `pendingFollowUp` counts). |

### 10.3 Modal stack

`compacting` and `blocked_ui` are *modal* states implemented with a small
state stack on the supervise goroutine. Push on entry, pop on the matching
exit event. This handles arbitrary nesting (e.g., extension UI dialog
during compaction during tool execution) without per-pair restoration
logic.

Defensive: if `compaction_end` or `extension_ui_response` arrives with an
empty stack (lost prior event, controller restart mid-flight, etc.), the
current state is preserved and a warning is logged. The state machine does
not get stuck in modal forever.

### 10.4 Notes on individual transitions

- **`tool_running` requires a counter** because pi supports parallel tool
  execution. Three concurrent tools means three `tool_execution_start`
  events back-to-back. State transitions from `streaming` to `tool_running`
  on the *first* start; back to `streaming` on the *last* end. Reset
  `activeTools = 0` on `agent_end` as a sanity guard.
- **Spawning kickstart**: pi in `--mode rpc` doesn't emit anything until
  prompted. Immediately after `cmd.Start()` succeeds, the supervise
  goroutine sends `{"type":"get_state"}` to elicit the first response and
  populate initial metadata (sessionId, sessionFile, sessionName, model).
  When the response arrives, transition to `idle`.
- **`blocked_ui` only for dialog methods**: pi's `extension_ui_request`
  covers both blocking dialogs (`select`, `confirm`, `input`, `editor`) and
  fire-and-forget notifications (`notify`, `setStatus`, `setWidget`,
  `setTitle`, `set_editor_text`). Only dialog methods trigger the modal
  transition. The supervise goroutine tracks pending dialog `id`s in a
  capped map (default 64 entries) to match responses to entries.
- **Compaction triggering**: pi initiates compaction manually (via
  `compact` command) or automatically (threshold / overflow). The
  controller does not initiate compaction itself, only observes the events.
- **Interception goes through `shutting_down`**: the intercepted path for
  `new_session`/`switch_session` is `<current> → shutting_down → exited →
  spawning → idle`, with the synthesized `response` event emitted on entry
  to `spawning`. From the client's perspective, this matches pi's native
  immediate `success: true` response timing.
- **Metadata sniffing is independent of state**: the supervise goroutine
  inspects responses to `get_state`, `get_session_stats`, `set_session_name`,
  `set_model`, `cycle_model` and updates the Session entry's mutable fields
  (`sessionId`, `sessionFile`, `sessionName`, `model`). These updates emit
  `ctrl_child_renamed` or equivalent events when fields actually change.

## 11. Replay and persistence

### 11.1 In-memory buffers per child

| Buffer | Default size | Source                              |
|--------|--------------|-------------------------------------|
| `out` ring | 5000 events or 64 MB (LRU) | pi stdout events |
| `in` buffer | 16 MB (drop-oldest on overflow) | controller-to-pi commands |
| `err` buffer | 4 MB (drop-oldest) | pi stderr |

Defaults tunable via env vars `PI_CONTROLLER_RING_EVENTS`,
`PI_CONTROLLER_RING_BYTES`, etc. Not per-spawn configurable.

`ctrl_get_recent` (§6.11) reads from the `out` ring. The other buffers are
exposed only via the on-disk dump.

### 11.2 The Bus

Each child owns a Bus that fans events out to its subscribers. Subscribers
each get a bounded channel (default 256 events). On a full channel, the Bus
drops the event for that subscriber and notes the gap. Subscribers are
expected to call `ctrl_get_recent` if they need to catch up after a gap.

The supervise goroutine is the only producer. It publishes to the Bus and
appends to the ring inside a single critical section, so per-child event
order is preserved across subscribers and replay.

### 11.3 Disk persistence

Modes (global config):

| Mode           | Behavior                                                              |
|----------------|-----------------------------------------------------------------------|
| `on_exit`      | Always dump all three buffers on child exit. **Default for dev.**     |
| `on_failure`   | Dump only on `exitCode != 0`, signaled, or last status `error`.        |
| `never`        | Discard on exit; in-memory only.                                      |

Layout per dumped child:

```
~/.pi/run/logs/<childId>/
  in.jsonl.gz    commands sent to pi's stdin
  out.jsonl.gz   events received from pi's stdout
  err.log.gz     stderr
  meta.json      spawn args, timing, exit code, signal
```

Format details:
- Gzip level 6, plain `.gz`. `zcat`, `zless`, `zgrep`, `rg -z` all work.
- All three streams compressed for consistency, even tiny ones.
- Written once at exit (sequential write, no flushing complexity).

Persistence is independent of `ctrl_forget`. Forgetting a child removes
its in-memory entry but never deletes disk artifacts.

### 11.4 Exit grace window

Default 7 days. Exited children stay in the in-memory store with status
`exited` so:

- Their replay ring is still queryable via `ctrl_get_recent`.
- They can be `ctrl_resume`d explicitly.
- A background sweeper removes entries older than the window.

Manual cleanup via `ctrl_forget` and `ctrl_forget_all_exited`.

### 11.5 State records (resume support)

For each known child, the controller maintains a JSON record at
`~/.pi/run/state/<childId>.json`:

```jsonc
{
  "childId":           "c_01HX...",
  "name":              "afk-impl",
  "cwd":               "/Users/.../dev",

  // Model / auth (apiKey is NEVER persisted; always null on disk)
  "provider":          "anthropic",
  "model":             "claude-sonnet-4",
  "thinking":          "medium",
  "apiKey":            null,

  // Session
  "sessionFile":       "/Users/.../session-abc.jsonl",
  "sessionId":         "abc123",
  "sessionDir":        null,
  "noSession":         false,

  // Tool / extension / skill scoping (full snapshot of spawn options)
  "tools":             null,
  "noTools":           false,
  "noBuiltinTools":    false,
  "extensions":        [],
  "noExtensions":      false,
  "skills":            [],
  "noSkills":          false,
  "promptTemplates":   [],
  "noPromptTemplates": false,
  "themes":            [],
  "noThemes":          false,
  "noContextFiles":    false,

  // System prompt
  "systemPrompt":      null,
  "appendSystemPrompt":null,

  // Process control (env is NOT persisted; controller re-inherits at resume)
  "piBinary":          null,
  "extraArgs":         [],

  // Runtime metadata
  "spawnedAt":         1716636789,
  "lastSeenAlive":     1716640000,
  "pid":               12345,
  "lastStatus":        "streaming"
}
```

**Persistence policy for security-sensitive fields:**

- `apiKey` is always `null` on disk. If a child was originally spawned with a
  per-call API key, `ctrl_resume` requires re-supplying it (see §6.4).
- `env` (the spawn-time merge map) is **not** persisted. On `ctrl_resume`, the
  controller's current environment is re-inherited; the original ad-hoc
  overrides do not survive. Callers that need stable env should set vars in
  the controller's environment rather than via per-spawn `env`.

Writes use atomic-rename (write to a tempfile, fsync, rename) to avoid
half-written records on crash.

Lifecycle:

- Written on spawn.
- `lastSeenAlive` is bumped periodically (on every event, batched at most
  once per N seconds).
- `sessionFile` is rewritten when pi changes session (`/new`, `/resume`,
  `/fork` in pi). The controller detects this via pi events that report
  the new `sessionFile`.
- `lastStatus` is rewritten on status transition.
- Deleted on `ctrl_forget` and when the grace window expires.

On controller startup:

1. Scan `~/.pi/run/state/*.json`.
2. For each, attempt `kill(pid, 0)`. If alive, SIGTERM/SIGKILL the orphan
   (we cannot reattach to dead pipes).
3. Load each record into the store with status `exited`.
4. **Do not auto-resume.** Resume is always explicit (`ctrl_resume`).

Auto-resume policy is the responsibility of an upstream coordinator, not
this layer.

## 12. CLI

A `pi-ctl` binary (Go + Cobra) provides thin command-line access to every
verb. Subcommands map 1:1:

| Subcommand                                | Verb(s) |
|-------------------------------------------|---------|
| `pi-ctl list [--status ...] [--json]`     | `ctrl_list` |
| `pi-ctl spawn NAME [--cwd ...] [--model ...] [--resume PATH]` | `ctrl_spawn` |
| `pi-ctl resume <id\|name>`                | `ctrl_resume` |
| `pi-ctl kill <id\|name> [--timeout 5s]`   | `ctrl_kill` |
| `pi-ctl forget <id\|name>` / `--all-exited` | `ctrl_forget` / `ctrl_forget_all_exited` |
| `pi-ctl send <id\|name> TYPE [--arg k=v ...] [--stdin]` | `ctrl_send` |
| `pi-ctl recent <id\|name> [--since ...] [--include ...]` | `ctrl_get_recent` |
| `pi-ctl tail <id\|name> [--filter PROFILE]` | `ctrl_subscribe` |
| `pi-ctl search QUERY`                      | `ctrl_search` |
| `pi-ctl status`                            | `ctrl_status` |
| `pi-ctl logs <id\|name> [--cat]`           | (reads `~/.pi/run/logs/<id>/...`) |

Identifier resolution: childId, name, or fuzzy-prefix. Ambiguous prefixes
are errors. Tab completion via Cobra's built-in mechanism, with dynamic
suggestions backed by `ctrl_list`.

Rendering: `pi-ctl tail` renders a minimum-viable event view (turn-level
summaries, tool calls one-line, divider on `agent_end`). It is deliberately
plain; the polished interactive client (`pi-attach`) is a separate concern.

A `~/.pi/run/active` file holds "the last child id touched" so `pi-ctl tail`
with no argument picks the obvious target.

## 13. Defaults

| Setting              | Default                                                    |
|----------------------|------------------------------------------------------------|
| Controller socket    | `~/.pi/run/controller.sock` (mode 0600, dir 0700)          |
| Controller TCP       | Off                                                        |
| `out` ring           | 5000 events / 64 MB, LRU                                   |
| `in` buffer          | 16 MB                                                      |
| `err` buffer         | 4 MB                                                       |
| Persistence mode     | `on_exit` (dev) → `on_failure` (later)                     |
| Logs                 | `~/.pi/run/logs/<childId>/`, gzip level 6                  |
| State records        | `~/.pi/run/state/<childId>.json`                           |
| Grace window         | 7 days                                                     |
| Bus per-sub buffer   | 256 events                                                 |
| Shutdown timeout     | 180s (3 min) — graceful close of stdin → wait              |
| Kill timeout         | 30s — after SIGTERM, before SIGKILL                        |
| Spawning kickstart timeout | 5s without first response (warning only, stays spawning) |
| Dialog UI pending cap | 64 in-flight dialog request ids per child                 |
| Pi binary discovery  | `piBinary` arg → `$PI_BINARY` → `exec.LookPath("pi")`       |
| Environment          | Full inherit (`os.Environ()`); merge spawn-time `env` map  |
| Reserved env vars    | `PI_CONTROLLER_CHILD_ID`, `PI_CONTROLLER_SOCKET` (injected) |
| Controller log       | `~/.pi/run/controller.log`                                 |
| Token (TCP)          | `~/.pi/run/controller.token` (mode 0600)                   |

## 14. Deferred (post-v1)

- Exclusive-control semantics (`ctrl_acquire` / `ctrl_release`).
- Historical content search across all session.jsonl files (use `rg`).
- Cross-host transport (use an SSH tunnel over UDS).
- Persistent index for fast content search (FTS5 or similar).
- TLS on TCP.
- Live disk streaming during runtime (the format is forward-compatible).
- Auto-resume on controller startup.
- Per-spawn buffer/ring sizing.
- Rich rendering in `pi-ctl tail` (deferred to `pi-attach`).
- `pi-attach` (the thin-TUI client) — separate document.
- Native interception of `fork` / `clone` (requires Go-side session-jsonl
  tree manipulation). v1 leaves these as pass-through; clients refresh
  metadata via `ctrl_send { type: "get_state" }` if needed.
- Periodic background `get_state` poll to refresh metadata without explicit
  client action.
- Per-controller config flag to disable interception for users who require
  pi's native `session_start { reason: "new" }` extension lifecycle
  semantics.

## 15. Executor management (ctrl_executor_*)

Commands for managing remote executor enrollment, labels, and state.
Unregistered commands receive an error response with code `invalid_args`.

### 15.1 `ctrl_executor_enroll`

Mint a one-time enrollment token. The token is returned once and not persisted
server-side in plaintext — only its hash is stored.

**Request**
```jsonc
{
  "type": "ctrl_executor_enroll",
  "name": "my-laptop",                      // names the MACHINE the executor will run on
  "labels": {"rafiki/env": "work"},        // trust labels bound to the executor row
  "roots": ["/workspace"],                  // accessible root paths
  "isolation": "none",                      // none | container | vm
  "workspaceMode": "pinned",                // ephemeral | pinned
  "admits": "",                             // executor-side admission selector
  "ttlSeconds": 3600                        // token lifetime, required
}
```

**`owner` and `machine` are written by the DAEMON, and a request carrying
either as a label is REFUSED** with `ERR_INVALID_ARGS` — not silently
overwritten, so a caller learns their selector will not mean what they wrote.

- `owner` comes from the connection, by the same rule as `ctrl_executor_session`
  (§15.6): `conn.Identity().Username` on an authenticated TCP/TLS connection,
  and the daemon's own OS user on a local UDS connection. Both halves of
  executor selection match on it — an executor's `admits` selector and a
  client's own binding — so a caller able to state it could claim another
  operator's machines.
- `machine` comes from `name`, validated daemon-side (letters, digits, `-`, `_`,
  `.`, at most 63 characters). A comma or an equals sign would silently reparse
  the `owner=…,machine=…` selector into a different one, which for a value
  deciding where a child lands is a confinement bug, not a formatting one.
  `name` is optional; omitting it leaves the row without a `machine` label,
  which is right for a fleet executor selected purely by `env=prod`.

`name` names the machine the executor will RUN on, which is **not** necessarily
the one that minted the token: an enrollment token is routinely carried
elsewhere and handed to the executor as `--enroll-token`. Run `rafiki executor
name` on that box to see what to pass.

`(owner, machine)` is unique among rows carrying a `machine`, but **this verb
does not detect a collision** — a token lives in `executor_enrollment_token`,
not in `executors`, so two tokens minted with the same `--name` both succeed
here. The index fires later, when the executor REDEEMS its token and the row is
actually written. That failure reaches the executor as a *terminal*
`executor_hello` error (`retryable:false` — see the executor protocol
reference), so it stops rather than reconnecting against a name it can never
claim. `ctrl_executor_create` (§15.7), which writes the row immediately, does
answer the collision inline with `ERR_INVALID_ARGS`.

**Response** (success)
```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_executor_enroll",
  "success": true,
  "data": {"token": "<one-time bearer token>"}
}
```

### 15.2 `ctrl_executor_list`

List enrolled executors, optionally filtered by a label selector.

**Request**
```jsonc
{
  "type": "ctrl_executor_list",
  "selector": "env=work",                   // optional label selector filter
  "limit": 50                               // unspecified/zero defaults to 50; > 500 clamps to 500
}
```

Two branches, not one: `limit <= 0` defaults to 50 (not the ceiling); `limit >
500` clamps down to 500. `ctrl_user_list` (§16.2) clamps differently — zero,
negative *and* oversized all collapse to the same 500 — so do not assume the
two `*_list` verbs share a convention.

The response is the union of the executor rows and the live pool, not the rows
alone. A transient executor (`kind=session`) has no row, so it appears only
from the pool; a durable executor that is currently connected appears once,
with `connected` and `connectedAt` filled in from the pool.

**Response** (success)
```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_executor_list",
  "success": true,
  "data": {
    "executors": [
      {
        "id": "abc123...",
        // The executor's NAME is the `machine` label -- there is no separate
        // display_name field (0020 dropped the column). `(owner, machine)` is
        // unique, so the pair names exactly one executor.
        "labels": {"env": "work", "owner": "brent", "machine": "my-laptop"},
        "enabled": true,
        "workspaceMode": "pinned",
        "admits": "",
        "enrolledAt": "2026-01-01T00:00:00Z"
      }
    ]
  }
}
```

### 15.3 `ctrl_executor_label`

Set or remove labels on an executor's database row. Changes take effect on the
executor's next connection without requiring a restart.

This is how `owner` and `machine` are changed after the fact — they cannot be
set through `ctrl_executor_enroll` or `ctrl_executor_create` (§15.1), which
write them from the connection and from `name`, but they are ordinary labels on
the row once written. Setting `machine` to a name this owner already uses is
refused with `ERR_INVALID_ARGS` and the row is left untouched: `(owner,
machine)` names exactly one executor.

`executorId` may be the full row id or any unique trailing fragment of at
least four characters. Fragments match by SUFFIX, not prefix: ids are UUIDv7s
whose leading bits are a timestamp, so rows minted in the same window share
their front and only the tail distinguishes them — this is what makes the
short form `rafiki executor list` displays usable verbatim here. An exact id
always wins. A fragment matching several rows fails with `ERR_INVALID_ARGS`,
naming the rows it matched; one matching none fails with `ERR_NOT_FOUND`.

**Request**
```jsonc
{
  "type": "ctrl_executor_label",
  "executorId": "abc123...",
  "set": {"env": "home"},                   // keys to set
  "remove": ["legacy"]                      // keys to remove
}
```

**Response** (success) — the full executor object post-mutation.

### 15.4 `ctrl_executor_disable` / `ctrl_executor_enable`

Disable or re-enable an executor. A disabled executor's credential fails
authentication on its next connection. `executorId` follows the same
exact-or-unique-trailing-fragment rule as `ctrl_executor_label` (§15.3).

**Request**
```jsonc
{
  "type": "ctrl_executor_disable",
  "executorId": "abc123..."
}
```

**Response** (success) — `data` is omitted (null).

### 15.5 `ctrl_executor_delete`

Permanently remove an executor row. Unlike disable, this cannot be undone —
there is no tombstone for executors, because nothing else keys off an
executor id for historical resolution (the enrollment token table's
`executor_id` is `ON DELETE SET NULL`). `executorId` follows the same
exact-or-unique-trailing-fragment rule as `ctrl_executor_label` (§15.3).

Deleting a currently-connected executor's row evicts it from the live pool
within one health interval, the same as disabling it.

**Request**
```jsonc
{
  "type": "ctrl_executor_delete",
  "executorId": "abc123..."
}
```

**Response** (success) — `data` is omitted (null).

### 15.6 `ctrl_executor_session`

Let an authenticated client get an executor for its own machine, so the
operator's filesystem can be the workspace. The daemon writes every field that
gates access — owner, isolation, workspace_mode, admits — from the connection;
the request carries only non-gating fields.

Two answers, one selector. When a durable executor already covers this
machine and owner (matched on Labels, not SelfReported), the response returns
its `executorId` with `runLocal:false` and no ticket — the client targets that
executor and starts nothing of its own. That executor outlives the client,
which is what keeps an agent working after the operator detaches.

Otherwise the daemon mints a one-shot session ticket. The client starts
an executor, connects with the ticket, and the executor lives exactly as long
as this control connection — no database row, no permanent credential. The
selector is `owner=<user>,machine=<name>` in both cases, so a child can move
between a durable executor and a transient one without its stored selector
ever changing.

**Owner resolution.** On an authenticated TCP/TLS connection the owner is
`conn.Identity().Username`. On a local UDS connection — which carries no
authenticated identity — the owner is the daemon's own OS user
(`os/user.Current().Username`). The control socket is created under a `0177`
umask and owned by that user, so anyone who can open it already IS that user.
The request has no username field and must never grow one.

**Request**
```jsonc
{
  "type": "ctrl_executor_session",
  "name": "my-laptop",              // this machine's operator-chosen executor name
  "roots": ["/home/brent/src"]      // descriptive only — nothing enforces them
}
```

`name` comes from `paths.MachineName()` on the client: `$RAFIKI_EXECUTOR_NAME`,
then the name written by `rafiki executor name <name>`, and a request with
neither is refused with `ERR_INVALID_ARGS`. It is matched against the
`machine` trust label an operator wrote at mint time (§15.1) — never against
`SelfReported`, which is only the executor's own account of itself and cannot
gate anything. Like every other field on this request, `name` does not gate
access on its own: `owner` still comes from the connection, so a client naming
it can only ever narrow which of its OWN executors it reaches.

**Response** (success)
```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_executor_session",
  "success": true,
  "data": {
    "executorId": "abc123...",     // always populated — the executor to target
    "runLocal": true,              // false when a durable executor already covers this machine
    "ticket": "...",               // one-shot session ticket — empty when runLocal is false
    "selector": "owner=brent,machine=my-laptop"  // spawn selector for both durable and transient
  }
}
```

### 15.7 `ctrl_executor_create`

Mint an executor row and its durable credential in one step, with no enrollment
handshake — the STATELESS path, for a deployment with no durable local storage.
The credential is returned once and only its hash is stored.

The trade runs the other way from enrollment (§15.1): the operator handles a
long-lived secret, and a theft of it is silent, where a stolen one-time
enrollment token announces itself by being consumed. Prefer `ctrl_executor_enroll`
where the machine can keep a file.

**Request**
```jsonc
{
  "type": "ctrl_executor_create",
  "name": "prod-runner-1",                  // names the MACHINE the executor will run on
  "labels": {"env": "prod"},                // trust labels written onto the row
  "roots": ["/workspace"],                  // accessible root paths
  "isolation": "none",                      // none | container
  "workspaceMode": "pinned",                // ephemeral | pinned
  "admits": ""                              // executor-side admission selector
}
```

`name`, `owner` and `machine` follow exactly the rules in §15.1: `owner` is
written from the connection, `machine` from `name`, and a request carrying
either as a label is refused with `ERR_INVALID_ARGS`. So is a `name` that
collides with an existing executor of the same owner.

**Response** (success)
```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_executor_create",
  "success": true,
  "data": {
    "executorId": "abc123...",
    "credential": "<long-lived credential — shown once>"
  }
}
```

## 16. User management (ctrl_user_*)

Commands for managing rafiki user identities. These are ordinary `ctrl_*`
commands, subject to the same connection auth as everything else (§2.2): UDS
is always trusted, a TCP/TLS connection needs a valid `ctrl_auth` identity —
**except** `ctrl_user_create`, which is also the one command a *bootstrap*
connection may send with no identity at all, per §2.2's bootstrap rule. All
three verbs return `no_agent_db` when the daemon has no database pool
(`RAFIKI_DB` unset) — there is no in-memory fallback for identity.

### 16.1 `ctrl_user_create`

Mint a user row and its bearer token.

**Request**
```jsonc
{
  "type": "ctrl_user_create",
  "username": "brent"
}
```

**Response** (success)
```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_user_create",
  "success": true,
  "data": {
    "id": "...",
    "username": "brent",
    "token": "rfk_...",           // PLAINTEXT token — shown once, never again
    "created_at": "2026-08-18T00:00:00Z"
  }
}
```

A `username` already held by an active (non-tombstoned) user returns
`invalid_args`.

### 16.2 `ctrl_user_list`

Enumerate users. Tokens are never returned.

**Request**
```jsonc
{
  "type": "ctrl_user_list",
  "include_deleted": false,
  "limit": 900                    // > 500: clamped down to 500
}
```

`limit` collapses to 500 whenever it is **zero, negative, or greater than
500** — one clamp, no default-vs-ceiling distinction. This differs from
`ctrl_executor_list` (§15.2), which treats "not specified" (`<= 0`) and "too
large" (`> 500`) as two separate branches, defaulting the former to 50 rather
than 500. Two neighbouring `*_list` verbs, two different conventions for what
"no limit given" means — do not infer one from the other.

**Response** (success)
```jsonc
{
  "type": "ctrl_response",
  "command": "ctrl_user_list",
  "success": true,
  "data": {
    "users": [
      {"id": "...", "username": "brent", "created_at": "..."}
    ]
  }
}
```

### 16.3 `ctrl_user_rm`

Tombstone a user: its token stops authenticating, but history keeps
resolving the username.

**Request**
```jsonc
{
  "type": "ctrl_user_rm",
  "username": "brent"
}
```

**Response** (success) — `data` is omitted (null). An unknown or already
tombstoned username returns `not_found`.

## 17. Reference: pi RPC pass-through

For completeness, the controller is transparent for routing and content of
pi's `--mode rpc` protocol, with the two intercepted commands documented in
§5.1. Pi's RPC commands flow inside `ctrl_send.frame`; pi's events flow
inside `ctrl_event.event`. The authoritative documentation is pi's own
`rpc.md`.

Notable pass-through commands:

- `prompt`, `steer`, `follow_up`, `abort`
- `compact`, `abort_retry`
- `set_model`, `cycle_model`, `get_available_models`
- `set_thinking_level`, `cycle_thinking_level`
- `set_session_name`
- `set_steering_mode`, `set_follow_up_mode`
- `set_auto_compaction`, `set_auto_retry`
- `fork`, `clone`
- `get_state`, `get_messages`, `get_session_stats`,
  `get_last_assistant_text`, `get_fork_messages`, `get_commands`
- `export_html`
- `bash`, `abort_bash`
- `extension_ui_response`

Intercepted commands (see §5.1): `new_session`, `switch_session`.

Notable events (all pass-through):

- `agent_start`, `agent_end`
- `turn_start`, `turn_end`
- `message_start`, `message_update`, `message_end`
- `tool_execution_start`, `tool_execution_update`, `tool_execution_end`
- `queue_update`
- `compaction_start`, `compaction_end`
- `auto_retry_start`, `auto_retry_end`
- `extension_error`
- `extension_ui_request`

If pi adds a new pass-through command or event, no controller change is
required. Adding to the intercepted set requires a protocol version bump.
