# pi-controller daemon — task tracker

Plan: `tasks/2026-05-25-implementation-plan-daemon.md`
Spec: `tasks/pi-controller-protocol.md`
Mode: subagent-driven-development (sdd-implementer → sdd-spec-reviewer → sdd-code-quality-reviewer per task)

## Tasks

- [ ] **Task 1** — Repo bootstrap and `go.mod` (Makefile, deps)
- [ ] **Task 2** — Protocol package — types
- [ ] **Task 3** — Protocol package — JSONL framing
- [ ] **Task 4** — Bus — generic publish/subscribe
- [ ] **Task 5** — Ring buffer
- [ ] **Task 6** — Store — Session struct and Snapshot DTO
- [ ] **Task 7** — Store — indexed lookups and mutations
- [ ] **Task 8** — State machine
- [ ] **Task 9** — Persistence — state records
- [ ] **Task 10** — Persistence — log dumps
- [ ] **Task 11** — Child supervise — process lifecycle
- [ ] **Task 12** — Child supervise — wire to state machine, store, sniffing
- [ ] **Task 13** — Interception (new_session, switch_session)
- [ ] **Task 14** — UDS server — listen, accept, framing
- [ ] **Task 15** — Dispatch — wire ctrl_* commands
- [ ] **Task 16** — Controller glue and main
- [ ] **Task 17** — Integration tests

## Final pass

- [ ] Final code review across the entire implementation
- [ ] Manual end-to-end smoke against real pi

## Parallelization note (for revisit)

Per the SDD adaptation note: I'm executing sequentially. Tasks that *could* have been parallelized in a hypothetical concurrent-implementer setup:

- Tasks 2–5 are entirely independent (different packages, no shared dependencies after Task 1).
- Tasks 9 and 10 are independent of each other (both in persist/, but different files).
- Task 13 (intercept) is independent of Tasks 11–12 (child).

Sequential execution is fine — coordination cost outweighs the savings at this scale — but worth recording.
