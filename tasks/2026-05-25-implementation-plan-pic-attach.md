# pic create / pic attach — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Let users get the native pi TUI experience driving a pi-controller-managed child. Two new pic subcommands:

- **`pic create <name> [pi-flags...]`** — spawn a new daemon-managed child via the controller, then attach the local TUI. With `--detached`, skip the attach and just return after spawn.
- **`pic attach <id|name>`** — open the pi TUI driving an existing daemon-managed child.

**Architecture:** Reuse pi's existing `InteractiveMode` TUI as-is. Replace only its `AgentSessionRuntime` dependency with a remote implementation (`RemoteAgentSessionRuntime`) that proxies all calls to the pi-controller daemon over UDS. The result: identical to native pi rendering (markdown, model picker, keybindings, extension UI dialogs), but the agent loop runs in the daemon-owned pi child instead of in-process.

There is only one kind of child — daemon-spawned. No "registered" concept. The TUI is a thin client; the daemon's pi child is the source of truth.

**Tech Stack:** TypeScript with `bun --compile` for a static binary (mirrors how pi itself ships). Depends on `@earendil-works/pi-coding-agent` for `InteractiveMode` + types. New shared Go code: minimal additions to pic for `create` / `attach`.

**Spec impact:** No changes to the wire protocol. All ops use existing verbs (`ctrl_spawn`, `ctrl_get`, `ctrl_subscribe`, `ctrl_send`).

---

## Why this is tractable

From inspecting the installed pi package:

- `cmd/modes/interactive/interactive-mode.d.ts` header literally says: *"Handles TUI rendering and user interaction, delegating business logic to AgentSession."*
- `InteractiveMode` constructor signature: `constructor(runtimeHost: AgentSessionRuntime, options?: InteractiveModeOptions)`.
- `AgentSessionRuntime` is documented and publicly exported from `@earendil-works/pi-coding-agent`.

The TUI is designed for runtime injection. We don't fork, vendor, or modify it — we construct it with a runtime that happens to be remote.

## The proxy boundary

`AgentSessionRuntime` exposes ~5 lifecycle methods (switchSession / newSession / fork / importFromJsonl / dispose) plus the underlying `AgentSession` (~30 methods + ~10 events) via the `session` getter.

`AgentSession` does session-management work (message store, queue modes, event firing) on top of the actual `Agent`. The cleanest split: implement the *whole* `AgentSessionRuntime` + `AgentSession` shape as remote shims. They forward operations to the daemon and synthesize the expected events when responses arrive.

What stays local:
- `SessionManager` (read-only view of the jsonl the daemon's pi child is writing) — for the message list display.
- `SettingsManager` (local config files).
- `ModelRegistry` (local config files).

What goes remote:
- `agent.prompt(...)`, `agent.steer(...)`, `agent.abort()`, etc. → ctrl_send to daemon.
- All `AgentSession` event emissions (`agent_start`, `agent_end`, `turn_end`, `message_update`, etc.) → translated from incoming ctrl_event frames.
- `AgentSession.executeBash(...)`, `setModel(...)`, `setThinkingLevel(...)`, etc. → ctrl_send.
- Extension UI requests come in as ctrl_event frames; responses go out as ctrl_send.

The `SessionManager` instance the remote runtime hands the TUI is constructed locally from the session.jsonl file path the daemon reports. Pi's `SessionManager` already supports being constructed against an existing file; we use that path, read-only, and re-read on changes (file watcher) to pick up new messages the daemon's pi appended.

---

## Scope discipline

What this plan does:

- ✅ Build a `pic-attach` binary (bun-compiled TS) that opens the pi TUI driving a daemon-managed child.
- ✅ Add `pic create` (spawn + auto-attach with `--detached` flag) and `pic attach` (attach to existing).
- ✅ Make `pic create --detached` skip the attach step (equivalent to today's `pic spawn`).
- ✅ Wire pic-attach so that when the user quits the TUI (Ctrl+D / `/quit`), the daemon's child keeps running (the daemon doesn't see this as a kill; it's a detach).

What this plan does NOT do:

- ❌ Multi-attach (two terminals attached to the same child simultaneously). The first version supports one TUI at a time per child. A second `pic attach` against an already-attached child either blocks or errors (TBD per Task 4).
- ❌ Detaching from a TUI that crashed / was killed (reattach works because the daemon doesn't know about TUI lifecycle).

---

## Repo layout additions

```
~/home/pi-controller/
├── cmd/pic/
│   ├── cmd_create.go            # new — spawn via daemon + exec pic-attach
│   └── cmd_attach.go            # new — exec pic-attach
├── attach/                      # new top-level dir for the TS TUI
│   ├── package.json
│   ├── tsconfig.json
│   ├── src/
│   │   ├── main.ts              # entry: parse args, construct runtime, run InteractiveMode
│   │   ├── client.ts            # UDS JSONL client (TS port of internal/client/)
│   │   ├── runtime.ts           # RemoteAgentSessionRuntime
│   │   ├── session.ts           # RemoteAgentSession (the AgentSession shape, proxied)
│   │   └── local-services.ts    # SessionManager / SettingsManager / ModelRegistry helpers
│   ├── README.md
│   └── Makefile or build.sh     # `bun build --compile` → bin/pic-attach
└── Makefile                     # add `build-attach` target (only if bun is available)
```

The `attach/` directory is TypeScript inside an otherwise-Go monorepo. The Makefile detects `bun` and only builds `pic-attach` when present.

---

## Tasks

### Task 1: TS package skeleton + build pipeline

**Files:**
- Create: `attach/package.json`
- Create: `attach/tsconfig.json`
- Create: `attach/src/main.ts` (placeholder that prints args)
- Create: `attach/README.md`
- Create: `attach/Makefile`
- Modify: root `Makefile` to add a `build-attach` target gated on `bun` availability.

**Reference:** pi's own build approach. Pi uses `bun build --compile ./dist/bun/cli.js --outfile dist/pi`.

- [ ] Step 1: `mkdir -p attach/src` and initialize `package.json`:

```json
{
    "name": "@graveland/pic-attach",
    "version": "0.1.0",
    "type": "module",
    "private": true,
    "scripts": {
        "build": "bun build --compile --target bun-darwin-arm64 ./src/main.ts --outfile ../bin/pic-attach",
        "dev": "bun ./src/main.ts"
    },
    "dependencies": {
        "@earendil-works/pi-coding-agent": "*"
    }
}
```

Adjust the `--target` per host arch; the Makefile target should compute the right one. (For now, hardcode `bun-darwin-arm64` since that's the dev environment.)

- [ ] Step 2: `tsconfig.json` (strict, ES2022, node module resolution):

```json
{
    "compilerOptions": {
        "target": "ES2022",
        "module": "ESNext",
        "moduleResolution": "bundler",
        "strict": true,
        "esModuleInterop": true,
        "skipLibCheck": true,
        "allowImportingTsExtensions": true,
        "noEmit": true
    },
    "include": ["src/**/*"]
}
```

- [ ] Step 3: `src/main.ts` placeholder:

```typescript
#!/usr/bin/env bun
console.log("pic-attach v0.1.0 — args:", process.argv.slice(2));
```

- [ ] Step 4: `attach/Makefile`:

```makefile
.PHONY: build install-deps clean

BUN ?= bun

install-deps:
	$(BUN) install

build: install-deps
	mkdir -p ../bin
	$(BUN) build --compile --target=$$($(BUN) --print "process.platform + '-' + process.arch" | sed 's/darwin-arm64/bun-darwin-arm64/; s/darwin-x64/bun-darwin-x64/; s/linux-x64/bun-linux-x64/') ./src/main.ts --outfile ../bin/pic-attach

clean:
	rm -rf node_modules bun.lockb
```

(If the `sed` mapping is awkward, hardcode for now.)

- [ ] Step 5: Root Makefile addition:

```makefile
.PHONY: build-attach

build-attach:
	@if command -v bun >/dev/null 2>&1; then \
	    $(MAKE) -C attach build; \
	else \
	    echo "skipping pic-attach build: bun not installed"; \
	fi

build: build-controller build-pic build-attach
```

- [ ] Step 6: Run `cd attach && bun install` and confirm `node_modules/@earendil-works/pi-coding-agent` is present.

- [ ] Step 7: Run `make build-attach` and verify `bin/pic-attach` exists and runs (`./bin/pic-attach hello` prints the args).

- [ ] Step 8: Commit: `attach: bun-compiled TS package skeleton`.

### Task 2: TS UDS client (mirror of internal/client/)

**Files:**
- Create: `attach/src/client.ts`
- Create: `attach/src/client.test.ts`

Implements:
- `Client` class with `dial(path)`, `request(req)`, `subscribe()`, `close()`.
- JSONL framing (LF only, no Unicode line breaks, ~16MB cap — same rules as Go side).
- Request/response correlation by id.
- Event channel for non-response frames.

This is the TS analogue of `internal/client/client.go`. About 150-200 LOC.

- [ ] Step 1: Implement `Client` per the API above using `node:net`.
- [ ] Step 2: Write tests using bun's built-in test runner (`bun test`).
- [ ] Step 3: Test exercises dial + roundtrip against a Go test harness OR a local mock TS server.
- [ ] Step 4: Commit: `attach: TS UDS client with request/response correlation`.

### Task 3: RemoteAgentSession (the agent-side proxy)

**Files:**
- Create: `attach/src/session.ts`
- Create: `attach/src/session.test.ts`

This implements the `AgentSession` shape (the thing `InteractiveMode` reads from via `runtime.session`).

**Critical decision:** AgentSession is a concrete class, not an interface, in pi. We can't implement an interface that doesn't exist. Three approaches:

1. **Inherit from `AgentSession`** and override methods. Requires understanding what state AgentSession holds internally and how it interacts with the Agent.
2. **Build a wrapper class** that satisfies the *shape* duck-typed and pass it where the TUI expects an AgentSession. Pi's type system might require us to lie (use `as unknown as AgentSession`).
3. **Construct a real AgentSession with a remote Agent** — replace just the Agent inside. Lets AgentSession do its session/message store work, but means we have two SessionManagers (ours and the daemon's pi's) writing to the same jsonl.

(3) is unsafe (concurrent jsonl writers). (1) needs source-level knowledge. (2) is the pragmatic choice.

Build `RemoteAgentSession` as a fresh class implementing every method/property of `AgentSession` we observed in the d.ts. Each method:

- For methods that return a value (queues, settings) — store local state, return synchronously.
- For methods that mutate the agent (prompt/steer/abort) — send ctrl_send, await response, fire local events synthesized from the daemon's event stream.
- The `subscribe()` channel emits events translated from incoming ctrl_event frames.

Cast it to `AgentSession` at the boundary (where the TUI consumes it) since the structural compatibility is all that matters at runtime.

- [ ] Step 1: Enumerate the methods/events of AgentSession from the d.ts and write stubs for each.
- [ ] Step 2: Implement the read-only getters (model, isStreaming, sessionFile, etc.) backed by local cache updated from daemon events.
- [ ] Step 3: Implement the mutation methods (prompt/steer/etc.) as ctrl_send wrappers.
- [ ] Step 4: Implement event translation: ctrl_event → fire local `subscribe()` listeners with the right shape.
- [ ] Step 5: Tests against a mock Go server.
- [ ] Step 6: Commit: `attach: RemoteAgentSession proxying to daemon over UDS`.

### Task 4: RemoteAgentSessionRuntime

**Files:**
- Create: `attach/src/runtime.ts`
- Create: `attach/src/runtime.test.ts`

Same pattern as Task 3, but for `AgentSessionRuntime`. Methods to implement:

- `services` getter (returns the local services bag — SessionManager pointing at the daemon's jsonl, local SettingsManager, local ModelRegistry).
- `session` getter (returns the `RemoteAgentSession` from Task 3).
- `cwd` getter (from initial spawn metadata).
- `switchSession(path, options)` → ctrl_send a new_session frame (which the daemon intercepts and respawns).
- `newSession(options)` → same.
- `fork(entryId, options)` → ctrl_send a fork frame; daemon passes through.
- `importFromJsonl(path)` → daemon doesn't support this directly; either implement client-side (read file, replay events) or return "not supported" for v1.
- `dispose()` → close UDS connection. **Does NOT kill the daemon's child** — the user is just detaching, not killing.

The runtime is constructed once at attach time. It dials the daemon, calls `ctrl_get <childId>` to fetch initial metadata (cwd, sessionFile, model), constructs local services pointed at the right paths, instantiates `RemoteAgentSession`, returns.

- [ ] Step 1: Constructor performs initial handshake (dial, ctrl_get, build services).
- [ ] Step 2: Implement getters returning cached metadata.
- [ ] Step 3: Implement session-replacement methods.
- [ ] Step 4: dispose() closes the connection cleanly.
- [ ] Step 5: Tests.
- [ ] Step 6: Commit: `attach: RemoteAgentSessionRuntime with session-replacement methods`.

### Task 5: Local services (SessionManager, SettingsManager, ModelRegistry)

**Files:**
- Create: `attach/src/local-services.ts`
- Create: `attach/src/local-services.test.ts`

**SessionManager:** pi's existing `SessionManager` can be constructed against an existing jsonl file path. We construct it pointed at the daemon's session file. The tricky bit: when the daemon's pi appends new entries, our SessionManager needs to see them. Two approaches:

- **File watcher**: `fs.watch(path)`, re-read on changes.
- **Polling**: re-stat every N ms, re-read tail on size change.

Or — simpler — don't watch at all. The TUI re-displays the conversation when prompted (via the event stream). The SessionManager is mostly used for context and history queries; if it's stale by milliseconds it's fine. Re-read on each `getEntries()` call (cheap for normal session sizes).

For v1: simple re-read on access. Optimize later if it's slow.

**SettingsManager** and **ModelRegistry**: standard local construction. They read `~/.pi/agent/settings.json` etc. Same as pi does when running locally.

- [ ] Step 1: Sketch the SessionManager wrapper that re-reads on access.
- [ ] Step 2: Construct SettingsManager + ModelRegistry from local config.
- [ ] Step 3: Tests verify the SessionManager picks up appended entries.
- [ ] Step 4: Commit: `attach: local services (SessionManager tail, SettingsManager, ModelRegistry)`.

### Task 6: main.ts — argv parsing + bootstrap

**Files:**
- Modify: `attach/src/main.ts`

Parse args: `pic-attach <childId>` (or take from env `PIC_CHILD_ID`).

Bootstrap:

```typescript
import { InteractiveMode } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSessionRuntime } from "./runtime.ts";

const childId = process.argv[2] || process.env.PIC_CHILD_ID;
if (!childId) {
    console.error("usage: pic-attach <childId>");
    process.exit(1);
}

const runtime = await RemoteAgentSessionRuntime.connect({
    socket: process.env.PI_CONTROLLER_SOCKET || `${process.env.HOME}/.pi/run/controller.sock`,
    childId,
});

const tui = new InteractiveMode(runtime, {});
await tui.start();   // or whatever the TUI's entry method is — check the .d.ts
```

The exact method name on InteractiveMode for "start" isn't in the d.ts excerpt I have; need to look it up. Likely `run()` or `start()`.

- [ ] Step 1: Parse args.
- [ ] Step 2: Construct runtime, instantiate InteractiveMode.
- [ ] Step 3: Wire signal handling (SIGINT/SIGTERM clean disconnect).
- [ ] Step 4: Manual smoke: `pic-attach <real-childId>` against a real daemon.
- [ ] Step 5: Commit: `attach: main.ts entry point bootstrapping the remote runtime`.

### Task 7: pic create + pic attach Go-side commands

**Files:**
- Create: `cmd/pic/cmd_create.go`
- Create: `cmd/pic/cmd_attach.go`
- Modify: `cmd/pic/main.go`

`pic create`:
1. Build a SpawnRequest from flags (same flags as `pic spawn`).
2. Add `--detached` boolean flag.
3. Send ctrl_spawn, get childId.
4. If `--detached`: print childId, exit.
5. Else: exec `pic-attach <childId>` with stdio inherited.

`pic attach <id|name>`:
1. Resolve id-or-name (existing `resolveTarget`).
2. exec `pic-attach <childId>`.

If `bin/pic-attach` is missing (bun not installed during build), print a clear error directing the user to install bun and run `make build-attach`.

- [ ] Step 1: Write cmd_create.go (mostly wraps cmd_spawn with the exec step).
- [ ] Step 2: Write cmd_attach.go.
- [ ] Step 3: Helper: locate pic-attach binary. First check `${binDir}/pic-attach` (sibling), then PATH, then error.
- [ ] Step 4: Wire into main.go.
- [ ] Step 5: Smoke: `pic create test --cwd /tmp --no-extensions --model anthropic/claude-haiku-4-5` opens the TUI.
- [ ] Step 6: Commit: `pic: create and attach subcommands`.

### Task 8: Integration tests

**Files:**
- Modify: `test/integration/cli_integration_test.go`

End-to-end testing the TS TUI from Go is hard — the TUI takes over a TTY. Two pragmatic checks:

1. **`pic create --detached`** exercises the spawn flow; assert childId returned.
2. **`pic attach --help`** confirms the binary is wired and the help renders.

For testing the actual TUI behavior, document a manual smoke procedure in a comment.

- [ ] Step 1: Add 2-3 Go integration tests for the detached / help paths.
- [ ] Step 2: Document manual smoke procedure.
- [ ] Step 3: Commit: `test/integration: pic create/attach integration tests`.

---

## Verification before declaring done

- [ ] `make build` builds all three binaries: `pi-controller`, `pic`, `pic-attach`.
- [ ] `pic create demo --cwd /tmp --no-extensions --model anthropic/claude-haiku-4-5`:
  - Opens a real pi TUI in your terminal.
  - Typing a prompt and hitting enter shows the assistant streaming response.
  - `/model` switches the model.
  - Ctrl+D quits — child stays running in the daemon (verify with `pic list` in another terminal).
- [ ] `pic attach demo` in another terminal reattaches with full TUI (or errors if already attached, per Task 4 decision).
- [ ] `pic create demo2 --detached` exits immediately with childId; pic list shows it.

---

## Known risks worth surfacing

1. **`AgentSession` is a class, not an interface.** We're duck-typing. If pi's TUI uses any AgentSession field we miss, it'll fail at runtime. Mitigate by exhaustive d.ts coverage and integration testing.

2. **Event timing.** AgentSession emits events synchronously in some paths. The remote proxy is asynchronous (events arrive when the UDS read loop schedules them). If InteractiveMode assumes synchronous emission anywhere, we'll see ordering glitches. May need fine-grained event batching.

3. **Extension UI.** Pi's `extension_ui_request` events expect a response with matching id. The TUI handles them and writes the response back via the AgentSession. We forward those response writes back over UDS. Works if the response shape matches; needs careful testing.

4. **Compaction.** Pi's local compaction runs in-process. If the daemon's pi compacts, our session.jsonl shrinks/changes underneath the local SessionManager. The TUI may be confused. Worth testing once compaction lands.

5. **bun availability.** We're adding a build-time + runtime dep on bun. macOS users will have it (most likely via Homebrew or curl); Linux users may need to install it. Document clearly.

6. **Multi-attach.** Two `pic attach` against the same childId would both subscribe to the bus and both send via ctrl_send. The daemon doesn't reject this. Each TUI would see all events; both could send prompts. May or may not be usable. v1 doesn't try to handle it.

7. **The daemon doesn't surface session changes well.** If pi switches sessions (via /new etc., which the daemon intercepts as respawn), the daemon emits `ctrl_child_exited` for the old child and `ctrl_child_spawned` for the new one — with the same childId. Our TUI sees this and needs to refresh its local SessionManager + metadata. The runtime's `switchSession()` should handle the round-trip.
