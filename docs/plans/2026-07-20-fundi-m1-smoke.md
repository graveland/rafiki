# Fundi M1 — Live Smoke Gate (Task 18)

**This is the user-driven acceptance gate.** All M1 code is complete and reviewed;
this is the real-model, real-daemon, Zoe-end-to-end verification the automated
tests can't do. Record results inline as you go.

Branch state (updated 2026-07-27):
- `~/home/fundi` **main** @ `abd4702` — M1 merged linearly; branch + worktree deleted. NOT pushed (40 commits ahead).
- `~/home/rafiki` main @ `a12fdbb` — pushed.
- `~/home/sentinel-plugins` @ `fundi-agent-kind` (5b7f9e3) — pi-plugin `agent` kind; not pushed.

**fundi is now a SEPARATE daemon from pi-controller, not a replacement for the
running one.** It listens on its own socket and has its own service identity, so
both run at once — do NOT stop pi-controller for this gate:

| | fundi | pi-controller |
|---|---|---|
| socket | `~/.local/state/fundi/controller.sock` | `~/.pi/run/controller.sock` |
| service | `dev.graveland.fundi` | `dev.graveland.pi-controller` |

`pic` built from THIS repo defaults to fundi's socket. A `pic` from elsewhere on
`$PATH` will talk to pi-controller — check which one you are running. Override
with `FUNDI_SOCKET` (the old `PI_CONTROLLER_SOCKET` still works but warns).

Model ids are now **single provider-qualified knobs**: `anthropic/sonnet-latest`,
`deepseek/deepseek-chat`. There is no `--provider`. `anthropic/…` → native
Anthropic SDK; anything else → OpenRouter. An unset/bare model errors.

## Steps

- [ ] **1. Build + start.** `cd ~/home/fundi && make build-controller build-pic`.
  Start `./bin/fundi` in a terminal (foreground is easiest for this gate — no
  service install needed). Confirm the log says **"fundi daemon listening"** on
  the XDG socket, that `./bin/pic ls` reaches it, and that the running
  pi-controller and its children are untouched.

- [ ] **2. Real-model spawn (native Anthropic).** `ANTHROPIC_API_KEY` in the daemon env.
  `pic spawn --kind agent --model anthropic/sonnet-latest --cwd /tmp/fundi-smoke`
  Prompt: "Create hello.txt containing 'hi', then run `wc -c hello.txt` and report
  the byte count." Verify: file exists; frames render live in `pic attach`;
  the per-turn usage frame carries non-zero tokens **and non-zero cost**.

  > Cost is the one thing no automated test can prove: pricing resolves the
  > served model id against OpenRouter's live catalog, so a stub pricer verifies
  > the arithmetic but not that the model has a catalog entry. An unpriced model
  > reports cost 0 by design, which looks identical to a free turn — so check for
  > a non-zero `cost.total` on `agent_end` explicitly.

- [ ] **3. Steer mid-turn.** Give a multi-step task, `pic send --steer` an extra
  instruction while a turn is in flight. Verify the steer lands within the turn.

- [ ] **4. In-band abort (the keystone, live).** Abort mid-turn. Verify the turn
  settles WITHOUT a process restart (`pic get` → same PID, status idle) and a
  follow-up prompt works on the same child. (Automated `-race` proof exists; this
  confirms it against a real model.)

- [ ] **5. Skill + cheap OpenRouter worker.** Drop a test `SKILL.md` under the cwd's
  `.claude/skills/`. `OPENROUTER_API_KEY` in the daemon env.
  `pic spawn --kind agent --model deepseek/deepseek-chat` and prompt it to use the
  skill. Verify: skill inventory in the system prompt + invocation works, AND the
  OpenRouter routing path works (this exercises the non-anthropic model branch).

- [ ] **6. Resume (DB mode).** With `--db` / `FUNDI_AGENT_DB` set, spawn an agent,
  run a turn, kill it mid-tool, `ctrl_resume` it. Verify it reattaches the same
  conversation and the boot-time orphan repair leaves a clean history (no API
  error on the next turn). (Automated DB test covers the mechanism; confirm live.)

- [ ] **7. MCP (optional but recommended).** Point `--mcp-config` at a real
  `.mcp.json` with a server that exposes a tool with a `.` in its name (e.g. a
  github MCP). Verify the tool registers as `mcp__server__github_create_issue`
  (dot normalized to `_`) and is callable — this is the T13 fix that only got
  its independent review post-hoc; a real-server check is worth it.

- [ ] **8. Zoe end-to-end.** First point the pi-plugin entry's `socket` config at
  fundi: `~/.local/state/fundi/controller.sock`. Its default still resolves to
  pi-controller's socket, and since BOTH daemons are alive it will silently talk
  to the wrong one. Then from Zoe: `subagent_spawn` with `kind=agent` +
  `model=anthropic/sonnet-latest`. Verify the `_fleet` line and signal batching
  behave as with claude children.

- [ ] **9. pic rendering eyeball.** `go-runewidth` jumped 0.0.16→0.0.23 (several
  width-table releases) as a DIRECT dep of `cmd/pic/render_tail.go` with no test.
  Eyeball `pic attach` / `pic tail` rendering (wide chars, box drawing) for
  regressions.

- [ ] **10. Record results here; commit this doc.** Then the **default-kind flip**
  (`default_kind: "agent"` in the pi-plugin config) is a deliberate decision to
  make AFTER this gate passes — do NOT flip it in code.

## Merge decisions (after smoke passes)
- **rafiki** (0e2a26e): already on main + pushed. It's backward-compatible and
  upstream-bound; consider the upstream PR separately. One rafiki follow-up noted:
  malformed bare `anthropic/` (nothing after) resolves to "" instead of erroring.
- **fundi**: already merged to main (linear). Remaining after this gate: push
  main, merge sentinel-plugins `fundi-agent-kind`, then the default-kind flip.
- **sentinel-plugins** `fundi-agent-kind` → its own PR/merge (separate repo).

## Deferred follow-ups (tracked, non-blocking — see .superpowers/sdd/progress.md)
- Provider-as-configured-backend-registry (the agreed post-M1 direction; design
  doc has it). Provider stays legacy-only for pi/claude until then.
- Fork-wide pre-existing `gofmt` drift (16 files, predates M1) — separate sweep.
- rafiki `agentloop TestConcurrentResume` pre-existing flake (~1-3/10).
