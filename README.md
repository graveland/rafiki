# rafiki

Standalone LLM proxy / capture / agent library, extracted from
savannah-client's LLM stack.

**Built (phases 1–4):** the routing core (per-upstream breaker, OpenRouter
catalog, model resolution, prefix_hash, SSE capture parsing); the typed
builder API + DB-backed `Conversation` (trim policy, cache breakpoints,
write-ahead persistence); agent-loop primitives (`agentloop.Run`/`Resume`
with fabricated-error crash recovery); both proxy faces — Anthropic
`/v1/messages` and OpenAI `/v1/chat/completions` — behind an `Authenticator`
seam; static bearer-token auth; Prometheus metrics; OTLP tracing; and the
standalone binary (`cmd/rafiki serve|migrate`, `--dev` mode). sc imports the
module in-process and mounts the same faces; sc's diagnose loop, Slack agent
and core-dump analyzer run on the library.

## Layout

```
llm/        typed builder + Conversation — the library front door
routing/    breaker, OpenRouter catalog, model resolution, ClassifyFailure,
            prefix_hash, SSE capture parsing, turn store
agentloop/  ToolSet, Run/Resume, Events — tool-use loop primitives
store/      conversations schema migrations (source of truth), message
            persistence, recovery queries
server/     HTTP faces (/v1/messages, /v1/chat/completions), Authenticator
            seam, static token auth, Prometheus metrics
cmd/rafiki/ standalone binary (serve, migrate, --dev)
goldenwire/ frozen golden-wire fixture definitions (see note below)
```

## Model selection

Requests on the `/v1/messages` face (and `llm.Conversation`s) take:

- a concrete Anthropic id (`claude-opus-4-8`) — sent to the Anthropic
  primary, with breaker-gated OpenRouter failover when configured;
- a `<family>-latest` alias (`opus-latest`) — resolved live from the
  OpenRouter catalog to the family's newest Anthropic model;
- a **short model alias** (`kimi-k3`, `deepseek-v4-pro`, `deepseek-v4-flash`,
  `glm-5.2`) — resolved live from the catalog to the newest release of that
  model line (the id itself or a stamped point release like `-0905`, never a
  variant fork or a new line), yielding an OpenRouter slash id;
- any OpenRouter slash id (`moonshotai/kimi-k3`) — routed directly to
  OpenRouter with no failover: the caller asked for that specific model.

Some model lines carry a **provider pin** (`routing.ProviderPrefsFor`):
open-weight models are served by many OpenRouter providers of varying
quantization and data-retention policy, so pinned lines get an OpenRouter
`provider` routing object injected restricting them to vetted hosts
(`glm-5.2` → Fireworks). A caller-supplied `provider` field always wins
over the pin.

No concrete model ids are hardcoded: aliases name families/lines and the
catalog is the source of truth, so an unresolvable alias errors instead of
falling back to a stale id. Slash ids and model aliases require
`OPENROUTER_API_KEY`. The `/v1/chat/completions` face does no resolution —
it takes raw ids and routes by configured prefix.

## Development

```bash
make check   # vet + golangci-lint + unit tests (-race)
make test    # tests only
make build   # bin/rafiki

# integration tests (migrator, capture store) need TimescaleDB >= 2.22 on
# PostgreSQL 18:
docker run -d --name rafiki-test-db -p 5433:5432 \
  -e POSTGRES_PASSWORD=postgres timescale/timescaledb:2.28.2-pg18
RAFIKI_TEST_DSN='postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable' \
  make test
```

### Running locally

```bash
cp .env.example .env   # fill in ANTHROPIC_API_KEY (+ OPENROUTER_API_KEY)
make run               # serve --dev on :8035 against rafiki-test-db
```

`make run` sources the gitignored `.env`, defaults `RAFIKI_DB` to the
`rafiki-test-db` container above (auto-migrating in `--dev` mode), and
accepts the client token `dev`. Then, in another shell:

```bash
make claude                          # Claude Code through the local proxy
make claude ARGS='-p "quick question"'
psql 'postgres://postgres:postgres@localhost:5433/rafiki_live' \
  -c 'select model, status, upstream from conversations.conversation_turn'
```

`make claude` preflights `/healthz` and fails with a hint if the server
isn't up; `RAFIKI_URL`/`RAFIKI_TOKEN` retarget it (any Anthropic-protocol
client works the same way via `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`).

## `rafiki agent`

`rafiki agent <stats|search|export|analyze|findings>` is a DSN-backed CLI
over the captured `conversations` schema: read-only insights, the
LLM-driven skill-gap detector, and finding triage. See
[`docs/agent-cli.md`](docs/agent-cli.md) for every verb, flag, and the dev
loop.

Install it first — `make install` puts `rafiki` in `GOBIN` (else
`$(go env GOPATH)/bin`); without that, run it as `go run ./cmd/rafiki agent`.

```bash
make install
rafiki agent analyze --corpus DIR --compact --out DIR   # no DSN, no credentials
```

## Schema ownership

This repo owns the `conversations` schema. The migration chain baselines at
scadmin's 0007–0009 state; `store.Migrate` detects an existing scadmin-shaped
schema and adopts it (records the baseline as applied without executing).

## Model effort adaptation

Some OpenRouter models reject an `output_config.effort` value Claude Code sends
(e.g. `gpt-5-codex` accepts only `medium`). The proxy learns each model's
allowed set at runtime: on a rejection that enumerates supported values, it
records the constraint in an in-memory cache, clamps the effort, and retries the
request once, so the client gets a working response instead of a dead turn.
Subsequent requests to that model clamp proactively. The cache is per-process
(never persisted) and starts empty, so it always reflects the current provider
behavior. See `routing/effortmap.go` (`EffortCache`) and `server/proxy.go`
(`effortRetry`).

## goldenwire status

The committed goldens (`routing/testdata/goldenwire.json` and
`goldenwire_insitu.json`) were generated from savannah-client's
PRE-extraction `pkg/routing` at 4fa61974 and are asserted by
`routing/golden_test.go` / `golden_insitu_test.go` in CI;
`llm/golden_builder_test.go` additionally binds the llm builder's constructed
requests byte-identical to the in-situ recordings.

`goldenwire/` holds the fixture definitions those goldens were computed over,
as a plain package the tests import. The generator that produced the testdata
is retired (the pre-extraction code it ran against was deleted in the phase-1
swap) and survives only in git history — the testdata is a frozen recording
and must never be regenerated.
