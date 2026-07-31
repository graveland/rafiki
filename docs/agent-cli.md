# `rafiki agent` CLI

`rafiki agent <verb>` is a DSN-backed CLI over the same `conversations`
schema the proxy captures into: read-only insights (`stats`/`search`/
`export`), the LLM-driven skill-gap detector (`analyze`), and finding
triage (`findings`). It talks to Postgres directly — no gRPC, no auth layer
— via `pkg/agentcli.Backend`, implemented today by `pkg/agentcli/local.Backend`.
See `cmd/rafiki/agent.go` for the flag/dispatch code this doc describes.

Every subcommand accepts `-j` (indented JSON) or `-J` (compact JSON) instead
of the human-readable table/markdown render.

## `stats`

Global stats, or stats for one conversation if given a positional id.

```
rafiki agent stats
rafiki agent stats <conv-id>
rafiki agent stats --since 24h --owner alice@example.com --model claude-sonnet-5
```

Verified: `RAFIKI_DB=postgres://... rafiki agent stats` against a live DB
prints the real stats table.

Filter flags (global stats only, ignored when a conv-id is given):
`--since`, `--until` (RFC3339 or a duration like `24h`), `--owner`,
`--persona`, `--source`, `--model`, `--path`.

## `search`

```
rafiki agent search --since 24h --status failed --min-tokens 5000 --limit 20
rafiki agent search --text "skill gap"
```

All the `stats` filter flags, plus `--status`, `--min-tokens`, `--text`
(full-text search over first messages), `--limit` (0 = backend default).

## `export`

```
rafiki agent export <conv-id>
rafiki agent export <conv-id> -j
```

Requires exactly one positional conversation id. Renders the transcript as
markdown by default, or JSON with `-j`/`-J`.

## `analyze`

Runs the pipeline: resolve a population → skip already-analyzed
conversations (unless `--force`) → per-conversation Export/Compact/Detect →
cross-batch Rank → Draft skill edits for the top candidates. Streams
progress to stdout (or stderr, if `-j`/`-J` is set, keeping stdout pure
JSON), then prints a summary.

**Population** (choose exactly one):

```
rafiki agent analyze <conv-id> [<conv-id> ...]     # named conversations from the DB
rafiki agent analyze --corpus DIR                  # exported *.json transcripts, no DSN needed
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
`RAFIKI_PROXY_URL`/`RAFIKI_PROXY_TOKEN`) wins if set; otherwise
`ANTHROPIC_API_KEY` goes direct to Anthropic. Direct-to-Anthropic can only
serve concrete Anthropic ids — any OpenRouter-native id (a `provider/model`
slash id, or a `~`-prefixed catalog alias) fails fast with an actionable
error *before* any per-conversation work starts, rather than mid-batch.

**Other flags:** `--force` (re-analyze even if already analyzed under this
exact configuration), `--limit N` (cap conversations analyzed; 0 = profile
default), `--out DIR` (write per-conversation JSON+markdown artifacts, plus
a prompts sidecar), `--repo DIR` (resolve current skill files for draft
matching, from `.claude/skills/*/SKILL.md` or `skills/*/SKILL.md`),
`--no-store` (rank/draft in-memory without persisting analysis rows).

Verified corpus run, no DSN or credentials at all (compact is a pure local
transform):

```
rafiki agent analyze --corpus DIR --compact --out DIR
```

Verified full pipeline through a rafiki proxy:

```
rafiki agent analyze --corpus DIR --model claude-haiku-4-5 \
  --proxy-url https://rafiki.example.com --proxy-token $TOKEN --out DIR
```

### `--compare`: model sweep over a corpus

```
rafiki agent analyze --corpus DIR \
  --compare claude-haiku-4-5,claude-sonnet-5,deepseek/deepseek-v4-flash \
  --proxy-url https://rafiki.example.com --proxy-token $TOKEN --out DIR
```

Runs the same corpus once per model in the comma-separated list, overriding
only `DetectorModel` per run. Requires `--corpus` (re-analyzing a stored
population per model would thrash the skip key). Each model's artifacts
land in `<out>/<model-slug>/` (`/` and `~` become `-`); a failed model is
recorded and does not stop the rest of the sweep. Prints one row per model:
findings count broken down by axis (skill-gap/knowledge-to-persist/grind),
tokens, cost, and status (`ok` or `ERROR: ...`).

## `findings`

```
rafiki agent findings                        # open findings (default status)
rafiki agent findings --axis skill-gap --skill td-go
rafiki agent findings dismiss <finding-id>
rafiki agent findings action <finding-id>
```

`--axis`, `--skill`, `--status` (default: open) filter the list. The
`dismiss`/`action` subcommands take exactly one finding id and set its
status to `dismissed`/`actioned`.

## Environment variables

| Variable | Read by | Effect |
|---|---|---|
| `RAFIKI_DB` | every subcommand's `--db` default | Postgres DSN; checked before `RAFIKI_TEST_DSN` |
| `RAFIKI_TEST_DSN` | every subcommand's `--db` default | fallback DSN if `RAFIKI_DB` is unset |
| `RAFIKI_ANALYZER_DIR` | `analyze --analyzer-dir` default | analyzer directory (`profiles.yaml` + `detector.md`/`draft.md`) |
| `RAFIKI_PROXY_URL` | `analyze --proxy-url` default | rafiki proxy base URL for LLM calls |
| `RAFIKI_PROXY_TOKEN` | `analyze --proxy-token` default | bearer token for `RAFIKI_PROXY_URL` |
| `ANTHROPIC_API_KEY` | `analyze` upstream resolution | used direct-to-Anthropic when no proxy URL is configured |

None of these are required for `analyze --corpus DIR --compact` — that path
needs no DSN, no proxy, and no API key.

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

`rafiki agent` and `sc agent` cover the same domain — conversation
insights and skill-gap analysis over the same `conversations` schema — but
differ in transport. `rafiki agent` talks straight to Postgres via
`pkg/agentcli/local.Backend`, useful for local/dev work against a DSN you hold
directly. `sc agent` is expected to mount the same `pkg/agentcli.Backend`
interface over a gRPC backend in savannah-client, adding what a
multi-tenant deployment needs on top: auth, per-environment config
resolution, and tailnet-routed connectivity. The CLI surface
(`agentcli` package: filters, renderers, the `AnalyzeRequest`/`AnalyzeEvent`
contract) is the seam meant to be reused as-is — only the `Backend`
implementation changes.
