# fundi → rafiki Merge Implementation Plan

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax for tracking. Execute
> task-by-task, verifying at each gate. Do not batch tasks 2–4 — task 3 is the only
> step in the plan that cannot be verified incrementally, and it must be isolated.

**Goal:** Merge fundi into rafiki as one repo, one module, laid out as `cmd/` + `pkg/`,
public-ready and `make check` green.

**Architecture:** Import fundi into rafiki *first* (unrelated-histories merge, files
landing at their original fundi paths), *then* move + rewrite imports inside rafiki
where both halves coexist. This inverts the handoff's stated sequence deliberately —
see "Sequencing rationale" below.

**Tech Stack:** Go 1.26.5, golangci-lint v2.12.1, bun (attach/), npm (pi CLI).

---

## STATUS: executed 2026-07-31 — tasks 1,2,3,4,6,7 done; task 5 deferred

Branch `feat/fundi-merge`, pushed to `local` only. **Not on `gh`** — see task 5.

| task | outcome |
|---|---|
| 1 | baseline captured: 358 tests / 10 packages, vet+lint clean |
| 2 | `ac90515` — **7** conflicts, not the 2 predicted (see below) |
| 3 | `7723745` — gate passed, 104 renames detected, `internal/` gone |
| 4 | `e1f919e` + `89bcc2b` — `pkg/` 24 → **22** |
| 5 | **deferred** — pi submodule still present; blocks the public push |
| 6 | `c821072` — fixtures neutralised, JSONL shape byte-verified |
| 7 | `3ef9131` + `ccfc083` — Makefile/README merged, `tasks/` resolved |
| — | `582251a` — unplanned: fixed the `inproc` test the merge revealed |

Final: **1191 passed, 0 failed, 29 packages.** `make check`, `make build` and
`make build-linux` all green. Baseline was 358, so fundi contributed ~833 tests
that had not been running at all.

### Where this plan was wrong, for the next one

- **Task 2 predicted 2 conflicts and got 7.** The miss that mattered was
  `go.mod`/`go.sum`: neither side can simply win, since `--ours` drops fundi's
  dependencies and `--theirs` drops rafiki's module path. Resolution was
  rafiki's module path plus fundi's 10 direct requires layered on, letting MVS
  take the max, with `go.sum` unioned so `tidy` needed no refetch.
- **The package mapping table missed `cmd/fundi/helpersembed`**, imported by
  full module path. The rewrite rules covered `internal/`, `client` and
  `protocol` but not `cmd/`. The plan's own "no stale module refs" check caught
  it, which is the whole argument for putting verification *between* the rewrite
  and the commit rather than after it.
- **Two silent shell failures, both worth internalising.** zsh does not
  word-split unquoted parameters, so `sed $files` passes every path as a single
  argument. And BSD sed's ERE has no `\b`, so a qualifier rewrite anchored on it
  matched nothing, rewrote zero files, and still exited 0 — indistinguishable
  from success. Use `xargs -0`, and `perl -pi -e` when you need word boundaries.
- **`go.work` was never tracked** — gitignored and disk-only. The file that hid
  the stale pin was invisible to anyone reading the repo, which is worth
  remembering next time a build resolves differently than the repo implies.
- **Consolidation reached 22 packages, not the ~19 first estimated.** The import
  graph rejected the deeper folds; `ring` has three consumers and `agent` is not
  among them.
- **A revealed failure is not a caused failure, but it is still yours.**
  `pkg/inproc`'s Kill test failed 4 runs in 5 the moment the merge made the
  package compile again. It had been unrunnable since rafiki moved to `pkg/`.
  Diagnosis: the test's own 32 MiB fixture took ~6.5s to parse under `-race`, so
  `Kill()` landed before the engine existed — and on its passing runs it had
  been asserting nothing at all, because the pipe was never filled. Fixed in
  `582251a`.

## Global Constraints

- Module path: `go.graveland.dev/rafiki`. Single `go.mod`. No nested modules, no `go.work`.
- Layout: `cmd/` + `pkg/` only. **No `internal/`.** Genuinely private helpers become
  unexported identifiers inside their consuming package.
- Collisions are resolved on **fundi's** side, never rafiki's. `pkg/store` and
  `pkg/server` do not move.
- Preserve fundi's commit messages (the `ctrl_child_status` and Critical-3 reasoning
  lives there). Two roots is fine.
- No `git add -A` / `git add .` — stage files by name. Verify with
  `git diff --cached --stat` before every commit.
- No Co-Authored-By trailers.
- **The `pi` submodule stays for now** (decision, 2026-07-31): getting the merge green is
  the priority, and the submodule swap is independent of it. See "Deferred by decision".
- **Therefore: do not push to `gh` (public) or to `main` in this plan.** The branch goes
  to `local` (gitea) only. rafiki is already public, so merging in a submodule that
  points at `git@git.graveland.dev` would break `git clone --recursive` for anyone
  outside — a visible regression on a public repo. The `gh` push waits for task 5.

## Deferred by decision

**Task 5 (pi submodule → npm) is deferred.** It is a public-readiness blocker, not a
merge blocker: the submodule is load-bearing only for the TypeScript side
(`attach/package.json`'s three `file:../pi/packages/*` deps) and for `pi-install`. Nothing
in the Go merge depends on it. Deferring keeps one risky change (a version swap that has
to be proven with `bun test`) out of the change that has to land first.

Consequences to hold in mind while executing:
- `.gitmodules` and `pi/` survive tasks 2–7. Checks that would flag them are adjusted.
- fundi's eight pi Makefile targets are **kept intact** in task 7, not rewritten.
- The public `gh` push is blocked until the swap lands. Task 7 step 7 enforces this.

## Sequencing rationale (divergence from the handoff)

The handoff's §2 sequence says rename fundi's packages, *then* import. This plan
imports first. Reason: there is exactly one unavoidably atomic step — "move files and
rewrite every import path" — because Go cannot compile a half-moved tree. Doing the
rewrite inside fundi's repo means it is uncompilable *and* unverifiable (fundi's module
cannot be `go.graveland.dev/rafiki` while a workspace sibling already claims that path,
so no `go.work` trick rescues it), and a mistake is only discovered *after* an
awkward-to-redo unrelated-histories merge. Importing first puts both halves in one
module, so the rewrite can be iterated against real `go vet` output until green.

Cost: rafiki's history gains one large move commit. This is acceptable — the value the
handoff asks to preserve is the commit *messages*, and `git log --follow` /
`--find-renames` handle moves as first-class.

## Baseline state (verified 2026-07-31, before any work)

- fundi `ac102d6`, clean tree, **pushed** to `local/main` (was 40 commits ahead; the
  safety push in the handoff's step 1 is **done**).
- rafiki `0c90134`, on `main`, in sync with `gh/main`. Two untracked scratch files
  (`rafiki.md`, `rafiki-prev.md`) — leave them alone, they are not tracked.
- **fundi does not currently build.** `go vet ./...` fails with 2 distinct errors, both
  in `internal/agent/engine.go`:
  - `423:76: too many arguments in call to agentloop.Run`
  - `579:17: undefined: llm.WithStreamHandler`
  `GOWORK=off` produces identical errors, and there are no `replace` directives — so
  `go.work` is **inert** and the stale module-cache pin
  (`git.graveland.dev/brent/rafiki v0.0.0-20260726010043-10a8ca5bf6f6`) is what supplies
  rafiki. This is the spike doc's §A trap, live.
- **fundi's source is already correct against rafiki HEAD.** Verified:
  `pkg/agentloop/agentloop.go:148` is
  `Run(ctx, conv *llm.Conversation, tools ToolSet, ev *Events, userContent []anthropic.ContentBlockParamUnion, opts ...llm.SendOption)`
  — exactly the 6 arguments fundi passes — and `WithStreamHandler` exists at
  `pkg/llm/conversation.go:259` as a `SendOption`. **Both errors are artifacts of the
  stale pin and must disappear at task 3 with no API adaptation.** If either survives
  task 3, stop: the assumption is wrong and the plan needs revisiting.

## Package mapping

fundi's 4 rafiki imports are all at **pre-`pkg/`** paths and must be rewritten too:

| old import | new import |
|---|---|
| `git.graveland.dev/brent/rafiki/llm` (10×) | `go.graveland.dev/rafiki/pkg/llm` |
| `git.graveland.dev/brent/rafiki/agentloop` (7×) | `go.graveland.dev/rafiki/pkg/agentloop` |
| `git.graveland.dev/brent/rafiki/routing` (3×) | `go.graveland.dev/rafiki/pkg/routing` |
| `git.graveland.dev/brent/rafiki/store` (1×) | `go.graveland.dev/rafiki/pkg/store` |

fundi's own packages (prefix `git.graveland.dev/brent/fundi/` → `go.graveland.dev/rafiki/pkg/`):

| fundi path | new path | note |
|---|---|---|
| `internal/store` | `pkg/childstore` | **collision** — package clause `store` → `childstore` |
| `internal/server` | `pkg/control` | **collision** — package clause `server` → `control` |
| `internal/envvar` | *merged into* `pkg/paths` | task 4; `paths` already imports it |
| `internal/intercept` | *unexported inside* `cmd/fundid` | task 4; sole consumer |
| `internal/agent` (+`/tools`) | `pkg/agent` (+`/tools`) | |
| `internal/bus` | `pkg/bus` | |
| `internal/child` | `pkg/child` | |
| `internal/inproc` | `pkg/inproc` | |
| `internal/models` | `pkg/models` | |
| `internal/paths` | `pkg/paths` | |
| `internal/persist` | `pkg/persist` | |
| `internal/ring` | `pkg/ring` | **stays its own package** |
| `internal/skills` | `pkg/skills` | |
| `internal/version` | `pkg/version` | **stays its own package** |
| `client` | `pkg/client` | public by design |
| `protocol` | `pkg/protocol` | public by design |
| `cmd/fundi`, `cmd/fundid` | unchanged | no collision with `cmd/rafiki` |
| `test/integration` | unchanged | |

**Consolidation is 24 → 22 top-level `pkg/` packages, not the ~19 first estimated.**
The import graph rejects deeper folding, verified:
- `ring` ← `cmd/fundid`, `internal/child`, `internal/store` (3 consumers; `agent` does
  **not** import it). Folding into `agent` would have been wrong.
- `intercept` ← `cmd/fundid` only. Folds into the command, not into `child`.
- `version` ← `cmd/fundi`, `cmd/fundid`. Two consumers, so it cannot be unexported in
  one; and rafiki has **no** version/build-info package to merge into (checked).
- `envvar` ← `client`, both cmds, `internal/paths`. `paths` already imports it and every
  other consumer takes both — the one clean merge.

**Out of scope, deliberately:** fundi's `pkg/models` (LLM catalog discovery) overlaps
rafiki's `pkg/llm`/`pkg/routing` model knowledge, and fundi's catalog is the stale one
(no Claude 5 family). Reconciling them is a *behaviour* change, not a layout move. File
as follow-up; do not attempt here.

**Ordering hazard (will silently corrupt the tree if ignored):** apply specific rules
before generic ones. A blanket `…/fundi/internal/` → `…/pkg/` rule applied first turns
`internal/store` into `pkg/store` — colliding with rafiki's — and `internal/server` into
`pkg/server`. Rewrite `internal/store` and `internal/server` **first**, and keep the
`brent/rafiki/store` rule from being caught by the fundi rules (different prefix, so
anchor on the full prefix, never on a bare `/store`).

---

### Task 1: Isolated workspace and captured baseline

**Files:** none modified.

- [ ] **Step 1: Create the worktree**

```bash
cd /Users/brent/home/rafiki
git worktree add .worktrees/fundi-merge -b feat/fundi-merge
cd .worktrees/fundi-merge
```

- [ ] **Step 2: Confirm the baseline is green before adding anything**

```bash
go vet ./... && golangci-lint run ./... && go test -race ./...
```

Expected: all pass. Note that `RAFIKI_TEST_DSN` comes from rafiki's gitignored `.env`,
which the worktree does **not** inherit — copy it in, or DB-backed tests silently skip:

```bash
cp /Users/brent/home/rafiki/.env .
grep -q RAFIKI_TEST_DSN .env && echo "DSN present" || echo "WARNING: DB tests will skip"
```

- [ ] **Step 3: Record the baseline test count for later comparison**

```bash
go test ./... 2>&1 | tee /tmp/rafiki-baseline.txt | tail -20
go test ./... -json 2>/dev/null | grep -c '"Action":"pass"' 
```

Keep this number. Task 3 must *increase* it (fundi's suites join), never decrease it.

---

### Task 2: Import fundi with history, files at original paths

**Files:** adds fundi's entire tracked tree into rafiki at its fundi-relative paths.

**Interfaces:**
- Produces: fundi's files present at `internal/*`, `client/`, `protocol/`, `cmd/fundi*`,
  `attach/`, `test/`, `docs/`, `tasks/`, plus `go.work`, `tools.go`, `Makefile`,
  `README.md`, `.gitmodules`. The tree is **red** after this task — expected.

- [ ] **Step 1: Add fundi as a remote and fetch**

```bash
git remote add fundi /Users/brent/home/fundi
git fetch fundi main
git log --oneline fundi/main -1   # expect ac102d6
```

- [ ] **Step 2: Merge with unrelated histories, deferring conflict resolution**

`Makefile` and `README.md` exist in both and will conflict. Everything else is disjoint.

```bash
git merge --allow-unrelated-histories --no-commit fundi/main
git status --short | grep -E '^(AA|UU|DD)' || echo "no content conflicts"
```

- [ ] **Step 3: Resolve the two expected conflicts by keeping both, temporarily**

Keep rafiki's `Makefile` and `README.md` at their canonical paths; park fundi's copies
for task 7 to merge properly.

```bash
git checkout --ours Makefile README.md
git show fundi/main:Makefile  > Makefile.fundi
git show fundi/main:README.md > README.fundi.md
git add Makefile README.md Makefile.fundi README.fundi.md
```

- [ ] **Step 4: Verify nothing unexpected is staged**

```bash
git diff --cached --stat | tail -5
git diff --cached --name-only | grep -E '^(pkg|cmd/rafiki)/' && echo "STOP: rafiki files touched" || echo "ok: rafiki untouched"
```

Expected: the `grep` finds nothing. If it finds anything under `pkg/` or `cmd/rafiki/`,
stop — the merge is touching rafiki's own code, which it must not.

- [ ] **Step 5: Commit the import**

```bash
git commit -m "Import fundi at its original paths, preserving history

Unrelated-histories merge of fundi ac102d6. Files land at their fundi
paths; the move to cmd/+pkg/ and the import rewrite follow in the next
commit. The tree does not build in between: Go cannot compile a
half-moved tree, so the move and the rewrite are one atomic step.

fundi's Makefile and README are parked as Makefile.fundi and
README.fundi.md pending the merge of both."
```

---

### Task 3: The atomic move + import rewrite (the one gate that matters)

**Files:** moves all of fundi's Go packages; rewrites imports in every fundi-origin
`.go` file; deletes `go.work`.

**Interfaces:**
- Consumes: task 2's imported tree.
- Produces: a **green** tree. `pkg/childstore` (package `childstore`), `pkg/control`
  (package `control`), `pkg/{agent,bus,child,client,envvar,inproc,models,paths,persist,protocol,ring,skills,version}`.

- [ ] **Step 1: Delete `go.work` and the stale rafiki pin**

`go.work` is inert (verified: `GOWORK=off` changes nothing) but it is the §A trap's
carrier and must not survive into one repo.

```bash
git rm go.work go.work.sum
go mod edit -droprequire=git.graveland.dev/brent/rafiki
```

- [ ] **Step 2: Move the packages with `git mv` (preserves rename detection)**

Specific-before-generic — the two collisions first:

```bash
mkdir -p pkg
git mv internal/store  pkg/childstore
git mv internal/server pkg/control
for p in agent bus child inproc models paths persist ring skills version envvar intercept; do
  git mv "internal/$p" "pkg/$p"
done
git mv client   pkg/client
git mv protocol pkg/protocol
rmdir internal 2>/dev/null; ls internal 2>&1   # expect: no such directory
```

`envvar` and `intercept` move to `pkg/` here and are consolidated away in task 4. Moving
them first keeps this step purely mechanical — one concern per commit.

- [ ] **Step 3: Rewrite import paths, specific rules first**

macOS BSD `sed` needs the empty `-i ''`. Order is load-bearing (see ordering hazard).

```bash
files=$(git ls-files '*.go')
# 1. fundi's two colliding packages — MUST precede the generic fundi rule
sed -i '' 's|git\.graveland\.dev/brent/fundi/internal/store|go.graveland.dev/rafiki/pkg/childstore|g' $files
sed -i '' 's|git\.graveland\.dev/brent/fundi/internal/server|go.graveland.dev/rafiki/pkg/control|g'   $files
# 2. generic fundi internal + the two root packages
sed -i '' 's|git\.graveland\.dev/brent/fundi/internal/|go.graveland.dev/rafiki/pkg/|g' $files
sed -i '' 's|git\.graveland\.dev/brent/fundi/client|go.graveland.dev/rafiki/pkg/client|g'     $files
sed -i '' 's|git\.graveland\.dev/brent/fundi/protocol|go.graveland.dev/rafiki/pkg/protocol|g' $files
# 3. fundi's PRE-pkg rafiki imports — full prefix anchored, so /store is unambiguous
for p in llm agentloop routing store; do
  sed -i '' "s|git\.graveland\.dev/brent/rafiki/$p|go.graveland.dev/rafiki/pkg/$p|g" $files
done
# 4. nothing may reference either old module path any more
git grep -nE 'git\.graveland\.dev/brent/(fundi|rafiki)' -- '*.go' && echo "STOP: stale refs remain" || echo "ok: no stale module refs"
```

- [ ] **Step 4: Rename the two package clauses and their qualifiers**

Scope the qualifier rewrite to the files that actually import each package — a blind
`store.` → `childstore.` would corrupt rafiki's own `pkg/store` consumers.

```bash
sed -i '' 's|^package store$|package childstore|'  pkg/childstore/*.go
sed -i '' 's|^package store_test$|package childstore_test|' pkg/childstore/*.go
sed -i '' 's|^package server$|package control|'    pkg/control/*.go
sed -i '' 's|^package server_test$|package control_test|'   pkg/control/*.go

for f in $(git grep -l 'rafiki/pkg/childstore"' -- '*.go'); do
  sed -i '' -E 's/\bstore\.([A-Z])/childstore.\1/g' "$f"
done
for f in $(git grep -l 'rafiki/pkg/control"' -- '*.go'); do
  sed -i '' -E 's/\bserver\.([A-Z])/control.\1/g' "$f"
done
```

Then confirm no rafiki-origin file was touched by the qualifier pass:

```bash
git diff --name-only | grep -E '^pkg/(llm|routing|agentloop|store|server|insights|analyze|agentcli)/' \
  && echo "STOP: rafiki packages modified" || echo "ok: rafiki packages untouched"
```

- [ ] **Step 5: Tidy and format**

```bash
gofmt -w $(git ls-files '*.go')
goimports -w $(git ls-files '*.go') 2>/dev/null || true
go mod tidy
```

- [ ] **Step 6: THE GATE — the tree must go green**

```bash
go build ./... && go vet ./...
```

Expected: **clean**. Specifically, both baseline errors
(`too many arguments in call to agentloop.Run`, `undefined: llm.WithStreamHandler`) must
be gone, because in-tree `pkg/agentloop` and `pkg/llm` are now HEAD rather than the
stale pin.

**If either error survives, stop and report.** It would mean fundi's source is *not*
in fact current against rafiki HEAD, and real API adaptation is needed — a materially
different task than this plan describes.

- [ ] **Step 7: Full check, and confirm the test count grew**

```bash
golangci-lint run ./...
go test -race ./... 2>&1 | tail -30
go test ./... -json 2>/dev/null | grep -c '"Action":"pass"'
```

Expected: greater than task 1's baseline number. A count that is equal or lower means
fundi's suites are not running — investigate before proceeding.

- [ ] **Step 8: Commit**

```bash
git diff --cached --stat | tail -3
git commit -am "Move fundi under cmd/+pkg/ and rewrite imports to the rafiki module

internal/ is gone: fundi's packages now sit in pkg/, resolving the two
name collisions on fundi's side as agreed — internal/store becomes
pkg/childstore and internal/server becomes pkg/control. rafiki's
pkg/store and pkg/server do not move.

fundi's rafiki imports were still on the pre-pkg/ paths and pinned to a
stale module version, which is why its build was red: the pin predates
both llm.WithStreamHandler and the current agentloop.Run signature. In
one module against HEAD, both resolve.

go.work is deleted. It was already inert — GOWORK=off produced identical
errors — but it is the carrier of the cross-module trap, and with one
repo there is nothing left for it to wire."
```

---

### Task 4: Consolidate `envvar` into `paths`, `intercept` into `cmd/fundid`

**Files:**
- Modify: `pkg/paths/*.go` (absorb), `cmd/fundid/*.go` (absorb)
- Delete: `pkg/envvar/`, `pkg/intercept/`

Two independent folds; commit separately so either can be reverted alone.

- [ ] **Step 1: Absorb `envvar` into `paths`**

```bash
git mv pkg/envvar/envvar.go pkg/paths/envvar.go
ls pkg/envvar/   # any remaining files (tests) move too
```

Move remaining files, then set the package clause and drop the now-self import:

```bash
sed -i '' 's|^package envvar$|package paths|;s|^package envvar_test$|package paths_test|' pkg/paths/envvar*.go
sed -i '' '/rafiki\/pkg\/envvar"/d' $(git grep -l 'rafiki/pkg/envvar"' -- '*.go')
for f in $(git ls-files '*.go'); do sed -i '' -E 's/\benvvar\.([A-Z])/paths.\1/g' "$f"; done
rmdir pkg/envvar 2>/dev/null
gofmt -w $(git ls-files '*.go') && go mod tidy
```

- [ ] **Step 2: Verify and commit the `envvar` fold**

```bash
go vet ./... && go test -race ./pkg/paths/... ./pkg/client/... ./cmd/... 
git grep -n 'pkg/envvar' -- '*.go' && echo "STOP: refs remain" || echo "ok"
git commit -am "Fold envvar into paths

paths already imported envvar, and every other consumer (client, both
commands) took both — so they were one concern split across two
packages. One fewer package at pkg/ top level."
```

- [ ] **Step 3: Absorb `intercept` into `cmd/fundid` as unexported**

`cmd/fundid` is its only consumer (verified), so this is the handoff's prescribed
discipline: a private helper becomes unexported inside the package that uses it.

```bash
git mv pkg/intercept/intercept.go cmd/fundid/intercept.go
ls pkg/intercept/   # move any test files the same way
sed -i '' 's|^package intercept$|package main|' cmd/fundid/intercept*.go
sed -i '' '/rafiki\/pkg\/intercept"/d' $(git grep -l 'rafiki/pkg/intercept"' -- '*.go')
```

Now lower-case the exported identifiers and their call sites. Enumerate them first
rather than guessing:

```bash
git grep -nE '^func [A-Z]|^type [A-Z]|^var [A-Z]|^const [A-Z]' -- cmd/fundid/intercept.go
```

For each exported name `Foo`, rewrite `intercept.Foo` → `foo` across `cmd/fundid/`, and
the declaration `Foo` → `foo`. Check for collisions with existing `cmd/fundid`
identifiers before renaming — if `foo` is taken, use `interceptFoo`.

```bash
rmdir pkg/intercept 2>/dev/null
gofmt -w $(git ls-files '*.go') && go mod tidy
```

- [ ] **Step 4: Verify and commit the `intercept` fold**

```bash
go vet ./... && go test -race ./cmd/fundid/...
git grep -n 'pkg/intercept' -- '*.go' && echo "STOP: refs remain" || echo "ok"
git commit -am "Fold intercept into cmd/fundid as unexported

cmd/fundid was its only consumer. With no internal/ to hide it in, an
unexported helper inside the consuming package is the right home — and
one fewer name in the public surface."
```

---

### Task 5: Drop the `pi` submodule for published npm packages — **DEFERRED**

> **Do not execute this task in this pass.** Deferred by decision on 2026-07-31 (see
> "Deferred by decision" above): the merge lands first, this follows. Skip straight to
> task 6. The steps below are kept as the recipe for when it is resumed — they are
> already researched and verified, and re-deriving them later would waste the work.
>
> While deferred: `.gitmodules` and `pi/` stay, task 7 keeps fundi's pi Makefile targets
> as they are, and **nothing is pushed to `gh`**.

**Files:**
- Delete: `pi/` submodule, `.gitmodules`
- Modify: `attach/package.json`, `Makefile` (8 pi targets)

The blocker: `pi`'s URL is `git@git.graveland.dev:brent/pi.git`, so a public clone
breaks on submodule init.

**Wider than the handoff states:** the Makefile has eight submodule-coupled targets
(`$(PI_DIST)`, `$(PI_MODULES)`, `pi-build`, `pi-install`, `pi-update`,
`pi-refresh-catalogs`, `bootstrap`, `pi-not-initialised`), and `pi-install` is how the
**daemon-spawned `pi` binary** reaches PATH. The npm swap must cover that global CLI
install, not only attach's three library deps.

- [ ] **Step 1: Confirm the three packages exist at 0.80.6 before deleting anything**

```bash
for p in pi-agent-core pi-ai pi-coding-agent; do
  npm view "@earendil-works/$p@0.80.6" version 2>&1 | tail -1
done
```

All three must print `0.80.6`. If any is missing, **stop** — the submodule is still
load-bearing and this task cannot proceed as designed.

- [ ] **Step 2: Repoint attach's dependencies**

`attach/package.json` currently has three `file:../pi/packages/*` deps. Replace with:

```json
    "dependencies": {
        "@earendil-works/pi-agent-core": "0.80.6",
        "@earendil-works/pi-ai": "0.80.6",
        "@earendil-works/pi-coding-agent": "0.80.6"
    },
```

- [ ] **Step 3: Verify attach builds and tests against the published packages**

This is the task's real gate — the handoff flags it as must-verify. The audit found none
of the three local patches is needed (`attach/src` never imports pi's TUI; nothing
references the rpc `ready` event), and this proves it.

```bash
cd attach && rm -rf node_modules bun.lock && bun install && bun test && bun run build
cd ..
```

Expected: install resolves, tests pass, build produces the bundle. If a test fails on a
missing patch, stop and report which — dropping a needed patch silently is exactly the
failure mode to avoid.

- [ ] **Step 4: Remove the submodule**

```bash
git submodule deinit -f pi
rm -rf .git/modules/pi
git rm -f pi
git rm -f .gitmodules
```

- [ ] **Step 5: Rewrite the Makefile's pi targets**

Delete `$(PI_DIST)`, `$(PI_MODULES)`, `$(PI_PKG)`, `$(PI_SRC)`, `$(PI_DIR)`,
`pi-build`, `pi-refresh-catalogs`, `pi-not-initialised`, and the
`$(PI_DIR)/package-lock.json` / `$(PI_PKG)/package.json` rules. Then:

- `build-attach`: drop the `$(PI_MODULES) $(PI_DIST)` prerequisites; keep the bun guard.
- `pi-install`: becomes `npm install -g @earendil-works/pi-coding-agent@$(PI_VERSION)`.
- `pi-update`: no longer a submodule bump. Either delete it or make it bump
  `PI_VERSION` — deleting is cleaner; a version bump is a deliberate edit.
- `bootstrap`: drop `git submodule update --init --recursive` and `pi-build`.
- Add `PI_VERSION ?= 0.80.6` near the other variables, and use it in `pi-install` **and**
  `attach/package.json`'s pin so the two cannot drift silently.

- [ ] **Step 6: Verify and commit**

```bash
go build ./... && go vet ./...
make build-daemon build-cli
git grep -nE 'submodule|\bpi/|PI_DIST|PI_MODULES' -- Makefile .gitmodules 2>/dev/null \
  && echo "check each hit is intentional" || echo "ok: no submodule refs"
git status --short | grep -E '^\?\?' | head   # pi/ must not linger untracked
git commit -am "Replace the pi submodule with published npm packages at 0.80.6

The submodule pointed at git@git.graveland.dev:brent/pi.git, so a public
clone broke on submodule init. It is not repointed at pi's upstream: the
pinned commit d3a7b8d4 is on the fork branch brent/inherit, not main.

0.80.6 is exactly the pi CLI version in use, so there is no version skew
and no minor-version jump. attach never imported pi's TUI and nothing
referenced the rpc ready event, so none of the three local patches was
load-bearing here — verified by bun test against the published packages.
Those patches want upstreaming to pi rather than dropping silently.

pi-install now installs the published CLI globally instead of building
the submodule, keeping the daemon-spawned backend reachable. PI_VERSION
is the single pin, shared with attach/package.json."
```

---

### Task 6: Scrub the session-init fixtures

**Files:** Modify `pkg/child/testdata/claude/startup_and_turn.jsonl`,
`pkg/child/testdata/claude/turn_with_tool.jsonl`, and `NOTES.md` if it names anything.

Not secret, but they publish the tooling setup: plugin paths, MCP server names, memory
paths, and `cwd: /Users/brent/home/pi-controller`.

- [ ] **Step 1: Inventory exactly what needs replacing**

```bash
cd pkg/child/testdata/claude
git grep -nE '/Users/brent|pi-controller|mcp|plugin|memory' -- . | head -40
```

- [ ] **Step 2: Replace with neutral values, preserving structure**

The fixtures are asserted against by `pkg/child` tests, so the JSONL shape, field set
and ordering must not change — only the values. Use `/home/user/project` for `cwd`,
generic `example-*` names for MCP servers and plugins.

- [ ] **Step 3: Verify the tests still pass against the scrubbed fixtures**

```bash
cd /Users/brent/home/rafiki/.worktrees/fundi-merge
go test -race ./pkg/child/... -v 2>&1 | tail -30
```

Expected: pass. If a test asserts on a scrubbed literal, update the assertion — the
fixture is the input, not the contract.

- [ ] **Step 4: Confirm nothing identifying remains, then commit**

```bash
git grep -nE '/Users/brent|pi-controller|tigerdata|graveland' -- 'pkg/child/testdata/**'
git commit -am "Scrub the local tooling setup out of the claude session fixtures

The session-init dumps carried plugin paths, MCP server names, memory
paths and a real home directory. Nothing secret, but nothing anyone
needs either. Values are neutralised; the JSONL shape is unchanged
because the child tests assert against it."
```

---

### Task 7: Merge the Makefiles, READMEs and docs; final gate

**Files:** Modify `Makefile`, `README.md`; delete `Makefile.fundi`, `README.fundi.md`.

- [ ] **Step 1: Fold fundi's Makefile targets into rafiki's**

rafiki's Makefile is kubebuilder-style with `##@` sections and a `check: vet lint test`
gate. Fold fundi's targets in under those conventions, not alongside them:

- `##@ Development`: `build-daemon`, `build-cli`, `build-attach`, `build-linux`,
  `install`, `print-config`, `bootstrap`, `pi-install`.
- **Keep all eight pi targets exactly as they are** (`$(PI_DIST)`, `$(PI_MODULES)`,
  `pi-build`, `pi-install`, `pi-update`, `pi-refresh-catalogs`, `bootstrap`'s submodule
  init, `pi-not-initialised`), along with `$(PI_DIR)`/`$(PI_PKG)`/`$(PI_SRC)`. The
  submodule stays this pass, so these still work and are still needed. Their comments are
  load-bearing too — `$(PI_DIST)`'s records why it must not be a plain `npm run build`
  (packages/ai's build refetches live model catalogs, making builds non-reproducible and
  the submodule dirty). Do not "tidy" them in passing.
- Keep rafiki's `build` (bin/rafiki) as-is and add fundi's binaries; `build` should
  produce all three (`rafiki`, `fundid`, `fundi`).
- `##@ Quality`: rafiki's `check`/`vet`/`lint`/`test`/`test-nodb` **win**. Drop fundi's
  `test`, `test-race`, `vet`, `fmt`, and especially `test-ci`/`test-both` — they exist
  only to work around the pinned-vs-workspace split that no longer exists.
- Keep fundi's `build-linux`: its comment records that the daemon and CLI silently
  bitrotted on Linux for a whole phase, and there is still no CI to catch it.
- Drop the deprecated aliases `build-controller` / `build-pic`.
- Preserve rafiki's `RAFIKI_TEST_DSN` warning in `test` — it is what stops ~107
  DB-backed tests silently not running.

```bash
git rm Makefile.fundi
```

- [ ] **Step 2: Merge the READMEs**

One README describing one project: rafiki's proxy/insights surface plus fundi's
daemon/CLI/attach surface. Rewrite fundi's install instructions to drop the submodule
bootstrap.

```bash
git rm README.fundi.md
```

- [ ] **Step 3: Reconcile the docs tree**

fundi brought `docs/` (18 files) into a repo that already has `docs/`. Check for name
collisions and that fundi's docs' module paths and package names are updated:

```bash
git status --short docs/ | head
git grep -lE 'git\.graveland\.dev/brent/(fundi|rafiki)|internal/(store|server|agent)' -- 'docs/**' '*.md'
```

Update every hit — stale paths in docs are how the next reader gets misled. This plan
document included: it lives at `docs/plans/2026-07-31-fundi-rafiki-merge.md`.

- [ ] **Step 4: Decide `tasks/`**

fundi brought tracked files under `tasks/` (`2026-05-25-implementation-plan-*.md`,
`pi-controller-protocol.md`, `todo*.md`) that predate the never-commit-`tasks/` rule.
The handoff calls this a deliberate call, not a drive-by — so **ask** before removing
them. If confirmed: `git rm --cached` those paths and ensure `/tasks/` is in
`.gitignore`. `pi-controller-protocol.md` looks like reference material rather than a
task file; it likely belongs in `docs/` instead of being deleted.

- [ ] **Step 5: THE FINAL GATE**

```bash
make check
make build
make build-linux
```

All must pass. `make build-linux` matters specifically because nothing else exercises
`GOOS=linux` and there is no CI.

- [ ] **Step 6: Verify the tree is genuinely public-ready**

```bash
test -f go.work && echo "STOP: go.work exists" || echo "ok: no go.work"
test -f .gitmodules && echo "expected while task 5 is deferred" || echo "ok: no submodules"
git grep -nE 'git\.graveland\.dev/brent/(fundi|rafiki)' || echo "ok: no stale module paths"
go list ./... | grep -c internal   # expect 0
git ls-files | grep -c .           # sanity: fundi's files + rafiki's
```

`.gitmodules` present is **expected** this pass, not a failure. `go.work` present is
still a hard stop.

- [ ] **Step 7: Commit and push**

```bash
git diff --cached --stat
git commit -m "Merge the fundi and rafiki docs, READMEs and Makefiles"
git push local feat/fundi-merge
```

**Push to `local` only. Do not push to `gh`, and do not merge to `main`.** Two reasons,
both hard gates:

1. Task 5 is deferred, so the tree still carries a submodule pointing at
   `git@git.graveland.dev`. rafiki is already public on `gh`; pushing this would break
   `git clone --recursive` for everyone outside.
2. There is no CI on the public repo, so `main` has no gate but a human.

Ask before either push. `git remote -v` in the worktree will show `gh` — that it is
reachable is not permission to use it.

---

## Out of scope, tracked as follow-ups

- **Task 5: pi submodule → npm 0.80.6** (deferred by decision, recipe already written
  above). **This is the gate on the public `gh` push** — until it lands, the merged tree
  cannot go public without breaking recursive clones. Also the point at which the two
  dropped pi patches should be upstreamed rather than quietly lost.
- **No CI on the public rafiki repo.** The excised workflow was ~2 lines per job from
  portable: `runs-on: ubuntu-latest`, drop the two self-hosted `runs-on`/`action@` steps.
  Keep `RAFIKI_REQUIRE_DB: "1"` (it makes DB-backed tests *fail* rather than skip when
  the DSN is absent — the only thing stopping ~107 tests silently not running), the
  golangci-lint pin at `v2.12.1`, and `make build` in the check job. **Consequence for
  this plan: local `make check` is the only gate, so task 7 step 5 is not optional.**
- **`pkg/models` ↔ `pkg/llm`/`pkg/routing` reconciliation.** fundi's catalog is stale
  (no Claude 5 family) while rafiki's routing knows real model ids. Behaviour change.
- **Upstream pi the two dropped patches** (`fix(tui)`, rpc-mode `ready`).
- **Gitea gc** on the bare rafiki repo so pre-redaction objects stop being fetchable:
  `git reflog expire --expire=now --all && git gc --prune=now`. Gitea's admin "run
  garbage collection" does not expire reflogs.
- **Move the `/tmp` pre-rewrite bundles** somewhere durable, or accept losing them.
- **fundi's `forget`/`kill` help text** claims disk artifacts are untouched while the
  code deletes the log dump, so a child's stderr is unreadable after a normal `kill`.
  Text or behaviour is wrong; nobody has decided which.
- **Phase 2 plan** — write it against the merged tree, and give
  `docs/plans/2026-07-30-execution-and-storage-design.md` a paths/layout pass first
  (its decisions hold; only the coordinates changed).

## Self-review notes

- Spec coverage: handoff §2 blockers 1 (submodule) → task 5 **deferred**, 2 (fixtures) →
  task 6, 3 (`go.work`) → task 3 step 1. Sequence items 1 → done pre-plan, 2 → task 2,
  3 → tasks 3–4, 4 → task 6 (+ deferred task 5), 5 → task 7.
- Deliberate divergences from the handoff, each flagged inline with rationale:
  import-before-rename (sequencing); consolidation landing at 22 rather than ~19 packages
  (the import graph rejects the deeper folds); pi submodule deferred rather than deleted
  (user decision — speed to green, at the cost of holding the public push).
- Executable tasks this pass: **1, 2, 3, 4, 6, 7.** Task 5 is skipped.
- The one unverifiable interval is task 2 → task 3 step 6. It is bounded by a single
  commit and has an explicit stop condition.
