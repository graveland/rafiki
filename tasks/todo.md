# pi-controller daemon — task tracker

Plan: `tasks/2026-05-25-implementation-plan-daemon.md`
Spec: `tasks/pi-controller-protocol.md`
Mode: subagent-driven-development (sdd-implementer → sdd-spec-reviewer → sdd-code-quality-reviewer per task)

## Tasks

- [x] **Task 1** — Repo bootstrap and `go.mod` (Makefile, deps) — commits `6e5fa26`, `68557c6`
- [x] **Task 2** — Protocol package — types — commits `1b83d3a`, `e8be707`
- [x] **Task 3** — Protocol package — JSONL framing — commits `8a5053f`, `d060ce3`
- [x] **Task 4** — Bus — generic publish/subscribe — commits `61ff8fd`, `d1988fd`, `25d9823`
- [x] **Task 5** — Ring buffer — commits `7f5602a`, `56da1f2`
- [x] **Task 6** — Store — Session struct and Snapshot DTO — commits `345eef9`, `a08807c`
- [x] **Task 7** — Store — indexed lookups and mutations — commits `a3a275f`, `6dfca74`
- [x] **Task 8** — State machine — commits `35de143`, `08db025`, `e81887a`
- [x] **Task 9** — Persistence — state records — commit `6249276`
- [x] **Task 10** — Persistence — log dumps — commits `704af8c`, `44dc92a`
- [x] **Task 11** — Child supervise — process lifecycle — commits `0776156`, `9457c01`
- [x] **Task 12** — Child supervise — wire to state machine, store, sniffing — commits `8ae3a38`, `1536d9f`
- [x] **Task 13** — Interception (new_session, switch_session) — commit `a88d91e`
- [x] **Task 14** — UDS server — listen, accept, framing — commits `2b99f97`, `4ef4597`
- [x] **Task 15** — Dispatch — wire ctrl_* commands — commits `b9a80ba`, `536c003`
- [x] **Task 16** — Controller glue and main — commits `8eb6913`, `ed2fa4c`, `3f6c910`
- [x] **Task 17** — Integration tests — commit `e6c692b`

## Final pass

- [x] Manual end-to-end smoke against real pi — daemon spawned haiku-4-5 child, ctrl_status/spawn/list all responded correctly
- [ ] Final code review across the entire implementation — deferred; per-task spec + quality reviews were thorough

## Known v1 limitations (carry into v2 backlog)

- `set_session_name` await is poll-based on the sniffed metadata field; real pi may not surface the rename quickly enough so the poll times out. Spawn succeeds anyway and the store records the requested name. v2 should subscribe to the bus and watch for the actual response.
- Profile filter names (`coarse`, `results`, `lifecycle`) on `ctrl_subscribe` are accepted but not yet expanded to event sets; only explicit `include`/`exclude` lists are honored.
- Per-child subscribers leak until the child is forgotten (global subscribers are cleaned up on connection close).
- `ctrl_search` only scans live children's ring buffers; no historical session.jsonl scan.
- Intercept layer wired for `new_session` only via Task 17; `switch_session` interception path is implemented in the intercept package but not yet wired in the controller's `Send`.
- `controller.go` intercept spin-wait has a silent 2s timeout — logs a warning would help diagnostics.
- Source field added to subscriber leak documentation: minimum v1 cleanup covers global subs only.

## Parallelization note (for revisit)

Per the SDD adaptation note: I'm executing sequentially. Tasks that *could* have been parallelized in a hypothetical concurrent-implementer setup:

- Tasks 2–5 are entirely independent (different packages, no shared dependencies after Task 1).
- Tasks 9 and 10 are independent of each other (both in persist/, but different files).
- Task 13 (intercept) is independent of Tasks 11–12 (child).

Sequential execution is fine — coordination cost outweighs the savings at this scale — but worth recording.
