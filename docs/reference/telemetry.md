# Telemetry

What `rafikid` exports, how to configure it, and what leaves the daemon.

`rafikid` exports **traces** over OTLP (HTTP/protobuf or gRPC) and serves
**metrics** over Prometheus. It does not export metrics or logs over OTLP:
metrics stay on the `/metrics` pull endpoint and logs stay on stdout.

Tracing is opt-in: with no OTLP endpoint configured the daemon runs a no-op
tracer provider, so nothing is produced on the wire. Metrics are independent
of tracing and are always recorded.

## Configuration

The daemon reads **one** config file, `rafikid.toml`, from `$RAFIKI_CONFIG` if
set, else `<ConfigDir>/rafikid.toml` (`~/.config/rafiki/rafikid.toml` by
default). A missing file is not an error — the defaults apply. Decoding is
strict: an **unknown key or section fails startup**, so a typo cannot silently
do nothing. `[telemetry]` is currently the only section.

```toml
[telemetry]
endpoint = "http://tempo.tempo.svc:4318"   # set ⇒ tracing on
protocol = "http/protobuf"                 # "http/protobuf" (default) | "grpc"
content  = "rules"                         # "off" (default) | "all" | "rules"

[[telemetry.content_rule]]                 # fields AND within a rule
user = "brent"

[[telemetry.content_rule]]                 # rules OR with each other
provider = "ollama"                        # providers.toml entry name
model    = "deepseek/*"                    # path.Match glob
```

| Key | Meaning |
|---|---|
| `endpoint` | OTLP collector URL. Setting it turns tracing on. |
| `protocol` | `http/protobuf` (default) or `grpc`. |
| `content` | `off` (default), `all`, or `rules`. See [Content policy](#content-policy). |
| `[[telemetry.content_rule]]` | One selection rule: `user` (the child owner's username), `provider` (a `providers.toml` entry name), `model` (`path.Match` glob). Every non-empty field must match; rules are OR-ed. |

The following are fatal at startup: an unknown `protocol` or `content` value;
`content = "rules"` with zero rules; a rule that sets no field (it would
otherwise match everything); an invalid model glob; a rule naming a provider
that is absent from `providers.toml`.

**Standard `OTEL_*` environment variables override the file**, which keeps
Kubernetes-injected configuration working:

- **Tracing on:** an endpoint set by either `OTEL_EXPORTER_OTLP_ENDPOINT` or
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` enables tracing and overrides
  `[telemetry] endpoint`. Tracing is on iff an endpoint is set by the env or
  the file.
- **Protocol precedence**, first non-empty wins:
  `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` > `OTEL_EXPORTER_OTLP_PROTOCOL` >
  `[telemetry] protocol` > `http/protobuf`.
- **Auth headers** are supplied only through `OTEL_EXPORTER_OTLP_HEADERS`
  (and the traces-specific form). They are deliberately **not** a
  `rafikid.toml` key — secrets do not live in that file.
- **Endpoint paths:** `[telemetry] endpoint` and
  `OTEL_EXPORTER_OTLP_ENDPOINT` are base URLs — the exporter appends
  `/v1/traces` (so `http://tempo:4318` is enough). The per-signal
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is used **verbatim**, per the OTel
  spec: with no path it means `/`, so give it the full
  `http://tempo:4318/v1/traces`.
- The SDK's own knobs (`OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BSP_MAX_QUEUE_SIZE`,
  `OTEL_TRACES_SAMPLER`, `OTEL_RESOURCE_ATTRIBUTES`, …) are honored as usual.

The exported resource carries `service.name = "rafikid"`,
`service.version`, and `service.instance.id` = the daemon id
(`RAFIKI_DAEMON_ID` or the generated one). The default sampler is
parent-based always-on.

See [`docs/examples/rafikid.toml`](../examples/rafikid.toml) for a commented
sample.

## Spans

One trace per turn. A turn is a root span; that turn's tool calls, LLM calls
and postgres queries nest under it. Both child kinds produce the same shape:
fundi emits `TurnStart` from its engine's `AgentStart`, and claude emits it at
its first assistant frame; both flow through the daemon's single event-publish
path, which feeds the span builder.

| Span | When | Parent |
|---|---|---|
| `invoke_agent <child name>` | one per turn, `TurnStart` → `TurnEnd` | **root** (no parent); causality is a link, see below |
| `execute_tool <tool name>` | `ToolExecutionStart` → `ToolExecutionEnd` (and the tool result, either order) | the turn, or the spawning `Task` tool span for a Claude sub-agent thread |
| `chat <model>` | one per LLM call: fundi through `pkg/llm`, claude through the proxy face | the enclosing span, else the child's open turn, else a root (daemon-own jobs) |
| `llm.conversation.send` | the conversation wrapper around history load, compaction and send | the caller's span, else the child's open turn |
| `llm.conversation.compact` | a compaction pass | as above |
| Connect server span | every **unary** Control RPC, on both the unix socket and the TLS mount | the request's inbound `traceparent`, else root |
| PostgreSQL query span | a query issued while a span is already active | that span |

Connect **streaming** RPCs are deliberately not instrumented: their lifetime is
owned by a handler (a parked `StreamEvents` can last a session), so a span
wrapped around one would end when the client goes away rather than when the
work does. What those streams carry is traced from the event pipeline instead.

PostgreSQL spans are started only **inside another span**, so background work
(indexer, budget sweeps) never produces root traces. Query parameters are
never recorded — prompt and tool-argument text must not leave the daemon on a
database span.

### Turn attributes (always)

`gen_ai.operation.name=invoke_agent`, `gen_ai.agent.id` (child id),
`gen_ai.agent.name`, `gen_ai.request.model`, `enduser.id` (the child owner's
username), `rafiki.kind` (`fundi` | `claude` | `script`),
`rafiki.root_id`, `rafiki.parent_id`, `rafiki.turn.trigger`, and on turn end
`rafiki.stop_reason`, `rafiki.tool_calls`, `rafiki.reply.bytes`, and
`rafiki.cost_usd` when priced. A turn interrupted by an overlapping turn, a
child exit or shutdown gets `rafiki.turn.interrupted=true` and status Error.

### Tool span attributes

`gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`,
`gen_ai.tool.call.id`, `rafiki.thread` (`main` | `subagent`), and for a
sub-agent thread `rafiki.thread.parent_tool_use_id`. Always-on sizes:
`rafiki.tool.args.bytes`, `rafiki.tool.result.bytes`, `rafiki.error.bytes` on
a failed result. A failed result sets status Error.

### `chat` span attributes

`gen_ai.operation.name=chat`, `gen_ai.request.model`,
`gen_ai.provider.name`, `gen_ai.response.model`,
`gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
`gen_ai.usage.cache_read.input_tokens`,
`gen_ai.usage.cache_creation.input_tokens`,
`gen_ai.response.finish_reasons`, `rafiki.upstream`, `rafiki.protocol`, and on
the fundi path `rafiki.model`, `rafiki.primary`, `rafiki.routing`,
`rafiki.breaker.open`, `rafiki.entrypoint`. Token counts live **on `chat`
spans only**: a turn's final usage is not a turn total, so summing cache reads
onto the turn would overstate it. The turn carries `rafiki.cost_usd`, which is
a real total. Retries and failovers are span events (`retry`, `failover`).

## Links and the agent tree

Cross-child causality is a **span link**, never a parent: a child's
`invoke_agent` links to the turn that caused it, and the link carries
`rafiki.link.reason`:

- `spawn` — the child was spawned by a parent agent (a link to the parent's
  open turn);
- `send` — a parent agent sent the child a prompt (a link to the sender's open
  turn);
- `operator` — the request came from outside the agent tree: a Connect `Spawn`
  or `Send` whose context carries the operator's span (the Connect unary
  server span, itself a child of the inbound W3C `traceparent` when one was
  sent, or a CLI/scheduler caller's span).

Links are attached only at the target's next `TurnStart`, and the first
pending link's reason becomes `rafiki.turn.trigger` (`spawn` | `send` |
`operator` | `unknown`; `unknown` when nothing recorded a cause). A prompt
sent mid-turn therefore links the target's **next** turn.

Because links do not reparent, a whole agent tree is one query over the
root-child attribute rather than a trace walk. In Grafana/Tempo TraceQL:

```
{ span.rafiki.root_id = "<root child id>" }
```

## Content policy

Metadata — ids, `enduser.id`, model, provider, durations, sizes, tool names,
stop reason, cost, token counts on `chat` spans — is always exported.

Prompt, reply and tool-argument **text** is opt-in through
`[telemetry] content`:

- `off` (default) — no content attributes at all, only sizes;
- `all` — content for every child;
- `rules` — content only for a child matching at least one
  `[[telemetry.content_rule]]` (fields AND within a rule, rules OR).

The rule decision is made **once per turn**, at `TurnStart`, from the child's
owner and its resolved provider and model, and covers that turn's user
message, tool arguments and reply snippet. A fundi child's provider is the
first path segment of its model id (`anthropic/claude-x` → `anthropic`); a
claude child's is always `anthropic`. `chat` spans carry no opt-in content —
metadata and errors only.

When allowed, content is capped before it reaches a span:

| Content | Cap | Attribute |
|---|---|---|
| user message (for a child: the spawn/send prompt) | first 2 KiB, cut on a UTF-8 rune boundary | `rafiki.user_message` |
| tool arguments | each string leaf ≤ 256 B verbatim; longer → `{"sha256":"<12 hex>","bytes":N}`; whole JSON cut at 4 KiB with `rafiki.tool.args_truncated=true` | `gen_ai.tool.call.arguments` |
| reply | first 200 runes of the turn's final assistant text | `rafiki.reply_snippet` |
| tool result | size only, never content | `rafiki.tool.result.bytes` |

The 2 KiB / 256 B / 4 KiB / 200-rune caps are constants, not configuration: a
span attribute is a fixed-size carrier, and a cap an operator can raise is one
that eventually ships a megabyte to a collector. No regex secret redaction is
attempted — the opt-in is the control.

## Errors

**Errors are always exported, in full, regardless of `content`.** This is the
one path by which content leaves the daemon without opting in (for example, a
token in a failed command's argv), and it is deliberate.

An error is recorded as an OTel exception span event (`RecordError`) carrying
the whole text, with the span status set to Error. The only cap is size: 64
KiB, keeping the **first and last 32 KiB** with an elided-bytes marker between
them, because the decisive line of an error is usually last, and a single
multi-megabyte error would otherwise get the whole trace dropped by a
backend's per-trace byte limit. Invalid UTF-8 is replaced with U+FFFD before
the cap is measured. The true size is available as `rafiki.error.bytes`.

What is covered:

- a tool result with `is_error` (the tool span);
- an LLM call failure — provider status plus response body — on `chat`;
- an `Error` event during a turn (on the turn span);
- a retry/failover reason (span events on `chat`);
- an interrupted turn — status Error and `rafiki.turn.interrupted=true`.

## Metrics

Prometheus pull, served at `/metrics` with OpenMetrics enabled. These families
are independent of tracing and are always recorded:

| Metric | Type | Labels |
|---|---|---|
| `rafiki_turns_total` | counter | `kind`, `model`, `provider`, `stop_reason` |
| `rafiki_turn_duration_seconds` | histogram | `kind`, `model`, `provider` |
| `rafiki_tool_calls_total` | counter | `kind`, `tool`, `error` |
| `rafiki_tool_duration_seconds` | histogram | `kind`, `tool` |
| `rafiki_cost_usd_total` | counter | `kind`, `model`, `provider` |

There is deliberately no `child_id`, `user` or `conversation` label — that is
unbounded cardinality in Prometheus; those dimensions live in traces. The
latency histograms (these and the pre-existing `rafiki_llm_turn_latency_seconds`)
attach the active `trace_id` as an exemplar so a spike can be opened directly
as a trace.

## Logs

Logs are not exported — they go to stdout/stderr and are shipped by whatever
collects those (e.g. Alloy → Loki). Every record logged inside a valid span
through a context-carrying `slog` call gains `trace_id` and `span_id` fields,
so a log line joins to the trace it belongs to. Records logged through a
context-less `slog.Info(...)` stay as they were.
