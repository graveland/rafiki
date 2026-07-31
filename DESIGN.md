# Rafiki: Vision, Architecture, and Status

## 1. The End State

Rafiki is designed to be the **central LLM system for a whole organization**. Every agent, every Slack bot, every TUI,
every automated workflow, all LLM traffic routes through rafiki. It is the single place where:

- **Conversations are captured.** Every request and response, every tool call, every token, stored in a
  TimescaleDB-backed conversations database. The DB is the canonical record of all LLM activity across the company.

- **Multiple clients connect to the same conversation.** An incident fires → a conversation starts. An engineer joins
  from Slack. Another connects from a TUI. They all see the same transcript, the same tools, the same state. Rafiki is
  the shared conversation bus, not a point-to-point pipe.

- **Model selection and fallback are infrastructure, not application code.** Anthropic as primary, OpenRouter as
  fallback, configured once, inherited by all consumers. Model aliases (`opus-latest`, `kimi-k3`) resolve live. Circuit
  breakers and retry are transparent.

- **Out-of-band self-improvement replaces in-loop instruction-following.** Instead of asking a running agent to "learn
  from this conversation," conversations are stored in the DB and analyzed after the fact. The `analyze/` pipeline runs
  an LLM over captured conversations to find skill gaps, knowledge to persist, and wasted effort. Findings are ranked,
  deduplicated, and can auto-generate proposed skill edits. You can also re-analyze the same conversation with different
  prompts and different models to compare results, something impossible with in-loop self-improvement.

- **The proxy is a reconnaissance tool.** Because rafiki is also a generic Anthropic/OpenAI proxy storing every message,
  you can route Claude Code, Codex, Cursor, or any other harness through it and record exactly what they send. Then
  compare: what system prompt does Cursor use? How does it order tools? What context does it assemble? The DB lets you
  replay the same conversation through different models and measure correctness, cost, and latency, a harness evaluation
  platform.

- **Fundi + rafiki is the experimentation harness.** Fundi's native agent runtime, spawn tool, and coordinator
  architecture make it the platform for benchmarking: run the same task through different model tiers, measure cost per
  accepted leaf, compare functional correctness. More than that, the coordinator decomposes large jobs into appropriate
  sub-tasks with the appropriate models, not necessarily Anthropic. An Opus-tier planner + DeepSeek-tier workers can
  beat frontier-solo on cost at equal or better quality. The coordinator can dispatch to any model the catalog supports.

  **This is the north star.** Endor Labs' Agent Security League proved the harness moves coding benchmarks more than the
  model does: GPT-5.5 scored 61.5% functional correctness in Codex and 87.2% in Cursor, a 25.7-point swing from the
  harness alone. Claude Opus 4.7 scored 87.2% in Claude Code and 91.1% in Cursor. *Both frontier models performed better
  in a competitor's harness than in their own maker's.* A third-party harness can beat first-party. The discipline lives
  in the harness, cache architecture, context assembly, tool policy, subagent decomposition, and Cursor's
  closed-source harness is proof it's possible. Fundi is being built to be the
  answer. ([Tunguz, "Aftermarket Harnesses," July 2026](https://tomtunguz.com/aftermarket-harnesses))

---

## 2. The Problems This Solves

### 2.1 Conversations are ephemeral without a store

Without a DB, an agent conversation disappears when the process exits. You can't reconnect to it from a different
client. You can't audit it later. You can't analyze it for patterns. You can't re-run it with a different model to see
if the outcome improves. The conversations schema makes every interaction durable and queryable.

### 2.2 Self-improvement shouldn't compete with the agent's context window

Asking an agent to "review this conversation and improve" consumes its context window, costs tokens, produces
unreliable results, or is completely ignored. Out-of-band analysis, running a separate LLM over the stored conversation,
comparing outputs, ranking findings, doesn't compete for the agent's attention. It happens in parallel, after the fact,
and the results feed back into skills and prompts for the next session.

### 2.3 Harness evaluation requires recorded data

You can't compare Cursor vs Claude Code vs Codex without recording what each one sends. Rafiki's proxy captures every
request byte-for-byte. Replay the same stored conversation through different models. Swap the system prompt. Change the
tool set. The DB makes harness evaluation empirical instead of anecdotal.

### 2.4 Every consumer shouldn't re-solve the same problems

Without rafiki, every LLM consumer has its own API key management, model selection, circuit breaking, prompt caching,
conversation persistence, and cost tracking. Now they can import rafiki or route through the proxy and get all of it for
free.

### 2.5 Caching is too important to leave to individual developers

Prompt caching is a 90% discount on input tokens, and input tokens are 86-98% of LLM traffic. But caching requires
discipline: static content before dynamic, deterministic tool ordering, breakpoint budget awareness. Rafiki's
`Conversation` builder enforces these rules. `prefix_hash` on captured turns detects drift. The library makes it harder
to get caching wrong than right.

---

## 3. Architecture

```
                           ┌───────────────────────────┐
                           │    Multiple Clients       │
                           │                           │
                           │  Slack                    │
                           │  TUI (fundi-attach)       │
                           │  Claude Code (via proxy)  │
                           │  Automated workflows      │
                           │  Incident triggers        │
                           └────────┬──────────────────┘
                                    │ all connect to the same conversation
                                    │ via library import or HTTP proxy
                                    ▼
┌───────────────────────────────────────────────────────────────┐
│                       rafiki                                  │
│                                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │ Server (HTTP faces)                                     │  │
│  │  /v1/messages (Anthropic protocol)                      │  │
│  │  /v1/chat/completions (OpenAI protocol)                 │  │
│  │  Authenticator seam · Prometheus · OTLP                 │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │ Library                                                 │  │
│  │                                                         │  │
│  │  llm/       - typed builder, Conversation, cache hygiene│  │
│  │  agentloop/ - tool-use loop, Run, Resume, crash recovery│  │
│  │  routing/   - model catalog, breakers, SSE capture,     │  │
│  │               OpenRouter resolution, prefix_hash        │  │
│  │  analyze/   - detector, drafter, ranking (out-of-band)  │  │
│  │  insights/  - stats, search, transcript export          │  │
│  │  store/     - migrations, message persistence, pricing  │  │
│  └─────────────────────────────────────────────────────────┘  │
│                                                               │
└────────────────────────────┬──────────────────────────────────┘
                             │
                             ▼
┌──────────────────────────────────────────────────────────────┐
│  Postgres / TimescaleDB                                      │
│                                                              │
│  conversations schema, hypertables for time-series scaling   │
│  ┌──────────────┐ ┌──────────────────┐ ┌──────────────────┐  │
│  │ conversation │ │ conversation_turn│ │ conversation_msg │  │
│  └──────────────┘ └──────────────────┘ └──────────────────┘  │
│  ┌──────────────────┐                                        │
│  │ analysis_findings│ - skill gaps, knowledge, grind         │
│  └──────────────────┘                                        │
└──────────────────────────────────────────────────────────────┘
```

---

## 4. How Conversations Flow

### 4.1 Incident-driven conversation

1. An alert fires → an agent starts a conversation and triggers a diagnostic.
2. The conversation is created in rafiki's DB. Every turn, tool call, and response is captured.
3. An on-call engineer connects from Slack.
4. Another engineer connects from a TUI: `fundi attach <id>`.
5. Everyone sees the same state. Commands from any client route through the same conversation.
6. When the incident resolves, the transcript is stored. Some time later, `rafiki agent analyze` runs over it to find
   skill gaps or process/tooling improvements.

### 4.2 Harness evaluation

1. Route Claude Code through rafiki's proxy. Run a task. Every request is recorded.
2. Route Cursor through the same proxy. Run the same task. Every request is recorded.
3. Query the DB: diff the system prompts, tool definitions, context assembly order.
4. Replay the stored conversation through different models: `agentcli compare --corpus DIR`.
5. Measure: functional correctness, tokens consumed, cost, latency. Per-model, per-harness.

### 4.3 Coordinator-driven experimentation

1. A coordinator agent (fundi, Opus-tier planner) receives a complex task.
2. The coordinator decomposes it: these sub-tasks can run in parallel, these depend on each other, this leaf needs a
   security review, that leaf is a routine refactor.
3. Sub-tasks are dispatched to workers at appropriate model tiers: Opus for architecture decisions, DeepSeek for bulk
   implementation, Haiku for linting and formatting.
4. Every sub-agent call is captured in rafiki's conversations DB with full cost accounting.
5. The same task can be re-run with different model assignments, different context densities, different review policies.
   The DB holds the evidence, which combinations work, what they cost, how much the workers thrash.

### 4.4 Out-of-band improvement

1. `rafiki agent analyze` scans stored conversations for patterns: where the agent got stuck, what it re-asked, which
   skills would have prevented repeated failures.
2. The detector produces ranked `Finding` records. The drafter generates proposed skill edits. The detector and drafter
   have configurable prompts and models to optimize the results.
3. A human reviews and accepts the proposed skills.
4. The next agent session loads the updated skills. The loop closes, improvement happens without consuming the agent's
   context window.

---

## 5. What's Built

### Done (phases 1–4, shipped)

- **Proxy:** Anthropic `/v1/messages` and OpenAI `/v1/chat/completions` faces. Static bearer-token auth. Prometheus
  metrics. OTLP tracing. `--dev` mode for zero-config local use. Every request and response captured to the DB.

- **Library:** `llm.Client` + `llm.Conversation` with cache hygiene (static-first ordering, prefix_hash drift detection,
  trim policy). `agentloop.Run`/`Resume` with crash-safe recovery (synthetic errors for orphaned tool calls, max 3
  resume attempts). Bounded concurrency (6), 50KB tool-result truncation.

- **Routing:** Per-upstream circuit breaker. OpenRouter catalog with live model resolution. Short aliases (`kimi-k3`,
  `deepseek-v4-pro`). Provider pins for open-weight models. Runtime effort adaptation. SSE capture parsing.
  Failover triggers on a transient primary failure (5xx/429/transport — retried per `routing.RetryBackoffs` first)
  **and** on an out-of-credit primary account: a 400 that is pointless to retry but leaves the primary unusable, so it
  fails over immediately and trips the breaker (`routing.CreditExhausted`).

- **Storage:** TimescaleDB hypertable schema for `conversation`, `conversation_turn`, `conversation_msg`. Price-correct
  token cost tracking. Schema migration chain with baseline adoption of existing data.

- **Analysis:** `analyze/` pipeline, detection, drafting, ranking, compaction. `insights/`, stats, search, transcript
  export, per-model cost rollups. `rafiki agent analyze --corpus DIR` runs corpus-only (no API credentials needed).

- **Testing:** Golden-wire tests verify byte-identical request construction against pre-extraction recordings. 341
  tests, 0 skipped, green under both `go.work` and `GOWORK=off`.

### In Progress / Next

- **Multi-client conversation join:** The store and proxy exist. What's missing is the routing layer that lets Slack,
  TUI, and other clients attach to an existing conversation and participate, the protocol for submitting prompts and
  receiving events on shared conversations.

- **Provider-as-configured-backend registry:** Today there are two hardcoded upstreams. The design generalizes to a
  `Provider` interface where any OpenAI-compatible endpoint (local vLLM, OpenRouter, direct Anthropic) is a configured
  provider keyed by name. The current model-id-prefix approach is a forward-compatible subset.

- **Streaming sender in the library:** The library sender is non-streaming; only the proxy faces parse SSE. For fundi's
  token-level attach TUI, a streaming sender is needed. SSE parsing already exists in `routing/`.

- **Conversation compaction:** When context windows fill, compaction summarises the conversation and starts a new
  session. Compaction needs to interact with prompt caching correctly (cache-safe forking) and needs to work when
  multiple clients are connected.

- **Coordinator + spawn tool:** Fundi M2, the decomposition skill, spawn tool, and spec-density rules that let a
  planner-tier coordinator dispatch work to worker-tier sub-agents at appropriate model tiers. The coordinator is
  currently designed as prompt-based (Approach A in fundi's design doc); proven-out shapes may be hardened into scaffold
  tools.

---

## 6. Key Design Decisions

- **Library-first, proxy-optional.** Everything works as a library import. The proxy binary is a thin shell. Go
  consumers import the library; other languages hit the proxy over HTTP. Both paths store to the same DB.

- **The proxy doesn't modify the traffic.** Parsing and storage are secondary to traffic fidelity, so it shouldn't break
  as the upstream APIs evolve.

- **Capture is transparent and automatic.** No opt-in. Every conversation through library or proxy is persisted. The
  store is the source of truth for cost, model usage, and history.

- **Cache hygiene is enforced.** The `Conversation` builder's ordering rules make it hard to break prefix caching.
  `prefix_hash` on captures makes drift detectable.

- **Recovery is safe.** Agent crashes don't re-execute side effects. Synthetic errors let the model decide whether to
  retry. Attempt caps prevent infinite loops.

- **Improvement is out-of-band.** Analysis runs over stored conversations, not in the agent's context window. The same
  conversation can be re-analyzed with different prompts and models, empirical comparison, not vibes.

- **The harness is the product.** Endor Labs proved a third-party harness (Cursor) can beat first-party (Claude Code)
  with the same model. The discipline, context assembly, caching, decomposition, tool policy, lives in the harness.
  Rafiki provides the infrastructure; fundi provides the runtime. Together they're the open-source answer to Cursor's
  closed-source win.
