# Fundi M1 — Live Smoke Gate (Task 18)

**This is the user-driven acceptance gate.** All M1 code is complete and reviewed;
this is the real-model, real-daemon, Zoe-end-to-end verification the automated
tests can't do. Record results inline as you go.

Branch state (nothing pushed except rafiki):
- `~/home/fundi` @ `m1-agent-runtime` (9c3409e) — the runtime + daemon `kind=agent`.
- `~/home/rafiki` main (0e2a26e) — provider/model routing; already pushed.
- `~/home/sentinel-plugins` @ `fundi-agent-kind` (5b7f9e3) — pi-plugin `agent` kind; not pushed.

Model ids are now **single provider-qualified knobs**: `anthropic/sonnet-latest`,
`deepseek/deepseek-chat`. There is no `--provider`. `anthropic/…` → native
Anthropic SDK; anything else → OpenRouter. An unset/bare model errors.

## Steps

- [ ] **1. Build + install.** `cd ~/home/fundi && go build -o ~/bin/fundi ./cmd/fundi`
  (or the repo install target). Restart the daemon with the new binary. Confirm
  `pic` still talks to it and existing `claude`/`pi` children are unaffected.

- [ ] **2. Real-model spawn (native Anthropic).** `ANTHROPIC_API_KEY` in the daemon env.
  `pic spawn --kind agent --model anthropic/sonnet-latest --cwd /tmp/fundi-smoke`
  Prompt: "Create hello.txt containing 'hi', then run `wc -c hello.txt` and report
  the byte count." Verify: file exists; frames render live in `pic attach`;
  the per-turn usage frame carries non-zero tokens.

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

- [ ] **8. Zoe end-to-end.** From Zoe: `subagent_spawn` with `kind=agent` +
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
- **fundi** `m1-agent-runtime` → merge to fundi main when smoke passes.
- **sentinel-plugins** `fundi-agent-kind` → its own PR/merge (separate repo).

## Deferred follow-ups (tracked, non-blocking — see .superpowers/sdd/progress.md)
- Provider-as-configured-backend-registry (the agreed post-M1 direction; design
  doc has it). Provider stays legacy-only for pi/claude until then.
- Fork-wide pre-existing `gofmt` drift (16 files, predates M1) — separate sweep.
- rafiki `agentloop TestConcurrentResume` pre-existing flake (~1-3/10).
