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

## Capturing a raw provider exchange

`rafikid proxy record` is a standalone diagnostic: it forwards `POST
/v1/messages` to one upstream and writes every exchange to disk byte for byte.
It consults no config file, profile or database, opens no socket, and is the one
`rafikid` command path that runs with `RAFIKI_DB` unset — which is what makes it
usable when the daemon is the thing that is broken. (`rafikid`'s shared
pre-cobra environment-file load still runs, exactly as it does for every
subcommand, so `service.env` is read into the process environment; the recorder
reads none of it, and a missing file is silent.)

```bash
go run ./cmd/rafikid proxy record \
  --listen 127.0.0.1:0 \
  --upstream https://api.anthropic.com \
  --record-dir /tmp/anthropic-capture
```

`--listen` must be loopback (`127.0.0.1`, `::1` or `localhost`; port `0` picks a
free port). `--upstream` must be an absolute `http`/`https` URL with no
userinfo. `--record-dir` must be absent (created `0700`) or empty, and never
group/world accessible; the recorder refuses rather than relaxing permissions.
Any other route or method is a `404` and writes nothing.

Three lines go to stderr once the listener is bound:

```
rafikid proxy record: listening on http://127.0.0.1:64959
rafikid proxy record: recording to /tmp/anthropic-capture
rafikid proxy record: WARNING: the record directory holds raw HTTP headers and bodies, …
```

The first line's address is the one actually bound, so a port-0 listener is
discoverable by reading it. Each exchange becomes `NNNNNN-request.http` and
`NNNNNN-response.http`, numbered in arrival order from `000001`. Both files are
HTTP messages — CRLF line endings, the start line, the headers sorted by name
(one line per value, credentials included), a blank line, then the exact body,
an SSE stream included. The request file carries a `Host:` line, which Go's
server otherwise lifts out of the header map; a response the recorder generates
itself (a `502 Bad Gateway` for an upstream transport failure) is recorded in
the same file format, never a sidecar.

Files are staged as `0600` temp files in the record directory and renamed into
place only once both halves are written, request first and response second. A
reader must therefore treat the `-response.http` file as the completion marker:
a crash between the two renames can leave a lone request half, but a response
half never appears without its request. The pair is committed *after* the
response has been relayed, so a reader that has just read the last body byte
must wait for the `-response.http` file to appear; its presence implies the
request half is already there.

### Driving a session instead of waiting for one

`--exec` makes the recorder a harness: it runs a command with
`ANTHROPIC_BASE_URL` pointing at its own listener, waits for it to exit, drains
the exchanges it made, and exits with that command's status. Everything else in
the environment passes through untouched — including the credential, which the
recorder relays upstream and records.

Composed with `rafiki claude --turns`, one command captures a real multi-turn
session (a text turn, a tool-call turn, and the follow-up that reads its output)
with no daemon and no manual port juggling:

```bash
rafikid proxy record \
  --listen 127.0.0.1:0 \
  --upstream https://api.anthropic.com \
  --record-dir "$HOME/private/capture" \
  --exec 'rafiki claude --model anthropic/haiku-latest --turn-file turns.txt'
```

`turns.txt` holds one prompt per line (`#` comments and blank lines are
skipped), or pass `--turn "..."` repeatedly. `--script script.toml` records several
claude processes in one capture (a session killed mid-tool-call and resumed by
a second one, for example); see the scripted-sessions paragraph in
`docs/guide/claude-code.md`. The scripted session runs Claude
Code headless in the same stream-json mode a daemon-managed claude child uses,
so every turn is a prompt on one process, one conversation and one session id —
rather than one independent `-p` run per prompt.

A scripted run resolves **no profile**: it takes its endpoint from `--url` or
the inherited `ANTHROPIC_BASE_URL`, and its credential from `--token`,
`ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`. That is what lets the whole
capture run under `env -i` on a host with no rafiki config and no daemon —
`--turn` with none of those set is a clear error rather than a 401 from
upstream.

**Bodies are uncompressed.** The recorder forwards no `Accept-Encoding`, so the
upstream answers with an identity body and a response file is readable text — a
gzip blob is neither inspectable nor useful as a fixture. The client's own
`Accept-Encoding` is still recorded in the request file, because that is what
the client asked for.

**The recordings are raw, except for credentials.** Bodies are stored exactly as
they crossed the boundary, so prompts and completions are there in clear text.
The credential headers — `Authorization`, `Proxy-Authorization`, `X-Api-Key`,
`Anthropic-Api-Key` — are written as their scheme plus the sha256 of the token
(`Authorization: Bearer sha256:2143…`), which keeps a capture comparable across
runs without the recording ever holding a token that still works. Redaction is a
recording rule only: the upstream still receives the real credential.

`metadata.user_id` is reduced the same way, and for the same reason: Claude
Code puts a device id and the account uuid in it, which is what makes a capture
unshareable. Name further fields with `--redact-json-field <dotted.path>`. Like
the credential headers, this is a recording rule — the upstream receives the
body untouched, and every other byte of it is preserved.

Treat the directory as a secret anyway — the bodies are not sanitized.
