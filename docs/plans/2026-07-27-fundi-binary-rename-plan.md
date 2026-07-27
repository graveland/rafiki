# fundi binary rename + install target + pic-helpers migration

**Status:** planned, not started. Written 2026-07-27 at the end of a long session;
the findings below were all verified live, but no code has moved yet.

## Why

`pic` stands for **pi c**ontroller — wrong on both halves now. It is also the
last of a family of inherited-identity collisions with the standalone
pi-controller install, three of which have already bitten:

| Shared namespace | Symptom | Fixed in |
|---|---|---|
| `~/.pi/run/controller.sock` | "socket in use by a live process" | `87086c9` |
| launchd label `dev.graveland.pi-controller` | `pic service uninstall` would delete pi-controller's live service | `5a5acf8` |
| `~/.pi/agent/extensions/pic-helpers/` | both products install here; last writer wins, either `--uninstall` deletes the other's | **this plan** |

Plus: fundi has no install target at all, and `~/bin/pic` on this machine is
*pi-controller's* client — so "which `pic` am I running" is a live footgun the
smoke checklist currently has to warn about.

## Target state

Idiomatic Unix daemon/client split (`dockerd`/`docker`, `tailscaled`/`tailscale`):

| Binary | Was | Role |
|---|---|---|
| `fundid` | `fundi` | the daemon, plus `fundid agent` (the agent child is this binary re-exec'd, so it belongs with the daemon) |
| `fundi` | `pic` | the CLI client — the name users actually type |

Installed to `~/.local/bin` (XDG counterpart to the `internal/paths` work), which
also sidesteps `~/bin/pic` entirely.

## Sizing (measured, not estimated)

- `cmd/pic/` — 48 Go files; ~40 Go files across the repo mention `pic`
- Exactly **one** line sets the command name: `cmd/pic/main.go:24` `Use: "pic"`.
  The bulk is the directory/package move and doc strings, not logic.
- Non-Go coupling: `Makefile`, `README.md`, `attach/README.md`,
  `cmd/pic/picembed/pic-helpers/{package.json,README.md}`

## Tasks

### 1. Move the daemon: `cmd/fundi` → `cmd/fundid`

- Rename the directory; binary output `bin/fundid`.
- `internal/agent` and `client` are untouched (no name coupling).
- **Load-bearing:** `cmd/pic/cmd_service.go`'s daemon auto-detect currently does
  `exec.LookPath("fundi")` and a sibling lookup for `"fundi"` (changed in
  `5a5acf8`). Both must become `"fundid"`, or `service install` silently targets
  the *client* binary.
- `os.Executable()`-based agent spawn in `controller.go::resolveSpawnPlan` is
  self-referential and needs no change — it re-execs whatever the daemon is.
- Service identity (`dev.graveland.fundi` / unit `fundi`) can stay as-is: it
  names the product, not the binary. Do NOT churn it again — it was just fixed,
  and every change orphans an installed unit.

### 2. Move the client: `cmd/pic` → `cmd/fundi`

- Rename directory; binary output `bin/fundi`; `Use: "fundi"` in `main.go`.
- Sweep `pic` in help text, error strings and doc comments. Keep references that
  describe *pi-controller's* `pic` as a distinct thing (there are a few in
  comments explaining the fork).
- `attach/` bundles the TUI as `pic-attach`; rename to `fundi-attach` and update
  `attach/package.json`, `attach/README.md`, and the `build-attach` target.

### 3. `pic-helpers` → `fundi-helpers` (see the migration section below)

### 4. Makefile

- `build-controller` → `build-daemon` (`bin/fundid`), `build-pic` → `build-cli`
  (`bin/fundi`). Keep the old target names as aliases for one cycle — muscle
  memory and the smoke doc reference them.
- **New:** `make install` → copies `bin/fundid` and `bin/fundi` to
  `$(DESTDIR)` defaulting to `~/.local/bin`, creating it if absent. Print the
  resolved paths, and warn if a *different* `fundi`/`fundid` is earlier on
  `$PATH` (this machine has `~/bin/pic` and `~/bin/pi-controller`).

### 5. Docs

- `README.md` — binary names, build/install targets.
- `docs/plans/2026-07-20-fundi-m1-smoke.md` — step 1 and the socket/`pic` warning
  table. After the rename, the "which pic" warning collapses to "a `pic` on
  `$PATH` is pi-controller's; fundi's client is `fundi`", which is the point.

## The pic-helpers migration

**What it is:** `cmd/pic/picembed/pic-helpers/` is a small TypeScript pi
extension (`@graveland/pic-helpers` v0.1.2, `index.ts` + `package.json`) that
`pic install-extension` copies to `~/.pi/agent/extensions/pic-helpers/`. It
registers slash commands for the pi TUI — necessary because in `--mode rpc` pi
has no builtin handler for them. Install is version-checked and skips when
current; `--uninstall` is `os.RemoveAll(destDir)`.

**Three constraints:**

1. **pi loads every extension in that directory.** Ship `fundi-helpers` without
   handling the old copy and pi loads *both*, registering the same slash commands
   twice. That is functional breakage, not clutter.
2. **The directory is pi's, not fundi's.** It is deliberately outside the XDG
   move — writing extensions there is how pi works. But the *artifact name* is
   fundi's to own, which is why this is fundi's problem.
3. **pi-controller's own `pic` installs the same artifact to the same path**, also
   declaring `@graveland/pic-helpers`. So fundi **cannot distinguish its own stale
   install from pi-controller's working one.**

**Therefore: detect and warn, never delete.**

- Bundle as `fundi-helpers`, package name `@graveland/fundi-helpers`; install to
  `~/.pi/agent/extensions/fundi-helpers/`.
- On install, `os.Stat` the old `pic-helpers/` path. If present, print a warning
  that names the path, says it may belong to pi-controller, and instructs the
  user to remove it manually if it was fundi's — including the exact `rm -rf`.
  Do not remove it.
- `--uninstall` touches only `fundi-helpers`.
- Rationale: a one-time manual step is strictly better than a chance of ripping
  out a working pi-controller extension. Same principle as the service-identity
  fix — leave pi-controller's installs completely alone.

## Ordering — do this before two things

1. **Before pushing.** 40 commits are unpushed, so no published binary names need
   deprecating. This gets much more expensive once CI publishes artifacts.
2. **Before the M1 smoke gate.** The gate builds muscle memory and its checklist
   names binaries throughout; renaming afterwards means reworking the doc and
   re-running steps.

## Verification

- `make test-both` green (go.work **and** `GOWORK=off` — the pinned-rafiki path;
  a plain `make test` hides go.sum gaps).
- `./bin/fundid -h` and `./bin/fundid agent -h` both print usage; `./bin/fundi
  --help` names itself correctly.
- **Coexistence, live:** start `fundid` while pi-controller runs; both sockets
  present, `./bin/fundi ls` reaches fundid, pi-controller's children untouched.
- `fundi service status` reports fundi's service (`Installed: no` on a clean box)
  and never pi-controller's.
- `fundi service install --daemon-binary` auto-detect resolves to `fundid`, not
  `fundi` — the trap in task 1.
- Extension: install with a stale `pic-helpers/` present → warning printed, old
  directory still on disk, `fundi-helpers/` created, pi starts with no duplicate
  slash commands.
- No `gofmt -w` on whole directories: this repo carries pre-existing gofmt debt
  in files unrelated to the change, and a directory-wide format pulls that churn
  into the diff. Format named files only.

## Risks

- **Biggest:** the service auto-detect looking for the wrong binary after the
  daemon rename (task 1). It fails silently — `install` succeeds and points at
  the client, which then fails at launch. The verification step above exists
  specifically for it.
- Renaming two Go packages at once invites import-cycle-looking noise from gopls
  in this workspace. gopls diagnostics here are unreliable; trust
  `go build ./...` and `go vet ./...` only.
- `attach/` is a bun bundle — its rename is independent of the Go move and can be
  deferred if it fights back, as long as the Makefile target and README agree.
