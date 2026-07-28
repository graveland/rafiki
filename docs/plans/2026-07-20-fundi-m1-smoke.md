# Fundi M1 — Live Smoke Gate (Task 18)

**This is the user-driven acceptance gate.** All M1 code is complete and reviewed;
this is the real-model, real-daemon, Zoe-end-to-end verification the automated
tests can't do. Record results inline as you go.

Branch state (updated 2026-07-27, post-rename):
- `~/home/fundi` **main** @ `7f9dc62` — M1 merged linearly; branch + worktree deleted.
  NOT pushed: 18 commits ahead of `local/main` (`de782df`). The gitea remote is
  named **`local`**, not `origin`.
- `~/home/rafiki` main @ `a12fdbb` — pushed (`origin/main`).
- `~/home/sentinel-plugins` @ `fundi-agent-kind` (5b7f9e3) — pi-plugin `agent` kind; not pushed.

**fundi is now a SEPARATE daemon from pi-controller, not a replacement for the
running one.** It listens on its own socket and has its own service identity, so
both run at once — do NOT stop pi-controller for this gate:

| | fundi | pi-controller |
|---|---|---|
| socket | `~/.local/state/fundi/controller.sock` | `~/.pi/run/controller.sock` |
| service | `dev.graveland.fundi` | `dev.graveland.pi-controller` |
| daemon | `fundid` | `pi-controller` |
| client | `fundi` | `pic` |

The binaries no longer collide, so there is nothing to disambiguate: a `pic` on
`$PATH` is pi-controller's client, and fundi's is `fundi`. Override the socket
with `FUNDI_SOCKET` (the old `PI_CONTROLLER_SOCKET` still works but warns).

Model ids are now **single provider-qualified knobs**: `anthropic/sonnet-latest`,
`deepseek/deepseek-chat`. There is no `--provider`. `anthropic/…` → native
Anthropic SDK; anything else → OpenRouter. An unset/bare model errors.

## Steps

- [x] **1. Build + start.** `cd ~/home/fundi && make build-daemon build-cli`.
  Start `./bin/fundid` in a terminal (foreground is easiest for this gate — no
  service install needed). Confirm the log says **"fundi daemon listening"** on
  the XDG socket, that `./bin/fundi ls` reaches it, and that the running
  pi-controller and its children are untouched.

  > **PASS** (2026-07-27). Build clean. Retired the stale `fundid` left over from
  > the previous session (pid 94643 — it was a pre-rename build, so not a valid
  > gate subject); it removed its own socket on exit. Fresh daemon pid 2552 logged
  > `msg="fundi daemon listening" socket=/Users/brent/.local/state/fundi/controller.sock`
  > plus `loaded orphans count=0`. `fundi list` → `{"children": []}`.
  > pi-controller (pid 1502) stayed up and launchd-registered throughout — the
  > two-daemon split holds in practice, not just on paper.
  > Keys came from rafiki's `.env*` copied into the repo; `.gitignore` gained
  > `.env` + `.env-*`, and all four dotfiles verify as ignored.

- [x] **2. Real-model spawn (native Anthropic).** `ANTHROPIC_API_KEY` in the daemon env.
  `fundi create --kind agent --model anthropic/sonnet-latest --cwd /tmp/fundi-smoke`
  Prompt: "Create hello.txt containing 'hi', then run `wc -c hello.txt` and report
  the byte count." Verify: file exists; frames render live in `fundi attach`;
  the per-turn usage frame carries non-zero tokens **and non-zero cost**.

  > Cost is the one thing no automated test can prove: pricing resolves the
  > served model id against OpenRouter's live catalog, so a stub pricer verifies
  > the arithmetic but not that the model has a catalog entry. An unpriced model
  > reports cost 0 by design, which looks identical to a free turn — so check for
  > a non-zero `cost.total` on `agent_end` explicitly.

  > **PASS — cost resolves live.** `fundi create smoke2 --detached --kind agent
  > --model anthropic/sonnet-latest --cwd /tmp/fundi-smoke` → child
  > `c_01KYJJCTPW3X0D5W6YGSQY52DJ`, pid 65048, session `mem-…` (in-memory; no
  > `FUNDI_AGENT_DB`). `hello.txt` created, 2 bytes; agent reported "2 bytes".
  > 313 frames streamed live over `fundi tail`.
  > **`agent_end.usage.cost.total = 0.0253922`** — non-zero, with a full
  > breakdown (`cacheRead` 0.0017992, `cacheWrite` 0.022695, `input` 0.000008,
  > `output` 0.00089) over 18167 tokens. `anthropic/sonnet-latest` resolved to
  > served id **`claude-sonnet-5`** and that id HAS an OpenRouter catalog entry,
  > which is the thing no stub pricer could prove.
  > Bonus: labels carry the renamed `fundi/` prefix (`fundi/kind=agent`,
  > `fundi/model=sonnet-latest`, `fundi/provider=anthropic`).

- [x] **3. Steer mid-turn.** Give a multi-step task, send a steer frame with an extra
  instruction while a turn is in flight. Verify the steer lands within the turn.

  > ⚠️ **This step's syntax was wrong: there is no `fundi send --steer` flag.**
  > `fundi send` takes a raw pi-RPC frame, and steer is a frame *type*
  > (`internal/agent/frontend.go:129`). The real invocation is
  > `fundi send <name> '{"type":"steer","message":"…"}'` (and abort is
  > `'{"type":"abort"}'`). Corrected above.
  >
  > **PASS.** Sent a 5-file sequential task, then injected the steer 6s in while
  > `fundi get` reported `status: streaming`. Proof it landed *in-turn* rather than
  > being deferred to a new turn, read off the frame ordering: `agent_start` at
  > L312 → **steer text at L736, inside that turn** → a5.txt work continues at
  > L758 → `printf 'STEERED' > steered.txt` at L861 → final summary at L963 → one
  > closing `agent_end`. No second `agent_start`. (The other `agent_start`/`_end`
  > pair at L35/L266 is step 2's turn replayed from the ring buffer — worth knowing,
  > since a naive `grep -c agent_start` reads as "2 turns" and looks like a failure.)
  > The model's own summary: "steered.txt (STEERED): 7 bytes, created per your
  > change-of-plan request". That is the `PendingUser` injection seam working live.

- [x] **4. In-band abort (the keystone, live).** Abort mid-turn. Verify the turn
  settles WITHOUT a process restart (`fundi get` → same PID, status idle) and a
  follow-up prompt works on the same child. (Automated `-race` proof exists; this
  confirms it against a real model.)

  > **PASS — no restart, and history stayed coherent.** Long 40-step task, aborted
  > 7s in while `status: streaming`. **pid 65048 before the abort, after it, and
  > after the following turn**; `status` settled to `idle`, `exitCode` null.
  > The follow-up prompt worked on the same child — and the stronger evidence is
  > *what* it answered: asked what the last number it echoed was, it said
  > **"The last number I successfully echoed was 2."** That is only answerable from
  > the aborted turn's own history, so the abort left a well-formed conversation
  > (no orphaned `tool_use` without its `tool_result`, no API error on the next
  > turn) — which is the failure mode a process-restart abort was hiding.

- [x] **5. Skill + cheap OpenRouter worker.** Drop a test `SKILL.md` under the cwd's
  `.claude/skills/`. `OPENROUTER_API_KEY` in the daemon env.
  `fundi create --kind agent --model deepseek/deepseek-chat` and prompt it to use the
  skill. Verify: skill inventory in the system prompt + invocation works, AND the
  OpenRouter routing path works (this exercises the non-anthropic model branch).

  > **PASS on both technical claims — but the step conflates two things, and the
  > cheap model is the weak link.** Test skill `zamboni-report` (writes
  > `ZAMBONI-OK-4711` to `zamboni.txt`, replies `zamboni report filed`) under
  > `/tmp/fundi-smoke/.claude/skills/`.
  >
  > - **OpenRouter routing works.** `deepseek/deepseek-chat` ran a real turn,
  >   bootstrap `get_state` reported `{"id":"deepseek-chat","provider":"deepseek"}`,
  >   the write-file tool worked ("wrote 199 bytes").
  > - **OpenRouter pricing works** — the *other* half of step 2's risk, on a
  >   different code path: `cost.total = 0.0012343765`, non-zero, plain
  >   input/output with no cache tiers (vs anthropic's cacheRead/cacheWrite).
  > - **Skill discovery + invocation works.** Proven with a sonnet child
  >   (`smoke5b`) in the *same* cwd: the skill tool injected the SKILL.md body
  >   prefixed `Base directory for this skill: …/.claude/skills/zamboni-report`,
  >   wrote `zamboni.txt` = `ZAMBONI-OK-4711` (15 bytes), and replied exactly
  >   `zamboni report filed`.
  > - ⚠️ **`deepseek-chat` never reached for the skill.** Asked to "file a zamboni
  >   report" it improvised its own `zamboni_report.md` with placeholder prose,
  >   ignoring the inventory. Not a fundi bug — the same code path obeys under
  >   sonnet — but worth carrying into **M2's spec-density experiment**: this is a
  >   concrete instance of a DeepSeek-tier worker discarding supplied structure and
  >   inventing its own, which is exactly the thrash M2 wants to measure as "worker
  >   tokens per accepted leaf." Budget for weak workers needing the skill named
  >   explicitly rather than discovered.
  >
  > Note when re-running: a child's skill inventory is built **at spawn time**, so
  > a `SKILL.md` dropped after spawn is invisible to an already-running child.

- [x] **6. Resume (DB mode).** With `--db` / `FUNDI_AGENT_DB` set, spawn an agent,
  run a turn, kill it mid-tool, `ctrl_resume` it. Verify it reattaches the same
  conversation and the boot-time orphan repair leaves a clean history (no API
  error on the next turn). (Automated DB test covers the mechanism; confirm live.)

  > 🐛 **FOUND A REAL BUG — `ctrl_resume` was broken for EVERY agent-kind child.**
  > This is the step that justified the gate. Fixed in this same pass; PASS after.
  >
  > ```
  > error: ctrl_resume: spawn_failed: spawn plan: agent kind does not accept a
  > separate Provider: fold it into a provider-qualified Model instead
  > ```
  >
  > **Root cause.** At spawn the controller runs `splitModel` on the child-reported
  > model and stores the halves *separately* (`snap.Provider="anthropic"`,
  > `snap.Model="sonnet-latest"`). The agent kind requires the opposite shape —
  > provider folded into `Model`, `Provider` empty — and `resolveSpawnPlan`
  > *deliberately hard-rejects* a non-empty `Provider` rather than dropping it
  > silently. But `resumeRequestFromSnapshot` copied both fields across verbatim
  > (`controller.go:777`), so every agent-kind resume fed the planner exactly the
  > shape it is written to refuse. Fix: rejoin via the existing `joinModel` helper
  > and clear `Provider`.
  >
  > **Why the test suite missed it, which is the more useful lesson.**
  > `TestResolveSpawnPlanAgentKindRejectsProvider` tested the *rejection*, and
  > there were resume tests for the `claude` and `pi` kinds — but none for `agent`.
  > Both halves looked covered while the seam between them was broken. Added
  > `TestResumeRequestFromSnapshotAgentRejoinsModel`, whose load-bearing assertion
  > is that the rebuilt request is one `resolveSpawnPlan` *accepts* (implementation
  > -agnostic, so it survives a refactor), plus a bare-model case so `joinModel`
  > can't regress into emitting a leading `/`.
  >
  > **PASS after the fix**, verified live end to end:
  > - Hard `kill -9` mid-tool (not a graceful stop — that's what orphans a
  >   `tool_use` with no `tool_result`). Daemon restart then logged
  >   **`loaded orphans count=1`** (was 0), so boot-time repair saw it.
  > - `fundi resume smoke6 --detached` returned the **same `childId` AND the same
  >   `sessionId` `019fa52d-e403-77d8-9beb-98baccc6e7b4`** — reattached, not
  >   re-created. Reattach is by external ref: `fundid agent --ref` defaults to
  >   `$FUNDI_CHILD_ID` and resume reuses the childID, so `ResumeSession` is
  >   correctly empty for this kind (there is no pi session file). DB row confirms:
  >   `external_ref = c_01KYJJVRTF5JEJS494W4MZZ4AD`, UNIQUE on
  >   `(external_ref, driven_by)`.
  > - **The next turn hit the API with no error** — the real proof the history was
  >   repaired, since a dangling `tool_use` would have drawn a 400. It also read
  >   across the kill boundary: "Before I was interrupted, I completed only the
  >   first command." `conversation_turn` went 2 → 3 on the *same* conversation id.
  > - `resume_attempts` stays 0, which is correct — that counter is rafiki's
  >   *in-loop* turn recovery (`agentloop.go:180`, reset on success), not a
  >   child-respawn count. Don't read it as a missed bump.
  >
  > **Setup note for re-runs:** `FUNDI_AGENT_DB` must be in the **daemon's** env.
  > `fundi create --forward-env` (default true) forwards the *caller's* env to the
  > child, but the agent kind is a daemon self-re-exec (`fundid agent`) and the
  > `-db` flag resolves from the daemon's own environment — a caller-side
  > `FUNDI_AGENT_DB` is silently ignored and you get an in-memory `mem-…` session.
  > The tell is the `sessionId`: `mem-…` = in-memory, a UUIDv7 = DB-backed.
  > Used rafiki's local `RAFIKI_DB` (`postgres://…@localhost:5432/rafiki`), which
  > is the right home — fundi's agent conversations live in rafiki's
  > `conversations` schema.

- [x] **7. MCP (optional but recommended).** Point `--mcp-config` at a real
  `.mcp.json` with a server that exposes a tool with a `.` in its name (e.g. a
  github MCP). Verify the tool registers as `mcp__server__github_create_issue`
  (dot normalized to `_`) and is callable — this is the T13 fix that only got
  its independent review post-hoc; a real-server check is worth it.

  > **PASS.** No github MCP was needed (and none was configured on this machine):
  > wrote a minimal but genuine stdio MCP server, `/tmp/fundi-smoke-mcp/server.py`
  > — newline-delimited JSON-RPC 2.0, real `initialize`/`tools/list`/`tools/call`
  > handshake against the official `modelcontextprotocol/go-sdk` client — exposing
  > exactly one tool named **`github.create_issue`**. A purpose-built fixture beats
  > a real github MCP here because it *guarantees* the dotted name the fix targets.
  > - Config auto-discovered from `<cwd>/.mcp.json`; no `--mcp-config` flag needed.
  > - Registered as **`mcp__smoketest__github_create_issue`** — dot → `_`.
  > - **Callable**: returned `MCP-DOT-OK issue created: smoke gate step 7`, which
  >   the child reported back verbatim.
  >
  > The fixture is under `/tmp` and so is disposable; recreate it from this
  > description if step 7 is ever re-run. Note the normalization path already has
  > substantial unit coverage (`internal/agent/tools/mcp_test.go`, ~16K, including
  > invalid-name and collision cases) — what this step adds on top is the
  > real-subprocess transport and handshake.

- [ ] **8. Zoe end-to-end.** First point the pi-plugin entry's `socket` config at
  fundi: `~/.local/state/fundi/controller.sock`. Its default still resolves to
  pi-controller's socket, and since BOTH daemons are alive it will silently talk
  to the wrong one. Then from Zoe: `subagent_spawn` with `kind=agent` +
  `model=anthropic/sonnet-latest`. Verify the `_fleet` line and signal batching
  behave as with claude children.

  > **Not attempted — needs you.** This one can't be driven headlessly: it needs a
  > config change in the running sentinel (pointing the pi-plugin entry's `socket`
  > at fundi) plus a Zoe conversation. Everything below step 8 is ready for it —
  > the daemon is up on the fixed binary and agent-kind spawn/resume both work.
  > Reminder of the trap from the header: the pi-plugin's default socket still
  > resolves to **pi-controller's** path, and pid 1502 is alive and answering, so a
  > missed config change doesn't error — it silently drives the wrong daemon.

- [ ] **9. CLI rendering eyeball.** `go-runewidth` jumped 0.0.16→0.0.23 (several
  width-table releases) as a DIRECT dep of `cmd/fundi/render_tail.go` with no test.
  Eyeball `fundi attach` / `fundi tail` rendering (wide chars, box drawing) for
  regressions.

  > **Not attempted — inherently visual, needs your eyes on a real terminal.**
  > `fundi tail --output json` was exercised heavily throughout steps 2-7 without
  > incident, but that path doesn't touch `render_tail.go`'s width tables at all,
  > so it is NOT evidence either way for this step.

- [ ] **10. Record results here; commit this doc.** Then the **default-kind flip**
  (`default_kind: "agent"` in the pi-plugin config) is a deliberate decision to
  make AFTER this gate passes — do NOT flip it in code.

  > Results for steps 1-7 recorded above (2026-07-27). Commit deliberately left to
  > you along with the resume fix — see the summary below.

## Gate outcome (2026-07-27)

**Steps 1-7 PASS. Steps 8-9 need the user. One real bug found and fixed.**

The gate earned its keep: **`ctrl_resume` was broken for every agent-kind child**
(step 6) and no automated test covered it, because the two halves either side of
the break were each tested separately. Everything else held up, including both
things flagged as highest-risk:

| Step | Result |
|---|---|
| 1 Build + start | PASS — two-daemon split holds; pi-controller untouched |
| 2 Cost (anthropic) | PASS — `cost.total` 0.0253922, live catalog hit on `claude-sonnet-5` |
| 3 Steer mid-turn | PASS — proven in-turn by frame ordering, not just by outcome |
| 4 In-band abort | PASS — same PID throughout, history stayed coherent |
| 5 Skill + OpenRouter | PASS on both technical claims; deepseek ignored the skill |
| 6 Resume (DB) | **BUG FOUND + FIXED**, then PASS |
| 7 MCP dot-normalization | PASS — `mcp__smoketest__github_create_issue`, callable |
| 8 Zoe end-to-end | not attempted (needs sentinel config + a Zoe conversation) |
| 9 Rendering eyeball | not attempted (inherently visual) |

Full suite after the fix: **36 package-runs ok, 0 failures**, in both `go.work` and
`GOWORK=off` modes — matching the pre-gate baseline.

Two pieces of documentation drift fixed in passing, both found by using the tool
rather than reading it:
- `fundi create --kind` help never listed `agent` — the kind this whole milestone
  added — while the daemon has handled it since `controller.go`'s `case "agent"`.
- `DiscoverSkills`' docstring claimed `<gitroot>/.claude/skills`; the caller
  actually passes `<cwd>`. Corrected to cwd, with a note on the consequence.

One thing worth carrying into M2 rather than filing as a bug: **`deepseek-chat`
ignored its skill inventory and improvised** (step 5). The identical code path
obeys under sonnet, so it's a model-capability result — and it's a concrete
instance of the thrash M2's spec-density experiment exists to quantify.

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
