# pic create / pic attach — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Let an interactive `pi` session register itself with the pi-controller daemon, so users get the normal pi TUI experience while the daemon can also observe/steer that session from outside. Two pic subcommands:

- **`pic create <name>`** — start a fresh pi TUI locally, registered with the daemon as a controlled child.
- **`pic attach <name>`** — connect to an existing registered child (open its TUI in this terminal).

**Architecture:** A new pi extension (`pi-controller-register`) bootstraps each pi session, dials the controller's UDS, and tells it "here I am — childId=X, sessionFile=Y, etc." The controller learns about a new kind of child: **registered** (vs the existing **owned** kind). Registered children are managed by the user's terminal — the controller can't kill them or capture their lifecycle, but it can:

- List them alongside owned children.
- Forward `ctrl_send` frames into the running pi via the extension (extension calls `pi.sendUserMessage` to inject the prompt).
- Subscribe to their events (extension forwards all pi events via the controller socket → controller fans out to subscribers).

For `pic attach`, the second-and-later "attacher" doesn't run a TUI at all — only the original `pic create` terminal does. Attachers see a streamed tail-style view (same as `pic tail`). This is the **honest v1 trade-off**: a true second TUI rendering the same session would need a real thin-client TUI implementation (the pi-attach Big Project we deferred). What we *do* offer here is enough for the AFK scenario: kick off an interactive session, walk away, observe/steer from anywhere.

**Tech Stack:** TypeScript for the pi extension; Go for the daemon-side and pic changes.

**Spec impact:** Adds `ctrl_register_self` verb and `registered` boolean on child summaries to `tasks/pi-controller-protocol.md`.

---

## Scope discipline

What this plan does:

- ✅ `pic create <name>` — spawn local `pi` with the register extension preloaded; the session registers itself with the daemon.
- ✅ `pic attach <name>` — currently equivalent to `pic tail`; documented as the "v1 attach" with a real thin-client TUI deferred.
- ✅ Daemon learns to handle registered children: list them, route ctrl_send to them, broadcast their events.
- ✅ pi extension that does the registration + event forwarding + command injection.

What this plan does NOT do:

- ❌ Full thin-client TUI for multiple attachers (deferred; would need Go-side TUI implementation).
- ❌ Detach + reattach support beyond "pic tail closes and reopens" — the original `pic create` terminal still owns the session; closing it terminates pi.
- ❌ Migration of an already-running pi to register itself after the fact (require restart with the extension loaded).

---

## Repo layout additions

```
~/home/pi-controller/
├── cmd/pic/
│   ├── cmd_create.go            # new
│   └── cmd_attach.go            # new (thin wrapper around tail for v1)
├── internal/protocol/
│   └── types.go                 # add ctrl_register_self request/response, registered flag
├── cmd/pi-controller/
│   └── controller.go            # new RegisterSelf method on Controller; registered-children plumbing
├── extensions/                  # new top-level dir for shipped extensions
│   └── controller-register/
│       ├── package.json
│       ├── index.ts
│       └── README.md
└── tasks/
    └── 2026-05-25-implementation-plan-pic-attach.md   # this file
```

The extension is a TypeScript package the user can install per-session via `pi -e <path>` or install globally via `pi install <path>`. `pic create` will pass `-e <abs path to extensions/controller-register/index.ts>` automatically.

---

## Wire protocol additions

### `ctrl_register_self` (client → controller)

Sent by the extension immediately after pi's `session_start`.

```jsonc
{
  "type":        "ctrl_register_self",
  "id":          "req-1",
  "pid":         12345,
  "cwd":         "/Users/brent/some-project",
  "name":        "afk-impl",          // optional; from pic create --name
  "sessionId":   "abc123",
  "sessionFile": "/Users/.../session.jsonl",
  "model":       "anthropic/claude-sonnet-4",
  "thinking":    "medium"
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_register_self", "id": "req-1",
  "success": true,
  "data": { "childId": "c_01HX..." }
}
```

The controller assigns a fresh `childId` (using the same ULID scheme as for owned children).

### Per-child flag: `registered`

`ChildSummary` (in `protocol.types.go`) gains a `registered bool` field. `ctrl_list` / `ctrl_get` responses include it. True for register-self children; false (or absent) for daemon-spawned children.

### Behavior differences for registered children

- **`ctrl_kill`**: refused. Returns `child_not_owned` error code (new). The user kills it by closing their terminal.
- **`ctrl_resume`**: refused for the same reason.
- **`ctrl_send`**: the controller writes the frame to the registered child's connection (the extension's UDS connection). The extension parses it; for `prompt`/`steer`/`follow_up`, calls `pi.sendUserMessage(...)`. For others, attempts a best-effort mapping (or returns an error event).
- **`ctrl_subscribe`**: works exactly as for owned children. The extension forwards every pi event to the controller; the controller's bus fans out.
- **`ctrl_forget`**: removes from the in-memory store. Doesn't affect the still-running pi session.
- **State record**: NOT persisted to disk (no crash-recovery for registered children — they're tied to the user's terminal).
- **Log dumps**: NOT written on exit (no log dumper invoked for registered children).
- **Grace window**: registered children disappear immediately when their extension's UDS connection drops; no grace.

### New error code

`child_not_owned` — operation requires an owned (daemon-spawned) child; this is a registered one.

---

## Tasks

### Task 1: Protocol additions

**Files:**
- Modify: `internal/protocol/types.go` — add `RegisterSelfRequest`, `RegisterSelfResponseData`, `TypeCtrlRegisterSelf`, `Registered bool` field on `ChildSummary`, `ErrChildNotOwned` constant.
- Modify: `internal/protocol/types_test.go` — round-trip test for `RegisterSelfRequest`.
- Modify: `tasks/pi-controller-protocol.md` — document the new verb and the registered behavior. New §6.17 for `ctrl_register_self`; add `registered` flag to §6.1 ChildSummary; add `child_not_owned` to §8 error codes; new §11.6 "Registered children" subsection.

- [ ] **Step 1**: Add types + constants to `internal/protocol/types.go`.
- [ ] **Step 2**: Add round-trip test for `RegisterSelfRequest`.
- [ ] **Step 3**: Document in the spec — new sub-sections in protocol.md.
- [ ] **Step 4**: `make test -race` clean.
- [ ] **Step 5**: Commit: `protocol: add ctrl_register_self verb + registered flag for self-registered pi sessions`.

### Task 2: Daemon-side registered-child plumbing

**Files:**
- Modify: `internal/store/session.go` — add `Registered bool`, `RegisterConn` (a small interface for the connection that registered this child).
- Modify: `cmd/pi-controller/controller.go` — implement `RegisterSelf` method (creates Session marked registered, inserts to store, sets up event-relay subscription on the conn).
- Modify: `internal/server/dispatch.go` — handle `ctrl_register_self`; add `RegisterSelf` to the Controller interface.
- Modify: `cmd/pi-controller/controller.go` — refuse `ctrl_kill`/`ctrl_resume` for registered children with `child_not_owned`; route `ctrl_send` to the conn's writer instead of the child's cmdCh; treat connection-close as the "child exited" signal.
- Modify: `cmd/pi-controller/controller.go` — `ctrl_list` / `ctrl_get` populate the new `Registered` field.
- Modify: tests as needed.

This is the biggest task. Key design decision: how does the controller forward events from a registered child to subscribers?

The extension's UDS connection is bidirectional. The controller sees:
- Inbound: `ctrl_register_self` once, then a stream of `ctrl_event`-wrapped frames (the extension wraps each pi event before forwarding) AND `ctrl_response` frames for synchronous extension-handled pi commands.
- Outbound: `ctrl_send` frames the controller wants the extension to inject as user messages.

Actually simpler: the extension's outbound frames are already in `ctrl_event` envelope form (since the daemon expects that). The controller's per-child Bus accepts them as-is.

Connection death = registered child exited. The controller's `HandleClose` (added in the daemon's important-fixes) needs to additionally check if this connection was a registration source; if so, mark the child exited and emit `ctrl_child_exited`.

- [ ] Step 1: Add `Registered` field to Session + Snapshot, with deep-copy preservation.
- [ ] Step 2: Add `RegisterSelf(ctx, conn, req)` method to Controller. Creates Session, inserts, sets up the bus, returns the new childId. Subscribes to the conn's incoming frames as the event source.
- [ ] Step 3: Add `Controller.RegisterSelf` to the server Controller interface; implement handler in dispatch.
- [ ] Step 4: Modify `Kill`, `Resume` to return `child_not_owned` for registered.
- [ ] Step 5: Modify `Send` for registered children: instead of writing to `cmdCh`, write to the registering connection.
- [ ] Step 6: Connection-close handler now needs to detect registered-child ownership and mark exited.
- [ ] Step 7: Update `MarkExited` to handle registered (no ring snapshot since there's no Child object).
- [ ] Step 8: `ctrl_list` and `ctrl_get` data includes the registered flag.
- [ ] Step 9: Tests — unit tests for the new dispatch handler with a fake conn; integration test where a fake "registration client" calls ctrl_register_self and sends events.
- [ ] Step 10: Commit: `cmd/pi-controller: support registered (externally-managed) children`.

### Task 3: pi extension (controller-register)

**Files:**
- Create: `extensions/controller-register/package.json`
- Create: `extensions/controller-register/index.ts`
- Create: `extensions/controller-register/README.md`

The extension:

1. On `session_start`, dial the controller UDS (`~/.pi/run/controller.sock`).
2. Construct + send `ctrl_register_self` with metadata from `ctx.sessionManager`.
3. Receive the assigned `childId`.
4. Subscribe to every pi event via `pi.on("agent_start", ...)` etc.; for each, wrap in `ctrl_event` envelope (with the assigned childId) and write to the UDS.
5. Read incoming frames from the UDS. For `ctrl_send`-wrapped frames whose inner `type` is `prompt`/`steer`/`follow_up`, call `pi.sendUserMessage(message, options)`. For `abort`, call `ctx.abort()`. For others, ignore (or log).
6. On `session_shutdown`, close the UDS connection.

Pi extension API to reach for:
- `pi.on("session_start", ...)` — get the initial metadata via `ctx.sessionManager`.
- `pi.on("agent_start"|"agent_end"|...)` — every pi event the controller wants. Look at the daemon's existing subscription pattern; the extension needs to forward *every* event type the protocol §7 covers.
- `pi.sendUserMessage(content, options?)` — inject prompts.
- `ctx.abort()` — abort the agent.

The extension also needs to handle pi's session-replacement events (`session_start` with `reason: "new"|"resume"|"fork"`) by re-registering or updating the metadata on the controller. For v1: simplest path is to re-call `ctrl_register_self` on every session_start — the controller can recognize the conn and update in place (or assign a new childId if simpler).

For v1: each `session_start` registers fresh. The previous registration becomes orphaned and times out. Coordinator-coordinator can deal with the cleanup later.

Configuration: extension reads `PI_CONTROLLER_SOCKET` from env (or falls back to default path) — same convention as the Go client.

- [ ] Step 1: `package.json` with pi extension metadata.
- [ ] Step 2: `index.ts` skeleton — dial, register, subscribe to events.
- [ ] Step 3: Forward all relevant pi events as ctrl_event-wrapped JSONL frames.
- [ ] Step 4: Read incoming and dispatch to sendUserMessage / abort.
- [ ] Step 5: On session_shutdown, close cleanly.
- [ ] Step 6: README documenting install + behavior + caveats.
- [ ] Step 7: Manual smoke against the daemon.
- [ ] Step 8: Commit: `extensions/controller-register: pi extension that registers a session with pi-controller`.

### Task 4: pic create + pic attach commands

**Files:**
- Create: `cmd/pic/cmd_create.go`
- Create: `cmd/pic/cmd_attach.go`
- Modify: `cmd/pic/main.go` — wire them in.

`pic create <name>`:
- Determine the extension path (relative to the pic binary's repo? Or via env var `PI_CONTROLLER_EXTENSION_PATH`? Or a config file?).
- Build the pi argv: `pi --extension <path-to-controller-register> [--cwd ...] [other-flags...]`.
- Pass `PIC_REGISTER_NAME=<name>` and `PI_CONTROLLER_SOCKET=<path>` in the environment so the extension knows what name to use.
- `exec.Command(...).Run()` with the current process's stdio inherited — so pi takes over the terminal.
- After pi exits, pic exits.

`pic attach <name>`:
- For v1, this is essentially `pic tail <name>`. Document it as such. The tail subcommand already handles identifier resolution and event streaming.
- Maybe add a small "you are attaching (read-only)" header so the user knows they're in tail mode, not a real interactive session.

Flags for `pic create`:
- `--cwd PATH` (default: current dir)
- `--model MODEL`
- `--thinking LEVEL`
- `--no-session`
- (pass-through for any other pi flag via `--`)

- [ ] Step 1: `cmd_create.go` with argv assembly, env injection, exec passthrough.
- [ ] Step 2: `cmd_attach.go` aliasing to tail's behavior with a clarifying header.
- [ ] Step 3: Wire both into main.
- [ ] Step 4: Smoke: `pic create test --cwd /tmp` opens a real pi session that shows up in `pic list` (in a separate terminal).
- [ ] Step 5: Commit: `pic: create and attach subcommands`.

### Task 5: Integration tests

**Files:**
- Modify: `test/integration/cli_integration_test.go` (or a new file).

End-to-end test:
1. Boot daemon.
2. Launch a fake "registered child" — a Go test helper that mimics what the extension does (dial, register, send events, accept incoming).
3. Verify `pic list` shows it with `registered: true`.
4. Verify `pic kill <name>` returns `child_not_owned`.
5. Verify `pic send <name> '{...prompt...}'` reaches the fake registration target.
6. Disconnect; verify `pic list` shows the child gone (or marked exited immediately).

Hard part: this test doesn't exercise the actual TS extension. That's only smoke-testable manually. Document the manual smoke procedure in a comment.

- [ ] Step 1: Add a `registerSelf` helper in the test file.
- [ ] Step 2: 5 integration tests covering the above.
- [ ] Step 3: Manual smoke instructions documented.
- [ ] Step 4: Commit: `test/integration: end-to-end tests for registered children`.

---

## Verification before declaring done

- [ ] All tests pass with `-race`.
- [ ] Both binaries build clean.
- [ ] Manual smoke: launch `pic create demo --cwd /tmp` in one terminal, then in another terminal:
  - `pic list` shows `demo` with `registered: true`
  - `pic tail demo` streams pi events
  - `pic send demo '{"type":"prompt","message":"Hello"}'` makes the prompt appear in the first terminal's pi TUI
  - `pic kill demo` returns `child_not_owned`
  - Closing the first terminal makes `demo` disappear from `pic list`

---

## Known limitations carried into v1+

- `pic attach` is read-only (tail-style); no real second-terminal TUI.
- Closing the original `pic create` terminal kills the session — no detach + reattach.
- Registered children: no log dumps, no state-record persistence, no resume.
- Per-child subscriber leak when the registering connection dies — should be cleaned up by the existing global-subscriber-close hook, but registered-child-specific cleanup needs verification.
- Concurrent `pic create` calls with the same name will spawn two separate registered children both named the same — store doesn't enforce name uniqueness (which is correct per the protocol, but worth noting for the UX).
