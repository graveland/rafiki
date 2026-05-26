# pic CLI — task tracker

Plan: `tasks/2026-05-25-implementation-plan-cli.md`
Spec: `tasks/pi-controller-protocol.md` (§12 for CLI specifics)
Mode: subagent-driven-development (sdd-implementer → sdd-spec-reviewer → sdd-code-quality-reviewer per task)

## Tasks

- [x] **Task 1** — Bootstrap (Cobra dep, root command, global flags) — commit `ba47fbe`
- [x] **Task 2** — Client lib — connection + framing — commit `6659971`
- [x] **Task 3** — Client lib — subscribe stream — commit `ecf8072`
- [x] **Task 4** — Client lib — identifier resolution — commit `27d5d0a`
- [x] **Task 5** — Output formatting (JSON, table, color) — commit `d78de5f`
- [x] **Task 6** — Read-only subcommands: list, get, status — commit `e58afb2`
- [x] **Task 7** — Lifecycle subcommands: spawn, resume, kill — commit `bef1a98`
- [x] **Task 8** — Forget subcommand — commit `133f882`
- [x] **Task 9** — Recent, search, send subcommands — commit `b2575a8`
- [x] **Task 10** — Active file + tab completion — commit `1f72b9f`
- [x] **Task 11** — Tail subcommand + event renderer — commit `a16a4a5`
- [x] **Task 12** — Logs subcommand + integration tests — commit `22f8c26`

## Final pass

- [x] Final whole-implementation review — 3 IMPORTANT + 7 MINOR issues found and fixed (`3281a08`)
- [x] Manual end-to-end smoke against real pi — spawn/list/get-by-prefix/kill/forget all work; logs has a minor race with handleChildExit dump (kill returns before dump completes; documented limitation)

## Parallelization note

Sequential execution per SDD adaptation note. Tasks that could have been parallelized:

- Tasks 2-4 are all in `internal/client/`; they could have been one larger task but the per-task review boundary is useful.
- Tasks 6, 8, 9 (read-only / single subcommands) are independent of each other once Tasks 2-5 are done.
- Task 5 (output formatting) is independent of everything else.

Sequential is fine for coordination simplicity.
