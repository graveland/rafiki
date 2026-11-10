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
| `^G` | Toggle the help page: every key with a sentence on what it does. `PgUp`/`PgDn`, `home`/`end` scroll it when it does not fit |
| `^O` | Expand tool arguments in full rather than folded |
| `^Y` | Toggle image thumbnails (Kitty-graphics terminals only) |
| `^T` | Expand the messages the daemon and the harness injected — drawn short by default (see below) |
| `^C`/`^D` | Quit — press the same one twice within two seconds. Children keep running; reattach any time |

**Input pane**

| Key | Does |
|---|---|
| `⏎` | Send. Against an agent that is mid-turn the message joins the running turn; otherwise it starts one |
| `^Q` | Queue — run this as the next turn once the current one ends, instead of joining it (`⌃⏎` where the terminal reports it) |
| `⇧⏎` / `^J` | Insert a newline (`⇧⏎` needs a Kitty-keyboard-protocol terminal — Ghostty, Kitty, WezTerm, recent iTerm2/Alacritty; `^J` works everywhere) |
| paste | Over six lines or 800 characters folds to `[pasted #1: 40 lines]`; paste again to insert in full |
| `^U` | Kill the cursor's line; on an empty line, remove the line above, so repeating it unrolls the input |
| `⌥K` | Clear the whole input, folded pastes included |
| `PgUp`/`PgDn`, `home`/`end` | Scroll the transcript without leaving the input box |
| `↑` / `↓` | Move the cursor; with nowhere to move, scroll the transcript |

The key picks the mode; nothing is inferred from agent state. `⏎` sends a
`STEER`, and the daemon resolves it — inject into the turn that is running, or
run it as the next turn when none is (`fundi.HandleSteerWithAttachments`). That
is what makes `⏎` mean "send, or steer if busy" with no window in which a stale
status can turn it into a queued message instead. `^Q` sends a `PROMPT`, which
is queueing and nothing else.

The distinction is real for a **fundi** child mid-turn, and only there. A
`claude` child's prompt and steer both encode to the same stdin user message
(`ClaudeProvider.EncodeOutbound`), so the mode makes no difference on that path
— a claude turn cannot be steered with text, and both keys queue. A **script**
child sees the `mode` on the message it pulls from its receive stream, and can
treat the two differently if it wants to.

A line that begins with a registered `/command` is not a message at all: it goes
as a `PROMPT` whichever key was pressed (`slashOverride`), because the daemon
only classifies a command in `PROMPT` mode. Otherwise `⏎` on `/exit` would
deliver the literal text to the model rather than killing the child. `esc` is
unaffected — it still stops the turn.

**Rail pane**

| Key | Does |
|---|---|
| `↑` / `↓` | Move the cursor — the conversation pane previews the highlighted agent, live: once the cursor rests, that agent's own event stream takes over (the previously open one resumes when you come back) |
| `⏎` | Open the selected agent and return to the input box |
| `esc` | Back to the input box, leaving focus where it was |

The rail mirrors `rafiki list`, so it also follows changes made elsewhere: a
child closed by another cockpit, by `rafiki forget`, or by the daemon's own
grace reaper disappears from it. The daemon publishes a `ChildClosed` event for
that, and the rail answers it by re-listing rather than patching the one row — a
close cascades, and `ListChildren` is what membership means. A fifteen-second
re-list runs as a backstop, so against a daemon older than that event a row can
linger up to that long. A row that merely *exited* stays until you close it.

## Reading the transcript

Follows new output at the bottom, holds position when scrolled back; sending
returns you to the bottom. A bottom-right readout (`↓ 1840/2272 81%`) shows
position. Three weights make it scannable: solid `▌` gutter for the agent's
own prose, dotted `┊` for thinking, none for tool calls — which show their
argument (`⚒ bash go test ./...`) and truncate output from the *end* (where
the error usually is). No mouse support, deliberately — capturing it would
take away your terminal's own select-and-copy.

A solid `▌` bar marks the messages nothing human typed: the agent's own
harness's reminders (a heartbeat nudge, an environment update, a file-changed
notice) and the messages rafiki's daemon injects. It is the user message's bar
in a dark grey rather than the user's cyan — the shape says "something was said
to this agent", the weight says "and it was not you who said it".

The KIND gets a title row of its own (`▌ ⟳ daemon restart`), in magenta —
a colour used nowhere else in the transcript — with the urgent kinds in the
warning colour. That row identifies the block before a word of the body is
read, which is why the body can stay in ordinary text colour instead of being
dimmed into illegibility. The bar runs unbroken through the padding row below
the block, so the message reads as one unit.

They are always drawn. Hiding them by default was wrong: a reader who cannot
see that the daemon spoke cannot tell why the agent did what it did. They are
drawn SHORT instead — three rows, then `[+N lines]` — and `^T` expands every
injected message to its full text, the same two-level shape `^O` gives tool
arguments. Both keys are named in the footer. Each kind carries its own glyph
and names itself on the first row, expanded or not, so a restart is not a job
exit:

| Glyph | Kind |
|---|---|
| `⟳` | the daemon restarted under a working agent (in the warning colour) |
| `⏸` | an upstream rate limit paused a turn (warning colour) |
| `⊘` | a budget breach or a raised budget (warning colour) |
| `⚑` | a subagent settled |
| `♥` | a periodic "still working" check-in |
| `▣` | a background job exited |
| `◷` | a schedule fired |
| `▶` | a script child reported |
| `✉` | a third-party notification |
| `⌁` | an executor-side condition |
| `↳` | anything else — a harness reminder, a context update, a kind this build does not know |

The level of detail is a view decision: the messages stay in the transcript
already fetched, so the toggle costs no refetch, and scrolling and the position
readout are unaffected either way. A transcript whose only blocks are injected
is content, not an empty conversation, and is drawn as such.

## Unacknowledged sends

A send to a **claude** child moves through three states, and never through a
bright duplicate:

- **pending** — a grey-ground `▌` ghost in the overlay above the transcript
  (`Cockpit.optimistic`, `pkg/tui/optimistic.go`): rafiki still owns delivery.
  The ledger keeps the local attempt id, the original text and images, and the
  inbox id `Send` returns.
- **accepted** — the durable `message_delivered` receipt naming that inbox id
  removes the ghost and puts the same text INTO the transcript as a provisional
  user block with a muted `✓ delivered` note (`Session.InsertProvisional`). It
  draws in the ordinary user style and holds the position its captured message
  will occupy, so the reply already streaming stays BELOW the prompt instead of
  the prompt jumping under it. Claude Code's UUID echo is the custody handoff;
  capture is not required.
- **permanent** — a capture the daemon linked (`UserMessage.message_id`, see
  `docs/reference/control-protocol.md`) replaces that block IN PLACE
  (`applyUserMessage`'s provisional branch) and the ledger row retires.

A send the receipt accepted, whose linked capture never arrives, is dropped at
its turn's end: the turn that answered it is over, so the row it stood for is
not coming. Both event feeds carry `turn_end`, and the rule differs per feed
because they are unordered — the focus feed's ends the accepted send, while the
rail feed's ends one only for a child that is NOT the focused one, since the
focused child's linked capture must not be raced out of its block. A child's
EXIT is that same ending under the same per-feed rule — `Session.applyPayload`'s
`Event_ChildExited` case drops the provisional blocks, and the rail's
`endAcceptedSends` covers an unfocused child — because a claude child publishes
`turn_end` only from its result frame and a mid-turn kill or crash publishes
none, which would otherwise leave a `✓ delivered` row with no exit at all. Switching
away and back re-inserts every accepted send: a `GetHistory` read never carries
`message_id`, so the fetched transcript cannot hold the block and the ledger
rebuilds it, to be replaced in place when the capture lands.

Text and history never acknowledge a claude send. A receipt that beats its
`Send` response is buffered until the inbox id can be joined to its local
attempt. Receipts also ride the rail feed, so an unfocused child's send is
accepted too. A send made before the rail knows the child's kind uses the
receipt path conservatively and can linger without a receipt.

A **fundi** child is unchanged: its ghost retires when its persisted user row
arrives, matched on text. Pinned by
`TestClaudeDeliveryEchoMovesTheSendIntoTheTranscript`,
`TestClaudeDeliveryLinkedCaptureRetiresTheAcceptedSend`,
`TestClaudeDeliveryHistoryRefetchReinsertsAcceptedSends`,
`TestClaudeDeliveryRailTurnEndDropsAnUnfocusedChildsAcceptedSend`,
`TestClaudeDeliveryRailTurnEndSparesTheFocusedChild`,
`TestClaudeDeliveryChildExitEndsTheFocusedChildsAcceptedSend`,
`TestClaudeDeliveryRailChildExitDropsAnUnfocusedChildsAcceptedSend`,
`TestClaudeDeliveryLedgerDeliverIsIdempotent`,
`TestClaudeDeliveryFundiKeepsItsTextAcknowledgement` and
`TestClaudeDeliveryReceiptDoesNotAcknowledgeFundi` (`pkg/tui`), and by
`TestProvisionalUserBlockHoldsItsPlaceAboveTheReply`,
`TestTurnEndDropsAnUnlinkedProvisional` and
`TestLinkedCaptureAfterASettleReopensTheWatermark` (`pkg/tui/session`).

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
