# The proxy and library

Routing core (per-upstream breaker, OpenRouter catalog, model resolution,
`prefix_hash`, SSE capture parsing); a typed builder API and DB-backed
`Conversation` (trim policy, cache breakpoints, write-ahead persistence);
agent-loop primitives (`agentloop.Run`/`Resume` with crash recovery); both
proxy faces — Anthropic `/v1/messages` and OpenAI `/v1/chat/completions` —
behind an `Authenticator` seam; static bearer-token auth; Prometheus metrics;
OTLP tracing. `rafikid` serves the proxy face itself and applies the schema
(`rafikid migrate`) — there is no separate `rafiki serve` binary.

## Model selection

Requests on the `/v1/messages` face (and `llm.Conversation`s) take:

- a concrete Anthropic id (`claude-opus-4-8`) — sent to the Anthropic
  primary, with breaker-gated OpenRouter failover when configured;
- a `<family>-latest` alias (`opus-latest`) — resolved live from the
  OpenRouter catalog to the family's newest Anthropic model;
- a **short model alias** (`kimi-k3`, `deepseek-v4-pro`, `glm-5.2`) —
  resolved live from the catalog to the newest release of that model line,
  yielding an OpenRouter slash id;
- any OpenRouter slash id (`moonshotai/kimi-k3`) — routed directly to
  OpenRouter with no failover.

Some model lines carry a **provider pin** (`routing.ProviderPrefsFor`):
open-weight models are served by many OpenRouter providers of varying
quantization and data-retention policy, so pinned lines get an OpenRouter
`provider` routing object restricting them to vetted hosts (`glm-5.2` →
Fireworks). A caller-supplied `provider` field always wins over the pin.

### Routing specs

Any OpenRouter model string may carry a **routing spec** — a bracketed tail
after the id, e.g. `openrouter/z-ai/glm-5.3-flash[sort=price,quant=fp8+]` —
that controls the OpenRouter `provider` routing object sent with the request
(`routing.ParseModel`/`ParseSpec`, `pkg/routing/spec.go`). The same grammar is
the stored form of a routing-policy row (below) and a preset's model. Six
keys, comma-separated; a malformed or unknown item is a parse error, never
silently ignored (a misread spec silently changes where requests are served):

- `sort=price|throughput|latency|balanced` — provider sort preference;
  `balanced` means send no sort (a decision, unlike leaving `sort` out,
  which inherits from the next level of the resolution chain);
- `quant=fp8+` (a floor) or `quant=fp8|bf16` (a list) — quantizations of
  the serving host. Tiers, lowest first: `int4 fp4 mxfp4 nvfp4` < `fp6` <
  `int8 fp8 mxfp8` < `fp16 bf16` < `fp32`. A floor admits its whole tier
  and everything above; `unknown` (a host OpenRouter hasn't labelled) is on
  no tier, so no floor admits it and `unknown+` is a parse error — a list
  may name it explicitly;
- `prefer=slug|slug` — provider slugs to try FIRST, with fallback: sets the
  OpenRouter `provider.order`. Unlike `only`, it is a preference, not a
  restriction — the request may still be served by any other eligible host,
  and it RESPECTS operator bans and the cache guard's ejections (`only`
  bypasses them);
- `only=slug|slug` — OpenRouter provider slugs the request may be served by;
- `nodata` — sets `data_collection: "deny"`;
- `zdr` — sets `zdr: true` (zero data retention).

**Resolution** merges one spec per level, most specific first: the spawn's
spec > the preset's spec > the routing-policy row for the model line > the
global `*` row. Per key the more specific level wins when set; `nodata` and
`zdr` are **monotone** — set at any level they hold, nothing clears them.
Resolution happens ONCE, in `Controller.Spawn`, and the merged spec is stored
on the child's session: `rafiki get <child>` reports it (`routing`), resume
never re-resolves it, and a policy edit after the spawn never rewrites a
running child. Everything downstream sees the base id plus the stored spec —
brackets are never re-parsed.

A spec is inspectable and steerable without a respawn:

```
rafiki models route 'deepseek/deepseek-v4.1-flash[sort=price,quant=fp8+]'
rafiki route set <child> 'prefer=fireworks|deepinfra'
```

`rafiki models route '<model>[spec]'` lists every endpoint OpenRouter could
serve a model from, marked eligible or excluded by the spec (each with price,
quantization, measured throughput/latency and uptime); the server parses the
bracket, `--all` includes excluded rows and `--sort` orders the table
client-side, with a missing measurement sorting last. `rafiki route set
<child> <spec>` merges a bracket-free delta over a running child's stored
spec — per key, over what the child already carries — and the child obeys it
on its next request. From an agent, the same steering is the `agent_route`
tool (agent id + spec), bounded to the caller's own subtree; `only=` in a
delta is operator-only, so a child may set `prefer`, `sort` and `quant` but
never pin.

Below every spec level sits a **pin** (a providers.toml alias's `only`, or
the built-in per-line pins): used only when no spec level set `only`. A pin
and an operator **ban** are defaults, not authority — any caller that can
spawn can route around them with `only=[...]`. **Data policy cannot be
bypassed**: `nodata`/`zdr` ride along even under a spec `only`, and the
cache guard's ejections are dropped only by an explicit `only`. Authority
splits the other way: a spec is caller-settable by anyone who can spawn (it
rides the model string's brackets), while policy rows are operator-only
(set/delete need a user credential or the local socket).

**Batch transport interplay:** a parked `:batch` call's envelope carries
`provider.only` ONLY (the spec's `only`, else the pin) — the Batch API
rejects every other provider key at submit. A `:batch` model whose merged
spec carries any other key (sort, quant, nodata, zdr) is therefore refused
at spawn.

### Routing policy (`rafiki providers route`)

```
rafiki providers route set 'z-ai/glm-5.3' 'sort=price,quant=fp8+'
rafiki providers route set '*' 'nodata'
rafiki providers route list
rafiki providers route delete 'z-ai/glm-5.3'
```

Per-model-line routing defaults stored in the daemon
(`openrouter.route_policy`, `pkg/routepolicy`). Resolution is per key: a row
fills the keys a spawn/preset spec leaves unset (a spawn's `[...]` beats a
preset's, which beats these rows), and `nodata`/`zdr` ALWAYS hold — they OR
into whatever the spec says and reach running children too, so a row set
after a spawn governs that child's later requests. Reading the rows is open to
any caller; writing (set, delete) requires a user credential or the local
socket. A `set` appends and takes effect immediately — no restart; a
`delete` appends a tombstone.

A policy **line is a `-` prefix FAMILY**: a row's line matches a model id
equal to it or extending it with `-` (after stripping the provider segment
of a three-segment id) — the line `z-ai/glm-5.3` governs `z-ai/glm-5.3`,
`z-ai/glm-5.3-0905` AND `z-ai/glm-5.3-flash`. This is a different vocabulary
from the cache guard's stamp-exact model lines, which answer a different
question (who is locked out vs what defaults apply). A line names at most
`<model>/<id>` — the provider segment is not part of a policy line, and the
store refuses longer shapes. Among matching line rows the LONGEST line wins.

No concrete model ids are hardcoded: aliases name families/lines and the
catalog is the source of truth, so an unresolvable alias errors instead of
falling back to a stale id. Slash ids and model aliases require
`OPENROUTER_API_KEY`. The `/v1/chat/completions` face does no resolution —
it takes raw ids and routes by configured prefix.

### The provider cache guard

OpenRouter picks which provider serves an unpinned model, and that choice can
change without warning — silently, at up to 15× the input-token cost, because
the new provider doesn't cache the way the old one did. `routing.ProviderGuard`
watches completed OpenRouter turns for the tell (same conversation, same
`prefix_hash` and provider as the previous turn, a prompt over 4096 tokens,
and a cache miss). Five consecutive misses eject the provider for 24 hours,
capped at 3 ejected providers per model line so the guard can never blacklist
a line into unroutability. It needs capture enabled — it judges misses using
`prefix_hash` and conversation id, both capture-only — so with capture off it
is inert by design, and it applies **even when the caller supplied its own
`provider` object**. Set `RAFIKI_PROVIDER_GUARD=off` to disable automatic
ejection; operator bans (below) still apply.

Ejections are logged append-only and reseed the in-memory list at startup:

```sql
SELECT created_at, provider, model_line, reason, expires_at, note, evidence
  FROM openrouter.provider_ejection
 ORDER BY created_at DESC LIMIT 20;
```

OpenRouter's catalog can't answer this for you — its `supports_implicit_caching`
flag doesn't correlate with actual cache behavior — which is why the guard is
observational rather than a lookup.

### Operator provider bans

The guard only notices cache misses. For anything else (a provider that is
slow, looping, or you simply don't trust), ban it by hand at runtime:

```
rafiki providers ban open-inference --note "spinning"   # every model, until lifted
rafiki providers ban some-host --for 6h
rafiki providers bans                                    # operator bans + guard ejections
rafiki providers unban open-inference
```

A ban applies to every model line on the next OpenRouter request, including
for children already running, and is written to the ejection log above
(reason `operator`; a lift is a superseding `lift` row, never a delete), so it
survives restarts. The daemon checks the provider against OpenRouter's provider directory, so a typo'd or display-name-guessed slug is refused rather than silently matching nothing. Bans are exempt from the guard's per-line cap. Banning and
unbanning require an admin user credential or the local socket. The provider
is OpenRouter's slug; a display name is lowercased with spaces turned into
dashes. See `docs/reference/control-protocol.md` §"Provider bans".

### Batch transport (`:batch` model ids)

A fundi child on an OpenRouter model id ending `:batch` (e.g.
`openrouter/z-ai/glm-5.3:batch`) sends its **first call** through the
OpenRouter Batch API: the call parks, rafikid coalesces parked calls per
model every 30 s into one batch, polls every 60 s, and delivers the result.
Every later call of that child (once an assistant turn exists) goes live on
the base model id. There is no batch flag, id, or verb anywhere — batch is a
property of the model id.

While parked the child's status is `batch_wait` (cockpit glyph `⧖`, static; a
parked child counts as working and stays attachable). The wait can last
hours — OpenRouter's completion window is 24 h, and measured turnaround
(2026-09-25) was 38 min to 3+ h. Batch seats should be tool-less
(`tools: []`, `mcp_servers: []`): a child whose first request already
contains an assistant message (a tool-shaped pre-fill) is never parked.
Discounts are per model, not a rule — check `rafiki models` pricing for the
`:batch` id against the base id before choosing one (e.g.
`glm-5.3-flash:batch` costs MORE than live). A spawn carrying its own API
key on a `:batch` model is refused; batches use the daemon's `openrouter`
provider key.

## Model effort adaptation

Some OpenRouter models reject an `output_config.effort` value Claude Code sends
(e.g. `gpt-5-codex` accepts only `medium`). The proxy learns each model's
allowed set at runtime: on a rejection that enumerates supported values, it
records the constraint in an in-memory (per-process, never persisted) cache,
clamps the effort, and retries once. Subsequent requests to that model clamp
proactively. See `pkg/routing/effortmap.go` (`EffortCache`) and
`pkg/server/proxy.go` (`effortRetry`).

## Model aliases and declared context windows

A local model's real id is often long, and its context window is never in
the OpenRouter catalog, so Claude Code falls back to assuming 200K — which
overflows a small local model with an opaque `prompt_too_long` from the
inference server itself, outside rafiki's view. A provider can declare short
aliases, each optionally carrying its real context window:

```toml
[providers.vmlx.models.qwen]
id                   = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window       = 16384
context_files_tokens = 12288      # optional; default 20% of context_window, clamped [1024, 30000]
skills               = ""         # "" = none; "*"/omitted = all; "a,b,c" = only those
mcp_servers          = "codescan" # same tri-state convention

[providers.openrouter.models."glm-flash@together"]
id   = "z-ai/glm-5.3-flash"
only = ["together"]              # openrouter only: provider slugs allowed to serve this alias
```

`rafiki claude --model vmlx/qwen` sends the real id upstream and pins
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` accordingly; the alias shows up in `rafiki
models` and `--model` completion (`source: alias`) once a reachable daemon
knows about it. `context_files_tokens`/`skills`/`mcp_servers` bound what gets
built into a **fundi** child's system prompt and tool inventory for that
model — a `rafiki claude` child assembles those itself and only
`context_window` reaches it. An explicit `--skills`/`--mcp-servers` flag on
the spawn always overrides the model's declared default.

`only` is an OpenRouter-only pin on an `anthropic-openrouter` provider's
alias: requests made through the alias are sent with
`"provider": {"only": [...]}` so only those OpenRouter provider slugs may
serve them, and it replaces any built-in routing pin for that model line for
that request (the cache guard's ignore list still merges in). Because the
pin lives on the alias, two aliases can route the SAME model id to different
providers — an eval compares `openrouter/glm-flash@together` against
`openrouter/glm-flash@fireworks`, both sending `z-ai/glm-5.3-flash` with
different `only` lists. On any other provider kind, `only` is a config error.
