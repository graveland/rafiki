# Deploying rafiki for real

[Installing rafiki](install.md) covers one machine: `rafiki setup` puts the
database, daemon and executor on your laptop, and everything talks over a unix
socket. This page is for the other shape: a daemon on a server (a VM or
Kubernetes) that several people, executors and integrations reach over the
network.

There is no `rafiki setup` for this and no published Helm chart (one is
planned). You run the pieces yourself; this page lists what they need.

## The pieces

```
  laptops ── rafiki / web UI / SDK ──┐   Connect + proxy faces, TLS, bearer tokens
                                     ▼
  senders ── rafiki-hooks ──────▶  rafikid ──────▶ PostgreSQL
  (GitHub, Gitea, devices)           ▲  ▲          (vector, pg_textsearch)
                                     │  │
        executors dial IN over TLS ──┘  └── LLM providers (Anthropic, OpenRouter, …)
        (VMs, containers, laptops)
```

| Piece | Where it runs | Needs |
|---|---|---|
| PostgreSQL | managed or self-run | version 18 or newer; two extensions for recall search (below) |
| `rafikid` | one process per daemon identity | the database, TLS cert, provider keys |
| executors (`rafiki executor serve`) | wherever agents should work | outbound TLS to the daemon, an enrollment token |
| `rafiki-hooks` | optional, internet-facing | the daemon's URL and a narrow token |
| clients | anywhere | a profile naming the daemon and a token |

Executors and clients always dial the daemon; the daemon never dials them.
Executors can therefore sit behind NAT or inside a VM with no inbound ports.

## PostgreSQL

`rafikid` refuses to start without `RAFIKI_DB`: history, cost accounting, the
task ledger, user identity, the executor plane and conversation leases all live
in the database, so there is no database-less mode. (The `embedded` mode is a
single-machine install — `rafikid` runs its own PostgreSQL there; see [Installing
rafiki](install.md#the-embedded-database). A server deployment uses a real
database, below.)

- **PostgreSQL 18 or newer.** The schema defaults every id to `uuidv7()`, which
  is PostgreSQL 18's own function and ships with no shim here, so an older server
  fails at the first migration. (`NULLS NOT DISTINCT`, which the schema also
  uses, only needs 15.)
- **Extensions `vector` and `pg_textsearch` are needed for recall SEARCH only.**
  Both are untrusted extensions, and `pg_textsearch` must be in
  `shared_preload_libraries`. The recall migration creates them when the server
  has them available, but that needs superuser and the daemon's role does not
  need to be, and in production should not be, one — so have a superuser
  `CREATE EXTENSION` both before `rafikid migrate`. Without `pg_textsearch` the
  daemon runs and memory put/get/tree/delete work, but recall search is off —
  `Recall` and `RecallBackfill` answer Connect `Unavailable` ("recall search
  needs the pg_textsearch extension, which this database does not have") and the
  recall indexer and summarizer do not run. Without `vector` alone, recall
  search is BM25-only and embeddings are skipped.
- **They must exist BEFORE the first `rafikid migrate`.** The recall migration
  picks the schema's shape once — `vector` embedding columns and the three bm25
  indexes where the extensions are present, `bytea` placeholders and no bm25
  indexes where they are not — and nothing re-runs it later. Install `vector` or
  `pg_textsearch` after that first migrate and the startup probe reports them
  present, so the daemon does not take the degraded path, while the tables keep
  the old shape: recall search then fails at query time. Nothing converts such
  an install automatically: it needs `ALTER TABLE ... ALTER COLUMN embedding
  TYPE vector` on each of `memory`, `conversation_window` and
  `conversation_summary`, plus the three bm25 `CREATE INDEX` statements, run by
  hand — the exact DDL is in `pkg/store/migrations/0036_recall.up.sql` — or a
  database migrated from empty.
- **TimescaleDB is optional.** Migrations create a hypertable only where
  `timescaledb` is installed, and a plain table otherwise, so a database
  without it migrates and runs.
- **The daemon owns the schema.** `rafikid migrate` brings the database to the
  head of the embedded migration chain. It is idempotent, and concurrent callers
  are serialized by an advisory lock, so two daemons booting together apply it
  exactly once. The daemon itself migrates **only with `--dev`**
  (`cmd/rafikid/main.go`); a production daemon started without it runs against
  whatever schema is there and fails loudly on the first write a new column
  needs, so run `rafikid migrate` as its own step.

### Backups

`rafikid db backup <path>` writes a `pg_dump -Fc` archive over the wire (the
connection rides `PG*`, never argv), and `rafikid db restore <path> --force`
puts one back. For a configured `RAFIKI_DB` both need a `pg_dump`/`pg_restore` on
the daemon host's `PATH`: `pg_dump` is refused when it is older than the server
(it cannot dump a newer one), and `pg_restore` must be new enough to read the
archive it is given. The embedded server needs neither: its own tree ships them,
at its exact major, and they are what run. A physical copy of the data directory
(`--format txz|tgz`) is a different thing, embedded-only, chosen only explicitly,
and stops that server for the duration.

The restore is `pg_restore --clean --if-exists --no-owner --single-transaction`:
every object the ARCHIVE contains is dropped and recreated in one transaction, so
a failure rolls back, and everything the archive does not contain is left alone.
It destroys what it covers, so `--force` is required; a run without it only
names the database it would replace. On a database with `timescaledb` the
command brackets the restore with that extension's own
`timescaledb_pre_restore()`/`timescaledb_post_restore()` calls, which is what
lets `--clean` drop and recreate a hypertable's constraints at all — without
them pg_restore stops at the first `ALTER TABLE ONLY ... DROP CONSTRAINT` with
"ONLY option not supported on hypertable operations". The post call runs even
when the restore fails, because `pre_restore` leaves the extension's DDL hooks
off database-wide until it does.

That bracketing has consequences a production operator at this privilege level
will meet.

**The hooks need a superuser connection**: they run `ALTER DATABASE ... SET
timescaledb.restoring`, which no other role may set, and then stop and restart
the extension's background workers, which only a superuser may do. `GRANT SET ON
PARAMETER timescaledb.restoring TO <role>` does not get you there — it clears the
setting and then fails on the workers (measured on a TimescaleDB 2.x server). So
an in-place restore over a TimescaleDB database is refused cleanly, before
anything is touched, unless `--db` names a superuser's connection. The daemon's
own role deliberately is not one, which is why this verb is an operator step and
not something the daemon runs.

**Ownership is the connecting role's, decided rather than constant.** `pg_restore
--no-owner` makes that role the owner of everything it creates, so a superuser
restore does not pass it: the archive's `OWNER TO` statements put the original
owners back, which for an in-place restore is the daemon's unprivileged role. A
dump restored by a superuser *with* `--no-owner` hands the whole schema to the
superuser, and the daemon then gets `permission denied` on its first query —
during an incident. If the archive names a role this server does not have, the
restore fails whole rather than reassigning.

**A restore into a fresh database is a superuser operation too**, even though it
needs none of those hooks: rafiki's dump creates the untrusted `vector` and
`pg_textsearch` extensions, and only a superuser may `CREATE EXTENSION` them.

**Stop every daemon on the database first**, dump restores included: `pre_restore`
works by setting a database-level default, so a connection opened while the
restore runs inherits the restoring flag for its whole life, and the post call
cannot clear a session that is already open. `rafikid db restore` refuses to
start while a local daemon answers on its socket for that reason, and `--force`
is the acknowledgement.

## The daemon image

The `Dockerfile`'s default target, `release`, builds the daemon image:
`rafikid` plus the `rafiki` CLI, `ripgrep` (which the `fundi` runtime requires
on `PATH`) and `rtk`. It runs as uid 1000 (`rafiki`), has `rafikid` as its
entrypoint, and exposes two ports:

| Port | What |
|---|---|
| `8035` | the proxy face (`RAFIKI_PROXY_LISTEN`): Anthropic `/v1/messages`, OpenAI `/v1/chat/completions`, `/mcp`, `/healthz`, `/metrics`. Children reach their own daemon here over loopback |
| `8036` | the TLS control listener (`RAFIKI_CONTROL_LISTEN=tcp:8036`) |

```sh
docker build -t rafikid .
docker run -d --name rafikid -p 8036:8036 \
  -e RAFIKI_DB=postgres://rafiki:…@db.internal:5432/rafiki?sslmode=verify-full \
  -e RAFIKI_DAEMON_ID=rafikid-0 \
  -e RAFIKI_CONTROL_LISTEN=tcp:8036 \
  -e RAFIKI_CONTROL_TLS_CERT=/tls/tls.crt \
  -e RAFIKI_CONTROL_TLS_KEY=/tls/tls.key \
  -e RAFIKI_EXECUTORS_ENABLED=1 \
  -e ANTHROPIC_API_KEY=… \
  -v /etc/rafiki/tls:/tls:ro \
  rafikid
```

### What the daemon needs

| Setting | Why |
|---|---|
| `RAFIKI_DB` | required; see above |
| `RAFIKI_DAEMON_ID` | a stable name for this daemon, recorded as the holder of its conversation leases so a restarting daemon reclaims its own children at once instead of waiting out the 5-minute lease TTL. **Required where the filesystem is ephemeral** (Kubernetes) because the laptop default generates one into the data dir. **Must be unique per running daemon**: two daemons sharing a value reclaim each other's leases and both drive the same conversation |
| `RAFIKI_CONTROL_LISTEN`, `RAFIKI_CONTROL_TLS_CERT`, `RAFIKI_CONTROL_TLS_KEY` | the remote control plane. All three or the daemon refuses to start; TLS is mandatory on TCP. Certificates are re-read on every handshake, so cert-manager rotation needs no restart |
| `RAFIKI_EXECUTORS_ENABLED=1` | executor enrollment is **off** by default once a TCP control listener exists, so that opening the control plane does not silently open executor enrollment. Turn it on deliberately |
| provider keys | `ANTHROPIC_API_KEY` for `fundi` children (and `OPENROUTER_API_KEY` for non-Anthropic models) in the daemon's environment, or a `providers.toml` in its config dir |
| `RAFIKI_CONFIG` | optional path to `rafikid.toml` (telemetry) |

The whole variable list is in [Configuration](configuration.md#environment) and
`.env.example`. Config files (`providers.toml`, `oidc.toml`, `rafikid.toml`,
`instructions.md`, `mcp.json`) are read from the daemon's config dir
(`$XDG_CONFIG_HOME/rafiki`, i.e. `~/.config/rafiki` for the `rafiki` user);
mount them as a ConfigMap/Secret or a volume.

### One hostname, one port, one certificate

The TLS listener serves everything remote parties need: the Connect control
plane, the executor link (`/executor/connect`), the daraja link
(`/daraja/connect`), the proxy faces, `/rafiki.v1.Login/`, `/healthz` and
`/metrics`. The executor and daraja links are HTTP/1.1 connection upgrades, so
the listener advertises only `http/1.1` in ALPN and requires TLS 1.2 or newer.

If you put a load balancer or ingress in front, it must pass HTTP/1.1 upgrades
through to the daemon. Terminating TLS there is possible, but the daemon's
listener is TLS-only, so re-encrypt to it. Every request is authenticated
by the daemon itself; the network layer is not trusted to do it.

## Identity

There is no bootstrap mode. A fresh daemon with a database has zero users, logs
that once, and refuses every unauthenticated remote command. Create the first
admin on a host that can reach the database (a `kubectl exec` into the pod, or
any machine with the DSN):

```sh
rafikid user create alice --admin --token
```

The plaintext token prints once. Then, from a laptop:

```sh
rafiki profile add prod --url https://rafiki.example.net --token "$TOKEN"
rafiki profile use prod
```

For anything beyond a single shared token, configure single sign-on (`oidc.toml`)
so people log in with `rafiki login` and tokens expire on your terms. Both
flows, the `oidc.toml` keys and the provider recipes are in
[Authentication](authentication.md). Automation (CI, cron) uses minted service
tokens from the same page.

Ownership follows the user: presets, conversations and executors belong to the
user who created them, and an executor only serves its owner's agents.

## Executors

Agents have no workspace tools until an executor exists, and on a server the
daemon's own filesystem is deliberately not one. Run executors where you want
agents to work:

```sh
# on the machine the executor will run on
rafiki executor name builder-1
# wherever you are logged in as the owner: mint the enrollment token
rafiki executor enroll --name builder-1
# back on the executor machine
rafiki executor serve --connect rafiki.example.net:443 --enroll-token <token> \
  --root /work --launch claude
```

Run it under a service manager (`rafiki executor service install`) or as a
container:

```sh
docker run -d --name rafiki-executor -v /srv/work:/work \
  ghcr.io/graveland/rafiki-sandbox:latest \
  rafiki executor serve --connect rafiki.example.net:443 --enroll-token <token> --root /work
```

Declare what the machine is on its row, because the executor asserts nothing
about itself: `rafiki executor create --isolation container --workspace-mode
ephemeral --root /work --label env=ci`. `ephemeral` machines are interchangeable
(a lost executor reschedules its children elsewhere); `pinned` ones park their
children until they return. `--launch claude` lets the machine host Claude Code
children through daraja, and `--launch script` lets it host script children.

Isolation is whatever you compose: a container's mounts, a VM, a dedicated
host user. `--root` is a working directory, not a sandbox. Details, the relay
for local inference servers and sandboxes are in
[Executors, daraja and sandboxes](executors.md).

## Webhooks from third parties

Put `rafiki-hooks` (a separate static, nonroot, distroless image) at the
internet edge instead of exposing the daemon. It holds one token that can do
nothing but `Notify`, and no database credential. Setup, the route config, the
response codes and the deploy-order rule for notify tokens are in
[Webhook gateway](webhooks.md).

## Observability

- `GET /healthz` answers `ok` on the daemon and on the gateway (the gateway's
  probe does not touch the daemon, so liveness and delivery are separate
  signals).
- `GET /metrics` serves Prometheus metrics.
- Traces: set `OTEL_EXPORTER_OTLP_ENDPOINT` (or `[telemetry]` in
  `rafikid.toml`). One trace per agent turn; prompt text is off unless you opt
  in. See [Telemetry](../reference/telemetry.md).
- `RAFIKI_LOG_LEVEL=debug` for per-request housekeeping lines.

## Upgrading

1. Upgrade the **daemon** first, and run `rafikid migrate` before starting it
   (a production daemon does not migrate itself — only `--dev` does).
2. Then upgrade executors and clients. The wire protocol carries an epoch that
   is bumped on every breaking change; a mismatched peer is refused loudly
   rather than misbehaving, so a client or executor left behind shows up as an
   error naming the epoch, not as corrupt data. `rafiki upgrade` upgrades a
   self-managed install (verifying the release signature against keys built
   into the binary); Homebrew users use `brew upgrade`.
3. Restarting a daemon interrupts live agents. They recover on restart: leases
   are reclaimed by `RAFIKI_DAEMON_ID`, undelivered inbox messages replay, and
   executors reconnect with backoff.
4. **Mint notify tokens only after every daemon sharing the database is
   upgraded, and revoke them before rolling any daemon back.** An older daemon
   reads a notify-scoped token as a full-authority one.
5. Before adding a migration while another branch is deployed, check the
   highest version in the target database: the chain must stay contiguous.

### Upgrading across a backfill

A migration that rewrites data (migration 0057, which places every existing
conversation on the content-addressed tree, is the worked example) is a
**stop-the-daemon** upgrade: it holds `ACCESS EXCLUSIVE` on the message table
while it runs, and a daemon of the *old* release still writing afterwards leaves
rows the new one cannot see. The full procedure and its reasoning are in
[Conversation tree → Cutover procedure](../reference/conversation-tree.md); the
order that matters:

1. Truncate the debugging tables you do not need in the backup
   (`conversations.raw_http_request`), then `pg_dump -Fc` with the daemon
   stopped (`rafikid db backup --format dump <path>` is the same dump, when the
   daemon host has a matching `pg_dump`). **The dump is the rollback** — a
   migration that rewrites data has no down path worth trusting.
2. Rehearse the restore (into a fresh database, or in place as `rafikid db
   restore --force <path>`, which runs `timescaledb_pre_restore()` and
   `timescaledb_post_restore()` around pg_restore itself when the target has the
   extension), migrate, `rafikid tree check`, repeat until clean.
3. On the real database: stop every daemon writing to it, `rafikid migrate`
   (capture the log — the backfill's own validity signal is a line only it
   prints), `rafikid tree check` (must exit 0), `VACUUM FULL` the rewritten table
   if the migration changed its `fillfactor`, then start the new daemon.
4. After go-live the same `rafikid tree check` is the health probe and must exit
   0; it reports structure only, so a fork or compaction does not move it.

An installation upgrading from a release before the conversation tree must first
upgrade to the last release carrying migrations 0057-0061 and run the gate there:
see [Conversation tree → Cutover procedure](../reference/conversation-tree.md).

Measured on a 3,583-conversation / 321,165-message database: 1 m 07 s to migrate
and 49 s for the check, about 2 minutes of downtime, 63 MB peak RSS.

## Production checklist

- [ ] Postgres ≥ 15 with `vector` and `pg_textsearch`; the daemon's role is not a superuser
- [ ] `RAFIKI_DB` uses a verified-TLS connection if the database is remote
- [ ] `RAFIKI_DAEMON_ID` set and unique per daemon
- [ ] Control listener has a real certificate; clients connect with `https://`
- [ ] First admin created with `rafikid user create --admin`; the token is stored somewhere safe
- [ ] SSO configured, or tokens minted per person and per automation, never shared
- [ ] `RAFIKI_EXECUTORS_ENABLED=1` only because you intend to enroll executors
- [ ] Executors run inside the isolation you declared on their rows
- [ ] Provider keys are in the daemon's environment or `providers.toml`, not on clients
- [ ] `/healthz` wired to your probes, `/metrics` scraped, an OTLP endpoint configured if you want traces
- [ ] The webhook gateway, if any, runs as its own image with only a notify-scoped token
