# Development

## Prerequisites

**[ripgrep](https://github.com/BurntSushi/ripgrep) (`rg`) must be on `PATH`.**
Not optional: the fundi runtime's file-discovery tools are built directly on
it, and `BuildRuntime` refuses to start without it (`apt-get install
ripgrep` / `brew install ripgrep`).

[rtk](https://github.com/rtk-ai/rtk) is genuinely optional (`RAFIKI_BASH_RTK`
defaults to `auto`).

```bash
make check    # vet + golangci-lint + unit tests (-race) — the full local gate, no CI on this repo
make test     # tests only
make build    # all Go binaries into bin/
make install  # copy them to ~/.local/bin (override with DESTDIR=)
make help     # every target
```

Integration tests (migrator, capture store) need TimescaleDB >= 2.22 on
PostgreSQL 18 with `vector` and `pg_textsearch`. Run a dedicated, disposable
server for them: every DB-backed test creates and drops its own scratch
database, and with durability off a drop costs ~20ms instead of ~1s.

```bash
docker run -d --name pg-test --restart unless-stopped \
  -p 127.0.0.1:5433:5432 \
  --tmpfs /home/postgres/pgdata:rw,uid=1000,gid=1000,size=4g --shm-size=1g \
  -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=rafiki_test \
  timescale/timescaledb-ha:pg18 postgres \
  -c shared_preload_libraries=timescaledb,pg_textsearch \
  -c fsync=off -c synchronous_commit=off -c full_page_writes=off \
  -c wal_level=minimal -c max_wal_senders=0 -c archive_mode=off \
  -c checkpoint_timeout=1d -c max_wal_size=8GB \
  -c max_connections=500 -c shared_buffers=1GB -c timescaledb.telemetry_level=off
RAFIKI_TEST_DSN='postgres://postgres@127.0.0.1:5433/rafiki_test?sslmode=disable' \
  make test
```

Never point `RAFIKI_TEST_DSN` at a server holding data you care about.

`make test` sources a gitignored `.env` and warns loudly when
`RAFIKI_TEST_DSN` is unset — without it every DB-backed test *skips* while
the run still reports success.

## Running locally

This section builds rafiki from source; if you installed a release, see [Install](install.md) instead.

```bash
cp .env.example .env   # fill in ANTHROPIC_API_KEY (+ OPENROUTER_API_KEY), and RAFIKI_DB
set -a; . ./.env; set +a
go run ./cmd/rafikid migrate   # once, against a fresh database
make run                       # rafikid in the foreground, proxy face on :8035
```

`rafikid` serves the proxy itself — `/v1/messages`, `/v1/chat/completions`,
`/v1/messages/count_tokens`, and `HEAD /api/hello` — so claude
children get capture, failover and model resolution with no second process.
The fundi kind never uses the face; it reaches the library in-process.

The face binds all interfaces by default (`RAFIKI_PROXY_LISTEN`, default
`:8035`). Auth is always required, and the first user is minted on the
daemon host ([deployment](deployment.md#identity)), so while `make run` is up:

```bash
go run ./cmd/rafikid user create dev --admin --token
```

The host CLI prints the plaintext token to stdout exactly once and writes no
profile of its own; copy it into the client profile's token file (`rafiki
profile add <name> --url … --token …`, or paste it where the profile's token
file lives), and `rafiki claude` picks it up with nothing further to export.

```bash
make claude                                   # Claude Code through the local proxy
make claude ARGS='--model glm-5.2'            # …on any model the proxy can route
make claude ARGS='-- --permission-mode plan'  # …passing claude its own flags
```

`--model` accepts a concrete id, a `<family>-latest` alias, or an OpenRouter
slash id — the launcher registers it as a *custom model option* rather than
setting `ANTHROPIC_MODEL`, which Claude Code would otherwise validate against
a client-side allowlist and reject. It also pins
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` to the model's real context window and
strips inherited `ANTHROPIC_*` variables so a nested launch can't land on the
outer session's captured conversation.

Every proxied session gets rafiki's MCP agent-control surface injected
automatically (a `--mcp-config=<path>` pointing at the proxy's `/mcp` mount,
with the credential carried as a `RAFIKI_MCP_TOKEN` env var Claude Code
expands at connect time — never in argv, which is world-readable via `ps`).
The config JSON is staged to a file, not inlined, for the same reason.
Daemon-spawned `--kind claude` children also get a short coordination prompt
staged to a `--append-system-prompt-file`, steering them toward `agent_spawn`
over the built-in `Task` tool. The system-prompt appendix and the MCP config
ride staged files on every claude launch path, including the executor's
`rafiki daraja serve` host process (`pkg/executor/admin.go` stages the
appendix and passes `--append-system-prompt-file <path>`, and
`cmd/rafiki/cmd_daraja.go` reads it back into the child spec; the host restages
the same content-addressed file for the claude grandchild). Any other
Anthropic-protocol client gets the same routing via
`ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`.
