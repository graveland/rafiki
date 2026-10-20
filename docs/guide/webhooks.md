# Webhook gateway

`rafiki-hooks` (`cmd/rafiki-hooks`) is how a third-party integration wakes one
of your agents. It authenticates a sender's webhook, decides from your rules
whether the delivery is worth waking an agent for, and reports it to `rafikid`
over the `Notify` RPC — one call, one narrowed token. It holds no database
DSN, no executor and no daemon code: it depends on no pgx package
(`TestHooksDoesNotLinkPostgres`) and on none of the daemon-side packages that
guard names — the inbox, the stores, the execpool, the server
(`TestHooksDoesNotLinkDaemon`, a denylist over importable daemon packages,
because `cmd/rafikid` is `package main` and can never appear as a dependency).
It never answers a sender `2xx` before `Notify` has returned, so an
acknowledged DELIVERABLE event is a stored one; the `ignored` and `duplicate`
`200`s are answered without calling `Notify` at all, because there is nothing
to store.

Config is one TOML file (`-config <path>`);
[`cmd/rafiki-hooks/example.toml`](../../cmd/rafiki-hooks/example.toml) is a worked
example. The shape:

```toml
[daemon]
url = "https://rafiki.example.com"                 # https unless the host is 127.0.0.1/localhost
token_file = "/run/secrets/rafiki-hooks-token"     # a {notify} token; a file, never a literal
listen = ":8080"                                   # optional: ca_file, tls_cert/tls_key
timeout_seconds = 10                               # optional: default 10, must be under the 30s write timeout

[[route]]
path = "/hooks/github"                             # the exact path a sender posts to
source = "github"                                  # github | gitea | generic
secret_file = "/run/secrets/github-webhook-secret"
target = "oncall"                                  # the NAME of the top-level agent to wake
notify_source = "github"                           # optional: the Notify.source the daemon sees; default = source

[[route.rule]]
event = "workflow_run"                             # "" matches any event
key = "github/workflow_run/{repository.full_name}/{workflow_run.id}"
match = { action = "completed", "workflow_run.conclusion" = "failure" }
fields = { repo = "repository.full_name", url = "workflow_run.html_url" }
```

A path that is not listed answers `404` with no body. `github` and `gitea`
verify their own HMAC-SHA256 signature header against the route's secret in
constant time; `generic` compares the header the route names
(`header = "X-Omada-Secret"`) the same way and is for a device inside your
network, because it cannot sign a body. The body is bounded by `max_body`
(default 1 MiB) before anything reads it. Rules are tried in order and the
first match is delivered; `match` is dotted-path → exact string equality and
`key`/`fields` interpolate `{dotted.path}` (only scalars have a text form — an
object or array is treated as absent). Numbers keep the sender's literal text.
A delivery id already delivered is answered `ok`/`duplicate` without waking the
agent twice (the dedup store is in memory with a 24 h window, so a gateway
restart can deliver one event again).

Config is validated at startup and the gateway refuses to run rather than fail
silently: an unknown key, a missing secret or token file, an empty one, a
`notify_source` the daemon would refuse (the same `pkg/notify` charset the
daemon enforces), a route on `/healthz` (the mux would shadow it), a route
path that is not clean (empty, no leading `/`, `//`, or a `.`/`..` segment —
`ServeMux` would redirect it before the gateway saw the request), and a
`timeout_seconds` at or past the server's 30 s write timeout (the sender's
connection would drop after the notification was accepted) are all startup
errors.

The rendered fragment has to fit the daemon's per-fragment cap — 8192 bytes
(`RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT`) — not just this surface's own field
caps, so a rule forwarding a long commit message or body is refused on EVERY
delivery with a `422` naming both sizes. Keep interpolated values short.

| Response | Meaning |
|---|---|
| `200` | delivered (`ok`), or deliberately not delivered (`ignored` — no rule matched; `duplicate` — this delivery id was already handled) |
| `400` | the body was unreadable, or was not JSON (`unreadable body`, `invalid json`) |
| `401` | the signature did not verify |
| `404` | no route is configured at this path |
| `405` | the path is a route but the method is not `POST` (`Allow: POST`) |
| `413` | body over `max_body` |
| `422` | the sender's input can never work — unknown target, invalid payload, an oversize fragment: a retry cannot help |
| `503` | the daemon could not take it right now (unavailable, timeout, a transport failure) — retry |
| `500` | anything else, including the gateway's OWN token being refused (expired, revoked, rotated) — an operator problem, and a redelivery after the fix succeeds, so a retrying sender should not give up |

A `422` on every delivery is the signature of a `target` naming no live
top-level agent (or of a payload the daemon refuses), not a sender problem —
the log line names the code. A refused gateway token is a `500` instead, and
its log line says the daemon refused the credential: mint a new one and
restart. `GET /healthz` answers `ok` without touching a route or the daemon, so
liveness and delivery are separate signals. A `GET` on a configured path
answers `405` while an unknown path answers `404`, which does reveal that a
route exists — accepted, because a bad-signature `POST` already reveals it and
the URL is never the secret.

The token is the only credential the gateway holds, and it can do nothing but
`Notify` — for the owner's own top-level agents. Mint it on the daemon host,
for the user that OWNS the agents `target` names:

```
rafikid user token mint oncall-bot --capability notify
```

The target is resolved among THAT user's live top-level agents only, so a
`target` belonging to another user is a permanent `422` (`not_found`) even
though the agent exists. The plaintext prints once; put it in the file
`token_file` names (a mounted secret, trimmed of surrounding whitespace — and
readable by the image's nonroot uid, 65532: a root-owned `0600` secret file
fails startup). Minting a narrowed token, and seeing one, is host-CLI only —
the Connect API carries no capability field (see
[`docs/agent-cli.md`](../agent-cli.md)).

**Deploy order matters.** A `{notify}` token is narrow only for a binary that
reads the capability column (migration `0055_user_token_capabilities`). A
daemon older than that reads such a row as `NULL` capabilities — full user
authority, on the credential that sits on an internet-facing host. So mint
notify tokens only AFTER every daemon sharing the database has been upgraded,
and revoke them BEFORE rolling any daemon back.

A leaked notify token is bounded in authority — it can call `Notify` and
nothing else, only against its owner's own live top-level agents — but not in
effect: it can enumerate those agents by guessing names (an unknown target is
`404`, a hit names the child id), and every notification is a durable inbox row
that wakes the agent and spends the owner's LLM budget, with no per-token rate
limit or pending-row cap on this path. That blast radius is accepted for v1.

Build it yourself as the `hooks` image target — a static binary on distroless,
running as nonroot (it is deliberately not the Dockerfile's default target,
which stays `release`). No release workflow publishes this image, and the
goreleaser `rafiki-hooks` binary lands in no archive, so `docker build` is the
only way to obtain it:

```
docker build --target hooks -t rafiki-hooks .
docker run --rm -p 8080:8080 \
    -v /etc/rafiki/hooks.toml:/etc/rafiki/hooks.toml:ro \
    -v /run/secrets:/run/secrets:ro \
    rafiki-hooks -config /etc/rafiki/hooks.toml
```

To deliver a notification by hand — for testing a route, or for a sender with
no webhook of its own — `rafiki notify` does the same call from the CLI:

```
rafiki notify oncall --source github --key repos/acme/rafiki/push event=push branch=main
rafiki notify oncall --source sentry --field-file body=payload.json
```

AGENT is the top-level agent's NAME (the daemon resolves it, so this works
with a notify-scoped token that may not list children); a field is
`name=value`, split on the first `=`, and `--field-file name=path` (`-` for
stdin, once) carries a value that is awkward to quote. The child id prints on
success. See `docs/reference/control-protocol.md` §"Third-party
notifications".
