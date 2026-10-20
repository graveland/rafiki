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
| PostgreSQL | managed or self-run | version 15 or newer, two extensions (below) |
| `rafikid` | one process per daemon identity | the database, TLS cert, provider keys |
| executors (`rafiki executor serve`) | wherever agents should work | outbound TLS to the daemon, an enrollment token |
| `rafiki-hooks` | optional, internet-facing | the daemon's URL and a narrow token |
| clients | anywhere | a profile naming the daemon and a token |

Executors and clients always dial the daemon; the daemon never dials them.
Executors can therefore sit behind NAT or inside a VM with no inbound ports.

## PostgreSQL

`rafikid` refuses to start without `RAFIKI_DB`: history, cost accounting, the
task ledger, user identity, the executor plane and conversation leases all live
in the database, so there is no database-less mode.

- **PostgreSQL 15 or newer.** The schema uses `NULLS NOT DISTINCT`.
- **Extensions `vector` and `pg_textsearch`.** Both are untrusted extensions.
  A superuser must `CREATE EXTENSION` them before the daemon's own role
  migrates, and `pg_textsearch` must be in `shared_preload_libraries`. The
  daemon's role does not need to be, and in production should not be, a
  superuser.
- **The daemon owns the schema.** `rafikid migrate` (and the daemon at
  startup) bring the database to the head of the embedded migration chain. It
  is idempotent, and concurrent callers are serialized by an advisory lock, so
  two daemons booting together apply it exactly once.

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

1. Upgrade the **daemon** first and let it migrate the database
   (`rafikid migrate`, or simply start it).
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
