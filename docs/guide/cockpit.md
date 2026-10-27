# The cockpit and web UI

A bubbletea program built into the `rafiki` binary itself, speaking Connect —
no separate build step. It reaches the same daemon every other `rafiki` verb
does (the resolved profile):

```bash
rafiki profile add prod --url https://rafiki.example.net --token "$TOKEN"
rafiki -P prod attach
```

## Entry points

| Command | Opens | Rail subscribes to |
|---|---|---|
| `rafiki create …` | the new child, full width | its subtree, plus itself |
| `rafiki attach <id\|name>` | that child, full width | its subtree, plus itself |
| `rafiki attach`, or bare `rafiki` | the rail, nothing focused | everything you can see |

A session follows its own delegation — spawn implementers and they appear in
the rail because they're in the subtree. The rail is hidden until a second
agent exists; `^R`/`⇥` peek it, and `n` (a rail key) opens the spawn form.
With no children at all there is nothing to peek, so a bare attach, and `^R`
against an empty rail, open the spawn form directly instead. Attaching loads
the full conversation from the database first (`GetHistory`), then follows
the live event stream.

## Watching without the cockpit

`rafiki tail [id|name]` follows events as they happen — `rafiki tail [id]`
is `rafiki logs -f -n 20`. With a child it backfills the child's last 20
history events (`-n` tunes this; `-1` all, `0` none) and follows the live
stream, exiting when the child exits. With no child it streams from every
child you can see (lifecycle events only), starting now and running until
you stop it — a child spawned later appears with no reopening, exactly like
the rail. `--label k=v`/`--has-label k` narrow the fleet-wide stream to
children whose labels match; `--types` narrows to named event types,
`--all-types` widens to every type plus the ephemeral deltas, and `-r` emits
one protojson `Event` per line instead of rendering:

```
14:32:01  spawn   c_01ABC  impl-auth  parent=c_root
14:32:02  status  c_01ABC  impl-auth  spawning → streaming
14:33:14  turn    c_01ABC  impl-auth  cost=$0.0142 stop=END_TURN (1m12s)
14:33:14  status  c_01ABC  impl-auth  streaming → idle (1m12s)
14:40:02  exit    c_01ABC  impl-auth  code=0 (lifetime 8m01s)
```

Not a TUI — live events plus the bounded backfill, from everything you can
see or one child when named. `rafiki logs <id> -f` is the same stream for
one child with the full history first; `-r` on either emits raw protojson
events.

A SCRIPT child has no conversation, so its backfill reads the durable event
log instead of GetHistory — which is why script output works against a
REMOTE daemon too: the record is in the log, not in any local process. Its
`script_output` events (stdout verbatim, stderr prefixed `stderr| `) and
`script_report` events replay from ordinal 0
and the live stream resumes from there — the same record the cockpit's pane
replays for a script row. A live script child's status is `running` from
spawn to exit — output never moves it — and its `cost_usd` is its subtree's
spend, the fundi and claude children it spawned priced into it.

## Keys

Two focus targets — the input box and the agent rail — toggled with `⇥`.

**Global**

| Key | Does |
|---|---|
| `⇥` / `⇧⇥` | Toggle between input box and agent rail; reveals it if hidden |
| `⌥N` / `^PgDn` | Hop to the next agent that needs you |
| `⌥P` / `^PgUp` | Hop to the previous agent that needs you |
| `^↑` / `^↓` | Hop up/down the rail without changing pane |
| `esc` / `^X` | Abort the running turn |
| `^R` / `^B` | Collapse/restore the agent rail; with one agent, peek it |
| `^G` | Toggle the help overlay |
| `^C`/`^D` | Quit — press the same one twice within two seconds. Children keep running; reattach any time |

**Input pane**

| Key | Does |
|---|---|
| `⏎` | Send a prompt — queues work for the agent |
| `⇧⏎` / `^J` | Insert a newline (`⇧⏎` needs a Kitty-keyboard-protocol terminal — Ghostty, Kitty, WezTerm, recent iTerm2/Alacritty; `^J` works everywhere) |
| paste | Over six lines or 800 characters folds to `[pasted #1: 40 lines]`; paste again to insert in full |
| `^U` | Clear the whole input, folded pastes included |
| `⌥⏎` / `^S` | Steer — inject into the turn already running |
| `PgUp`/`PgDn`, `home`/`end` | Scroll the transcript without leaving the input box |
| `↑` / `↓` | Move the cursor; with nowhere to move, scroll the transcript |

Prompt and steer are separate keys rather than one that guesses from agent
state, since only you know whether you mean "queue this for later" or
"interrupt now."

**Rail pane**

| Key | Does |
|---|---|
| `↑` / `↓` | Move the cursor — the conversation pane previews the highlighted agent, live: once the cursor rests, that agent's own event stream takes over (the previously open one resumes when you come back) |
| `⏎` | Open the selected agent and return to the input box |
| `esc` | Back to the input box, leaving focus where it was |

## Reading the transcript

Follows new output at the bottom, holds position when scrolled back; sending
returns you to the bottom. A bottom-right readout (`↓ 1840/2272 81%`) shows
position. Three weights make it scannable: solid `▌` gutter for the agent's
own prose, dotted `┊` for thinking, none for tool calls — which show their
argument (`⚒ bash go test ./...`) and truncate output from the *end* (where
the error usually is). No mouse support, deliberately — capturing it would
take away your terminal's own select-and-copy.

## Activity and attention

```
◌ spawning   ○ idle        ◐ streaming   ▶ running     ⚒ running a tool
⊛ compacting ‼ needs you   ◇ stopping    ⟳ retrying    ✓/✗ exited
⧖ batch_wait (static — hours-scale work already submitted to the provider)
```

`▶ running` is a script child's whole run: the process is alive from spawn to
exit (a script never streams), and it spins while the run is going.

The badge counts only events worth a human (a finished turn, going idle,
blocking on you, an error, a child exiting) — an agent merely working shows a
glyph and no badge. Retries aren't badged (automatic retry means a
recoverable stream error isn't your problem), but the `⟳` glyph keeps one
from looking identical to progress.

## The status line and footer

The status line above the input carries the daemon's state for the focused
agent on the left and, on the right, your Anthropic subscription's rate-limit
snapshot: `5h 94% (resets in 3h2m)  7d 60% (resets in 2d1h)`. Each window
carries its own countdown, because the two reset on very different clocks and
a bare percentage leaves "do I wait it out or route elsewhere" as arithmetic.
A window whose reported reset has already passed prints the percentage alone —
the snapshot predates the rollover, so a countdown there would be a lie. The
whole segment hides rather than showing a stale number once the poll lags by
more than `quotaStaleAfter`, and hides entirely for an account that has never
made a passthrough call. `rafiki claude --limits` prints the same data
verbosely.

The footer's right edge is, in order: scroll position, context usage, spend,
and the focused agent's model. The model is disambiguated by KIND, because a
model id alone does not name the runtime — `anthropic/claude-opus-5-5` is a
fundi child the daemon routes to that provider *and* a claude-kind child
running the Claude Code binary against your subscription, and those bill and
fail differently. A claude child therefore always reads
`claude/anthropic/claude-opus-5-5`; a fundi child needs no prefix while its id
starts with a provider (`openrouter/…`, `anthropic/…`) and gets `fundi/` when
it does not (`fundi/glm-5.2`). An agent row whose kind was never reported
shows the bare id rather than a guess.

## The web UI

**The web UI is a third client, served by the `rafiki` binary.** `rafiki web [--port N] [--no-open] [--dev-assets URL]` serves a browser UI from the `rafiki` binary itself: a `127.0.0.1`-only reverse proxy in front of the daemon's Connect control plane (rafikid never serves the UI). It prints a single-use link (`http://127.0.0.1:<port>/#k=<code>`) and opens it; the fragment code is exchanged for a per-tab session secret held in `sessionStorage` and sent as `X-Rafiki-Web` — no cookies, no CORS headers. A Connect `Unauthenticated` on any call the localhost gate did not refuse triggers in-browser sign-in through the daemon's OIDC login (a popup). The proxy forwards `/rafiki.v1.Control/*` verbatim — its own upstream transport adds the bearer and the protocol epoch — and never `/rafiki.v1.Login/*`. On a remote machine, forward the port with `ssh -L <port>:127.0.0.1:<port>`. See `docs/reference/control-protocol.md` §"Browser clients".

`make web` builds the UI into `pkg/webui/dist` and embeds it into the `rafiki` binary; a binary built without it refuses `rafiki web` (the one exception is `--dev-assets`, below). `make check-web` typechecks, lints and tests the UI under `web/`; both targets need node + pnpm + protoc on `PATH`. Dev loop: run `pnpm dev` in `web/`, then `rafiki web --dev-assets http://localhost:5173` to reverse-proxy static assets to the Vite dev server.
