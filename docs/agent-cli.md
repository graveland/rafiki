# `rafikid agent` CLI

`rafikid agent <verb>` is a DSN-backed CLI over the same `conversations`
schema the proxy captures into: read-only insights (`stats`/`search`/
`export`), the LLM-driven skill-gap detector (`analyze`), and finding
triage (`findings`). It talks to Postgres directly — no gRPC, no auth layer
— via `pkg/agentcli.Backend`, implemented today by `pkg/agentcli/local.Backend`.
See `cmd/rafikid/agent_cli.go` for the flag/dispatch code this doc describes.

Every subcommand accepts `-j` (indented JSON) or `-J` (compact JSON) instead
of the human-readable table/markdown render.

`stats`, `search`, and `export` have a socket-side twin in `rafiki
conversations <verb>`, which needs no DB credentials of its own. The two are
the same query — both reach `pkg/insights` through `local.Backend`, with no
`Pricer` on either side, so `cost_usd` is 0 for both — and every table on
both sides draws through the shared `pkg/table` renderer (lipgloss v2): one
style everywhere, single-line borders, dimmed headers when color is on, and
no width cap on a pipe, so no column is ever dropped. `rafikid agent` renders
through `pkg/agentcli` (`agentcli.Render` into `RenderStats`/`RenderSearch`/
`RenderTranscriptMD`); the socket twin renders through its pgx-free sibling
`pkg/conversationview`, extracted from `agentcli` so `rafiki` links no pgx.

The two differ in transport and in how the output mode is selected. Here it
is `-j` (indented JSON) / `-J` (compact single-line JSON, envelope included);
there it is the global `--output auto|json|jsonl` flag with `-j`/`-J`
shorthands — **a different contract**: table on a TTY and a pipe alike (no
TTY probe, no pipe→JSON rule), `-o json`/`-j` pretty JSON, and `-J`/`-o
jsonl` one compact record per line with any `{"rows": …}` envelope unwrapped
(`-j -J` together errors). The JSON *payload* still matches —
`ctrl_conversation_search` puts its rows in a `{"rows": [...]}` envelope on
the wire (control-protocol.md §6.18), and the client unwraps it before
printing, so `rafiki conversations search -o json | jq '.[]'` and `rafikid
agent search -J | jq '.[]'` iterate the same thing, as does `rafiki
conversations search -J`. `analyze` stays
`rafikid`-only — there is no wire verb for it, it needs an LLM client and
writes to the DB, and `rafiki` never holds a DSN. `findings` has a socket
twin now: `rafiki conversations findings` (the Connect plane's
`ConversationFindings`) serves the read half, while the `dismiss`/`action`
triage subcommands stay `rafikid`-only. Review submission runs the other way
— `rafiki conversations review` (its own section below) asks the daemon's
review worker to run this pipeline, and has no `rafikid agent` counterpart.

One thing the shared renderer cannot equalize: the two commands read whatever
DSN each was handed. `rafikid agent --db` defaults to your shell's `RAFIKI_DB`
then `RAFIKI_TEST_DSN`; `rafiki conversations` uses the daemon's, baked into
the service unit at install time. Differing *numbers* between them is a DSN
mismatch, not a rendering bug.

## `rafiki user`

The one non-`rafikid agent` command documented here, because `stats`/`search`'s
`--owner` filter takes a name straight out of `rafiki user list` and readers
need the source before the consumer. It talks to the daemon's control socket
(`ctrl_user_create`/`ctrl_user_list`/`ctrl_user_rm` — see
`docs/reference/control-protocol.md` §16), not Postgres directly, so it works
wherever `rafiki` itself works, DSN or none.

```
rafiki user create <name>   # mint a user; prints its token once
rafiki user list            # active users; --all also lists tombstoned ones
rafiki user rm <name>       # tombstone a user; its token stops working at once
```

`rafiki user create` is also how a fresh daemon gets its first identity: while
zero active users exist, the daemon is in bootstrap mode and `ctrl_user_create`
is the only command any listener accepts (see README's "First user" section).

- **`--no-write`** (create only): print the token but skip writing it to the
  current profile's token file (`~/.config/rafiki/profiles/<name>/token`,
  mode 0600). Without it, `rafiki user create` both mints the user AND logs
  this machine in as them — the plaintext token is shown exactly once either
  way, since the daemon stores only its digest and cannot show it again.
- **`--all`** (list only): include tombstoned (removed) users. Without it,
  `list` shows only active users — the ones a token could still authenticate
  as. A tombstoned user still resolves in historical conversation/turn
  attribution (`user rm` never deletes the row), so `--all` is what makes that
  history's names explicable.

Token storage moved to per-profile files with client profiles (2026-09): the
old global `~/.config/rafiki/token` (and the even older, already-unread
`~/.config/rafiki/control.token`) are no longer read at all — see README's
"Profiles" section for the schema and resolution order.

**This only bites on a remote (`https://`) daemon, not the local dev loop.**
`mustDial` (`cmd/rafiki/cli_helpers.go`) resolves the client's one profile
(`pkg/profile`) and dials it: a profile with a `url` presents that profile's
token file over TLS, a profile with a `socket` reads no token at all, because
UDS connections skip auth entirely and are never bootstrap-restricted. So a
stale token file cannot be why a *local* `rafiki user create` fails on a
fresh daemon — that path is structurally unaffected by the file's contents.
It genuinely bites during the `kubectl port-forward` first-user sequence: a
token file left over from a different (or wiped) remote daemon turns what
should be a no-credential bootstrap dial into an authenticated one, which
then fails `auth_invalid`. If `rafiki user create` unexpectedly refuses
against a remote daemon you know is fresh, check that profile's token file
before suspecting the daemon.

## `stats`

Global stats, or stats for one conversation if given a positional id.

```
rafikid agent stats
rafikid agent stats <conv-id>
rafikid agent stats --since 24h --owner alice --model claude-sonnet-5
```

Verified: `RAFIKI_DB=postgres://... rafikid agent stats` against a live DB
prints the real stats table.

Filter flags (global stats only, ignored when a conv-id is given):
`--since`, `--until` (RFC3339 or a duration like `24h`), `--owner` (a user
name from `rafiki user list`), `--persona`, `--source`, `--model`, `--path`.

## `search`

```
rafikid agent search --since 24h --status failed --min-tokens 5000 --limit 20
rafikid agent search --text "skill gap"
```

All the `stats` filter flags, plus `--status`, `--min-tokens`, `--text`
(full-text search over first messages), `--limit` (0 = backend default).

## `export`

```
rafikid agent export <conv-id>
rafikid agent export <conv-id> -j
```

Requires exactly one positional conversation id. Renders the transcript as
markdown by default, or JSON with `-j`/`-J`.

## `query`

```
rafikid agent query tools --since 24h
rafikid agent query classes -j
```

Requires exactly one positional query name, one of: `tools` (tool_use counts
by tool with ok/error/unmatched splits, case-insensitively merged under the
most common spelling and sorted by calls descending), `skills` (skill
invocations, namespace-normalized),
`classes` (behavioral breakdown: coordinator/brainstorming/planning/worker),
`models` (served-model distribution), `sizes` (turn-count histogram per
class), `coverage` (child-row instrumentation coverage by week, agent-kind
conversations only). Query names complete on `<TAB>`.

All the `stats` filter flags: `--since`, `--until`, `--owner`, `--persona`,
`--source`, `--model`, `--path`. Time filters are per query: tools and skills
filter message time, models filters turn time, classes and sizes filter turn
activity, coverage filters conversation creation week.

Renders a table by default; `-j`/`-J` emit JSON with real typed values (ints
and floats decode as numbers, never strings).

## `analyze`

Runs the pipeline: resolve a population → skip already-analyzed
conversations (unless `--force`) → per-conversation Export/Compact/Detect →
cross-batch Rank → Draft skill edits for the top candidates. Streams
progress to stdout (or stderr, if `-j`/`-J` is set, keeping stdout pure
JSON), then prints a summary.

**Population** (choose exactly one):

```
rafikid agent analyze <conv-id> [<conv-id> ...]     # named conversations from the DB
rafikid agent analyze --corpus DIR                  # exported *.json transcripts, no DSN needed
```

A `--corpus` run never persists analyses or findings, even without
`--no-store`: a corpus transcript has no `conversations.conversation` row for
an `analysis_finding` to foreign-key against, so it's forced no-store
regardless of the flag.

**Stage control** (mutually exclusive; default is the full pipeline through
draft):

- `--compact` — stop after the local Compact transform. No LLM call, no
  credentials, no proxy needed at all.
- `--detect` — stop after per-conversation Detect (skill-gap findings).
- `--rank` — stop after cross-batch Rank.
- `--draft` — explicit spelling of the default (full pipeline).

**Model / profile:**

- `--model` overrides the detector/rank/draft model directly, regardless of
  what `--analyzer-dir`/`--profile` resolved.
- `--analyzer-dir DIR` (or `RAFIKI_ANALYZER_DIR`) points at a directory
  containing `profiles.yaml` (required) plus optional `detector.md`/
  `draft.md` base prompts.
- `--profile NAME` selects a named profile from `--analyzer-dir`; an
  unknown name errors listing what's available. With `--analyzer-dir` set
  and no `--profile`, the profile named `default` is used if present.
- With neither `--analyzer-dir` nor `--model`, `analyze` fails with
  `no detector model: pass --model, --profile, or --analyzer-dir` (except
  when `--compact` is set — the compact stage needs no model at all).
- `--analyzer-dir` with no `--profile` and no profile named `default` in
  `profiles.yaml` fails fast, listing the available profile names — even
  when `--model` is also given, since `--model` only overrides the
  detector/rank/draft model fields and silently dropping the rest of the
  resolved config (filters, compact policy, prompt bases) would be a worse
  surprise than an actionable error.

**Upstream:** a rafiki proxy (`--proxy-url`/`--proxy-token`, or
`RAFIKI_URL`/`RAFIKI_TOKEN`) wins if set; otherwise
`ANTHROPIC_API_KEY` goes direct to Anthropic. Direct-to-Anthropic can only
serve concrete Anthropic ids — any OpenRouter-native id (a `provider/model`
slash id, or a `~`-prefixed catalog alias) fails fast with an actionable
error *before* any per-conversation work starts, rather than mid-batch.

`rafikid agent` is a daemon-side CLI — it reads `RAFIKI_URL`/`RAFIKI_TOKEN`
straight from `os.Getenv` (`cmd/rafikid/agent_cli.go`) and is unaffected by
client profiles; the two variables are retired only for the `rafiki`
**client** binary (see README's "Profiles" section).

**Other flags:** `--force` (re-analyze even if already analyzed under this
exact configuration), `--limit N` (cap conversations analyzed; 0 = profile
default), `--out DIR` (write per-conversation JSON+markdown artifacts, plus
a prompts sidecar), `--repo DIR` (resolve current skill files for draft
matching, from `.claude/skills/*/SKILL.md` or `skills/*/SKILL.md`),
`--no-store` (rank/draft in-memory without persisting analysis rows).

Verified corpus run, no DSN or credentials at all (compact is a pure local
transform):

```
rafikid agent analyze --corpus DIR --compact --out DIR
```

Verified full pipeline through a rafiki proxy:

```
rafikid agent analyze --corpus DIR --model claude-haiku-4-5 \
  --proxy-url https://rafiki.example.com --proxy-token $TOKEN --out DIR
```

### `--compare`: model sweep over a corpus

```
rafikid agent analyze --corpus DIR \
  --compare claude-haiku-4-5,claude-sonnet-5,deepseek/deepseek-v4-flash \
  --proxy-url https://rafiki.example.com --proxy-token $TOKEN --out DIR
```

Runs the same corpus once per model in the comma-separated list, overriding
only `DetectorModel` per run. Requires `--corpus`: a comparison sweep writes
one evaluation analysis per model into `conversation_analysis`, the
operational table the pipeline reads to decide what still needs analyzing —
corpus mode keeps throwaway comparison runs out of that state. Each model's
artifacts land in `<out>/<model-slug>/` (`/` and `~` become `-`); a failed
model is recorded and does not stop the rest of the sweep. Prints one row
per model: findings count broken down by axis
(skill-gap/knowledge-to-persist/grind), tokens, cost, and status (`ok` or
`ERROR: ...`).

## `findings`

```
rafikid agent findings                        # open findings (default status)
rafikid agent findings --axis skill-gap --skill td-go
rafikid agent findings dismiss <finding-id>
rafikid agent findings action <finding-id>
```

`--axis`, `--skill`, `--status` (default: open) filter the list. The
`dismiss`/`action` subcommands take exactly one finding id and set its
status to `dismissed`/`actioned`.

The read half has a socket twin: `rafiki conversations findings` (the
Connect plane's `ConversationFindings`) lists the same findings — same
filters, same open default, and the same limit convention except that the
CLIENT rejects a negative `--limit` outright while the daemon maps a
negative (or 0) to its default of 50 — plus a recent-analyses
table (model, status, cost, tokens) where a review run's outcome becomes
visible. It honors the shared `--output auto|json|jsonl` contract. Triage
stays here: it is a direct-DB write, and the client never holds a DSN.

## Conversation review (`rafiki conversations review`)

A client command documented here for the same reason `rafiki user` is: it is
the remote entry to the same analyze pipeline `analyze` above runs directly.
It asks the DAEMON's review worker to run the pipeline, over the Connect
plane's `ConversationReview` — `rafiki` never holds a DSN and never makes an
LLM call (the verb returns before anything runs). There is no `rafikid
agent` counterpart: submission is Connect-only.

```
rafiki conversations review <id|name>...                    # detect (default stage)
rafiki conversations review <id>... --stage rank            # rank persists findings
rafiki conversations review <id> --model openrouter/z-ai/glm-5.3-flash --budget-usd 0.05
```

The request carries no scope — the daemon derives it from the credential.
Each `<id|name>` argument resolves client-side to a child id (`c_…`, the id
`rafiki status` shows), and the daemon admits it through its
`conversations.child` mapping; a conversation UUID is accepted too. An id
matching neither spelling — one that names no conversation, or one out of
scope — is dropped silently with no error. A batch is N independent
per-conversation calls: the response carries one accept status per id
(`enqueued`, `already running`, `queue full`), never an analysis id — read
outcomes back through `rafiki conversations findings`, whose analyses rows
are inserted at completion only, so an in-flight review reads as empty.

Every optional field resolves flag > `~/.config/rafiki/review.json`
(`RAFIKI_REVIEW_CONFIG` overrides the path) > the daemon's `RAFIKI_REVIEW_*`
env > the analyzer profile's own defaults; a missing file is a working
configuration. `--model` must be provider-qualified — `openrouter/<id>` for
OpenRouter (a bare OpenRouter id reads as an unknown provider), a
providers.toml alias as `<provider>/<alias>`, or an Anthropic id — and an id
that resolves nowhere fails the whole request before anything is enqueued.
The analyzer-profile flag is `--analyzer-profile`, not `--profile`: the
global `-P/--profile` selects the DAEMON profile and keeps that meaning
here. `min_turns` gates when a job DEQUEUES, not at accept, so a
below-threshold job is skipped silently (no analysis row); `--force`
re-analyzes conversations that already carry analysis.

`rafiki close --review` is the same submission keyed off a close: after each
close succeeds, that conversation is handed to the review worker with the
stage left unspecified (the wire default, detect). It is best-effort by
design — a review failure prints a stderr note and never fails the close,
and only a non-`enqueued` status prints anything. `--no-review` is the
explicit no-op spelling of the default, for scripts that pass a fixed flag
set.

## Environment variables

| Variable | Read by | Effect |
|---|---|---|
| `RAFIKI_DB` | every subcommand's `--db` default | Postgres DSN; checked before `RAFIKI_TEST_DSN` |
| `RAFIKI_TEST_DSN` | every subcommand's `--db` default | fallback DSN if `RAFIKI_DB` is unset |
| `RAFIKI_ANALYZER_DIR` | `analyze --analyzer-dir` default | analyzer directory (`profiles.yaml` + `detector.md`/`draft.md`) |
| `RAFIKI_URL` | `analyze --proxy-url` default | rafiki proxy base URL for LLM calls |
| `RAFIKI_TOKEN` | `analyze --proxy-token` default | bearer token for `RAFIKI_URL` |
| `ANTHROPIC_API_KEY` | `analyze` upstream resolution | used direct-to-Anthropic when no proxy URL is configured |

None of these are required for `analyze --corpus DIR --compact` — that path
needs no DSN, no proxy, and no API key.

`RAFIKI_URL` and `RAFIKI_TOKEN` here are read directly by this daemon-side
process from its own environment and are unaffected by client profiles —
they are retired only for the `rafiki` client, not for `rafikid agent`.

## The dev loop

Iterating on a detector prompt without touching Postgres or burning real
model calls:

1. Edit `detector.md` (and/or `profiles.yaml`) in a team-platform-style
   analyzer-dir checkout.
2. Point `--analyzer-dir` at it (or export `RAFIKI_ANALYZER_DIR`) and run
   against `--corpus DIR` — a directory of exported `*.json` transcripts,
   no DSN needed.
3. Start cheap: `--compact` renders the local transform with zero
   credentials, to sanity-check what the detector will actually see.
4. Move to a real run: drop `--compact`, add `--model` or `--profile` plus
   `--proxy-url`/`--proxy-token` (or `ANTHROPIC_API_KEY`), and inspect
   `--out DIR`'s per-conversation artifacts.
5. Once a prompt change looks promising, `--compare model-a,model-b,...`
   sweeps it across several models on the same corpus in one run, so the
   findings/cost/token table is directly comparable.

## Relationship to `sc agent`

`rafikid agent` and `sc agent` cover the same domain — conversation
insights and skill-gap analysis over the same `conversations` schema — but
differ in transport. `rafikid agent` talks straight to Postgres via
`pkg/agentcli/local.Backend`, useful for local/dev work against a DSN you hold
directly. `sc agent` is expected to mount the same `pkg/agentcli.Backend`
interface over a gRPC backend, adding what a multi-tenant
deployment needs on top: auth, per-environment config
resolution, and tailnet-routed connectivity. The CLI surface
(`agentcli` package: filters, renderers, `agentcli.Render`, the
`AnalyzeRequest`/`AnalyzeEvent` contract) is the seam meant to be reused as-is
— only the `Backend` implementation changes. `rafiki conversations` is the
first proof that the seam holds: a different transport reusing every renderer
unchanged.

## `rafiki create` and executor targeting

`rafiki create` spawns a child and attaches to it. Its model/kind/preset
precedence is documented in the command's own `--help`; what concerns this
section is WHERE the child runs — the executor targeting flags, which answer
the question for every kind in one vocabulary:

| Flag | Env default | Meaning |
|------|-------------|---------|
| `--executor <ref>` | `$RAFIKI_EXECUTOR` | Target ONE specific executor by its machine name (e.g. `greyshift`) or raw id. Sent as `SpawnRequest.ExecutorRef`; resolved by the daemon against the same confinement checks any candidate must pass, so a pin bypasses search, never confinement. **Mutually exclusive with `--executor-selector`** — passing both is a usage error, not a silent precedence rule |
| `--executor-selector <sel>` | `$RAFIKI_EXECUTOR_SELECTOR` | A label selector choosing from the daemon's pool (e.g. `owner=brent,env=home`). A selector matching several executors keeps the documented silent-first-match behavior |
| `--no-local-executor` | — | Do not offer this machine as a workspace at all |

Tab-completion: `--executor` completes machine names (ids for unlabeled
executors) from the daemon's `ListExecutors` RPC, scoped to the resolved
`--kind` — the same kind-scoping rule `--model` completion applies. Answers
are cached briefly, like `--model` completion's cache.

The default depends on the kind:

- **`fundi`** keeps its zero-config default: with nothing set, the client
  starts a throwaway local *session executor* and points the spawn at it, so
  the workspace tools run where your files are.
- **A launch-required kind (anything but `fundi`)** can never be served by
  that throwaway executor — it never advertises launch support — so it is
  never pinned to it. With neither flag given, the client resolves via the
  daemon's live executor catalog before spawning: the executor last used for
  this kind is reused when it is still live and eligible, exactly one eligible
  executor is picked, and an ambiguous answer (several candidates) is an error
  listing them — pass `--executor` to pick one. The resolved choice is
  remembered per (profile, kind), so this costs a decision once per machine.

Passing any of these (or any other shaping flag) spawns directly; `-i` opens
the interactive form anyway, prefilled — where the executor field, `^E`'s
picker, and the same kind-aware default apply.

### Pre-filled spawns

`--prefill-files <list>` makes the child read files **before its first
turn**: the engine reads them through the child's own Read tool and persists
the tool_use/tool_result pairs as real conversation history, so the model
starts its task already holding the files. The daemon carries the same
pre-fill on `agent_spawn`'s `prefill` param and on the SpawnRequest `prefill`
field (`docs/reference/control-protocol.md` §6.3).

The list is a file, one entry per line:

- `#` starts a comment (to end of line); blank lines are ignored;
  leading/trailing whitespace is trimmed.
- Paths are relative to the child's cwd (absolute also accepted).
- An optional range suffix is recognised only when the entry ends in `:` +
  optional digits + `-` + optional digits, so a path containing `:` still
  works. Ranges are **1-based and inclusive**, matching Read's printed line
  numbers: `:N-M`, `:N-` (to EOF), `:-M` (from the start). `:-` alone (both
  sides empty), `N > M`, and `0` in either position are errors.
- A **glob** is an entry whose path contains any of `*?[{`. A glob may not
  carry a range.
- An empty list (after comments/blank lines) is an error.

Example (`plan-files.txt`):

```
# context every worker needs
CLAUDE.md
docs/plans/2026-09-24-prefilled-spawn-plan/task-2.1-brief.md:1-80
pkg/**/*.go
```

`--prefill-files -` reads the list from stdin and is only allowed with
`--detached` (stdin is not available once create attaches). It is mutually
exclusive with `-i`: the interactive form cannot carry a pre-fill, so the
combination is refused at parse time rather than silently dropping the list.

Pre-fills are **fundi only** — the daemon refuses the spawn for any other
kind. The child needs the `read` tool in its tool set (`glob` too when any
entry is a glob); a spawn whose allowlist omits them is refused. The total
read volume is estimated and capped at **60% of the model's context window**
(128k assumed when the catalog doesn't know the model); over the cap,
nothing is persisted and the child ends with an error naming the estimate,
the cap, and the five largest reads.

## `rafikid fundi` flags

`rafikid fundi` runs a single agent child on stdio. Its flags configure the
agent runtime (model, thinking, tools, persistence). Every flag has a
corresponding `RAFIKI_*` env var default; an explicit flag always wins.

| Flag | Env var / default | Description |
|------|-------------------|-------------|
| `--model` | *(required)* | provider-qualified model id |
| `--thinking` | `off` | extended-thinking level: `off`, `low`, `medium`, `high`, `xhigh` |
| `--bash-rtk` | `$RAFIKI_BASH_RTK` / `auto` | route bash through rtk: `auto`, `on`, `off` |
| `--tools-web` | `$RAFIKI_TOOLS_WEB` / off | enable the webfetch/websearch tools; boolean, so disable with `--tools-web=false` to override `$RAFIKI_TOOLS_WEB=1` |
| `--record-requests` | `$RAFIKI_RECORD_REQUESTS` | capture raw LLM API requests/responses |
| `--db` | `$RAFIKI_DB` | postgres DSN for conversation persistence |
| `--ref` | `$RAFIKI_CHILD_ID` | conversation ref for reattachment |
| `--no-context-files` | — | skip CLAUDE.md/AGENTS.md context |
| `--no-skills` | — | disable skill discovery |
| `--skills-dir` | *(repeatable)* | additional skills directories |
| `--skills` | — | comma-separated skill allowlist |
| `--tools` | — | comma-separated allowlist of built-in tools (empty = all); MCP tools are governed by `--mcp-servers`/`--no-mcp` |
| `--no-builtin-tools` | — | disable every built-in tool; wins over `--tools` |
| `--mcp-config` | `$RAFIKI_MCP_CONFIG` | path to .mcp.json |
| `--mcp-servers` | — | comma-separated allowlist of .mcp.json server names to connect |
| `--no-mcp` | — | disable MCP entirely, even when --mcp-config is set |
| `--lsp-config` | `$RAFIKI_LSP_CONFIG` | path to lsp.json (language server config). When absent, scans PATH for well-known LSP servers (gopls, rust-analyzer, …) automatically. |
| `--no-lsp` | `$RAFIKI_LSP_DISABLE` | disable LSP entirely, including auto-detection |
| `--spill-dir` | *(derived)* | directory for clipped tool output |
| `--max-output-tokens` | `0` (default 16384) | per-turn output token cap |
| `--system-prompt` | — | override the base system prompt |
| `--append-system-prompt` | — | append to the system prompt |
| `--prefill` | — | JSON list of files/globs the engine reads before turn 1 (`[{"path":…,"start":…,"end":…}]`); normally set from the client's `--prefill-files` list (see "Pre-filled spawns" under `rafiki create`) |
| `--fake-turns` | — | replay a recorded turn file for testing |

## `rafiki daraja`

Per-child process host for remote executor-launched children. Subcommands:
`serve` (run the daraja binary on an executor) and `launch` (from the operator's
machine, request a child launch through the daemon).

`daraja launch` below is the manual, operator-facing entry point — useful for
debugging the launch path directly. It is not the only caller: an ordinary
`rafiki create --kind claude` (or `rafiki send` on an existing one) routes
through the same `AdminService.Launch`/relay machinery automatically whenever
the daemon has an executor pool configured and one of its executors declares
`claude` in `--launch`. With no executor pool at all, `--kind claude` falls
back to spawning claude as a local subprocess of the daemon, unchanged from
before this existed.

A daraja-routed `--kind claude` child is proxied through the daemon (capture,
cost accounting, routing visibility) while still billing the user's own
Claude subscription by default. `rafiki create --passthrough-auth
auto|on|off` (default `auto`, or `$RAFIKI_CLAUDE_PASSTHROUGH`) controls who
gets billed — `auto` bills the subscription when `--model` resolves to an
Anthropic id and the daemon's key otherwise, mirroring `rafiki claude
--passthrough-auth` exactly. Only meaningful for a daraja-routed child; the
local-subprocess fallback has no passthrough support at all (its env
construction can only ever append to the daemon's own inherited environment,
never unset a variable it already carries).

### `rafiki daraja launch`

```
rafiki daraja launch --cwd /path/to/workspace --model claude-sonnet-5 \
  [--executor "env=prod"] [--resume session-id]
```

Launches a claude child via daraja on a matching executor:

1. Resolves an executor whose labels satisfy the `--executor` selector AND that
   declares "claude" in its `LaunchKinds`. A zero-match returns a per-candidate
   refusal reason.
2. Calls `AdminService.Launch` on the selected executor with a one-shot ticket.
3. Waits for the daraja process to reverse-dial back into the daemon's pool
   (up to 30 seconds).
4. Prints `child_id`, `pid`, `pgid`, and `connected_at` (Unix ms epoch).

Through `newConnectEndpoint` — honours the resolved profile for remote
daemons (a profile with a `url`; see README's "Profiles" section). Requires
a TCP control address on the target daemon (`RAFIKI_CONTROL_LISTEN` must be set);
UDS-only daemons refuse with a clear diagnostic.

**Flags:**

| Flag | Required | Description |
|------|----------|-------------|
| `--cwd` | yes | Working directory for the hosted child |
| `--model` | yes | Provider-qualified model id for the claude child |
| `--executor` | no | Label selector; defaults to the first available executor |
| `--resume` | no | Claude session id to continue from |

Output format: `child_id=<id> pid=<n> pgid=<n> connected_at=<epoch_ms>`
on stdout; errors go to stderr.

## `rafiki skills`

Manage the daemon's database-backed skill corpus (`conversations.skills`) —
the tier a fundi child's skill tool serves after the file-based tiers: its
inventory is read once at child spawn, and invoking a skill fetches the body
from the store at call time. It talks to the daemon over the Connect plane
(`ListSkills`/`GetSkill`/`UpsertSkill`/`DeleteSkill`/`SetSkillEnabled` — see
`docs/reference/control-protocol.md` §2.3), so it needs a reachable profile,
never a DSN, and works against a remote daemon exactly as against a local
one.

Every skill lives in a **namespace** and is referenced as
`<namespace>:<name>`; a bare `<name>` means the default namespace `rafiki`.
The namespace is the name a model sees, and the fold that merges the
database tier with file-based skills keys on the qualified name — which is
why an imported corpus keeps its upstream plugin's name as its namespace
(see `import` below).

```
rafiki skills list [--all]              # the corpus; --all adds disabled rows
rafiki skills show <namespace:name>     # one skill's body, frontmatter stripped
rafiki skills add --file <SKILL.md> [--namespace ns]
rafiki skills rm <namespace:name>       # hard-delete (both rows, in the override state)
rafiki skills enable <namespace:name>
rafiki skills disable <namespace:name>
rafiki skills import <dir> [--namespace ns]
```

- **`list`** omits bodies: an inventory is a handful of lines and a body is a
  document, so none of the corpus rides along. `show` fetches one body.
- **`add`** reads a local `SKILL.md` (frontmatter `name:`/`description:`,
  body after it), parses it the same way `skills.DiscoverSkills` does, and
  upserts it as `source: manual`. The daemon defaults the namespace to
  `rafiki` and refuses the reserved source `rafiki-core`, which belongs to
  the daemon's own startup sync.
- **`disable`** flips a row off — it stops being served, and its NAME IS THEN
  FREE for a replacement: because the database tier can hold an override
  under an existing name only while the old row is disabled, disabling is
  how you retire a core skill before adding your own under the same name.
  `enable` is the inverse. A name with no row in the requested state is an
  answer (`not found`), not a failure.
- **`rm`** deletes every row under the name — there is no attribution
  history to preserve.
- **`import`** walks a corpus laid out as `<dir>/<name>/SKILL.md` (a Claude
  Code plugin's `skills/` tree, or any checkout in that shape) and upserts
  every skill into one namespace, `source: import:<namespace>`. The
  namespace is derived from upstream's own `.claude-plugin/plugin.json` or
  `marketplace.json` (the PLUGIN's name, never the marketplace's), falling
  back to the directory's own name; `--namespace` overrides. Keeping the
  upstream name is the point: a claude child on a machine where that plugin
  is really installed deduplicates the two instead of seeing the same skill
  twice.

Authenticated by the same per-user bearer credential as every other Connect
verb: any valid user token may read and write the corpus, in any namespace,
until multi-user scoping is built.

## `rafiki python` (alias `py`)

Manage your saved pymodules — reusable, owner-scoped Python snippets
(`conversations.pymodules`). What you save here is exactly what children you
spawn see: the store is scoped to the owner the daemon resolves from the
connection itself (unattributed on the local unix socket), so there is no
owner flag to pass and no way to see anyone else's corpus. It talks to the
daemon over the Connect plane (`ListPymodules`/`GetPymodule`/`PutPymodule`/
`DeletePymodule`), so it needs a reachable profile, never a DSN, and works
against a remote daemon exactly as against a local one.

```
rafiki python list [--repo <name>]          # the inventory; code is never printed here
rafiki python get <name>                    # one module's code, raw
rafiki python put <name> --file <path>      # save a new version (--file - reads stdin)
rafiki python put <name> --file - --description "one line"
rafiki python delete <name>                 # soft-delete every live version
```

- **`list`** shows `NAME`, `REPO`, `VERSION`, `SAVED`, `DESCRIPTION` and
  never the code — an inventory is a handful of lines and a body is a
  document. `--repo` narrows the inventory to one scope: `local` (the saved
  blob store), a registered git source's name, or unset for everything —
  the one place an empty value means "no filter", because it is a filter
  flag, not an address. Git-sourced rows show the source's name in REPO and
  `-` for VERSION and SAVED: they have neither, the repo's own history
  being their versioning. `-o json`/`-o jsonl` work on `list` too, and the
  rows stay codeless in every mode (json wraps them in the shared
  `{"rows": …}` envelope, jsonl prints one bare row per line). `get` prints
  only the code by default, so it can feed a file or an editor unchanged;
  `-o json` (on `get` or `put`) emits the full row including code, with no
  envelope around it. get/put/delete operate on the `local` scope only, so
  their name completion narrows to it too.
- **`put`** always inserts a NEW version — saving again under an existing
  name never modifies what is saved there, and the printed version id tells
  the two apart. The name must be a bare Python identifier (it becomes both
  `<name>.py` on every executor and the argument to `import <name>`), checked
  locally before the code is sent and re-checked by the daemon.
- **`delete`** removes every live version of the name at once — there is no
  per-version delete. A later `put` under the same name restores it.
- On a daemon with no agent database (a profile-scoped dev instance without
  `RAFIKI_DB`), every verb reports `pymodules backend not yet wired` rather
  than an empty answer.
- Agents have the same corpus: the `pymodule_get`/`pymodule_put` tools a
  child carries read and write exactly what this command does, so a module
  saved here is importable by a child and vice versa.

### `rafiki python repo` (git-backed sources)

Manage git-backed pymodule sources: owner-scoped `(name, url, ref)`
registrations whose discovered scripts and packages children address as
`repo=<name>` on the pymodule tools. The blob store above is itself one such
scope under the reserved name `local`, which is why `repo add` refuses it.
These are rare, setup-shaped operations, so they live here and on no agent
tool surface — registration goes over Connect (`AddPymoduleGitSource`/
`ListPymoduleGitSources`/`RefreshPymoduleGitSource`/`RemovePymoduleGitSource`)
and works against a remote daemon exactly as against a local one.

```
rafiki python repo add <name> <url> [--ref <ref>]   # register + first refresh (--ref main)
rafiki python repo list                             # NAME, URL, REF
rafiki python repo refresh <name>                   # re-pull on every eligible executor
rafiki python repo remove <name>
```

- **`add`** registers the source and the daemon fires the FIRST refresh
  synchronously, so a bad clone (bad URL, unreachable host, failed auth)
  fails the add outright. The printed summary is the discovered inventory:
  script and package counts plus the venv state. Note the summary itself is
  a SECOND refresh call the CLI makes after registering; if that call fails,
  the source is still registered — rerun `python repo refresh`.
- **`refresh`** re-pulls the source on every live executor of yours whose
  `Describe` reports `pymodule_git_sync`, in parallel, and prints the same
  summary. A failed venv build is printed as `venv build failed: …` and does
  NOT fail the command — the refresh itself succeeded, and a broken build
  only fails the runs that actually need the venv.
- **`remove`** deletes the registration; executors are never told, and the
  discovered inventory simply stops being reported.


## `rafiki recall` and `rafiki memory`

Search the recall index and manage your saved memories. Recall is what a
fundi child's `recall` tool serves, exposed to the operator: conversation
summaries (`s:` ids), verbatim conversation windows (`w:` ids) and memories
saved with `memory put` (`m:` ids), ranked by keyword AND meaning. It talks to
the daemon over the Connect plane (`Recall`/`RecallContext`/`GetMemory`/
`MemoryTree`/`PutMemory`/`DeleteMemory`/`RecallBackfill`/`RecallStatus`), so
it needs a reachable profile, never a DSN. Conversation results cover what
your credential can see — an admin reads the whole daemon, a user only their
own rows; memories are ALWAYS your own, admin included. On a daemon with no
agent database every verb reports `recall backend not yet wired`.

```
rafiki recall <query...> [--source memory|summary|window]... [--under PATH]
                         [--repo NAME] [--since 7d|36h|RFC3339] [--limit N]
rafiki recall context <hit-id> [--before N] [--after N] [--max-chars N]
rafiki memory tree <path> [--depth N]
rafiki memory get <path> <name>
rafiki memory put <path> <name> [--body TEXT | --file PATH | stdin] [--meta JSON]
rafiki memory delete <path> <name>
rafiki memory backfill --since DURATION|RFC3339 --max-cost USD   # admin credential
rafiki memory status
```

- **`recall`** prints `ID`, `SOURCE`, `WHEN`, `WHERE`, `SNIPPET` — one line
  per hit, never full text. WHERE is a memory's `path/name`, else a
  conversation's `repo·name`. `-o json` prints the hit array as-is and
  `-o jsonl` one hit per line. `--source` is repeatable and accepts
  `memory`, `summary` and `window`; anything else the daemon refuses.
- **`recall context <id>`** expands a hit into its full text — a window into
  its surrounding messages (`--before`/`--after` ordinals, `--max-chars`
  cap), a summary into its title and full text, a memory into its body. It
  prints plain text in every output mode; cobra resolves the literal word
  `context` as the subcommand, so it is never read as a query.
- **`memory put`** resolves the body from `--body`, else `--file` (`-` reads
  stdin), else piped stdin; `--meta` must be a JSON object. A path is 1+
  dot-separated labels (`[A-Za-z0-9_-]`); the daemon tombstones deletes.
- **`memory status`** reads the index: conversation/window/summary/memory
  counts, unembedded windows, pending summaries, the running total of
  summary spend, and the embedding and summarizer models (empty when
  keyword-only search or summaries are off).

### The zero asymmetry (`--since` on recall vs backfill)

The wire's zero means different things on the two verbs, deliberately:

- **`Recall`'s `since_unix`/`until_unix` 0 = unbounded.** A search without
  `--since` covers all time; the CLI sends 0 only when you did not pass the
  flag.
- **`RecallBackfill`'s `since_unix` 0 = "from the beginning", and
  `max_cost_usd <= 0` is REFUSED (`CodeInvalidArgument`).** A wire
  `RecallBackfillRequest{}` — both fields zero — can never start an
  all-history, budgetless backfill: the summarizer would otherwise read a
  missing budget as unlimited spend. The CLI makes both flags required, so
  "all history" only ever happens because an operator typed a timestamp
  (e.g. `--since 1970-01-01T00:00:00Z`), never because a field defaulted.

`backfill` is admin-credential only — the daemon refuses anyone else with
`PermissionDenied` — and arms three state keys (`backfill_since` as RFC3339,
`backfill_budget_usd`, `backfill_spent_usd` reset to `"0"`), then nudges the
indexer. A backfill that has spent its budget disables itself
(`backfill_since` cleared; visible as an empty `backfill_since` in `memory
status`).
