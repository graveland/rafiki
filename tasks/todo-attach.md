# pic-attach — task tracker

Plan: `tasks/2026-05-25-implementation-plan-pic-attach.md`
Spec: `tasks/pi-controller-protocol.md` (no protocol changes)
Mode: subagent-driven-development (sdd-implementer → sdd-spec-reviewer → sdd-code-quality-reviewer per task)

## Tasks

- [ ] **Task 1** — TS package skeleton + bun-compile build pipeline
- [ ] **Task 2** — TS UDS client (mirror of internal/client/)
- [ ] **Task 3** — RemoteAgentSession (proxies AgentSession shape)
- [ ] **Task 4** — RemoteAgentSessionRuntime (+ opt-in kill-on-exit)
- [ ] **Task 5** — Local services (SessionManager tail, SettingsManager, ModelRegistry)
- [ ] **Task 6** — main.ts entry point
- [ ] **Task 7** — pic create + pic attach Go-side commands
- [ ] **Task 8** — Integration tests

## Final pass

- [ ] Final whole-implementation review
- [ ] Manual end-to-end smoke against real pi

## Exit semantics (locked)

- Default: detach (UDS close, daemon's child unchanged)
- `--kill-on-exit` flag (on `pic create` and `pic attach`) → send `ctrl_kill` before disconnect
- Startup banner explains both
- Slash-command `/kill-session` deferred to v2

## Build dep

- `bun` required for building/running pic-attach
- Root Makefile's `build-attach` target gated on `bun` being available
- Document install path: `brew install oven-sh/bun/bun` or `curl -fsSL https://bun.sh/install | bash`
