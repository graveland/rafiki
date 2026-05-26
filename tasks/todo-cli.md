# pi-ctl CLI — task tracker

Plan: `tasks/2026-05-25-implementation-plan-cli.md`
Spec: `tasks/pi-controller-protocol.md` (§12 for CLI specifics)
Mode: subagent-driven-development (sdd-implementer → sdd-spec-reviewer → sdd-code-quality-reviewer per task)

## Tasks

- [ ] **Task 1** — Bootstrap (Cobra dep, root command, global flags)
- [ ] **Task 2** — Client lib — connection + framing
- [ ] **Task 3** — Client lib — subscribe stream
- [ ] **Task 4** — Client lib — identifier resolution
- [ ] **Task 5** — Output formatting (JSON, table, color)
- [ ] **Task 6** — Read-only subcommands: list, get, status
- [ ] **Task 7** — Lifecycle subcommands: spawn, resume, kill
- [ ] **Task 8** — Forget subcommand
- [ ] **Task 9** — Recent, search, send subcommands
- [ ] **Task 10** — Active file + tab completion
- [ ] **Task 11** — Tail subcommand + event renderer
- [ ] **Task 12** — Logs subcommand + integration tests

## Final pass

- [ ] Final whole-implementation review
- [ ] Manual end-to-end smoke against real pi

## Parallelization note

Sequential execution per SDD adaptation note. Tasks that could have been parallelized:

- Tasks 2-4 are all in `internal/client/`; they could have been one larger task but the per-task review boundary is useful.
- Tasks 6, 8, 9 (read-only / single subcommands) are independent of each other once Tasks 2-5 are done.
- Task 5 (output formatting) is independent of everything else.

Sequential is fine for coordination simplicity.
