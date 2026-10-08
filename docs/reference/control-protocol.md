# rafiki control protocol — Connect

The contract between the `rafikid` daemon and its clients — the `rafiki` CLI,
the cockpit (`rafiki attach`), the Python SDK, script children, and any
headless driver — over the Connect control plane: protobuf/JSON RPCs defined
in `proto/rafiki/v1/control.proto`, served over one unix socket locally and an
optional TLS listener remotely.

## 1. Goals

- One always-running daemon. Agent children are subprocesses it owns (kind
  `fundi` in-process, `claude` and `script` as child processes).
- Many concurrent clients per daemon. Drivers, watchers, CLI commands all
  attach freely. No exclusive-control semantics.
- Forever decoupled from a child kind's own protocol. The controller never
  introspects or validates a child's native frames; they flow through
  transparently (via `SendFrame`, for debugging and scripting), and rafiki's
  own event model is what clients see.
- Coordinator-friendly. Subscriptions support filtering so a high-level agent
  can listen for "interesting moments" without eating the token-by-token
  firehose, and pull details on demand.
- Survives controller restarts. Children that died are listed and explicitly
  resumable; the persisted session record is the durable conversation state.
- Forensics by default during development. Each child's streams are buffered
  in memory and dumped compressed to disk on exit.

## 2. Transport

One control plane, Connect (`connectrpc.com/connect`), with schema
`proto/rafiki/v1/control.proto` and generated Go in `pkg/gen/rafiki/v1/`,
served on two listeners:

### 2.1 Unix domain socket (default)

- **Path:** the daemon always binds `<RuntimeDir>/controller.sock` —
  `$XDG_RUNTIME_DIR/rafiki/` if that's set to an absolute path, else
  `$XDG_STATE_HOME/rafiki/` (default `~/.local/state/rafiki/`); see
  `pkg/paths.SocketPath`. There is no client-side socket override:
  `RAFIKI_SOCKET` is a hard client-side error (`profile.CheckRetiredEnv`) — a
  profile's `socket` field names the daemon instead — while the daemon itself
  injects `RAFIKI_SOCKET=<socket path>` into some children's environments so a
  spawned process can find its own daemon.
- **Mode:** socket `0600`, parent directory `0700`. Filesystem permissions are
  the trust boundary: admission is decided by filesystem permission, and a
  request that carries no credential proceeds anonymously. The identity
  resolution rule is under §2.3 below.
- **Transport:** h2c (HTTP/2 cleartext) AND HTTP/1.1 — a non-nil `Protocols`
  lists ONLY the supported protocols, so both are set explicitly (the same
  rule `pkg/childsock` serves the per-child socket under): the Go Connect
  client speaks prior-knowledge h2c, and a plain-httpx/curl client (the
  Python SDK's standalone face, a debugging `curl --unix-socket`) speaks
  HTTP/1.1; unary and server-streaming both work over either. Connect's
  server-streaming (`StreamEvents`) is what wants HTTP/2.
- Multiple concurrent client connections. There is exactly one control plane:
  every RPC in this document is reachable on this socket, and the mount is
  the same handler the TLS listener serves (`connectControlRoute`, both faces
  composed through `cmd/rafikid`'s `connectControlRoute` so there is exactly
  one place that decides what guards the plane).

### 2.2 Remote TCP/TLS (optional)

- Address: `RAFIKI_CONTROL_LISTEN` (e.g. `tcp:8036`). Unset → TCP disabled;
  the daemon listens on the UDS only.
- TLS is **mandatory** on TCP — the daemon refuses to start without
  `RAFIKI_CONTROL_TLS_CERT` and `RAFIKI_CONTROL_TLS_KEY`. No plaintext TCP
  control plane, ever. Certificates are re-read from disk per handshake via
  `tls.Config.GetCertificate`, so cert-manager rotation needs no pod restart.
  Minimum TLS version: 1.2. The listener advertises `http/1.1` only in ALPN
  (`execpool.ALPNProtocols`): net/http can hijack an HTTP/1.1 connection and
  not an HTTP/2 one, and every protocol on this listener is reached by
  upgrading out of a plain request.
- `RAFIKI_CONTROL_LISTEN` also **requires `RAFIKI_DB`**: control-plane
  identity is row-backed and there is nothing to degrade to. Without a
  database the daemon exits at startup rather than serving a listener it
  cannot authenticate.
- **What it serves:** the same Connect control plane (below), the executor
  link at `/executor/connect` (upgraded out of HTTP/1.1), the daraja link at
  `/daraja/connect`, and the proxy faces (`/v1/messages`,
  `/v1/chat/completions`, `/mcp`, `/rafiki.v1.Login/`, `/healthz`,
  `/metrics`) — one hostname on
  one port serves all of it. The face keeps its loopback listener too;
  children talk to their own daemon over 127.0.0.1 and need no certificate.
- **Authentication:** per-user bearer tokens, mandatory —
  `Authorization: Bearer` (also `x-api-key` / `X-Rafiki-Token`), resolved
  against the users store by `server.UserTokenAuth` and stored as a digest in
  `conversations.users`. The resolved identity is attached to the request
  context. Failure modes are distinct and must stay distinct: an unknown or
  tombstoned token answers `unauthenticated` with a fixed message ("invalid
  auth token"); an identity store that cannot be reached answers
  `unavailable` with a fixed message ("identity store unavailable") and never
  the store's own error text — reporting an outage as an invalid credential
  makes clients discard working tokens, and a store's error text names the
  database host, user and database.
- **No bootstrap mode.** There is no user-create-by-first-connection window:
  a credential is always required, and the Connect plane has no RPC that
  mints an admin. With zero active users the daemon logs **once, at
  startup**: `no users exist: create one on this host with rafikid user
  create <name> --admin --token`. `rafikid user create` opens the database directly —
  no running daemon needed — and `--admin` is never inferred; it must be
  passed explicitly or the created user is an ordinary non-admin user. A
  plaintext token is printed exactly once when one is minted (`--token`, or
  later `rafikid user token mint`); with no `--token` the user has no
  credential and logs in through OIDC (`rafiki login`) when the daemon has it
  configured. The daemon stores only a digest either way.
- An unauthenticated peer never receives an underlying error's text: an
  infrastructure failure on this listener carries a fixed message and the
  real error goes to the daemon log — the same redaction rule every Connect
  handler applies (§4).

## 2.3 The Connect control plane

`proto/rafiki/v1/control.proto`, generated Go in `pkg/gen/rafiki/v1/`, served
by `pkg/connectapi` on both listeners. Reachable at whatever the resolved
client profile names — the local unix socket for a profile with a `socket`,
or a remote daemon's TLS listener (`https://...`) for one with a `url`.

**Which endpoint a client picks:** `newConnectEndpoint`
(`cmd/rafiki/connectclient.go`) is the one place the CLI and cockpit resolve
an endpoint, from the client's one profile (`pkg/profile`) and nothing else —
a profile with a `url` is a remote daemon, one with a `socket` is local.
There is no `RAFIKI_URL`/`--socket` fallback: both are hard errors
client-side (`profile.CheckRetiredEnv`), because the failure they used to
allow was silent — an exported `RAFIKI_URL` used to outrank a local socket
with no message, so `rafiki list` and the cockpit could resolve to two
different daemons in the same shell with nothing to say so. A completion
handler, the cockpit and every verb resolve through the same helper; a
completion handler swallows errors by design, so a wrong dial fails silently
as "no candidates" rather than visibly.

Remote calls carry `Authorization: Bearer <token>` — the resolved profile's
own token — attached by a `RoundTripper` on the client's transport rather
than per call site, because the cockpit's `*http.Client` is handed to
`pkg/tui` and never seen again: a per-call header would authenticate the
pre-flight and leave `StreamEvents` unauthenticated, failing only once the
alt screen is already up. A profile naming a remote daemon with no token
fails before the round trip: this plane has no bootstrap mode, so an absent
credential can only ever produce a 401.

**Transport, remote:** HTTP/1.1. The shared TLS listener advertises
`http/1.1` only in ALPN because net/http can hijack an HTTP/1.1 connection
and not an HTTP/2 one, and `/executor/connect` is an Upgrade. connect-go
refuses only **bidi** streaming below HTTP/2; `StreamEvents` is
server-streaming and rides HTTP/1.1 chunked encoding. Pinned by
`TestStreamEventsSurvivesHTTP11ALPN`, which also asserts the negotiation
really was HTTP/1.1 so the test cannot go vacuous.

**Transport, local:** h2c (HTTP/2 cleartext) AND HTTP/1.1 on the same unix
socket (§2.1) — the Go Connect client speaks prior-knowledge h2c, a
plain-httpx/curl client (the Python SDK's standalone face, a debugging
`curl --unix-socket`) speaks HTTP/1.1; unary and server-streaming both work
over either.

- **Mode:** socket `0600` in a `0700` directory. The socket IS the credential
  for admission — the trust mechanism is filesystem permission, matching
  what a 0600 socket in a 0700 directory means — and no token is required.
- **Auth:** an interceptor (`pkg/connectapi.NewAuthInterceptor`) performs a
  constant-time bearer comparison on every call when the mount configures a
  token; **an empty configured token disables the check** — that is the unix
  socket case. The TLS mount configures a real token via `server.UserTokenAuth`.
- **Identity (both mounts):** `UserTokenAuth` resolves a presented credential
  and `server.WithIdentity` puts it on the request context, where the
  handlers read it. On the local unix socket the credential is resolved by
  the `optionalIdentityInterceptor` (`cmd/rafikid/connect_uds.go`): no
  credential → anonymous (the zero identity; the socket decided admission);
  a credential that resolves → that user (which is what lets a per-user read
  like `GetRateLimitStatus` resolve "who is asking" locally); a credential
  that is PRESENT but does not resolve → `unauthenticated` ("invalid auth
  token"), never a silent downgrade to anonymous; a store that cannot be
  checked → `unavailable` ("identity store unavailable", never the store's
  error text).
- **Authorization:** the policy interceptor (`cmd/rafikid/connect_policy.go`)
  classifies every procedure through a `procedure → policy` table and refuses
  whatever the caller's credential may not reach — the same table on the
  local socket and the TLS face, since both mounts compose through
  `connectControlRoute`. See "Who may call what" below.
- **Path prefix:** `/rafiki.v1.Control/`
- **Encoding:** protobuf or JSON. A unary call is an ordinary HTTP POST:

```bash
curl -H 'Content-Type: application/json' \
     -H "Authorization: Bearer $RAFIKI_TOKEN" \
     -d '{"childId":"c_01HX..."}' \
     http://127.0.0.1:8035/rafiki.v1.Control/GetHistory
```

A server stream's **headers arrive with its first message**, not before — a
stream with nothing yet to say sends no bytes at all (connect-go flushes
headers on the first `Send`). A client must therefore budget an idle-read
timeout rather than wait for headers: a quiet stream is the normal case, not
a dead connection. The Python SDK turns that quiet window into a
`deadline_exceeded` error its callers re-open on (cursor-replayed for
`settled()`, at-most-once for `Receive`).

The **per-child unix socket** (script children, and any child kind whose
tools are hosted) is described under "Script children" below: the socket is
the credential, and it reverse-proxies the daemon's whole proxy face.

### The protocol epoch

The wire carries a version so a stale peer fails loudly instead of silently
exchanging fields the two sides no longer agree on. `protocol.Epoch` is the
value — **2** today, and a peer that sends no header is an epoch-1 peer
(`pkg/protocol/epoch.go`; the constant is bumped on every wire-breaking
change). It rides the HTTP header **`Rafiki-Protocol`** (`protocol.EpochHeader`).

**Connect (control plane).** Every client request carries `Rafiki-Protocol: 2`
— the Go client transport sets it in `bearerTransport` (`cmd/rafiki/connectclient.go`),
unconditionally, token or not, and `rafiki-py` sets it in its own transport.
The daemon gates every `/rafiki.v1.` request with `pkg/server.RequireEpoch`,
applied at the OUTERMOST layer of BOTH mounts (before authentication, so the
authentication-exempt `Login` service is gated too — the `Login` route carries
no interceptors but still speaks the current wire). The gate sets
`Rafiki-Protocol: 2` on the response unconditionally (refusals included, so a
client can always read the daemon's epoch) and passes a request through only
when its header is exactly the daemon's epoch. A missing or different value is
a Connect error — code `failed_precondition` (HTTP 400), `ErrorInfo` reason
`protocol_mismatch`, message "this daemon speaks rafiki protocol 2; upgrade
your rafiki client (peer sent none)". A stream is refused with the SAME code,
message and reason, but in the form the caller's protocol expects: a Connect
streaming request (`Content-Type: application/connect+`) gets the enveloped
HTTP-200 end-of-stream error, so a connect-go streaming client surfaces the
`failed_precondition` and the message instead of a bare `internal: HTTP status
400 Bad Request` (its streaming client ignores the body of a non-200 response);
a unary request keeps the HTTP-400 JSON form. Every refusal — stream or unary —
still carries `Rafiki-Protocol: 2`.

**Connect (client side).** The Go transport (`checkDaemonEpoch`) and the
Python SDK both check the response's `Rafiki-Protocol`: a response without it
(an old daemon) or with another value fails with a **terminal**
`failed_precondition` (reason `protocol_mismatch`) — "daemon speaks rafiki
protocol N; this client speaks 2 — upgrade the daemon". It is never surfaced as
`unavailable`, which would read as "is rafikid running?" and make an
Unavailable-retrying caller (the cockpit's stream loop included) retry an old
daemon forever. The daemon's own `protocol_mismatch` refusal is never masked —
the client surfaces that more specific message as-is.

**Executor and daraja upgrade links.** The dialing side sets the header on the
HTTP Upgrade request (`upgradeconn.Dial`); `upgradeconn.Handler` refuses a
mismatch with a **plain 400** and a body naming both epochs ("rafiki protocol
mismatch: this side speaks 2, peer sent none"). It runs BEFORE `authorize`:
pure, no side effects, and a plain status that never hijacks — so a peer never
spends a one-shot ticket or enroll token on an exchange the far side cannot
finish. The 400 is terminal for the client (the existing 400/401/403-terminal
rule), so a stale executor fails at once rather than retrying forever. A 101
whose response carries no header (an old peer) or a different one is reported
by the dialer as a 400 `Refused`, terminal the same way.

### Deploy runbook (one deploy, at an idle time)

Stop every child (including daraja-hosted claude children, which survive a
daemon restart). Upgrade in this order: **daemons first**, then every executor
and daraja, then the CLIs. The daemons must lead because a new executor or
daraja dialing a NOT-yet-upgraded daemon has its one-shot `Enroll` token or
ticket SPENT — the old daemon runs `authorize` with no epoch gate, redeeming
the credential — before the new client refuses the header-less 101: the
credential is burnt for nothing (avoid enrolling a new executor during the
window). Rebuild and restart BOTH daemons (work, home) and EVERY executor and
CLI binary from the same commit; re-pin `rafiki-py` in the
workflow/eval/review-swarm repos; restart agents (sessions resume from the
database; script children cannot resume). Run `rafikid migrate` on each daemon
before starting it — rafikid applies migrations on startup only with `--dev`,
so a production deploy needs the explicit command — and confirm prod migration
max is ≤ 44 first. The protocol epoch makes a forgotten binary fail with a
clear message instead of misbehaving.

### Two event tiers

| Tier | Contents | Cursor | Resumable |
|---|---|---|---|
| Durable | Persisted messages, lifecycle events (`child_spawned`, `child_exited`), turns, tool executions | Per-child gap-free `ordinal` | Yes, exactly |
| Ephemeral | Live content deltas (`content_block_delta`) | None | No, by design |

`StreamEvents` uses `EventSubject` predicates, `EventTier`, and `EventCursor` (per-child ordinals).
Note that the durable event ordinal is **not** `conversation_message.ordinal`: it is a separate per-child sequence starting at 0 stored in `conversations.event_log`.

#### `EventSubject`

| Field | Meaning |
|---|---|
| `child` | One child, itself. |
| `subtree` | Descendants of this child, bounded by `max_depth`. **Never includes the root itself** unless `include_self` is set. |
| `all` | Everything the caller is entitled to. |
| `max_depth` | Hops below `subtree`. Unset or 0 means **unlimited**; 1 means direct children only. Ignored for `child` and `all`. |
| `label_selector` | Narrows only, never widens. Not the authority — authority is evaluated server-side and intersected. A malformed selector **excludes**. |
| `include_self` | Admits the `subtree` root itself. Ignored for `child` and `all`. |

`include_self` is the one subject field that **widens**. It is safe because it widens by exactly
one child — the one the subscriber already named — and authority is still intersected
server-side, so naming a child you are not entitled to admits nothing. It does not bypass
`label_selector`. The default is false so `subtree` keeps the meaning the agent-facing path
relies on: `max_depth=1` is "my direct children", never "me and my children".

#### Worked example: the cockpit's two subscriptions

The TUI runs two concurrent `StreamEvents` over one connection.

```
Rail    subject: {subtree: <id>, include_self: true}   -- or {all: true} for a bare attach
        tier:    DURABLE
        types:   [turn_end, agent_status, error, retry, child_spawned, child_exited]
        cursor:  {ordinals: {<child>: <highest received>}, floor: "<RFC3339>"}

Focus   subject: {child: <focused>}
        tier:    ALL
        cursor:  {ordinals: {<focused>: <highest rendered>}}
```

They are split rather than being one stream with a widened filter so that hopping never
interrupts rail coverage and a slow focus consumer cannot degrade the rail. The rail's type
filter deliberately excludes `assistant_message` and `user_message`: it needs to know a turn
*happened*, not what it said, and carrying them would ship every child's full content — tool
results included — to a pane a few glyphs wide. `content_block_delta` needs no exclusion
there because it is ephemeral and the rail subscribes at `DURABLE`.

`include_self` exists for the rail's first line. Attached to a child, a plain `subtree`
subscription hears about every descendant and never about the child itself — and because the
focus stream is `child`-scoped, the transcript looks correct right up until you hop away and
watch that row freeze.

### Verbs

Policy classes (`userOnly`/`anyCaller`/`childScoped`/`ownerScoped`) are
defined under "Who may call what" below; each row names its own.

| RPC | Kind | Purpose |
|---|---|---|
| `GetHistory` | unary · childScoped | Durable events for one child, after an optional ordinal |
| `StreamEvents` | server-streaming · childScoped | Follows events matching an `EventSubject` predicate (child; subtree with `max_depth` and optional `include_self`; or all) and `EventTier` (`DURABLE` or `ALL`), with optional replay from `EventCursor` |
| `Send` | unary · childScoped | Submit a prompt, steer, or abort to a child via the inbox seam; `message_id` is the durable row id, and is **empty** for an abort to a `claude` child (see "`Send` and the durable inbox" below). Optional `steps` run at send time and their rendered output is appended to the text; refused with `ABORT` (see "`Send` steps" below). An image block is resized on ingest to at most 1568px on either edge and 1.15MP (PNG stays PNG, JPEG stays JPEG, EXIF orientation applied; GIF/WebP pass through); a PNG/JPEG source above 50 megapixels (`imagefit.MaxSourcePixels`) is refused `InvalidArgument` before decoding; what is stored, echoed and sent to the model is the resized image. An image block whose bytes claim PNG/JPEG but do not decode is refused `InvalidArgument` |
| `ListChildren` | unary · childScoped | List children, optionally filtered by status (reports `started_at`/`last_activity` as `Timestamp`s, `latest_ordinal`, `cost_usd` and `max_cost` per child); answers only the caller's own subtree |
| `GetChild` | unary · childScoped | Get one child's summary by id (reports `started_at`/`last_activity` as `Timestamp`s, `latest_ordinal`, `cost_usd` and `max_cost`). Post-spawn state is observable here: `Spawn` is unary and returns as soon as the child is registered, so state is read back through `GetChild`, not through the spawn call |
| `Spawn` | unary · childScoped | Create a child with budget, executor, and label options. `kind` selects the child: `fundi` (default), `claude`, or `script` — a saved pymodule run as the child's process (§"Script children" below). For `kind: script` the request carries `script` (`ScriptSpec{repo, script, modules, args}`); every fundi/claude-only field is refused on a script spawn, and `prefill` with them. Fields 15–29 are operator-only (§"Spawn's operator-only fields" below) |
| `Kill` | unary · childScoped | Stop a child gracefully, escalating to SIGKILL if necessary. `shutdown_timeout`/`kill_timeout` (Durations) bound the graceful window and the SIGTERM→SIGKILL escalation; each unset or zero selects the daemon default (180 s / 30 s). `include_descendants` first stops every live descendant, deepest first (reported in `descendant_ids`); off, the subtree is left running |
| `Close` | unary · childScoped | Finalize an exited child: it leaves the store and its `conversations.child` row is dropped. An error for a live child — closing is never an implicit kill. `include_descendants` closes every descendant first, deepest first, each of which must already be exited (reported in `descendant_ids`). A descendant that cannot be ended aborts before the parent is touched, so no child is orphaned; one already exited or gone is skipped. Native Task subagents are never listed: they end and close with their parent. The daemon never refuses a parent for having descendants; the CLI and cockpit ask first |
| `SetBudget` | unary · childScoped | Change one child's `max_cost` (0 = unlimited). A user credential acts with operator authority — any child, no lineage or remaining-grant check; a per-child credential gets `agent_set_budget`'s rule — a direct child only, bounded by the caller's own remaining grant |
| `SetRouting` | unary · childScoped | Merge a routing-spec delta over one child's stored spec, effective on its next OpenRouter request (§"Route policy"). `delta` is the bracket-free spec string (`prefer=fireworks`, `sort=throughput,quant=fp8+`); the merge is delta-over-stored per key (`routing.Spec.Merge`), so a key the delta names wins and every other stored key stays. An empty `child_id` or `delta` is `CodeInvalidArgument`, as is a parse failure or a provider slug the directory does not know. Authority: a user credential may set any key and target any child; a per-child credential is bounded to its own subtree and may set only `prefer`, `sort` and `quant` — it may not set or change `only=`, and may not clear an operator's `only`/`nodata`/`zdr` (`CodePermissionDenied`). Returns the stored canonical spec after the merge |
| `ListModels` | unary · anyCaller | The daemon's model rows: one per id the daemon can resolve for a kind, with source and — when the catalog knows the id — optional context window, per-token USD prices and input modalities. `kind` scopes the sources (`claude` resolves only Anthropic ids; empty means the fundi default), `provider` filters by provider. Each row also carries `created` (a `Timestamp`; unset = no catalog entry) and `expires_at` (a `Timestamp`; unset = none). This is what `rafiki models` and `--model` completion read |
| `ModelInfo` | unary · anyCaller | The daemon's own catalog answer for ONE model, so a client never reads the OpenRouter catalog itself. `model` is required (`CodeInvalidArgument` otherwise). Never an error for an unknown model: `known=false` is an ordinary answer, and every caller degrades by leaving the model's own defaults alone — an unconfigured catalog answers the same way |
| `ModelRoutes` | unary · anyCaller | Where a request for one model WOULD go under a routing spec. `model` (required) is an OpenRouter id with an optional routing bracket (`deepseek/deepseek-v4.1-flash[sort=price,quant=fp8+]`); a leading `openrouter/` is stripped. Returns the base `model`, the canonical `routing` spec string (empty when none), one `RouteEndpoint` per hosting endpoint, `stale` when the endpoint list was served from an expired cache entry, and `stats_note` when measured stats are unavailable (e.g. the scrape failed) — stats are advisory, so a scrape failure is a note, never an error. Each endpoint carries the provider display name, `slug`, `tag`, `quantization`, optional `prompt_usd_per_mtok`/`completion_usd_per_mtok` (USD per MILLION tokens; absent when the catalog reports no price — a free price is `0`), `context_length`, optional `uptime_30m`, `tools`, `eligible` with `excluded_reason` (`""`, `quant`, `not-in-only`, `banned`), the 1-based `rank` among eligible rows and `preferred` (a slug in the spec's `prefer`), plus the matched stats (`p50_tokens_per_sec`, `p90_tokens_per_sec`, `p50_latency` (a `Duration`, unset when no stats matched), `stats_requests`; each absent when no stats row matched). Eligible rows come first, in predicted try-order — that order is a PREDICTION of OpenRouter's fallback, not a guarantee: OpenRouter's live health-based fallback is not reproducible here. A model line whose bracket does not parse is `CodeInvalidArgument`; an unavailable endpoint catalog, or a daemon that does not route via OpenRouter (no explainer wired), is `CodeUnavailable`. Open to any caller |
| `ListExecutors` | unary · userOnly | The executors in the daemon's pool **right now**, scoped to the caller. `kind` scopes eligibility the way ListModels' `kind` scopes sources: each row carries `id`, `machine` (the `machine` trust label), `labels`, `isolation`, `workspace_mode`, `roots`, `admits`, `enabled`, `connected`, `connected_at` and `last_seen` (Timestamps of the current connection's join time and of the row's last pool sighting — unset means not connected / never seen; the empty-kind management listing populates both, the kind-scoped rows leave both unset), `launch_kinds` and — when `kind` was given — `eligible` plus the `reason` it is not, computed by the SAME per-row reasoning a spawn attempt would produce (a hypothetical top-level spawn, which is what a human picking an executor is actually asking). The kind-scoped preview deliberately enumerates the WHOLE live pool — a foreign-owned row appears with its refusal `reason` rather than disappearing: the explanation names facts (labels, machine, admits) the row itself already displays, so hiding it buys nothing, while `eligible` never over-reports (eligible ⟺ an empty reason). Deliberately **live-only**: an offline durable executor cannot serve a fresh spawn either, so this answers exactly what `Spawn` would see, and is what `--executor` completion and the cockpit's executor picker read. The **empty-kind** branch is the full management listing (offline rows included, eligibility unevaluated), itself ownership-scoped: the anonymous unix socket and an admin user credential see every row, any other user credential sees only rows whose `owner_user_id` equals its own — durable rows and live transient session executors alike |
| `ListTasks` | unary · childScoped | One conversation's task ledger, mapped from `Controller.TaskList`. `conversation_id` empty means every conversation; `include_dropped` (or `all`) surfaces rows an agent abandoned, hidden by default; `child_id`, `status` and `limit` narrow further. rafiki requires a database, so a ledger that cannot answer is a real failure and surfaces as `CodeInternal` rather than as an empty list; the cockpit chooses to hide the box rather than surface it. Rows are clamped to 2000 — `tasks.ListFilter.Limit == 0` means unlimited. Each `TaskRow` carries `handle` — the dotted ordinal path ("2.1"), computed on read and never persisted — plus `content`, `active_form`, `status`, `assignee`, `drop_reason` and `conversation_id` (the row's owning conversation — a conversation id, not a child id; the child working the row is `assignee`) |
| `GetRateLimitStatus` | unary · anyCaller | The CALLER's own latest captured Anthropic subscription rate-limit snapshot (`anthropic-ratelimit-unified-*` response headers, captured by the proxy off genuine OAuth-passthrough traffic to `api.anthropic.com` — never OpenRouter-routed traffic, and never API-token usage, which has its own separate usage endpoint). Takes no request fields; the daemon resolves identity from the authenticated connection, the same as `Spawn`'s owner attribution — there is no way to ask for another user's usage. Returns `CodeNotFound` (not an empty message) when this user has never made a passthrough call, which is the expected state for anyone who has not used `rafiki claude --passthrough-auth`. `rafiki claude --limits` and the cockpit's status-line quota readout both poll this |
| `ListSkills` | unary · userOnly | The daemon's database-backed skill corpus (§"Skill management verbs") |
| `GetSkill` | unary · userOnly | One skill's full row, body included |
| `UpsertSkill` | unary · userOnly | Create or replace a skill at `(namespace, name)` |
| `DeleteSkill` | unary · userOnly | Hard-delete every row under a skill name |
| `SetSkillEnabled` | unary · userOnly | Flip one skill name's enabled flag |
| `ListPymodules` | unary · childScoped | The caller's own saved pymodules — latest live version per name (name, repo, version, description, `created_at` (a `Timestamp`); **no code**: an inventory is not a document) plus each registered git source's cached discovery (its scripts and packages as rows: repo = the source's name, zero version/created-at, no code). The request's optional `repo` is a scope FILTER — the one place an empty value means "everything" rather than the `local` sentinel: `local` lists the blob-store rows only, a git source's name only that source's rows, an unrefreshed name an empty list. The owner is resolved server-side from the authenticated context, never a request field — a user credential reaches its own bucket, and the anonymous unix-socket caller shares the daemon's single unattributed bucket, the same owner its own children resolve to. Unwired (DB-less daemon) → `CodeUnavailable` |
| `GetPymodule` | unary · childScoped | One saved pymodule's full row including source code, by name; an unknown or deleted name answers `CodeNotFound` |
| `PutPymodule` | unary · childScoped | Save a pymodule (name, code, optional description) — insert-only, so an existing name gains a new version, never an overwrite. The name must be a bare Python identifier (`CodeInvalidArgument` otherwise) and a successful write pushes the owner's corpus to its executors |
| `DeletePymodule` | unary · childScoped | Soft-delete every live version of one pymodule by name (`CodeNotFound` when none is live); a later Put under the same name restores it |
| `AddPymoduleGitSource` | unary · userOnly | Register (or repoint) one of the caller's git-backed pymodule sources at `(owner, name, url, ref)` — the human `name` becomes the `repo` argument across the pymodule tool surface, so `"local"` (the blob store's reserved sentinel) and anything but 1-64 letters, digits, `_` and `-` (not starting with `-`) are refused with `CodeInvalidArgument`. The daemon writes `conversations.pymodule_git_sources` (an upsert: an existing `(owner, name)` repoints url/ref in place — a registration is a pointer to repoint, not append-only history) and then fires the FIRST refresh synchronously, so a bad clone fails the call immediately (the row stays registered) |
| `ListPymoduleGitSources` | unary · userOnly | The caller's own registered git sources (name, url, ref), owner-scoped exactly as `ListPymodules`. Rows are the registrations only — the discovered inventory is served by `RefreshPymoduleGitSource` and by the pymodule discovery surfaces, never by this |
| `RefreshPymoduleGitSource` | unary · userOnly | Fan a re-pull of ONE named source out to every LIVE executor the caller owns whose `Describe` reports `pymodule_git_sync`, in parallel. Each executor fetches/hard-resets its checkout at the source's stored ref, re-runs discovery and rebuilds the repo's one shared venv; the daemon caches the LAST response as the source's inventory snapshot (an executor that fails contributes nothing and leaves any prior snapshot in place; only when every responder fails — or none is eligible — does the call error). Executors disagreeing on the discovered names are logged, not reconciled. The response carries the discovered `scripts`/`packages` (name + description), the venv's `venv_ready`/`venv_error`, and `name` echoing the refreshed source — so a client that refreshes several sources in turn (the CLI's `python repo refresh` with no name refreshes every registered source) attributes each response without pairing it to its request; a failed venv build is reported, not an error — the refresh itself succeeded |
| `RemovePymoduleGitSource` | unary · userOnly | Delete one of the caller's git source registrations outright (`CodeNotFound` when none is registered). Executors are never told — there is no prune model for git sources; a source's own history is its versioning, and the cached inventory for the name is simply never read again |
| `ListPresets` | unary · anyCaller | The caller's own agent presets — the latest live row per name, names optionally narrowed to a `prefix` (e.g. `"default:"` for one group). Owner-scoped like `ListPymodules` (§2.3 Presets) |
| `GetPreset` | unary · anyCaller | One preset: the latest live version's row by default, every version live and deleted with `history`; `CodeNotFound` when no version exists |
| `PutPreset` | unary · childScoped | Save a new version of a preset — insert-only, never an overwrite. The name must be `<name>` or `<group>:<role>` and the spec must validate (kind, thinking level off|low|medium|high|xhigh, budgets >= 0, known tool names, label keys mirroring the spawn path's rules — `[A-Za-z0-9_./-]` only, never `owner` or a `rafiki/`/`fundi/` key); a user credential, or the per-child secret of a TOP-LEVEL child (stamped as the writer); a parented child is refused `CodePermissionDenied` (`connectPresets.authoringChild`) and every other child shape by the provenance gate ("Who may call what") |
| `DeletePreset` | unary · childScoped | Stamp `deleted_at` on every live version of one preset's name — the only mutation a preset row ever undergoes; the history keeps the rows. Admitted like `PutPreset` |
| `Recall` | unary · ownerScoped | Hybrid search (BM25 + vector, RRF-fused) over memories, conversation summaries and windows. `query` is required (`CodeInvalidArgument` otherwise); `sources` filters to some of `memory`/`summary`/`window` (empty = all three, an unknown name `CodeInvalidArgument`); `under` is an ltree path prefix for memories, `repo` a basename filter for conversation sources; `since`/`until` are `Timestamp`s bounding hit activity (**unset or the epoch = unbounded**); `limit` 0 means the default (10), clamped to 50. Hits carry `id` (`m:`/`s:`/`w:`-prefixed — the key `RecallContext` expands), `source`, `snippet`, `when` (a `Timestamp`), conversation identity/repo/kind, ordinal span, memory path/name, summary title and the fused `score` |
| `RecallContext` | unary · ownerScoped | Expand one hit id: a window (`w:`) renders its message span plus `before`/`after` neighbouring messages (the wire takes 0 as "the window's own span" — the 3-each default is the fundi `recall_context` tool's substitution, not this verb's); a summary (`s:`) renders title + summary with its conversation header; a memory (`m:`) renders its full body. `max_chars` caps the text with **0 = uncapped** (the 8000 default is likewise the tool's substitution). An empty `id` is `CodeInvalidArgument`; a well-formed but unknown id is `CodeNotFound`; a malformed one (no `m:`/`s:`/`w:` prefix) falls through to `CodeInternal` |
| `GetMemory` | unary · ownerScoped | One of the CALLER'S own memories by `(path, name)`, body and meta included. Memories are always owner-scoped to the caller's own user id — an admin gets no one else's |
| `MemoryTree` | unary · ownerScoped | The caller's memories under `path`, to `depth` (0 = unlimited), ordered by path; the rendered tree is byte-budgeted (`recall.TreeMaxChars`) on the tool surface |
| `PutMemory` | unary · ownerScoped | Save or replace a memory at `(path, name)` under the CALLER'S OWN user id — even for an admin. `path` must be dot-separated labels matching `[A-Za-z0-9_-]{1,256}` (`CodeInvalidArgument` otherwise), `meta_json` must parse as JSON (empty means `{}`) and an empty owner is `CodeInvalidArgument` (`recall.ErrNoOwner`). Replace is a new row; the old one is tombstoned, never deleted |
| `DeleteMemory` | unary · ownerScoped | Tombstone (`deleted_at`) one of the caller's memories at `(path, name)`; an unknown name is `CodeNotFound`. Never a hard delete |
| `RecallBackfill` | unary · userOnly | Arm summarization of conversations whose last activity predates `summaries_enabled_at`: `since` (a `Timestamp`) sets the cut-over (**unset = from the beginning**, unlike `Recall`'s unset = unbounded), `max_cost_usd` is the spend ceiling after which backfill auto-disarms. An out-of-range `since` is `CodeInvalidArgument`. A non-admin caller is refused with `CodePermissionDenied`; a non-positive `max_cost_usd` is refused with `CodeInvalidArgument` (`connectapi.ErrNoBackfillBudget` — a zero-value request can never arm an all-history, budgetless backfill) |
| `RecallStatus` | unary · userOnly | The recall index's state: conversation/window/summary/memory counts, unembedded windows and pending summaries, cumulative summary cost, `backfill_since` (a `Timestamp`; unset when backfill is off), its budget and spend, and the configured embedding and summary models |
| `ConversationSearch` | unary · childScoped | Query the conversation corpus. The request carries caller-chosen FILTERS (since/until `Timestamp`s bounding turn activity — unset or the epoch means unbounded, owner, persona, source, model, status, path, min_tokens, text, limit) — never a scope: the daemon derives scope server-side from the authenticated credential (`IsUserCredential` → the user's own rows, `IsAdmin` → all, a per-child credential → its own subtree, anything else `CodePermissionDenied`) and ANDs it on top, so an `owner` filter can only ever narrow, never widen. An out-of-range `since`/`until` is `CodeInvalidArgument`. `limit` 0 means the default (50) and is clamped to 500. Each row carries id/name/owner/persona/source/model/status/driven_by, `created_at`, turn and token aggregates, cache-hit ratio, total USD and the first user message snippet, plus `closed_at` (a `Timestamp`; unset = the conversation is not closed, never a zero Timestamp). A child credential's conversation reads — and its spend — cover ALL its descendants including closed ones |
| `ConversationExport` | unary · childScoped | One conversation's decomposed transcript: header identity plus ordered turns (role, verbatim content-block JSON, skills invoked, per-turn tokens/`latency` (a `Duration`, unset when not reported)/model/prefix hash) and the recovered available-skills catalog. Scope is derived exactly as `ConversationSearch`. A conversation that does not exist OR is outside the caller's scope returns `CodeNotFound` — the two are deliberately indistinguishable, because "you may not read X" would confirm X exists |
| `ConversationQuery` | unary · childScoped | Run one named catalogue query (`tools`, `skills`, `classes`, `models`, `sizes`, `coverage`) over the conversation corpus. The request carries caller-chosen FILTERS (since/until `Timestamp`s — unset or the epoch means unbounded, owner, persona, source, model, path) — never a scope: the daemon derives scope server-side from the authenticated credential exactly as `ConversationSearch`, so an `owner` filter can only ever narrow, never widen. Time filters are per query: tools and skills filter message time, models filters turn time, classes and sizes filter turn activity, coverage filters conversation creation week. The `tools` rows group tool names CASE-INSENSITIVELY — each row is displayed under the spelling that carried the most calls (ties: the sort-first spelling) — sort by calls descending, and count `conversations` as DISTINCT conversations over the whole merged group, never a sum of per-spelling counts. The response carries the query's declared schema — typed `QueryColumn`s (name/kind/format, where `format` is a display-only hint) and rows of oneof-typed `QueryValue` cells (`str_value`/`int_value`/`float_value`), so ints and floats decode as numbers, never strings. An unknown query name OR a query whose admission class refuses the caller's derived scope returns `CodeNotFound` — the same fold as `ConversationExport` |
| `ConversationReview` | unary · userOnly | Enqueue a detector/rank pass over one or more conversations. Every config field (stage, model, profile, budget_usd, min_turns, force) is OPTIONAL and resolves request field > `RAFIKI_REVIEW_*` daemon env > the named analyzer profile's own defaults — the request and env tiers are resolved at accept time, the profile tier (detector model, population filters, compact policy) lazily inside the worker. The response carries one status per in-scope requested conversation_id — `ENQUEUED`, `ALREADY_RUNNING`, or `QUEUE_FULL` — never an analysis id or run state; read the outcome back through `ConversationFindings`' `analyses` rows, which are inserted at completion only, so an in-flight review reads as absent. Never blocks on an LLM call: accepted jobs enter a bounded in-memory queue (capacity 8) drained by a single worker goroutine, and a full queue answers `QUEUE_FULL` without leaving the id marked in-flight. Scope is derived exactly as `ConversationSearch`. A requested id may be spelled as a conversation UUID or as a child id (`c_…` — the client's own `Resolve` produces child ids, so that is what `rafiki` sends); a child id is matched through the authoritative `conversations.child` mapping and canonicalized to its conversation UUID — the value the job and the single-flight guard key on, so two spellings of one conversation collide rather than run twice — and an id matching neither spelling is dropped from the response silently with no error, the same fold as `ConversationExport`. A batch is N independent per-conversation calls — no shared budget, no shared queue slot — and exactly one job per conversation runs at a time, enforced by an in-memory, per-daemon guard taken at accept and released when the job completes: a duplicate request for a conversation already queued or running gets `ALREADY_RUNNING` rather than a second job. `min_turns` gates at DEQUEUE, not at accept, so a below-threshold job is skipped silently |
| `ConversationFindings` | unary · userOnly | The review read: detected findings (axis/skill/status filters, status defaults to `open`) plus recent analysis run rows (model, profile, status `ok|failed`, error, tokens, cost_usd, created_at), both scoped exactly as `ConversationSearch`. The analysis rows are where an enqueued review's outcome becomes visible over the wire. Filter ids may be a conversation UUID or a child id (`c_…`, matched through `conversations.child`); an id matching neither spelling contributes no row and no error, the same not-found-shaped scope miss as `ConversationExport`. `limit` 0 or negative means 50 and anything above 500 clamps to 500 (server-side, the same convention as `ConversationSearch`). Findings sort most-impactful first (`expected_savings_tokens` DESC), analyses most recent first. Finding triage (`dismiss`/`action`) is deliberately NOT on this plane — it stays a direct-DB write |
| `DarajaLaunch` | unary · userOnly | Launches a claude child via daraja on a matching executor. Selects an executor that admits the request's label selector AND declares "claude" in its `LaunchKinds`, calls `AdminService.Launch` on that executor with a one-shot ticket, then waits for the daraja's reverse dial into the pool. A match yielding zero candidates returns `explanation` with per-candidate refusal reasons. Response carries `child_id`, `pid`, `pgid`, and `connected_at` (a `Timestamp`). Requires a configured executor pool and daraja connect address (**`RAFIKI_CONTROL_LISTEN` must be set**; Unix socket paths are refused because remote executors cannot reach daemon-local sockets). |
| `DarajaSend` | unary · userOnly | Writes bytes to a live child's stdin via the connected daraja. Unary rather than bidi because the HTTP/1.1 remote plane cannot carry the bidirectional Relay stream. Returns `acknowledged=true` on success; the child id must name a currently-connected daraja. |
| `DarajaWatch` | server-streaming · userOnly | Streams stdout and lifecycle markers (ProcessRestarted, ProcessExited) from a connected daraja. Server-streaming only — client sends no messages during the watch. The stream follows the child until disconnect or context cancellation. Lifecycle events let callers distinguish process-boundary resets from ordinary output. LIVE-ONLY: this admin surface never consumes the replay belt — belt consumption is pump-only (the child's own runner via `Pool.Watch`); an admin watch attaching while the belt holds events, even inside the pump's re-Watch backoff, sees none of them and leaves the belt for the pump, so it cannot re-strand a finished script. What the admin misses while unsubscribed is dropped for it. With no live connection the watch fails rather than serving the belt. |
| `ListProviderBans` | unary · anyCaller | Every live exclusion of an OpenRouter provider from routing: operator bans (`reason` `operator`, `model_line` `*` = every model line) and the provider cache guard's own ejections (`reason` `no_cache`, one model line). `expires_at` (a `Timestamp`) is ABSENT for a ban that lasts until lifted; `persistent` is false when the daemon has no ejection log, meaning bans are lost on restart. Open to any caller (§"Provider bans") |
| `BanProvider` | unary · userOnly | Ban one provider from every model line, effective on the next OpenRouter request on both the proxy face and fundi children. `provider` may be a slug or a display name; the daemon resolves it through OpenRouter's provider directory (`/api/v1/providers`) and refuses a name the directory does not list (`CodeInvalidArgument`) — OpenRouter silently ignores an unknown slug. With the directory unreachable the slug is guessed (lowercased, spaces to dashes) and the ban accepted. `duration` (a `Duration`) is optional: absent = until lifted, present must be > 0 (`CodeInvalidArgument` otherwise). Re-banning replaces expiry and note. Admin user credential or the anonymous unix socket only; anything else `CodePermissionDenied` |
| `UnbanProvider` | unary · userOnly | Lift an operator ban (`CodeNotFound` when none is live). `provider` matches the stored ban exactly first, then as its resolved slug, so a ban stored under a slug OpenRouter never had can still be lifted by that name. Does not clear the cache guard's own ejections of that provider. Same authority rule as `BanProvider` |
| `ListRoutes` | unary · anyCaller | Every live routing-policy row (§"Route policy"): the model line, its routing spec (`routing.ParseSpec`'s grammar, e.g. `sort=price,quant=fp8+`) and `created_at` (a `Timestamp`). Open to any caller — routing defaults name providers, not users |
| `SetRoute` | unary · userOnly | Write (or replace) the routing spec for `model_line` (`"*"` is the global line): the row is appended to `openrouter.route_policy` and the daemon's in-memory resolver reloads from `Active` immediately, so the next request routes by it — no restart. `model_line` is required and `spec` must parse with `routing.ParseSpec` (the empty string is the zero spec) — otherwise `CodeInvalidArgument`, carrying the parser's message. Returns the row as stored |
| `DeleteRoute` | unary · userOnly | Tombstone the live routing-policy row for `model_line` (`CodeNotFound` when none is live — a tombstone is itself the newest row, so a second delete is also `CodeNotFound`). The resolver reloads immediately |
| `Report` | unary · childScoped | Publishes one progress report from the CALLER to the CALLER'S OWN PARENT's event buffer, where it coalesces and defers exactly like a subagent settle (`scriptEventSource` — five reports between the parent's turns cost one injected frame, not five). Any child credential may call it, not only a script: an LLM child reaches it through its `agent_report` tool. A `progress` report is keyed on the caller's own id, so it REPLACES the caller's previous undelivered progress note (last write wins), while every other kind accumulates. A top-level child has no parent to push to, so its report is appended to its OWN durable event log instead as a `script_report` event. `kind` is the caller's discriminator (required, ≤64 bytes, no control characters — it is rendered verbatim inside the injected frame) and `data_json` the payload, required to parse as a complete JSON value — after trimming surrounding whitespace; the trimmed value is what is stored — and capped at 4 KiB (`connectapi.MaxReportDataBytes`); oversized or malformed data is refused with `CodeInvalidArgument`, never truncated. The caller's position is resolved from its credential, never from a request field — the request carries no address to authorize against |
| `Receive` | server-streaming · childScoped | The calling script child's inbox, streamed: everything addressed to it from now on arrives as a `ScriptMessage.Text` (text, `PROMPT`/`STEER` mode, the durable inbox row id on `message_ids` (one row per message today; the field is repeated so a future batching change needs no wire change), attachments), and the lifecycle news a script must react to arrives as the `Stop` variant — an inbox abort row ("abort requested"), the child entering `shutting_down` ("stopping"), or its exit ("child exited"); each stop ENDS the stream, and the text rows ahead of an abort row in the same pulled batch are delivered before its stop (work first, stop last; rows behind it are discarded). Delivery is at-most-once, in the same family as the claude children's contract but slightly weaker: a claude child's failed frame write leaves its rows pending for the idle-drain retry, while a stream that dies between pull and wire has nothing left to retry, because the rows were consumed at the pull — the stream does retain and deliver every row of a batch it has pulled, and the per-child lock still means a row is never handed to two consumers. `child_id` in the request is a SELF-CHECK, not an address: empty means the caller's own inbox, and anything else must equal the caller's own id (`CodePermissionDenied` — this is the one childScoped verb whose check is identity, not subtree, so it cannot go through `ChildScope.Authorize`, which refuses the caller's own id) |
| `SetResult` | unary · childScoped | Stores the calling child's structured final result — any child credential, script or LLM (an LLM child reaches it through its `agent_result` tool): verbatim JSON, required to parse as a complete JSON value — after trimming surrounding whitespace; the trimmed value is what is stored — and capped at 4 KiB (`connectapi.MaxResultBytes`), refused with `CodeInvalidArgument` otherwise. Last write wins: every call replaces the stored value (`conversations.child.result`, migration 0039 — nullable TEXT so the bytes the caller wrote are the bytes every reader gets), and the value present when the child settles rides the settle fragment injected into the parent's event buffer and `GetChild`'s `ChildSummary.result`. For a NON-SCRIPT child the stored result is per turn: when its next turn starts the turn transition clears it, so a result from an earlier turn never rides a later settle fragment; a script child's result is the work product of its whole run and is never cleared this way. Self-only: the caller's own row, resolved from the credential |
| `Resume` | unary · userOnly | Re-spawn an exited child from its persisted state record. `child_id` is required (`CodeInvalidArgument`); `api_key` is used at spawn time only and never persisted, so a child originally spawned with a per-call key must re-supply it here. Returns the same child id (§3). `not_resumable` when the child is not exited; a script child is never resumable ("spawn it again") |
| `CloseAllExited` | unary · userOnly | Finalize every exited child, optionally only those older than `older_than` (a `Duration`; unset or zero = all exited entries). Returns the closed child ids in close order, so a consumer can record exactly what was closed |
| `SetLabels` | unary · userOnly | Set entries apply first, then remove entries are deleted, and the full post-mutation map comes back. Keys using the `rafiki/` prefix are reserved and rejected. At least one of `set`/`remove` is required (`CodeInvalidArgument`), as is `child_id` |
| `Status` | unary · userOnly | The daemon's process vitals: `version`, `started_at` (a `Timestamp`), live/exited `children` counts, `memory_bytes`, the socket path and the logs dir |
| `Search` | unary · userOnly | In-memory content search across live children's session buffers. `query` is required (`CodeInvalidArgument`); `regex`, `limit`, `context` and an optional `session_filter` narrow the scan (`cwd_contains`, `name_contains`, `since` as a `Timestamp` with unset unbounded, AND-matched `labels`, key-presence `has_label`). Each hit carries the child id, entry id, `timestamp` (a `Timestamp`), role, snippet and match span, plus `total_hits`/`scanned`/`elapsed` (a `Duration`) |
| `ShutdownDaemon` | unary · userOnly | Trigger the daemon's own shutdown path as a remote operator: the same sequence a SIGTERM runs — a shutdown notice broadcast to connected clients, live children drained through the graceful ladder, listeners closed, process exit. It terminates the daemon process (a drain alone would poison the stopping latch and resurrect children on the next start); the refusal to do this from an RPC-shaped half-measure is why the verb landed only with the daemon shutdown path itself. `CodeUnavailable` until the daemon wires it |
| `ConversationStats` | unary · userOnly | Global (filtered) stats when `conversation_id` is empty, scoped to one conversation otherwise — in which case the filter fields are ignored. `since`/`until` are `Timestamp`s bounding turn activity (unset or the epoch means unbounded); an out-of-range value is `CodeInvalidArgument`. The stats ride as the daemon's own `insights.Stats` JSON in `stats_json`, opaque by design (precedent: `ToolUse.input_json`) — the caller asked for an aggregate, not a schema |
| `EnrollExecutor` | unary · userOnly | Mint a one-time enrollment token for a remote executor. `ttl` (a `Duration`) is the token's lifetime and must be positive (`CodeInvalidArgument` otherwise); the daemon's 72 h default applies where no positive ttl is supplied. The executor it creates is OWNED by the caller: the token carries the connection identity's durable user id ("" for the anonymous unix socket — unowned), and the `owner` LABEL stays the display username (§"Executor administration") |
| `CreateExecutor` | unary · userOnly | Mint an executor row and its durable credential in one step, with no enrollment handshake. Same ownership rule as `EnrollExecutor` (§"Executor administration") |
| `LabelExecutor` | unary · userOnly | Set or remove labels on an executor's row; `executor_id` may be the full id or a unique trailing fragment. Ownership-scoped: a non-admin user credential is refused `CodePermissionDenied` (`permission_denied`) when the row belongs to another user (§"Executor administration") |
| `DisableExecutor` / `EnableExecutor` | unary · userOnly | Disable (its credential stops authenticating) or re-enable an executor. Ownership-scoped like `LabelExecutor` |
| `DeleteExecutor` | unary · userOnly | Permanently remove an executor row — no tombstone; a connected row is evicted from the live pool within one health interval. Ownership-scoped like `LabelExecutor` |
| `ExecutorSession` | server-streaming · userOnly | The session-executor stream: mint or find the caller's own executor for this machine (§"ExecutorSession" below) |
| `CreateUser` | unary · userOnly (admin-gated) | Mint a user row and, conditionally, a bearer token: `mint_token` ABSENT means the daemon mints iff OIDC login is NOT configured on it (so the user can authenticate at all); PRESENT is honoured as given. `email` stores a normalized, unique-among-active-users address. The response carries `created_at` (a `Timestamp`), `token` (empty when none), `token_reason` ("requested" / "oidc not configured" / "") and `login_configured`, so a client can word its hint without guessing. NEVER mints an admin — admins come only from `rafikid user create --admin` (§"User administration") |
| `ListUsers` | unary · userOnly (admin-gated) | Enumerate users. Tokens are never returned |
| `RemoveUser` | unary · userOnly (admin-gated) | Tombstone a user: its token stops authenticating, but history keeps resolving the username |
| `UpdateUser` | unary · userOnly (admin-gated) | Edit a user: `email` is the only editable field (`optional` — an unset request is refused "nothing to update"; empty clears the address). The admin bit is deliberately NOT editable here — it comes only from `rafikid user create --admin` (§"User administration") |
| `MintToken` | unary · userOnly | Mint a service token (`origin "service"`) for the target user — the caller by default, another user with admin authority; `ttl` (a `Duration`; unset or zero = never expires, negative refused). The plaintext rides the response exactly once (§"User administration") |
| `ListTokens` | unary · userOnly | The target's credential rows — metadata only, never secrets. `username` empty = the caller; `all_users` (every user) requires admin authority (§"User administration") |
| `RevokeToken` | unary · userOnly | Tombstone one credential by id (`revoked_at`); already-revoked answers the unchanged row. Owner or admin; on a live daemon the revocation also cuts that token's open streams (§"Token revocation and open streams") |
| `GetStreams` | unary · userOnly | A live child's raw, uncompressed stdin/stderr capture, for debugging (§"The raw child channel") |
| `SendFrame` | unary · userOnly | Forward a raw child-protocol frame to a live child's stdin, verbatim and uninspected, for debugging and scripting (§"The raw child channel"). userOnly — children use `Send` |
| `CreateSandbox` | unary · childScoped | Create a named sandbox — a container, created by a launcher executor's declared `docker` proxy, that runs `rafiki executor serve` and enrolls as an ordinary executor. `spec` is a `SandboxSpec` (`name` required; `image` defaults to the daemon's `RAFIKI_SANDBOX_IMAGE`, else `ghcr.io/graveland/rafiki-sandbox:latest` and must be a plausible image reference; a `host_path` mount must sit under the launcher's `--sandbox-mount-root`; a child caller's memory/cpu/pids are bounded by `RAFIKI_SANDBOX_CHILD_MAX_*` — an omitted or 0 value takes the cap, an over-cap value is REFUSED). The owner is resolved from the credential — a per-child credential creates under its owner's NON-admin identity. Blocks until the sandbox's executor joins the pool (a 60 s budget) and returns the row; every failure rolls back. `CodeInvalidArgument` when no `docker` launcher is in scope, the owner's cap (`RAFIKI_SANDBOX_MAX_PER_OWNER`, which counts spawn blocks too) is reached, or the spec fails validation |
| `ListSandboxes` | unary · ownerScoped | The CALLER'S OWNER's live sandboxes — never an admin's whole fleet. The list is keyed on the owner's user id, so a local (unix-socket) caller, whose identity is empty, sees only the sandboxes owned by that local identity. `state` is one of `creating`/`ready`/`lost`/`removing`; a `ready` row whose executor is gone is reported `lost` when its launcher is live and confirms the container is missing (and that downgrade is persisted) |
| `RemoveSandbox` | unary · childScoped | Remove one of the owner's live sandboxes by `ref` (name, then row id). An operator credential (`callerChild` empty) may remove any of the owner's rows; a per-child credential may remove only a row it created or one a descendant created (`CodePermissionDenied` otherwise, `childstore.IsDescendant`; once the creating descendant is gone from the in-memory store, only an operator or the TTL can remove it). Removal stops and removes the CONTAINER first (a deleted executor row would make the sandbox's `unless-stopped` policy restart it in a loop), then evicts and deletes the executor row, then tombstones the sandbox row; named volumes are never removed, and a launcher that is offline leaves the row `removing` until it returns — unless the launcher's executor ROW is gone too, in which case the row is tombstoned and the container is left to the orphan arm (see §Sandboxes) |

#### Who may call what (the provenance gate)

Every Connect mount of the Control service — the proxy face's route and the
local socket — composes its interceptors through `connectControlRoute`
(`cmd/rafikid/connect_policy.go`), which appends ONE policy interceptor,
`connectPolicyInterceptor`, and then ONE stream-revocation interceptor,
`streamRevocationInterceptor` (`cmd/rafikid/stream_revoke.go`) — innermost,
behind the policy gate, so a call the gate refuses never registers. The
policy interceptor resolves each procedure's policy from a `procedure →
policy` table (`controlPolicyTable`) whose coverage over the generated
Control service descriptor is pinned by
`TestControlPolicyTableCoversEveryProcedure` (`cmd/rafikid`): a new RPC
cannot land unclassified, because the test fails until the table gains an
entry, and a name the table holds that the descriptor does not fails too.
The table's miss-default is `userOnly`, so a gap can only over-refuse, never
admit.

The stream-revocation interceptor is the cut side of the policy gate. Every
ADMITTED server-streaming handler whose caller presented a real user token
registers under that token's id in one per-daemon registry; a unary call
registers nothing (it is bounded and re-authenticates next time), and
neither a child credential nor an anonymous local-socket caller does — a
child's streams already die with its control connection. Revocation through
the daemon's own paths (`RevokeToken`, `RemoveUser`/`UserRm`) cancels the
matching streams immediately, and the client whose stream was cut sees
`canceled` rather than a bare EOF. It is immediate for NEW requests too: the
registry's revocation paths purge the daemon's auth cache
(`UserTokenAuth.ForgetToken`/`ForgetUser`) BEFORE the cut, so a stream
re-opened in the same instant re-authenticates against the store — where the
revocation is already committed — and is refused, instead of re-registering
under an identity still cached from the revoked credential. The residual is
a request whose store read predates the revoke's commit: it can re-insert a
warm entry for one TTL, but only for the span of that single in-flight read —
the same class of race a stream registration already has against the cut.
What the registry cannot see is revocation written to the database behind
the daemon's back — `rafikid user token revoke` on the host — so on such a
token open streams run on (a documented limitation, not a deliberate choice:
the host CLI has no channel to the running daemon) and new requests stop
within the auth cache's ≤5s window (`server.DefaultAuthCacheTTL`). Token
expiry does not cut an open stream, deliberately — expiry is a property of
time, not an act by anyone; the expired token's next request re-checks the
store and is refused.

| Policy | Meaning | Procedures |
|---|---|---|
| `userOnly` (the default) | Requires a real user credential — or no identity at all (the unix socket's local trust) | `ListExecutors`, `ListSkills`, `GetSkill`, `UpsertSkill`, `DeleteSkill`, `SetSkillEnabled`, `AddPymoduleGitSource`, `ListPymoduleGitSources`, `RefreshPymoduleGitSource`, `RemovePymoduleGitSource`, `RecallBackfill`, `RecallStatus`, `ConversationReview`, `ConversationFindings`, `DarajaLaunch`, `DarajaSend`, `DarajaWatch`, `BanProvider`, `UnbanProvider`, `SetRoute`, `DeleteRoute`, `Resume`, `CloseAllExited`, `SetLabels`, `Status`, `Search`, `ShutdownDaemon`, `ModelInfo` (no — see `anyCaller`), `ConversationStats`, `EnrollExecutor`, `CreateExecutor`, `LabelExecutor`, `DisableExecutor`, `EnableExecutor`, `DeleteExecutor`, `ExecutorSession`, `CreateUser`, `ListUsers`, `RemoveUser`, `UpdateUser`, `MintToken`, `ListTokens`, `RevokeToken`, `GetStreams`, `SendFrame` |
| `anyCaller` | Read-only, non-scoped; child credentials included | `ListModels`, `ListPresets`, `GetPreset`, `GetRateLimitStatus`, `ListProviderBans`, `ListRoutes`, `ModelInfo`, `ModelRoutes` |
| `childScoped` | A per-child credential may call these on its own subtree: the gate admits `ProvenanceChildToken` only, and the handler bounds it — the stored parent chain via `childstore.IsDescendant` (`connectapi.Server.SetChildScopeSource`, implemented in `cmd/rafikid/connect_childscope.go`), the caller itself refused (a child is not a descendant of its own id), unknown ids refused with the same answer. `Spawn` forces `ParentChildID` to the caller's own id — the child-spawn admission, whose depth/children/budget checks read the parent's grant through it — and a credential that names no child cannot spawn at all, since an empty forced parent would be the top-level spawn shape. `ListChildren` answers only the subtree; `StreamEvents` refuses the `All` subject and any non-descendant subject; `ListTasks` only a conversation inside the caller's subtree. The other child shapes stay refused. The source never resolves nil (the operator path) for a child-shaped credential — the empty-ChildID and vanished-row shapes resolve an always-refusing scope — and the daemon wiring is pinned end to end by `TestConnectChildScopedOnTheConnectPlane` (`test/integration`). The three script-hub verbs (`Report`/`Receive`/`SetResult`) resolve the caller's position from the credential rather than authorizing a target, and `Report`/`SetResult` admit ANY child credential — script or LLM, the LLM kinds reaching them through the `agent_report`/`agent_result` tools: `Report` acts outward on the caller's own parent (a top-level child appends to its own event log), `Receive` and `SetResult` are strictly self-only — `Receive`'s `child_id` is an identity self-check, not a subtree call, and both verbs refuse any caller that resolves to no child scope (a user credential has no position in the tree for a self-position verb to act on). `SetBudget` from a per-child credential is NOT operator authority: after the subtree check it applies `agent_set_budget`'s rule (`Controller.SetChildBudget` — direct parentage, bounded by the caller's own remaining grant). `SetRouting` from a per-child credential is bounded to `prefer`/`sort`/`quant`: `Controller.SetChildRouting` refuses `only=` from child provenance (it bypasses provider bans) and never clears an operator's `only`/`nodata`/`zdr`. The conversation reads `ConversationSearch`/`ConversationExport`/`ConversationQuery` answer a per-child credential from its own subtree (`insights.ScopeSubtree`, the MCP face's `conversation_*` boundary) — never its owner's corpus; a conversation outside it answers not-found. `ListPymodules`/`GetPymodule`/`PutPymodule`/`DeletePymodule` read and write the owner's corpus, exactly as the MCP face's pymodule tools do. `PutPreset`/`DeletePreset` admit only a TOP-LEVEL child (no parent — the operator's own session); a parented or unknown child is refused `CodePermissionDenied` by `connectPresets.authoringChild`. `CreateSandbox`/`RemoveSandbox` admit a per-child credential on its OWN containers: creates are clamped and bounded by the launcher's declared mount roots, and a remove is bounded to a row the caller or a descendant created (`childstore.IsDescendant`) — the Controller enforces both, since the gate admits by credential kind alone and a sandbox ref carries no subtree to check | `GetHistory`, `StreamEvents`, `Send`, `ListChildren`, `GetChild`, `Spawn`, `Kill`, `Close`, `SetBudget`, `SetRouting`, `ListTasks`, `Report`, `Receive`, `SetResult`, `ConversationSearch`, `ConversationExport`, `ConversationQuery`, `ListPymodules`, `GetPymodule`, `PutPymodule`, `DeletePymodule`, `PutPreset`, `DeletePreset`, `CreateSandbox`, `RemoveSandbox` |
| `ownerScoped` | A per-child credential may call these as its OWNER: the gate admits `ProvenanceChildToken` only (the per-boot shapes stay refused); the handler resolves the owner's NON-admin identity (`recallOwner`, `cmd/rafikid/recall.go`), so conversation-derived reads cover only the owner's rows — never `Scope{All: true}`, even for an admin's child — and memories are the owner's own. The surface a fundi child's `recall`/`memory_*` tools already have, and what the MCP face gives a claude child. `ListSandboxes` answers the caller's owner's live sandboxes the same way — keyed on the owner's user id, so a local (unix-socket) caller, whose identity is empty, sees only that local identity's sandboxes, never an admin's whole fleet. No subtree check | `Recall`, `RecallContext`, `GetMemory`, `MemoryTree`, `PutMemory`, `DeleteMemory`, `ListSandboxes` |

The gate distinguishes three credential shapes:

- **No identity** (nil) — the unix socket's anonymous caller. The socket
  itself is the credential (admission happened by filesystem permission), so
  the gate passes it, as before the gate existed.
- **A user credential** (`ProvenanceUser`) — passes every procedure.
- **A child credential** — everything else an identity can resolve to:
  `ProvenanceChildToken` (a per-child secret), `ProvenanceChildAttributed`
  (the per-boot token plus `X-Rafiki-Session`), and the empty `Identity{}` a
  bare per-boot token resolves to. Refused with `CodePermissionDenied` on
  every `userOnly` procedure, and on `childScoped` or `ownerScoped`
  procedures for every shape EXCEPT `ProvenanceChildToken` — whose subtree
  reach (childScoped) or owner's data (ownerScoped) the handler layer then
  bounds, per the table above. The refusal names the procedure and the
  credential kind, never the secret; without bounds, only the `anyCaller`
  reads admit a child credential.

This gate exists because every claude child holds a child credential in its
environment (`ANTHROPIC_AUTH_TOKEN`, `RAFIKI_MCP_TOKEN`) — as does every bash
command the model runs — and before the gate only `Spawn`, `ListExecutors`
and the conversation verbs checked provenance: one curl from inside any
child could kill any sibling tree, lift any budget, or rewrite the presets,
skills and pymodules every future agent loads. `scopeFor` (the conversation
verbs' scope derivation) refuses a child credential itself. The MCP
agent-control surface (§2.4) has its own entitlement gate, and its per-child
bindings are scoped too: conversation reads to the child's own subtree, the
recall/memory surface owner-scoped to the child's owner, and preset
authoring limited to top-level children — see `mcp_face.go`'s `getServer`.

Admin gates ride ON TOP of the policy table, in the handler: `CreateUser`,
`UpdateUser`, `ListUsers`, `RemoveUser` and the provider-ban mutations
require an admin
user credential (or the anonymous local socket, where the socket itself is
the credential) — `requireUserAdmin` / `requireBanAuthority`
(`cmd/rafikid/connect_users.go`, `connect_providerbans.go`) refuse before any
store call, so the ordering is pinned (`TestUserRPCsRefuseNonAdmin`) and a
non-admin user, a child-attributed identity and a per-child token are all
refused `CodePermissionDenied`. The token RPCs (`MintToken`, `ListTokens`,
`RevokeToken`) are userOnly at the gate but deliberately NOT admin-gated in
the handler: a user manages its OWN credentials — another user's name on any
of them requires admin (`resolveTokenTarget`, `cmd/rafikid/connect_users.go`),
and on the anonymous local socket a target username is required, since there
is no caller to default to.

#### Spawn's operator-only fields (15–29)

`SpawnRequest` fields 1–14, 30 and 31 are the child-ALLOWED set
(`childAllowedSpawnFields`, `pkg/connectapi/verbs.go`): a caller with child
provenance may set them, and `Spawn` forces `parent_child_id` to the caller's
own id. Fields 15–29 (`config_dir`, `append_system_prompt`, `thinking`,
`no_session`, `resume_session`, `fork_session`, `extensions`,
`no_extensions`, `verbose`, `extra_args`, `skills_dirs`, `mcp_config`,
`env`, `record_requests`, `passthrough_auth`) are **operator-only**: a caller
with child provenance naming any of them is refused with
`CodePermissionDenied`.

Field 30 (`skip_derived_index`) is child-ALLOWED despite sitting above that
operator-only range: it skips embedding and summarising this child's
conversations, and every descendant's — windows and BM25 search still work.
It is inherited down the subtree and never clearable beneath a parent that set
it, so it only reduces spend. `Spawn` ORs the request's value with the
parent's own (`inheritSkipDerivedIndex`, `cmd/rafikid/controller.go`); the
flag is stored on the child, so a resumed child keeps its stored value.

Field 31 (`sandbox`) is child-ALLOWED for the same deliberate reason:
`SandboxSpec` cannot express privilege. Host paths are bounded by the
launcher's declared `--sandbox-mount-root` (re-checked launcher-side, which
also refuses a source that is not a regular file or a directory — a socket
under a root would be a capability grant); volumes
are owner-prefixed, a child's memory/cpu/pids are bounded by the daemon's
`RAFIKI_SANDBOX_CHILD_MAX_*` caps (an omitted or 0 value takes the cap; an
OVER-cap value is REFUSED, never silently clamped), and the spec has no field
for a capability
(field 31 in `control.proto`'s comment, enforced by `pkg/sandbox.Validate`). A
child that may spawn may therefore also ask its own child to be provisioned
with a sandbox. See §"Sandboxes" on `CreateSandbox`.

The guard is enforced as the child-allowed SET, not a literal list of 15 —
so a NEW `SpawnRequest` field is operator-only by default. Adding one
requires an explicit decision: add its number to `childAllowedSpawnFields`
(and to the proto comment's child-allowed range) if a child should be able
to set it, or leave it unclassified and let
`TestSpawnRequestFieldsAreClassified` (`pkg/connectapi/verbs_test.go`) fail
until it is placed in one bucket or the other on purpose. The completeness
test is a reflection test over the generated descriptor: it fails the moment
a field number is in neither bucket. `thinking` (17) and
`append_system_prompt` (16) are labelled operator-only as a Connect-WIRE
policy, not a capability boundary — a child can already set both through the
tool plane (fundi/MCP `agent_spawn`, and any preset it can name, including
one it wrote itself) before this guard ever runs. The wire guard exists to
keep Connect at least as strict as the plane it replaced, not to claim these
two fields are otherwise unreachable.

#### Sandboxes

A **sandbox** is a container created by a launcher executor through its
declared `docker` proxy; it runs `rafiki executor serve` and enrolls as an
ordinary executor (see `docs/reference/executor-protocol.md` → "Sandbox
launchers"). There are two shapes. A **named** sandbox is created with
`CreateSandbox`, addressed afterwards by name or id, carries a TTL
(`RAFIKI_SANDBOX_TTL`, capped by `RAFIKI_SANDBOX_MAX_TTL`, both strictly
positive — a named sandbox ALWAYS expires), and is removed with
`RemoveSandbox`. A **spawn block** is `SpawnRequest.sandbox` (field 31): it is
unnamed, has NO TTL, carries a `scope` (`self` or `subtree`), and is tied to
the child it was spawned for (`owner_child`); its lifecycle follows the child.

- **A launcher is never chosen by default.** A create is refused
  `CodeInvalidArgument` when the creator's effective executor set contains no
  live executor advertising the `docker` proxy, when several do and no
  `launcher` names one, or when the chosen launcher declares no `--relay-dir`.
- **Binding is an ownership lookup, not selection.** A sandboxed child — and,
  for a `subtree` block, its descendants — binds to its sandbox through
  `controllerBinder.ChooseFor` and NEVER falls through to an ordinary
  executor, which would run it natively on a host the operator never offered.
  Child-owned sandboxes are dropped from ordinary selection candidates, so a
  `self` descendant (whose stored selector narrows to the sandbox) resolves to
  nothing and fails closed. `subtree` admits descendants to the same container
  (concurrent workspaces separated by workdir only; no path scoping). A fresh
  `subtree` descendant binds on its FIRST (eager) bind — which runs before its
  own `conversations.child` row is inserted — because `ownedSandbox`
  (`cmd/rafikid/sandbox.go`) takes the spawning child's PARENT as an ancestry
  hint: `ChooseFor` passes the request's `ParentChildID` (daemon-written — a
  child-scoped caller's is forced to its own id), falling back to the child's
  stored parent label on a rebind/recovery. A `subtree` row is therefore
  eligible when its owner is the child, the parent, or an ancestor of either
  (`childstore.IsDescendant`). The hint is fail-closed: a wrong or empty one can
  only fail to find an owned sandbox, never admit an unrelated one. An
  ancestor's stored selector must still match a sandbox's labels for the
  sandbox to be handed down. Every `ChooseFor` — for every child, sandboxed or
  not — runs this lookup, so a bind now depends on the sandbox table; a store
  error fails the bind closed rather than reading as "not owned". The lookup is
  a single indexed read of the owner's live rows (`ListLive`, partial index
  `sandbox_owner_live`, migration 0048) with a 5 s budget.
- **A sandboxed child's cwd is a CONTAINER path.** `provisionWorkspace` sends
  the child's cwd as the Provision workdir, and the executor validates it
  exists in ITS filesystem view — for a sandbox, inside the container. A spawn
  block's cwd is therefore rewritten at `Spawn` to the sandbox's resolved
  `workdir` (the container root when the spec names none), overriding the
  inherited host cwd; a NAMED sandbox's workdir is resolved the same way in
  `provisionWorkspace` from the executor's sandbox row
  (`sandboxWorkdirForExecutor`, `cmd/rafikid/workspace_wiring.go`), because
  `Spawn` never sees a named sandbox's spec. Without this, an inherited host
  cwd does not exist inside the container and every workspace tool fails at
  Provision.
- **A sandbox's `owner` label follows its CREATOR, not the connection.** A
  sandbox created by a child is stamped with the creator child's attested
  `owner` label (the user's name), the way `attestOwner` stamps the child's
  own. Every child-provenance identity carries an empty `Username`, so
  deriving it from the connection would stamp the daemon's OS user on a
  child's sandbox — the same owner for two users' children, a cross-owner
  collision on the executor's `(owner, machine)` unique index.
- **Removal is keyed on the OWNING child.** `Close`/`CloseAllExited` remove
  the sandboxes the closing child OWNS — its own spawn block — and nothing
  else, so a block a live descendant owns itself is left alone. But a
  `subtree` block is SHARED: closing its OWNER tears its container down from
  under the owner's live descendants, which then fail closed (their next bind
  finds no connected sandbox). A half-created
  (`creating`) row is reaped once it is older than 30 minutes or, on the first
  sweep after a restart, when it predates that process; a childless spawn
  block is protected for the same 30 minutes (`Close` removes promptly; the
  reaper is a backstop). A container is removed before its executor row (a
  deleted executor row would make `unless-stopped` restart it in a loop), and
  a sandbox whose launcher is not live stays `removing` until it returns —
  UNLESS the launcher's executor ROW is also gone (a permanently dead
  launcher, or one removed from under the sandbox), in which case the row is
  tombstoned and the container is left to the orphan arm. That arm needs some
  OTHER live docker launcher on the same engine to remove it; with none, the
  container restart-loops (`unless-stopped`, its executor row gone so auth
  fails terminally) until one appears.
  `lost` is never silently recreated. The reaper is row-keyed by design —
  **two daemons must not share one Docker engine**, or they reap each other's
  sandboxes.
- **The credential is not a secret from its own sandbox.** The create body
  puts the sandbox's durable executor credential in the container's
  environment; the relay carries it untouched. The container is entitled to
  it — it is that executor's own credential.

#### `ExecutorSession`: the session-executor stream

`ExecutorSession` is a **server-streaming** RPC, and the stream's lifetime
IS the connection's — the property that replaced the framed session
executor's connection-scoped credential. The request carries only
non-gating fields (`name`, descriptive `roots`): owner, isolation,
workspace_mode and admits are all decided by the daemon from the
connection, because a client that could name them could grant itself
anything. The first streamed message is `ready`; after sending it the
handler blocks until the stream ends — from the client's side (disconnect
or cancellation, which is the eviction trigger) or from the daemon's
(`Server.Stop` during shutdown, which must not be held up by a live stream).
The daemon evicts a transient executor when the stream ends; `Open` is
called with the STREAM's context, not the request's, because that is what
the backend watches to know the session has ended.

Two answers, one selector. When a durable executor already covers this
machine and owner (matched on labels, never on self-report), the response
returns its `executor_id` with `run_local:false` and no ticket — the client
targets that executor and starts nothing of its own. That executor outlives
the client, which is what keeps an agent working after the operator
detaches. Otherwise the daemon mints a one-shot session ticket: the client
starts an executor, connects with the ticket, and the executor lives exactly
as long as the stream — no database row, no permanent credential. The
selector is `owner=<user>,machine=<name>` in both cases, so a child can move
between a durable executor and a transient one without its stored selector
ever changing.

`name` comes from the client's machine name (`$RAFIKI_EXECUTOR_NAME`, then
the name written by `rafiki executor name <name>`; a request with neither is
refused with `CodeInvalidArgument`) and is matched against the `machine`
trust label written at mint time — never against the executor's own account
of itself, which cannot gate anything. Like every other field on this
request, `name` does not gate access on its own: `owner` still comes from
the connection, so a client naming it can only ever narrow which of its OWN
executors it reaches. An executor enrolled before its profile carried a
token is labelled with the daemon's OS user and will not match spawns from
the authenticated profile — re-enroll from the authenticated profile, or
relabel `owner=`, so the two agree.

#### The raw child channel (`GetStreams`, `SendFrame`)

Deliberately raw — no event model, no parsing — because these are the faces
a human debugging a wedged child uses, not the ones an agent talks through
(children use `Send`). `GetStreams` reads a live child's raw stdin/stderr
capture: `which` is `in`, `err`, or `all` (empty means `all`; an unlisted
value is `CodeInvalidArgument`). `alive=false` means the child has already
exited — the caller falls back to the on-disk dump. `err` is never populated
for a live child by design: the stderr buffer is unguarded and racing the
reader goroutine, so live stderr is never served — it is available post-exit
via the on-disk dump. `SendFrame` forwards a raw child-protocol frame to a
live child's stdin verbatim; the daemon does not inspect it. Both are
userOnly: children speak the modeled verbs (`Send`, `Receive`, `SetResult`),
never the raw channel.

### Script children (kind `script`)

A script child is a child whose brain is a saved pymodule process instead of
an LLM. It gets everything a child already has — an id, lineage, labels, a
budget/depth/`max_children` grant, an inbox, `list`/`get`/`logs`/cockpit
presence, `kill` — and its control channel is real Connect over a per-child
unix socket.

**Spawning.** `Spawn` with `kind: "script"` and a `script` spec
(`repo`, `script`, `modules`, `args`). `repo` is `"local"` (the spawning
owner's own saved pymodules — the materializer reads the owner's rows, so a
module another user saved is not yours to run) or a registered git source's
name (executor-hosted; a locally hosted child refuses it). The three budget
pointers, labels, executor selector and a preset behave as for any other
kind; every fundi/claude-only field — model, provider, api_key, thinking,
tools, skills, MCP, prompts, sessions, `extra_args`, `prefill` — is refused
(`field "…" does not apply to kind "script"`), and a script-kind preset is
refused the same fields at save time (pkg/presets.Validate; the database's
own CHECK, migration 0040, backs it). A script child cannot be resumed: its
exit IS its result, so `Resume` refuses with "spawn it again".

**Entry points.** Two surfaces build this request, and neither adds a new
wire shape — they feed the same `Spawn` above. `pymodule_start` is the agent
tool: a fundi child materializes it from its registry whenever it has a
spawner (like `agent_spawn`), and on the MCP face it is gated exactly like
`pymodule_run` — only a per-child credential whose own row carries a live
executor binding gets it, so the delegate verbs of the pymodule family appear
exactly where the caller demonstrably operates in the executor world that
hosts scripts (the interactive human has the CLI and its `rafiki create`).
Its input is the spec itself (`repo`, `script`, `modules`, `args`) plus the
child furniture (`labels`, `max_cost`, `max_children`, `executor`, an
optional `cwd`); it returns the child id at once and takes no prompt. The
child's name defaults to the script's name, on both surfaces. The CLI verb is
`rafiki create -d --kind script --pymodule <repo>:<script> [-- args]` —
`--pymodule` is required, the after-`--` positionals are the script's argv,
a positional before `--` names the child, and the profile's default and the
remembered per-kind model are deliberately NOT sent (a script child has no
model; a typed `--model` still travels and the daemon's field refusal names
it). On a daemon with no executor pool the CLI's executor pre-flight is
TOLERATED rather than fatal: a script child does not need an executor (the
local fallback hosts it), so a lister that cannot answer leaves the field
blank for Spawn's routing to decide — ambiguity (several eligible executors)
still refuses, because the daemon would silently pick one.

**Hosting.** A script child is hosted where its pymodule cache is. The
daemon routes the spawn through `scriptExecutorRouted`: when an executor pool
is configured AND a live executor advertises `--launch script`, the child is
launched under daraja ON that executor; otherwise the local runner stays the
fallback — the daemon forks the pymodule on its own host. The routing
predicate is the SAME one `checkKindNarrowing` consults, so a child whose
parent carries an executor grant is admitted exactly when it would launch on
the pool and refused when its fallback would fork on the daemon's host — the
two can never drift.

The executor-hosted path:

- The launch payload is a daraja `ChildSpec` with the `script` variant
  (`ScriptParams{repo, script, modules, args, env, child_secret}`). The
  EXECUTOR resolves it against its own synced pymodule cache at launch time —
  the same layout `SyncPyModules` (blob modules, with per-module venvs) and
  `SyncPyModuleGitSource` (checkouts) produce, and the same resolution
  `pymodule_run` performs: the script's venv python is the interpreter when
  one was built, the repo's single venv for a git checkout, `python3`
  (or `RAFIKI_PYMODULE_PYTHON` on the executor) otherwise. A name the cache
  does not hold is a `FailedPrecondition` refusal at launch, not a runtime
  surprise; a requirements block without a built venv is refused with
  `pymodule_run`'s readiness wording (including the build-in-progress case).
- `--launch script` implies the owner's pymodule corpus sync and git-source
  refreshes on that executor (unless `--pymodules-sync=false` /
  `--pymodule-git-sync=false` is spelled explicitly) — a script host without
  the corpus hosts scripts that cannot resolve. The corpus is owner-scoped:
  the executor's `owner` label gates what it receives, and the spawn's owner
  must be the same user.
- Respawn is OFF for scripts. daraja's respawn loop exists for claude (a
  process whose value is its continuing conversation); a script's exit is its
  result, so an unexpected exit ends the daraja with it — the host emits the
  one `Exited` event, gives up, and the daraja process exits. The process
  does not die before the relay tail is out: on the host-gave-up path it
  waits (bounded by `darajaRelayTailGrace`) for the daemon to acknowledge the
  ended relay stream by closing the connection, so the final frames — the
  `Exited` a script's settle is computed from — cannot be lost to the process
  exit. A Restart of a script kind is refused semantically: the wire spec carries names, not the
  executor-resolved argv, and nothing that can call Restart ever calls it for
  a script.
- A script child's stderr is RELAYED (a `stderr` arm on daraja's relay
  oneof), because the settle tail is computed from it; claude's stderr stays
  discarded. Both pipes are synchronous, and the daemon-side consumer drains
  stdout and stderr on their own goroutines (as `pkg/child` does) — a
  consumer that stops reading either pipe stalls the other.
- The per-child Connect socket moves to the executor: daraja serves
  `pkg/childsock` against the daemon's face — the address it already
  reverse-dials. A TLS executor dial verifies the same pinned certificate the
  executor itself dials with (`--pin-cert`/`--server-name` ride every
  launch); a socket-dialled executor proxies to the daemon's local Connect
  control socket. The child secret travels in the launch payload
  (`child_secret`) and then in the daraja process's environment
  (`RAFIKI_CHILD_SECRET`) — never argv, which `ps` renders world-readable —
  and is held in memory only, dying with the child. The script's environment
  is the executor's full environment minus every
  `RAFIKI_*`/`ANTHROPIC_*`/`OPENROUTER_*` variable (executor-set functional
  variables survive — the strip runs on the daemon, on the launch payload,
  and in daraja, all from the one list in
  `pkg/child.ScriptEnvStripPrefixes`), plus the forwarded `env`, the resolved
  `PYTHONPATH`, and the one channel: `RAFIKI_CHILD_CONNECT`.

The locally hosted fallback (no executor pool route, or no executor declares
the kind) forks on the daemon's own host, in its own process group, from the
daemon's state dir (`script-host/<childID>/`, 0700). Its environment is the
daemon's FULL environment minus every `RAFIKI_*`/`ANTHROPIC_*`/`OPENROUTER_*`
variable — no credential, no retired client socket, no child-id global — plus
a recomputed `PYTHONPATH` (script dir is `sys.path[0]`; each named module's
directory joins in call order) and exactly one control variable:

- `RAFIKI_CHILD_CONNECT` — the per-child socket path. NOT `RAFIKI_SOCKET` (a
  retired client variable and a hard CLI error).

The local runner resolves only the SPAWNING OWNER's own saved modules: a
git-sourced `repo` is refused with a pointer to executor hosting, and so is a
requirements block without a venv (venvs live in an executor's synced cache,
not on the daemon host).

**The per-child socket.** The socket is the credential: the process never
sees a token, only a filesystem path it can reach and nobody else can. The
socket (`pkg/childsock`; on the daemon host
`<script-host>/<childID>/`, on an executor under rafiki's own
cache dir, dir 0700 / socket 0600, HTTP/1.1 AND h2c) reverse-proxies every
request to the daemon's WHOLE proxy face — the request path is preserved, so
the socket reaches the Connect route AND the LLM face — with the child's
per-child secret injected as `Authorization: Bearer …` — the same per-child
secret every other child credential resolves through (`ChildForMCPToken`) —
authorizing Connect verbs AND LLM-face calls attributed to the child and its
owner through the face's token middleware, after stripping any inbound
`Authorization`/`X-Rafiki-*` header, so a caller cannot smuggle a stronger
identity past the injection (and there is no upstream-relay path: the strip
removes what one would need). The socket is closed and unlinked when
the child exits, and the proxy's per-spawn bind deliberately does NOT touch
the process umask (it is process-global; the 0700 directory is the access
boundary). Any language with an HTTP client can speak to it: unary calls are
`application/json`, server-streaming (`Receive`) is enveloped
`application/connect+json`. When the daemon is unreachable (restarting), the
proxy answers HTTP 503 with a Connect error body naming code `unavailable` —
the typed signal a retrying client waits on.

**Messages.** `agent_send` to a script child lands in its inbox and the
`Receive` stream delivers it. The inbox DELIVERY for a script child defers to
the stream (delivery returns a deferred sentinel rather than writing to the
script's stdin, which carries no protocol — the provider drops stray stdin
frames as a backstop), so a script child's rows stay pending
until its stream pulls them. Fragments the event buffer pushes INTO a script
child (its own children's settles, budget warnings) ride the same rows, and
the buffer does NOT busy-defer them: `childIsBusy` exempts script children,
because "busy" means a turn may be in flight that an injected frame would
corrupt, and a script has no turns — its whole life is one streaming run,
delivery is its Receive stream's pull of pending rows, and it goes idle only
at exit, so the gate could never release a batch except through
`RAFIKI_EVENTBUF_MAX_WAIT_MS` (default 60s) against the one consumer actively
waiting for exactly that news. The debounce still coalesces — five reports
still cost one injected frame; only the busy withhold is skipped.

**Settle.** A script child's exit is its settle: exit 0 settles `done`,
anything else `failed`, and the parent's settle fragment carries the stored
`SetResult` payload — or, when the script never called `SetResult`, the last
4 KiB of its stderr. The kill ladder is unchanged (stdin close, then the
SIGTERM/SIGKILL rungs after the caller's timeouts); a script holding an open
`Receive` stream sees the `shutting_down` stop within one poll interval and
can exit before the signal rungs. A script that holds NO stream is told
nothing before the signals — the default 180 s graceful window applies, so
callers that want a prompt death pass explicit timeouts.

**Cost.** On `ListChildren`/`GetChild`, a script child's `cost_usd` is its
SUBTREE's spend — the script drives no LLM turns of its own, but the fundi
and claude children it spawned do, and subtree pricing folds the script
child's own conversations in with theirs. When the cost source is missing or
the query fails, the field is left unset: absent means "not reported", and a
reported zero would claim a subtree measured at nothing. Every other kind's
`cost_usd` is still that child's own conversation rollup, unchanged.

The subtree rule shapes the CLIENT walks, not just the daemon's number:
`rafiki list`'s TOTAL column and the cockpit rail's `SubtreeCost` STOP
recursion at a script node — the script's figure already IS its subtree's,
so descending into the children it spawned would double-count them
(`cmd/rafiki/output.go`, `pkg/tui/cockpit.go`). An unpriced script (its cost
source missing) reports nil and its client-side subtree total is nil, not a
partial sum of the priced children beneath it.

**What a kill contains, and what it does not.** Killing a script child
signals its process group: the script's own subprocesses die with it. Its
DAEMON-MANAGED descendants — the fundi children it spawned through its
per-child socket — are NOT swept, and this is deliberate, the same boundary
every kind has: there is no daemon-level descendant sweep for any kind, so
adding one only for scripts would make kill semantics kind-dependent without
a design that answers recursion, ordering and the budget accounting of the
sweep itself. A surviving worker keeps its budget fence, stays visible in
`rafiki list`, and can be killed by name; its settle fragment becomes a
pending row in the dead parent's inbox (the inbox is dropped when the parent
is forgotten, and the retention sweep reaches terminal rows only). If
subtree teardown ever lands, it is a decision about ALL kinds at once, not a
script-only patch.

**Restart.** A LOCALLY hosted script dies with its daemon. Recovery loads
its still-live row as exited and settles it `failed` with the reason
"daemon restarted" toward its parent — never left running-with-no-process,
never re-run (a re-run would silently start the work over). A daraja-hosted
script does NOT die with its daemon: daraja keeps the process on the
executor, keeps re-dialing while the daemon is down (the script's own SDK
calls fail `Unavailable` during the gap and retry with bounded backoff — the
SDK's contract), and its row — marked by the `rafiki/daraja-pgid`
label, stamped at launch like claude's — is loaded as exited without the
settle. Re-attaching such a child to the RESTARTED daemon (persisting the
daraja credential across the restart and re-binding the per-child socket) is
the design's named next step, deferred beyond "the process survives and the
SDK retries": the daemon's daraja credential registry is in memory, so the
restarted daemon refuses the reconnect and daraja exits with its child.

### Skill management verbs (`ListSkills`, `GetSkill`, `UpsertSkill`, `DeleteSkill`, `SetSkillEnabled`)

These five verbs manage the database-backed skill corpus in
`conversations.skills` — the tier a fundi child's skill tool reads, with the
inventory taken at spawn and a skill's body fetched at invocation. They back
the `rafiki skills` CLI group.

A `SkillRow` carries `namespace`, `name`, `description`, `source`, `enabled`,
`shadowed_core_version`, `updated_at` (a `Timestamp`) and `body`.
`ListSkills` (`include_disabled` also returns rows an operator switched off)
always omits `body` — an inventory is a handful of lines and a body is a
document — so an inventory costs none of the corpus's weight;
`GetSkill` populates it.

`UpsertSkill` creates or replaces the row at `(namespace, name)` and returns
the stored row. `namespace` defaults to `rafiki` and `source` defaults to
`manual` when empty; `name` and `body` are required (`CodeInvalidArgument`
otherwise). `source: "rafiki-core"` is **rejected** with
`CodeInvalidArgument`: that provenance is owned by the daemon's own startup
sync of its embedded corpus, and a client that could claim it could plant a
row the sync would prune or fight over on every restart. An upsert never
changes an existing row's enabled flag — re-importing content must not
silently re-enable something an operator switched off.

`DeleteSkill` hard-deletes every row under the name (in the override state
that is two rows), and `SetSkillEnabled` flips one name's flag. Both return
`CodeNotFound` when the name has no row in the requested state. A missing
skill is an ANSWER (`CodeNotFound`), never an internal failure — getting that
wrong makes a typo look like a broken daemon.

Authentication is the same `UserTokenAuth` middleware as every other Connect
verb (§2.3): any valid user token may read and write, in any namespace, until
multi-user scoping is built. The verbs are mounted inside the proxy face via
`server.Handler.Mount`; there is deliberately no second mount in `main.go` —
a `mux.Handle` for `/rafiki.v1.Control/` there would shadow the face's auth
middleware, because ServeMux prefers the longer pattern.

### Provider bans (`ListProviderBans`, `BanProvider`, `UnbanProvider`)

Operator bans ride the provider cache guard (`routing.ProviderGuard`): a ban is
an ejection with reason `operator` recorded under model line `*`, merged into
every outgoing OpenRouter request's `provider.ignore` alongside the guard's
own ejections. Operator bans are exempt from the guard's per-model-line cap of
three. Every ban and lift is appended to `openrouter.provider_ejection` (a lift
is a superseding `reason = 'lift'` row, never a delete) BEFORE it takes effect,
and a failed write fails the call; the daemon rehydrates the latest row per
`(provider, model_line)` at startup. Another daemon on the same database sees a
ban only when it restarts. `RAFIKI_PROVIDER_GUARD=off` disables automatic
ejection only; operator bans still apply. Backs `rafiki providers`.

Responses name the serving provider by display name ("OpenInference") while
`provider.ignore` takes slugs ("open-inference"), and the two do not map
mechanically. When a configured provider routes via OpenRouter, the daemon
loads `https://openrouter.ai/api/v1/providers` lazily (first use, daily
refresh, never on a request's critical path) and resolves every ejection,
ban and `upstream_provider` log value through it; before the first load, or
with no OpenRouter provider configured, the slug is guessed.

A ban reroutes every user's children, so mutations require an admin user
credential or the anonymous unix socket (the socket is the credential); a
non-admin user, a child-attributed identity and a per-child token are all
refused `CodePermissionDenied`.

### Route policy (`ListRoutes`, `SetRoute`, `DeleteRoute`)

The route-policy verbs manage the daemon's model-line → routing-spec rows
(`openrouter.route_policy`, migration 0042, `pkg/routepolicy`): the
append-only log the resolver reads at request time. A row's `spec` is the
stored form of a routing spec — `routing.ParseSpec`'s grammar
(`sort=price,quant=fp8+,only=deepl,nodata,zdr`), the same bracketed tail a
model string may carry, stored bare. `model_line` `"*"` is the global line;
resolve merges the longest matching line row over the global row.

Writes are userOnly and take effect immediately: `SetRoute` appends (never
updates — the newest row per line wins) and `DeleteRoute` appends a
tombstone, then the adapter (`cmd/rafikid/connect_routes.go`) reloads the
in-memory `routepolicy.Policy` from `Store.Active`, so the very next request
resolves by the new rows. `ListRoutes` is anyCaller — routing defaults name
providers, not users — and reads the reloaded view. `ChildSummary.routing`
reports a child's RESOLVED spec in canonical form (`routing.Spec.String()`),
empty when none; the summary's `model` stays the base id. The resolution ran
ONCE in `Controller.Spawn` (`resolveRouting`): the spawn spec merged over the
preset spec over the policy row for the (alias-translated, `:batch`-stripped)
base id, stored on the session — a policy edit never rewrites a running
child's STORED spec, and resume reads that value back verbatim. The policy's
data flags are monotone, though: on every proxied request `RoutingFor` ORs
the model line's live row on top of the stored spec (stored wins per key,
`nodata`/`zdr` always hold), so a row set after the spawn — e.g. a `*`
`nodata` — governs that child's later requests by design.

`SetRouting` is the one sanctioned exception to "a spec is resolved once": an
EXPLICIT, authority-checked write of a child's stored spec, never a re-resolve
of policy. `delta` is parsed with the same grammar and merged OVER the stored
spec (`delta.Merge(stored)` — the delta is the more specific level, so a key it
names wins and every stored key it leaves out survives); the result is written
back in canonical form and persisted with the child's record, so it survives a
daemon restart and governs the child's next `RoutingFor` / `resolveRouting`
answer. For an in-process fundi child that next request is live: the daemon
wires `fundi.Config.RoutingSource` (→ `llm.WithRoutingSource`), so the child's
`llm.Client` re-reads its stored spec on every request instead of using the
value frozen at spawn — a request already in flight keeps the spec it started
with. The grammar cannot express "clear a stored key" — `prefer=` is a parse
error — so a delta only ever sets. Every provider slug the merged spec names is
resolved through the daemon's `ProviderDirectory` before the write; an unknown
slug is `CodeInvalidArgument` and changes nothing, while a degraded directory
(OpenRouter unreachable) accepts the slug as given so steering is never blocked
by an outage. `nodata`/`zdr` are monotone here too: the merge ORs them and the
grammar cannot un-set them, so a child's steer can never drop an operator's
data policy.

### Presets (`ListPresets`, `GetPreset`, `PutPreset`, `DeletePreset`)

The four preset verbs manage the daemon's agent presets in
`conversations.presets` — named seats fixing model, tools, prompt and budget —
backing `rafiki preset` and the `preset_*` tools (§2.4). A preset is
**owner-scoped**: the owner is resolved from the authenticated credential,
never a request field — a user token reaches its own bucket, and the
anonymous unix-socket caller shares the daemon's single unattributed bucket,
exactly as `ListPymodules`.

The store is **append-only**: `PutPreset` inserts a new row and never
modifies an existing one; `DeletePreset` stamps `deleted_at` on every live
version (the only UPDATE a preset row undergoes) and errors when none is
live; `GetPreset{history: true}` returns every version, deleted included,
newest first. A deleted name later re-put starts a new version line.

`PresetRow`'s allowlists (`tools`, `skills`, `mcp_servers`) are tri-state
through the `StringList` wrapper — proto3 cannot mark a repeated field
optional, so an ABSENT message means unset (the kind's default: everything),
PRESENT-AND-EMPTY means none, and a list means exactly those. Collapsing the
first two turns "all tools" into "no tools", so the daemon's conversions
(`pkg/presets/proto.go`) keep nil and empty distinct end to end, pinned by
`TestPresetProtoRoundTripTriState`. The other optionals (`context_files`,
`max_cost`, `max_depth`, `max_children`) are proto3 `optional` for the same
unset-vs-zero reason. `created_at` and `deleted_at` are `Timestamp`s; an unset
`deleted_at` means the row is live.

Spawn resolution happens ONCE, in `Controller.Spawn`: `applyPreset`
(`cmd/rafikid/presets.go`) resolves the request's `preset` name against the
owner's rows and merges it into the spawn fields before anything else reads
them — kind (confirmed, never contradicted), model/provider/thinking,
executor, labels, system prompt (fixed; the request may only append),
budgets, and allowlists a request may only NARROW. Resume rebuilds from the
persisted session, which already holds the RESOLVED fields, and never
re-resolves — so editing a preset never reaches a running or resumed child.

### Recall and memories (`Recall`, `RecallContext`, `GetMemory`, `MemoryTree`, `PutMemory`, `DeleteMemory`, `RecallBackfill`, `RecallStatus`)

The eight recall verbs serve the recall index: `conversations.memory` (the
saved memory tree), plus two DERIVED index tables over captured
conversations — `conversation_window` (extracted ~3200-char windows of
dialogue and compacted tool-call arguments) and `conversation_summary`
(rolling LLM summaries). Search fuses one BM25 list and one vector list per
source with RRF (`recall.Search`); tool results are never indexed (they
appear in context expansion as size markers only).

Scoping is asymmetric BY DESIGN, and the request carries no scope field —
the daemon resolves it from the authenticated credential exactly as
`ConversationSearch` does: conversation-derived sources (summaries, windows)
read under the credential's scope, so an **admin's hits cover the whole
daemon's users** and a named user's cover only their own rows (an identity
that is neither admits nothing, `CodePermissionDenied` via
`recall.ErrInvalidScope`). A per-child credential resolves to its OWNER's
NON-admin identity (`recallOwner`, `cmd/rafikid/recall.go`): a child of an
admin reads that admin's own rows, never the whole daemon, and a child's
memory reads AND writes are its owner's. That surface is WIDER than the
child's `conversation_*` tools, whose reads are subtree-only
(`insights.ScopeSubtree`): `recall` is scoped by OWNER, not by subtree, so a
child reaches its owner's whole conversation-derived corpus. **Memories are
always the caller's own** — every memory method takes the caller's user id, never a scope, admin
included: a saved memory is private to whoever saved it. An empty owner is
`CodeInvalidArgument` (`recall.ErrNoOwner`).

Error mapping (`recallError`, `pkg/connectapi/recall.go`) over the store's
sentinels: `recall.ErrNotFound` → `CodeNotFound` (an unknown hit id, memory
or tombstoned memory name); `recall.ErrInvalidPath` (a bad memory path
label) and `recall.ErrNoOwner` (no owner) → `CodeInvalidArgument`;
`recall.ErrInvalidScope` (a scope admitting nothing) → `CodePermissionDenied`;
`connectapi.ErrNoBackfillBudget` (a backfill armed without a positive spend
ceiling — refused before any state is written) → `CodeInvalidArgument`;
anything else, store failure or otherwise → `CodeInternal`. A handler-level refusal —
an empty `query`, an empty `id`, an unknown source name, non-JSON
`meta_json` — is `CodeInvalidArgument` before the manager is ever called.
When the daemon has recall unwired (no database), all eight answer
`CodeUnavailable`.

Zero asymmetry between the two time-bearing requests: `Recall`'s
`since`/`until` treat **unset or the epoch as unbounded** (a filter that is
simply absent), while `RecallBackfill`'s `since` treats **unset as "from the
beginning"** — a real cut-over instant. `Recall`'s `limit` 0 means the
default (10); `RecallBackfill` has no default instant.

`MemoryRow.created_at`/`updated_at` and `RecallHit.when` are `Timestamp`s;
`RecallStatus.backfill_since` is a `Timestamp`, unset when backfill is off.

### `Send` and the durable inbox

`Send` writes a row in `conversations.agent_inbox` before it reaches the
child, and `message_id` names that row: a prompt, a steer or an abort is
persisted before it is delivered, and retired only once the agent confirms it
entered a turn.

The one exception is an **abort aimed at a `claude` child**. `claude -p` has
no in-band abort frame — the only way to stop it is to signal the process and
resume the session — so that abort is a signal plus a lifecycle operation
rather than a message, and it is never persisted. It still aborts; the RPC
returns success with an **empty `message_id`**, because there is no row to
name and an invented id would resolve to nothing. Storing it would risk
replaying a cancellation into an unrelated later turn.

### `Send` steps

`SendRequest.steps` (a repeated `SendStep`) names tool calls the daemon runs
at send time, BEFORE the message is queued; each step's rendered output is
appended to the message text the child reads ("text\n\nrendered", or the
rendered output alone when the request carried no text blocks). Every step
carries `where` (`StepSite`: `CHILD` runs on the target child's executor and
workspace, `SENDER` on the caller's own) and exactly one of `read`, `bash`,
`pymodule_run`. A `bash` step carries an optional `timeout` (a `Duration`;
unset or zero selects the 30 s default, a value above 60 s is refused
`invalid_argument`). An unspecified site or an absent kind is refused
`invalid_argument` per step, never defaulted; steps with `ABORT` are refused
the same way, because an abort carries no content. The response carries one
`StepSummary` per step — index, tool, where, outcome, byte count, truncated
flag — and NEVER a step's output, except the `echo` prefix a step asked for.
The handler runs the steps through the wired `SendStepRunner`
(`pkg/connectapi/send_steps.go`); until the daemon attaches one, a send with
steps fails `unavailable` while a stepless send is unaffected. The daemon's
runner is `cmd/rafikid/send_steps.go`, shared by the Connect `Send`, the
child-bound `agent_send` tool (`controllerSpawner`, also what a child
credential gets on the MCP face) and the operator MCP path (`userSpawner`).

**Execution.** All steps are validated before the first one runs, so a
refused send has no side effects; then they run sequentially in request order
under one 120 s deadline. The target must be deliverable (known, not exited, not shutting down) or the send is refused before any step runs. A refusal during validation runs nothing. A failure
during execution (`CODE_DENIED`, a lost executor, the deadline, the rendered
cap) leaves the side effects of earlier steps in place; the send still
enqueues nothing.

**Whose authority.** `CHILD` steps run on the TARGET's snapshot: its
`rafiki/executor` (must be live), `rafiki/workspace` (may be empty) and cwd.
`SENDER` steps run on the CALLER's, and only the output reaches the target.
A caller with a position in the agent tree is also held to its own tool
allowlist for `CHILD` steps, so it cannot reach a tool through a descendant
that it could not run itself; the operator is ungated. A `SENDER` step is refused `failed_precondition` when the caller has no
position in the agent tree (the operator and MCP face have none), and
`permission_denied` when the caller's own tool allowlist does not include the
step's tool: a fundi child needs `read`/`bash`/`pymodule_run` admitted by its
`tools` (nil admits all, `no_tools`/`no_builtin_tools` admit none); a claude
child needs no `tools` restriction at all (restrictions carried in its launch args or permission mode are NOT consulted, so the gate is coarse for claude callers); a script never qualifies.

**Limits.**

| Limit | Value |
|---|---|
| steps per send | 16 |
| `bash` timeout | default 30 s, at most 60 s |
| per-step executor call | 60 s (`bash`: its `timeout` plus 5 s) |
| whole send | 120 s |
| per-step output | 32 KiB, kept from the front, marked `[truncated: showing N of M bytes]` |
| rendered block | 128 KiB, refused `invalid_argument` naming the five largest steps |
| `echo` | first 2 KiB of the (post-truncation) output |
| header subject | 200 bytes |

**Outcomes**, keyed on the executor's `Failure.Code` (never message text):

| Result | Outcome |
|---|---|
| normal result | `ok` |
| `CODE_TOOL_FAILED` | `error`; the message is rendered inline and the send continues |
| `CODE_TIMEOUT` | `timeout`; rendered inline and the send continues |
| `CODE_DENIED` | the whole send fails `permission_denied` |
| `CODE_EXECUTOR_LOST`, `CODE_UNSPECIFIED`, a transport error or the deadline | the whole send fails `unavailable` |

**Rendering.** The appended block opens with a line naming the snapshot
nature of the content and a per-send random nonce (16 hex characters from
`crypto/rand`); each result follows a header
`=== <nonce> <index> <tool> (<where>): <subject> ===`, so content that
imitates a header cannot be mistaken for one. A `SENDER` step is labelled as
having run in the sender's environment.

### Event vocabulary

Every event payload is classified into a tier:

| Event | Tier | Purpose |
|---|---|---|
| `UserMessage` | durable | User prompt message; attached images ride as `ImageBlock` content ahead of the text, live and in `GetHistory`. A `tool_result` rides a `UserMessage` too, and a tool's own images (a `read` of a PNG) ride inside its `ToolResultBlock` the same way |
| `AssistantMessage` | durable | Assistant response message |
| `TurnStart` | durable | Start of an agent turn |
| `TurnEnd` | durable | End of an agent turn |
| `ToolExecutionStart` | durable | Tool began executing (name, tool_use_id) |
| `ToolExecutionEnd` | durable | Tool completed (duration, is_error) |
| `Retry` | durable | Turn-level retry with attempt count and reason. Today's only producer is the daemon's rate-limit auto-resume (`cmd/rafikid/ratelimit_resume.go`): `will_retry: true` announces a scheduled resume of a claude child whose turn a 429 killed — `reason` is the cause ("rate limited (HTTP 429)"), `resume_at` (a `Timestamp`) the fire instant (unset on resolution events and pre-field rows), `max_attempts` the schedule cap; `will_retry: false` resolves it (fired, cleared by a clean completion, or the attempt cap reached), with the human sentence in `reason`. Wall-clock times are never embedded in `reason`: the producing daemon's clock zone is arbitrary (a container runs UTC), so clients render `resume_at` in the viewer's local zone. The cockpit renders the scheduling half as a system block in the transcript and shows ⟳ on the rail row until the resolution half arrives |
| `ChildSpawned` | durable | A sub-agent was created (child_id, parent_id, name) |
| `ChildExited` | durable | A sub-agent exited (optional exit_code, signal) |
| `AgentStatus` | durable | Status change from the daemon's closed vocabulary |
| `Error` | durable | Turn or engine level error event |
| `CompactionBoundary` | durable | Claude Code compacted this child's context (trigger, optional pre/post token counts) |
| `ScriptOutput` | durable | One coalesced chunk of a script child's stdout or stderr. Fields: `stream` (`"stdout"`/`"stderr"`) and `text` — the newline-terminated lines or split pieces carried verbatim. Produced by the daemon's per-child coalescer (`cmd/rafikid/script_output.go`); see "Script output" below |
| `ContentBlockDelta` | ephemeral | Live token/text streaming delta |

Fourteen payload types today: thirteen above plus `ScriptReport` (the
`Report` verb's stored payload — it reaches a child's own event log only
when the top-level script has no parent to push to). The count is prose, not
enforcement; the wire's `Event` oneof is.

### Script output

A script child's stdout and stderr are published as durable `ScriptOutput`
events, coalesced per child per stream: one event per line would flood the
event log at line rate. The contract (`cmd/rafikid/script_output.go`):

- **Coalescing** — lines buffer per stream and flush at 4 KiB or 250 ms
  after the stream's first unflushed line arrived, whichever first.
- **Seal-before-append** — buffered units are SEALED at the size trigger, so
  a size-triggered event is at most ~4097 bytes (a 4 KiB unit plus one line
  that arrives before the seal) and no line is ever split by coalescing.
- **Oversized lines split at exactly 4 KiB, at a rune boundary** — a lone
  line longer than the bound becomes sequential pieces, each capped at 4 KiB
  (the cap respects the UTF-8 rune, never cutting mid-rune), each carrying
  no newline; the pieces concatenate byte-faithfully. A newline-free run on
  stderr delivers unterminated 4 KiB FRAGMENTS past the bound, so a
  `\r`-only progress bar cannot grow daemon memory without bound.
- **UTF-8 fidelity, not byte fidelity** — every chunk is sanitized with
  `strings.ToValidUTF8` at ingestion, because a mid-rune cut would otherwise
  hand protobuf-go a string it refuses to marshal. The contract is
  line-faithful for valid UTF-8 input; arbitrary bytes are not guaranteed.
- **Concatenation reproduces the stream** — every `ScriptOutput` event for
  one stream, in ordinal order, concatenated, reproduces that stream's
  valid-UTF-8 output (each line carries its own `\n` terminator; split
  pieces carry none).
- **Flush-before-exit** — the final flush runs on child exit BEFORE
  `child_exited` publishes, so the last output event's ordinal is strictly
  below the exit's and a backfilling client sees the whole output before the
  exit.

Rendering: `rafiki logs`/`tail` render `ScriptOutput` text one-line-per-event
(stdout verbatim, stderr prefixed `stderr| ` at the EVENT's granularity, the
text flattened to that one-line form). The cockpit renders per rendered line
with a `stderr│ ` failure-colour prefix, joining adjacent same-stream
split-piece events so an oversized line stays whole on screen
(`pkg/tui/session.applyScriptOutput`).

`CompactionBoundary.pre_tokens`/`post_tokens` are both `optional`, and
deliberately asymmetric across the two ways this event reaches a client.
The LIVE event (emitted when Claude Code's own `system/compact_boundary`
frame arrives) carries both. A REATTACH-synthesized instance of this event
— built from a stored `conversation_message` row tagged
`kind='compaction_summary'`, not from the live stream — carries only
`pre_tokens` (an approximation: the previous turn's `input_tokens`, the
size of the context the boundary replaced), because the stored row has no
post-compaction count. That approximation appears only when a prior turn's
`input_tokens` is nonzero: with no prior turn, or one with 0, the stored
`input_tokens` is NULL and the reattach event carries NEITHER token — the
bare one-sided format. A client renders the two-sided and one-sided cases
differently for exactly this reason — see `pkg/tui/session`'s
`formatCompactionBoundary`.

`AgentStatus.state` is one of the ten `protocol.Status` values: `spawning`, `idle`,
`streaming`, `running`, `tool_running`, `compacting`, `batch_wait`, `blocked_ui`, `shutting_down`,
`exited`. It is a string rather than an enum so a new daemon status does not require
regenerating every client.

`running` is the script child's status, and it behaves differently from every
other state: a script child spawns DIRECTLY into it — the transition is
recorded once at spawn, before the child's stdout is read — and output never
moves it. A script has no turns, so the turn machinery
(`idle`/`streaming`/`tool_running`/…) never applies; only the process exit
replaces `running` (via `shutting_down` on a kill). It is a working status
for every consumer that asks "is it working": attach's busy wait, the rail's
live-set and animated glyph, and the CLI's status colour all include it.
(`pkg/child.StateMachine.ForScript` is the enforcement point; the machine
must be configured before the first event or the status is misreported.)

`AssistantMessage.cost_usd` and `TurnEnd.cost_usd` carry **different**
meanings despite sharing a name and a stream. `AssistantMessage.cost_usd` is
the RUNNING total for the CURRENT turn only, so far — every iteration of that
turn up to and including this reply, deliberately excluding every completed
prior turn. It fires once per LLM reply, before the turn ends, and is what
lets a live viewer (the TUI rail) show a cost figure moving during a
still-running turn. `TurnEnd.cost_usd` is that same turn's FINAL cost, fired
once when the turn actually ends. A client that wants a running
conversation-lifetime total sums settled `TurnEnd.cost_usd` values across
prior turns and adds the latest `AssistantMessage.cost_usd` — summing
`AssistantMessage.cost_usd` figures directly double-counts, since each one
already includes every earlier iteration of the same turn. Both fields are
`optional`: absence means "not yet priced" (no catalog entry for the model),
distinct from a reported zero (priced and genuinely free).

### Executor administration

The executor verbs above share one vocabulary. There are two kinds of
executor:

- **Durable** — minted by `EnrollExecutor` (a one-time enrollment token the
  executor redeems) or `CreateExecutor` (row and durable credential in one
  step, no handshake). Has a database row with operator-written labels,
  admission, isolation and workspace mode; lives until `DeleteExecutor`.
- **Transient** — started automatically by `rafiki create`/`rafiki attach`
  through the `ExecutorSession` stream. No database row, authenticated by a
  one-shot ticket over the already-authenticated control connection; dies
  when the stream ends.

**`owner` and `machine` are written by the DAEMON, and a request carrying
either as a label is REFUSED** with `CodeInvalidArgument` — not silently
overwritten, so a caller learns their selector will not mean what they wrote.
`owner` comes from the connection (`server.Identity`'s username; the
anonymous local caller owns nothing but shares the daemon's unattributed
bucket), and `machine` from `name`, validated daemon-side (letters, digits,
`-`, `_`, `.`, at most 63 characters — a comma or an equals sign would
silently reparse the `owner=…,machine=…` selector into a different one,
which for a value deciding where a child lands is a confinement bug, not a
formatting one). `name` names the machine the executor will RUN on, which is
not necessarily the one that minted the token: an enrollment token is
routinely carried elsewhere and handed to the executor as `--enroll-token`.
`(owner, machine)` is unique among rows carrying a `machine`; `EnrollExecutor`
does not detect a collision (the token lives in the enrollment-token table,
not in `executors`), so the index fires later, at REDEMPTION, as a terminal
401 on the executor's upgrade request — while `CreateExecutor`, which writes
the row immediately, answers the collision inline with `CodeInvalidArgument`.

**Ownership.** Every executor carries a durable `owner_user_id` beside its
labels, and selection requires `executor.owner_user_id = child.owner_user_id`
IN ADDITION to `Admits`, with NULL/empty on both sides counting as equal: an
unowned executor serves only unowned children — and never a user's child,
however permissive its `Admits` selector — while a user's executor serves
that user's children (their sub-agents inherit it) and nobody else's. There
is NO admin exception. `Admits` can only narrow, never widen; the `owner`
LABEL stays display-only, matched by an `Admits` selector and never compared
for identity. The id is set at mint time from the connection's identity
(`EnrollExecutor`, `CreateExecutor`; the anonymous unix socket owns nothing,
`""`), copied from the enrollment token at redemption, and stamped onto a
transient session executor's ticket from the session's identity; a resumed
child keeps the `owner_user_id` its stored session carries, and the binder's
TWO pinned-return arms are exempt from re-selection for the same reason: the
fundi re-provision-in-place arm (`boundExecutor.doRecover` on a still-live
executor) and the claude resume-snap arm (`darajaLaunchExecutor` returning
the executor pinned in the snapshot's `rafiki/executor` label from
`Live()`, with no owner check). In both, the pin was written by the daemon
under this rule at original launch, `owner_user_id` has no post-mint
setter, and the child's transcript lives on that machine's workspace. An
executor that already carried an `owner=<name>` label was backfilled to
that user's id by migration 0044, which is a behaviour change: such an
executor no longer serves token-less unix-socket children.

The management RPCs on this face are scoped the same way
(`executorAuthority` in `cmd/rafikid/connect_executoradmin.go`): the
anonymous unix socket and an admin user credential see and mutate
everything; any other user credential sees only rows it owns — the
`ListExecutors` empty-kind listing filters to them — and
`LabelExecutor`/`DisableExecutor`/`EnableExecutor`/`DeleteExecutor` resolve
the target first and refuse a foreign one with `CodePermissionDenied`
(reason `permission_denied`, "executor <short id> belongs to another user"),
which names the executor but never the other user. An unknown executor stays
a not-found answer from the Controller, not a permission refusal. A transient
session executor has no row at all, so the same check runs against its
in-memory owner. Pinned by the `TestSelect*`/`TestExecutor*` ownership tests
in `cmd/rafikid`.

`LabelExecutor` is how `owner` and `machine` are changed after the fact —
they cannot be set through the mint verbs, but they are ordinary labels on
the row once written. Setting `machine` to a name this owner already uses is
refused and the row left untouched. `executor_id` may be the full row id or
any unique trailing fragment of at least four characters: ids are UUIDv7s
whose leading bits are a timestamp, so rows minted in the same window share
their front and only the tail distinguishes them — this is what makes the
short form `rafiki executor list` displays usable verbatim. An exact id
always wins; a fragment matching several rows fails with
`CodeInvalidArgument` naming the rows it matched, one matching none with
`CodeNotFound`.

`DisableExecutor` turns a credential off (it fails authentication on its next
connection); `EnableExecutor` re-allows it; `DeleteExecutor` removes the row
outright — there is no tombstone, because nothing else keys off an executor
id for historical resolution — and evicts a currently-connected executor
from the live pool within one health interval, the same as disabling.

The trade between the two mint paths runs one way: `EnrollExecutor`'s token
is one-shot, so a theft announces itself by being consumed; `CreateExecutor`
hands the operator a long-lived secret whose theft is silent. Prefer
enrollment where the machine can keep a file.

### User administration

`CreateUser`, `ListUsers` and `RemoveUser` are the Connect faces of user
management, all `userOnly` with an admin gate in the adapter
(`cmd/rafikid/connect_users.go`): an admin user credential or the anonymous
local socket; a non-admin user, a child-attributed identity and a per-child
token are refused `CodePermissionDenied` BEFORE any store call, and the
ordering is pinned by `TestUserRPCsRefuseNonAdmin`.

- **`CreateUser`** mints a NON-admin user, whatever the caller: admin is
  never inferred from an empty user table, so Connect can never mint the
  first admin. **Admins come only from `rafikid user create <name> --admin`**
  — the direct-DSN path (`cmd/rafikid/user_cli.go`), which opens the
  database the way `rafikid migrate` does and needs no running daemon. This
  is the recovery path in both directions: a fresh daemon (no user exists to
  authenticate an RPC caller, so Connect's `CreateUser` is unreachable until
  one does), and a stale profile token (a credential that no longer resolves
  cannot refuse the verb that mints its replacement, because
  `rafikid user create` never contacts the daemon at all). The token mint is
  CONDITIONAL: `mint_token` ABSENT means the daemon mints iff OIDC login is
  NOT configured on it (a user on a daemon with no IdP would otherwise be
  unable to authenticate at all); PRESENT is honoured as given — absent and
  false mean different things, which is why the field is proto3 `optional`.
  When a token is minted its plaintext is printed exactly once; the daemon
  stores only its digest. `CreateUserResponse.created_at` and
  `UserRow.created_at`/`deleted_at` are `Timestamp`s; an unset `deleted_at`
  means the user is live.
- **`UpdateUser`** edits a user's email (`SetEmail`). `email` is the only
  editable field, and the admin bit is deliberately absent from the request
  type: it comes only from `rafikid user create --admin` — no Connect call,
  and no direct-DSN verb, can grant or revoke it after create.
  `users.User.email` rides `UserRow.email`: lowercased by
  `users.NormalizeEmail`, empty when none, unique among active users
  (`ErrEmailTaken`).
- **`MintToken` / `ListTokens` / `RevokeToken`** manage a user's credentials
  and are deliberately NOT admin-gated: a user manages its OWN. The target
  resolves through `resolveTokenTarget` — the caller by default; another
  username requires admin authority; on the anonymous local socket a username
  is REQUIRED (`--user is required on an unauthenticated socket`), since
  there is no caller to default to. `MintToken` mints an `origin "service"`
  token (`ttl`, a `Duration`; unset or zero = never expires, negative refused)
  whose plaintext rides the response
  exactly once; `ListTokens` returns metadata rows only, `include_revoked`
  adding tombstoned credentials (revocation is a filter, never a deletion);
  `RevokeToken` stamps `revoked_at` — an unknown id answers `CodeNotFound`,
  an already-revoked id the unchanged row, and the ownership check needs the
  row (`GetToken` first), so a foreign id answers `CodePermissionDenied` and
  a missing one `CodeNotFound` — a token id is a random UUID and the two
  leak nothing. A `TokenRow` carries `created_at`, and `expires_at`/`revoked_at`
  as `Timestamp`s (unset = never expires / not revoked).
- **`ListUsers`** returns rows WITHOUT tokens, `include_deleted` adding
  tombstoned rows. `limit` collapses to 500 whenever it is zero, negative or
  above 500 — one clamp, no default-vs-ceiling distinction (unlike
  `ListExecutors`, which defaults `<= 0` to 50 and only clamps `> 500`; two
  neighbouring `*_list` verbs, two different conventions — do not infer one
  from the other).
- **`RemoveUser`** tombstones the row: the token stops authenticating, but
  history keeps resolving the username. An unknown or already-tombstoned
  name is `CodeNotFound`. Deleting the last active user is what re-arms the
  zero-users state above.

All three answer `unavailable` (`no_agent_db` as the `ErrorInfo` reason) when
the daemon has no database pool — there is no in-memory fallback for
identity.

### The Login service (`rafiki.v1.Login`: `BeginLogin`, `CompleteLogin`)

A separate Connect service on the same proto file, served OUTSIDE
authentication on BOTH mounts: on the proxy face `server.Handler.Login`
(`pkg/server/handler.go`, registered by `Mount` WITHOUT the caller's auth
wrap — every other face is wrapped by middleware that 401s exactly the caller
Login exists to serve), and on the unix socket the login route
(`cmd/rafikid/connect_uds.go`) with NO interceptors at all — deliberately not
through `connectControlRoute`, whose policy gate fail-closes to `userOnly`
and would refuse exactly the credential-less caller. It is therefore not in
`connectPolicyTable`: that gate scopes `/rafiki.v1.Control/` only, and Login
is a different service whose whole job is minting a first or replacement
credential — an unknown bearer on `/rafiki.v1.Login/` reaches `BeginLogin`,
never `Unauthenticated` (pinned by
`TestServeConnectUDSLoginReachesBeginLoginWithUnknownBearer`, `cmd/rafikid`).
The handlers (`pkg/connectapi/login.go`) never read caller identity; the
engine is wired post-construction via `Server.SetLoginBackend` (a nil backend
is refused, the same rule as every `Set*` setter), and an unwired engine
answers `CodeFailedPrecondition` ("OIDC login is not configured on this
daemon") — the one state an unauthenticated caller can act on, so it must be
legible. `loginErr` passes an already-coded error through and redacts
everything else to `CodeInternal`: Login answers callers with NO valid
credential, so anything naming the IdP or database stays off the wire.

`BeginLogin` takes the loopback port the client bound and a `client_host` for
the token name, and returns `login_id` plus the `authorize_url`. The
response's `redirect_port` names the port the IdP redirects to — the client's
own unless the daemon PINS one (`oidc.toml`'s `redirect_port`), in which case
the client must rebind there. `CompleteLogin` takes the `login_id` and the
raw callback query string the IdP redirected with, and returns the minted
token (`origin "oidc"`, the config's `session_ttl` as its TTL, named
`"login <client_host> <date>"`), the username, and `expires_at` (a
`Timestamp`; unset = the token never expires). A login is
SINGLE-USE: the pending entry is removed first, success or failure.

The pending-login surface is bounded by the engine's own guards
(`pkg/oidclogin/service.go`): **1024 global pending slots** and a **10-minute
TTL** per pending login — a pending login is unauthenticated surface holding
a live PKCE verifier, so the TTL is short on purpose. The next `Begin`
after the TTL sweeps every expired entry; with all 1024 slots held, every
`Begin` answers resource-exhausted and ALL logins are deferred until slots
free — an accepted limitation, not a per-caller queue.

OIDC identity resolution: the verified login's claims resolve through
`usersdb.ResolveOIDC` — an identity binds once by email (the first login at
an issuer looks the active user up by the verified email and inserts the
binding; no active user for that email is `ErrOIDCNoUser`) and resolves by
`(issuer, subject)` afterward — the claim's email is never consulted once a
binding exists, so an email change at the IdP cannot re-point it. Lookups
are scoped to the configured issuer: an identity is only valid at the IdP
that issued it. `ErrOIDCConflict` (the email is bound to a different subject
at this issuer) is an answer, not an outage.

### Token revocation and open streams

Revocation through the daemon's own paths is a security act and CUTS:
`RevokeToken` (`cmd/rafikid/connect_users.go`) and `Controller.UserRm` end
every server stream the revoked token or user held open, through the
per-daemon stream registry (`cmd/rafikid/stream_revoke.go`), keyed by the
caller's `user_token` id and wired behind the policy gate on both Control
mounts — a refused call never registers. The same revocations purge the
auth cache (`UserTokenAuth.ForgetToken`/`ForgetUser`, called before the
cut), so the revoked credential's NEXT request is refused immediately
rather than within the cache TTL. Every Control server-streaming handler
returns on context cancellation, so a revoked stream ends as `Canceled`,
never a bare EOF. `UserRm` also disconnects every live executor whose
`owner_user_id` matches (`execpool.Pool.DisconnectOwner`; an empty id is a
no-op, so unowned executors are never mass-disconnected).

What is NOT cut: revocation from the HOST CLI (`rafikid user token revoke`,
pure database) cannot reach a running daemon's in-memory registry or its
auth cache — a documented limitation, not a deliberate choice (the host CLI
has no channel to the daemon) — so on such a token open streams run on and
new requests stop only within the ≤5s auth-cache window
(`server.DefaultAuthCacheTTL`); use `rafiki token revoke` against the daemon
for an immediate cut. Token EXPIRY never cuts a stream, deliberately —
expiry is a property of time, not an act; the expired token's next request
re-checks the store and is refused.

## 2.4 MCP agent-control surface (HTTP)

A third face on the proxy listener: MCP streamable-HTTP at **`/mcp`**
(`mcpFacePath`, `cmd/rafikid/mcp_face.go`) — POST for requests, plus GET for
the standalone SSE stream and DELETE to close the session — served by
`github.com/modelcontextprotocol/go-sdk` v1.7.0's `NewStreamableHTTPHandler`.
It exposes rafiki's
agent-control verbs as MCP tools, so a client whose runtime already speaks MCP
can spawn, steer and observe daemon agents without the CLI. The Connect plane
(§2.3) is the same daemon's control plane, and what `rafiki attach` and the
existing CLI speak.

**Transport.** The server→client leg is SSE, which is why the handler needs an
`http.Flusher` on its `ResponseWriter`: the SDK flushes keep-alive headers and
event boundaries through `http.NewResponseController` and treats a failed
flush as best-effort, so a writer that cannot flush loses the leg silently.
The shared TLS listener advertises `http/1.1` only in ALPN
(`execpool.ALPNProtocols`, §2.2), because net/http can hijack HTTP/1.1 and not
HTTP/2; streamable HTTP works over it, needing neither an Upgrade nor h2 —
only the flush.

**Where it is mounted.** Inside the proxy face, via `pkg/server.Handler`'s
`MCPPath`/`MCP` and `h.Mount` — the same tree as `/v1/messages` and the Connect
routes, under the same `UserTokenAuth` wrap. It is deliberately NOT mounted in
`main.go`: a `mux.Handle` there would compile, look right, and shadow the
face's authentication, because ServeMux prefers the longer pattern — the trap
§2.3 records for `/rafiki.v1.Control/`. `main.go` serves the whole proxy-face
mux at `"/"` on the TLS listener and wires the Controller in afterwards
(`face.MCP.SetController`); until that lands, `getServer` answers nil and
`Routes` turns it into **503 with `Retry-After: 1`** — a genuinely transient
condition a client can back off and recover from, unlike a bare 400 from the
SDK, which tells an MCP client nothing it can recover from. Once the
Controller is bound, an unentitled identity gets **403** (below).

**Auth.** A rafiki user token as a bearer credential (`Authorization: Bearer`,
`x-api-key` or `X-Rafiki-Token`) — the same credential the Connect plane
takes, resolved by the same `UserTokenAuth.Middleware`; there is no second
credential path. An invalid credential is 401; an unreachable identity store
is 503, never 401, because a 401 tells clients their credential is bad and
they respond by discarding it — a database blip answered with 401 logs
everyone out at once. The identity reaches the face on the request context,
never by re-reading the `Authorization` header, which would be a second
credential path. The daemon's **per-boot child token** also authenticates, and
the credential's provenance (`server.Identity.Via`, stamped by
`UserTokenAuth.resolve`) decides what it may reach. On its own it resolves to
`ProvenanceUnknown` and gets **403**; with `X-Rafiki-Session` it
resolves through the proxy face's pre-existing child-owner attribution — the
path that makes its LLM turns bill to its owner — to
`ProvenanceChildAttributed`, an identity carrying the owner's user id but NOT
the owner's powers: every agent-control surface refuses it with 403. Two
provenances are entitled to the surface: `ProvenanceUser`, a real user token
(passing `server.Identity.IsUserCredential`), and `ProvenanceChildToken`, a
per-child secret minted at spawn by the Controller (`mintMCPToken`, delivered
as the child's `RAFIKI_MCP_TOKEN`) and resolved through `UserTokenAuth`'s
`ChildTokenLookup` to `Identity{UserID: owner, ChildID: child, Via:
ProvenanceChildToken}` — never cached, dying with the child. The owner is
the subtree's: every child is stamped at spawn with it (a user credential's
spawn carries the id; the controller spawner hands its own row's id down),
and `OwnerUserIDForChild` walks the parent chain only for rows written
before that inheritance landed — resume preserves a stored row without
backfilling it — while a lineage with no owner anywhere (an anonymous
local-socket spawn) refuses. Any other
provenance gets **403**: the credential authenticated and is not entitled to
agent control — a status an MCP client can read, never a server that would
render the refusal as an empty tool list. Every non-`anyCaller` Connect verb
refuses a child credential the same way through the policy interceptor's
table (see the Control plane's "Who may call what"), and on the `childScoped`
verbs a per-child credential is bounded to its subtree instead of refused —
never given the operator path. Attribution and authority are separate:
`/v1/messages`
billing, raw-trace capture and quota attribution consume the UserID and are
deliberately unchanged, and a user token that also carries `X-Rafiki-Session`
stays `ProvenanceUser` — provenance is a property of the credential, not the
header — so hand-configured clients keep working. Each MCP session is bound
to the full principal that initialized it — user id AND child id, with an
empty child id denoting the interactive user — and a
later request presenting a session id owned by another caller — or an unknown
one — is refused with 403 before dispatch. Keying on the user id alone would
be a sibling-escalation path once children hold credentials: two children of
one owner share a user id, so child B presenting child A's session id would
execute against A's bound spawner. The face keeps this map itself
because the SDK's own session-hijack guard keys on the bearer middleware
rafiki does not run, so without it the session id alone would route one caller
onto another caller's bound tools.

**Scope.** Two caller shapes, keyed on the credential's provenance:

- **User credential** — every call acts as the authenticated user with no
  parent — the same shape as `rafiki create` from the CLI: a top-level child
  owned by that user. The tools are served by a user-bound
  `tools.AgentSpawner` (`newUserSpawner`, `cmd/rafikid/user_spawner.go`) that
  closes over the identity at construction; no method on it takes a user id, a
  username or an `*http.Request` — an identity in a method parameter is one
  refactor away from being a tool argument the model can be prompt-injected
  into naming, the same rule the fundi-side binding enforces. There is **no
  per-user scoping of anything**: `agent_list` and the steering verbs
  (`agent_view`, `agent_send`, `agent_kill`, `agent_route`) see **every child
  on the daemon**,
  not only the caller's, because rafiki has no per-user ownership filter and
  this surface does not invent one. That is the daemon's current
  single-operator posture, stated as what it is — not a guard.
- **Child credential** (`ProvenanceChildToken`) — the caller is an agent IN the
  forest: its spawns are parented under it (`ParentChildID` = its own child id)
  by the same `controllerSpawner` fundi children use
  (`newControllerSpawner(ctrl, id.ChildID)`), so spawn budgets descend and
  every verb authorizes against the caller's position in the tree via
  `controllerSpawner.authorize` — a child sees and drives only its own
  subtree, never a sibling's and never the whole daemon, no matter what its
  prompt is injected into asking for. The task ledger is keyed by the caller's
  OWNER (the conversation the `/v1/messages` attribution path already
  resolves), so a child shares its owner's ledger, exactly as a fundi child
  does. The rest of the child's
  tool set is narrowed to the same boundary (review-0's F1 finding): `conversation_*` read
  the child's own SUBTREE — its conversation plus its descendants' — through
  `insights.ScopeSubtree`, never its owner's corpus; the recall and memory
  tools are bound to the child's OWNER's non-admin identity (`recallOwner`,
  `cmd/rafikid/recall.go`); `agent_route` steers only the child's own subtree
  and only `prefer`/`sort`/`quant` — `only=` (which bypasses provider bans) is
  refused to a child by `Controller.SetChildRouting`; and preset
  authoring refuses at call time for a PARENTED child (`childPresetBinding`,
  chosen by `presetStoreForChild`) — a top-level child is the operator's own
  session and authors under its owner, stamped as the writer, while an
  agent-spawned child reads the presets it may spawn with but can never
  shadow what its owner's next spawn resolves. Pymodule authoring is open to
  every child: a child that can already write and run scripts in its
  workspace gains nothing dangerous from saving one.

`agent_set_budget` remains an exception for the user caller: **top-level
children only**. Any parented child is refused — a parented child's budget
belongs to the agent that spawned it, and reaching into another agent's
subtree to re-budget its worker is a different act from operating your own
fleet. The refusal names the fix: change the budget through that agent, or set
the budget of the top-level agent that owns the subtree. A child caller's own
spawns are already parented, so the same refusal reaches it from the other
side.

**Tools.** Thirty-two, materialized per caller: twenty-nine come from the same
registered blueprints the fundi registry serves (`DefaultBlueprint` — a fundi
child gets the same twenty-nine, subject to the same per-caller declines;
`pymodule_start` is one of them, which a fundi child with a spawner
materializes from its own registry), and three (`pymodule_list`,
`pymodule_run`, and the face's own `pymodule_start` wrapper over that registry
blueprint) from face-local blueprints fundi never sees — all assembled in
`mcpBlueprints`; descriptions
are reworded on this surface for a caller that is not a fundi child
(`mcpToolDescriptions`). The four pymodule tools decline together when the
daemon has no executor pool (`claudeExecutorRouted`); `pymodule_run` and
`pymodule_start` decline further unless the caller is a child with a live
executor binding (the pymodule family's delegate verbs — run one of the
caller's modules, start a saved script as a managed child — appear exactly
where the caller demonstrably operates in the executor world that hosts
them; the interactive human has `rafiki create` and `rafiki py`). The four
preset tools decline together when the daemon has no database — no preset
store, so nothing to read or write. A tool failure
is `CallToolResult.IsError = true` carrying the diagnostic — never a JSON-RPC
transport error, and never a successful result carrying the text.

| Tool | Purpose |
|---|---|
| `agent_spawn` | Start a top-level rafiki agent — daemon-managed, cross-process, budget/depth/executor-constrained; reworded to assert preference over a client's own native subagent tool (an earlier wording deferred to it for "lightweight" work, which Claude Code read as the whole rule and the surface was never used) |
| `agent_list` | Every agent the daemon knows: id, name, model, current status, working directory, assigned task handle |
| `agent_view` | Recent transcript of one agent — prompts, replies, tool calls with their results; a deliberate check, not a polling loop |
| `agent_send` | Deliver a prompt to a running agent |
| `agent_kill` | Shut an agent down and wait for the exit to be recorded |
| `agent_set_budget` | Change a TOP-LEVEL agent's USD budget (the exception above) |
| `agent_route` | Change where a child's model requests are served, on its next request: a bracket-free spec delta (`prefer=`, `sort=`, `quant=`) merged over the child's stored routing. A child caller steers only its own subtree and may not set `only=`; a user caller is an operator and may route any child at any depth |
| `agent_models` | Query the daemon's model catalog with filter/sort; a bare call returns a summary, rows come from a narrowed query |
| `task_add` | Add a task (imperative `content`, present-continuous `active_form`; optional `parent` handle to nest); returns the full list with handles |
| `task_update` | Change task statuses (pending, in_progress, blocked, completed, failed), touching only the named handles |
| `task_drop` | Abandon a task with a required `reason`; drops its subtasks |
| `task_list` | Read the ledger; filter by status, metadata or assignee; dropped rows hidden unless `include_dropped` |
| `quota_status` | The caller's own captured Anthropic subscription rate-limit snapshot; omitted when the daemon has no quota capture (a Materializer decline) |
| `conversation_search` | Search the CALLER's own past conversations by time, model, source, status or first-message substring; summaries with turn/token/cost figures. An admin caller reads the whole daemon's; a child caller reads its own subtree's; otherwise scoped to the caller's owner. Errors (`ErrNoPool`) at call time rather than declining when the daemon has no database |
| `conversation_export` | Read one conversation's full transcript (per-turn metrics, skills invoked), found by `conversation_search`; a conversation outside the caller's scope answers not-found, never a permission error |
| `conversation_query` | Run one named catalogue query (`tools`, `skills`, `classes`, `models`, `sizes`, `coverage`) over the caller's conversation history: typed columns rendered as tab-separated text. `tools` groups tool names case-insensitively and sorts by calls descending. Scoped like `conversation_search`; errors (`ErrNoPool`) at call time rather than declining when the daemon has no database |
| `pymodule_put` | Save a reusable Python module (name, source, one-line description) to the caller's own pymodule store — the required `repo` argument must be `"local"` (the blob store's sentinel; git sources are read-only through this tool, `CodeInvalidArgument` otherwise); saving again under an existing name is a new version, never an overwrite. A module that does not parse as Python is refused outright; advisory findings — `ruff` lint output, failed dependency-venv installs on the owner's executors — ride the result text as a notice and never block the save. A child caller writes its owner's bucket, like a fundi child's `pymodule_put` |
| `pymodule_get` | Fetch one of the caller's saved pymodules by name — full source plus description, version and creation time; the required `repo` argument must be `"local"` (git sources are read-only through this tool); review before `pymodule_run`, or read-modify-write: fetch, edit, save as a new version with `pymodule_put` |
| `pymodule_delete` | Soft-delete every saved version of one of the caller's pymodules by name; the required `repo` argument must be `"local"` (git sources are read-only through this tool); failed dependency-venv cleanups on the owner's executors ride the result text as a notice. |
| `pymodule_list` | List the caller's pymodules (name + one-line description) across all scopes — the blob store plus every registered git source's discovered scripts and packages, git rows labeled `reponame/name`; an optional `repo` argument narrows to one scope (`"local"` or a git source's name — the one place an empty value legitimately means "everything"). MCP-face-only — fundi renders the same inventory as a dynamic skill instead |
| `pymodule_run` | Run one of the caller's pymodules by name — the required `repo` argument selects the source: `"local"` runs a saved module out of the executor's synced pymodule cache, a git source's name runs its discovered script out of that source's synced checkout (`scripts/<script>.py`, the checkout root on `PYTHONPATH` so intra-repo imports resolve, the repo's shared `.venv` interpreter when present) — never a workspace file, with the child's workspace as the process cwd (or the call's own `cwd`); a thin proxy to the executor's own `pymodule_run`. Present only for a child with a live executor binding, absent otherwise; the interactive human never gets it. MCP-face-only blueprint — fundi's own `pymodule_run` routes through its tiered tool-routing instead |
| `pymodule_start` | Start a saved Python script as a SCRIPT child — a daemon-managed process that runs to completion with no model, no API key and no turn loop; returns the child id at once. The spec (`repo`, `script`, `modules`, `args`) rides the ordinary `Spawn` path — kind `script`, every fundi/claude-only field refused by the daemon — plus the child furniture: `labels`, `max_cost`, `max_children`, `executor`, an optional `cwd` (omit it to inherit the calling child's own cwd — the controller spawner substitutes it). Gated exactly like `pymodule_run` (a child with a live executor binding; the interactive human uses `rafiki create --kind script`); the child's name defaults to the script's name, and the child materializes the SPAWNING OWNER's pymodule rows, so a module another user saved is not yours to start. Registry blueprint — this face tool is the wrapper; a fundi child materializes the same blueprint from its own registry whenever it has a spawner |
| `preset_list` | List the presets the caller can spawn agents with — one row per preset with name, kind, model and description; an optional `prefix` narrows to one group (e.g. `default:`). Declined, with the other three, when the daemon has no database |
| `preset_get` | Read one preset — version stamp and full spec (kind, model, tools, prompts, budget) as JSON; `history` returns every past version, deleted ones included. What a spawn with that preset will get |
| `preset_put` | Create a preset or save a new version of one — each save is a new version, nothing already saved is ever overwritten. The spec's fields fix what `agent_spawn`'s `preset` gives a spawned worker. Only the operator authors presets: a user credential or a TOP-LEVEL child (stamped as the writer); a parented child is refused, since it would shadow — latest-live-wins — what the operator's next spawn resolves |
| `preset_delete` | Delete one preset by name — every live version is stamped deleted and the history is kept; a later `preset_put` under the same name starts a new version line |
| `recall` | Search your past conversations AND your saved memories by keyword and meaning — one line per hit (`m:` memory, `s:` summary, `w:` window), never full text. Conversation results cover what your credential can see (an admin reads the whole daemon); memories are always your own. Declined, with the five below, when the daemon has no recall store. A child caller gets the same six tools bound to its OWNER's non-admin identity (`recallOwner`): conversation results cover the owner's rows, never daemon-wide even for an admin owner, and memories are the owner's |
| `recall_context` | Expand one recall hit: a window returns the surrounding messages (tool results collapsed to size markers), a summary its full text and conversation id, a memory its full body |
| `memory_put` | Save or replace one of YOUR memories at `path/name` — tombstone-replace, one live version |
| `memory_get` | Fetch one of your saved memories — full body, metadata, timestamps; a missing path/name is a tool error |
| `memory_tree` | Fetch every memory under a path, full bodies; when the subtree exceeds the output budget it degrades to paths plus first lines and asks you to narrow |
| `memory_delete` | Remove one of your memories from recall results — tombstoned, not destroyed |

The time filters on `conversation_search`, `conversation_query` and `recall`
are **RFC3339 strings** (`since`/`until`) — the fundi agent tools bind the same
schema, and the handler parses the string into the `Timestamp` the Connect
verb carries (`rfc3339Time`, `pkg/fundi/tools`). A blank or absent value means
unbounded, and a time that will not parse is a tool error.

The `task_*` descriptions are likewise reworded: the ledger is shared, durable
and cross-agent — not the client's private per-session checklist (the native
checklist keeps that job), and the description now pairs the ledger with
delegation: add the task, pass its handle to `agent_spawn`.

This surface competes with a client's own native subagent machinery — Claude
Code's built-in `Task` tool especially — and tool descriptions alone lose that
fight, because the client's system prompt features its own tool far more
heavily than any description can. Two channels carry the preference therefore:
these descriptions (for any MCP client), and for daemon-spawned `--kind claude`
children a **coordination prompt** (`claudeargv.CoordinationPrompt`) merged
into the child's ONE `--append-system-prompt-file` — same preference, delivered
where the client's own tool guidance lives. It is injected only when the child
carries the MCP surface (the `--mcp-config=<path>` injection above; on the
daraja path, iff `ProxyUrl` is set), and it shares the one file with any caller
text (the flag is last-wins; two elements would drop one text). The
system-prompt appendix and the MCP config ride staged files on every claude
launch path, including the executor's `rafiki daraja serve` host
process (`pkg/executor/admin.go` stages the appendix and passes
`--append-system-prompt-file <path>`, and `cmd/rafiki/cmd_daraja.go` reads it
back into the child spec; the host restages the same content-addressed file for
the claude grandchild). Interactive `rafiki claude` sessions get neither the
prompt nor the gate (a human drives those).

**The task ledger.** The `task_*` tools scope by conversation id, and that
column is a UUID, so a per-user ledger cannot be a synthetic string. Each user
therefore gets a real, **turn-less** conversation row keyed by
`external_ref = "mcp:user:<user-id>"` with `origin_entrypoint = "mcp"` and
`driven_by = "client"`, resolved by
`CaptureStore.EnsureConversationByExternalRef` against the existing
`(external_ref, driven_by)` partial unique index — no migration. The row's
UUID is the ledger key the `task_*` tools scope by, and keying on the user id
means a future multi-user rafiki inherits per-user ledgers with no further
work. It appears in `conversations.v_conversation` and the insights surfaces
as a conversation carrying no turns, which is correct — the tasks in it are
real work. On a DB-less daemon the ledger degrades to an in-memory store keyed
`user:<id>` and is lost on restart, the same degradation a pool-less agent
already documents.

**Settlement notification (`notifications/message`).** On settlement, the
daemon fans out the same fragment a settling child's parent would receive
(`settleFragment`, `cmd/rafikid/subagent_events.go`) as an MCP logging message
— `notifications/message`, sent through `ServerSession.Log` with level `info`
and logger `rafiki` — to every live MCP session owned by the settling child's
user. The owner is read from stored state (`childstore.Snapshot.OwnerUserID`,
the `conversations.child.owner_user_id` column, stamped at spawn by the
user-bound spawner); an unattributed child fans out to nobody. The fan-out
runs before the parent gate in `notifySubagentSettled`, because every
MCP-spawned child is top-level and the gate would otherwise return before
anything reached the caller that spawned the agent.

**The caller's own kill excludes itself.** An `agent_kill` issued from an MCP
session marks the child with that caller's user (`userSpawner.Kill`,
`killMark{mcpUser}`), and `notifyMCPSettled` drops the fan-out when the
settling child's owner is that same user — the tool result already said the
agent stopped, and the fragment would say it again. The exclusion is per-USER
(the fan-out's own granularity, every session of the owner), applies only
when killer and owner match (killing another user's agent silences nothing),
and never touches the parent-facing half of the guard: an MCP caller can kill
somebody else's worker, and that worker's coordinator — which did not act —
still receives the fragment. The coordinator-facing half
(`controllerSpawner.Kill`, `killMark{parent}`) suppresses the parent's
event-buffer fragment instead, and only when the death was the kill's own
doing (`exitCausedByShutdown`): a daraja-hosted claude child exits 143 from
the SIGTERM the shutdown ladder sent — that is the kill — while a fundi
panic's exit code 2 or a foreign signal landing during the passive stdin
close wait is news even to the killer, and notifies.

The push is **best-effort by construction**: the SDK's `Log` returns nil
without writing anything when the client has never issued `logging/setLevel`
(`mcp/server.go` reads `ss.state.LogLevel`, empty until then), and rafiki's
face is stateful, so nothing presets a level — a delivered notification and a
dropped one are indistinguishable, and a session that fails to receive is
logged at debug and skipped. **`agent_list`'s status field is the reliable
answer**; the notification is an enhancement for a client that has set a
level, never something to wait on. Sessions are in-memory and per-process:
none survive a daemon restart, a reconnecting client re-initializes, and the
registry (`mcpSessions`, `cmd/rafikid/mcp_notify.go`) holds each user's live
sessions, snapping them out under a lock released before any send so one
stalled client cannot wedge another user's fan-out. **A session registers on
its first tool call, and — for a legacy-handshake client — when it sends
`notifications/initialized`**: the face wires the SDK's `InitializedHandler`
through the bridge's `ServerOptions` escape hatch and the bridge's
`RegisterSession` hook through `mcpserver.Options` (both stay identity-free —
the owner rides the face's closure, never a bridge signature). The second
registration point exists because a go-sdk v1.7.0+ client negotiates through
the SEP-2575 `server/discover` probe and never sends
`notifications/initialized`, so the initialized hook alone no longer covers
every client; a tool call is the earliest hook both handshakes share, and a
session that never calls a tool can never produce a settlement to receive. A
client that does neither is never registered. Removal rides a per-session
`Wait` goroutine because the SDK has no session-closed hook. The face's
`Mcp-Session-Id` map and this pointer registry are two keyed worlds that
never meet; the cost is one-directional — a DELETE unbinds the sid
immediately while the pointer lingers until Wait returns, and a Log to a
closed session fails at debug and is skipped.

On the proxy face (the `/v1/messages` capture path, not the MCP surface above),
a client-driven conversation's `external_ref` is the `X-Rafiki-Session` value,
optionally suffixed `:<threadID>`. Claude Code runs several conversational
threads in one process (the main thread, each Task subagent, the titler) under
one session header; each non-root thread gets its own conversation row so it
gets its own ordinal space. Which thread a request belongs to is decided from
`diagnostics.previous_message_id` and the request's `cc_is_subagent` billing
flag: a turn whose predecessor is the main thread keeps the bare session value
(that convention is `thread_id IS NULL` on the turn row), and a turn of a
subagent thread resolves to the branch named after the turn that founded the
thread. A turn with no resolvable predecessor is a thread root: when the
session's family of conversations does not exist yet it is the main thread's
founding turn (bare session value, `thread_id IS NULL`); when the family
exists AND the request carries `cc_is_subagent` it is an independent thread
(a Task subagent), and the proxy pre-mints that turn's id and routes the
request to the branch named after it from its first request, so a founding
request never shares the main thread's ordinal space. A predecessor-less
auxiliary request that does NOT carry the flag (the titler and the quota
probe in measured traffic) keeps the pre-routing behavior: it lands on the
root row and its response append fails loudly if it collides. The `:<threadID>` suffix namespace is daemon-reserved: a
caller that puts `:<id>` inside its own `X-Rafiki-Session` value collides with
branch refs of the session it names.

## 3. Identifiers

### `childId` — resource identity

Opaque ULID-shaped string assigned by the controller when a child is
spawned. Stable for the lifetime of the persistent record — it survives
`Resume`, so the same `childId` spans multiple underlying child processes.
Never reused.

**Always controller-assigned**. Clients cannot supply a desired `childId` on
spawn. The only RPCs that *take* a `childId` are those acting on an
existing resource: `Kill`, `Close`, `Send`, `GetHistory`, `StreamEvents`,
`Resume`, `SetLabels`, `GetStreams`, `SendFrame`. For these, the `childId`
references something the controller has already minted.

If clients want a stable user-visible identifier, that's what `name` is for.

## 4. Errors

Every failure is a Connect error: a Connect code plus, when the daemon
authored it, a precise **reason** riding as a `google.rpc.ErrorInfo` detail.

**Codes.** `connectapi.ConnectErr` maps every daemon error through
`errCodeTable` (`pkg/connectapi/errmap.go`): the code the daemon attached at
the source IS the classification, and Connect codes are coarser than
rafiki's reasons, so the precise reason also rides the error.

| Reason (`ErrorInfo`, Domain `rafiki`) | Connect code | Meaning |
|---|---|---|
| `child_not_found` | `not_found` | No child with the given `child_id` exists. |
| `child_exited` | `failed_precondition` | Child has exited; only reads, `Resume` and `Close` apply. |
| `child_in_grace` | `failed_precondition` | Equivalent state to `child_exited`; explicit for clarity in errors. |
| `child_shutting_down` | `failed_precondition` | Child is mid-graceful-shutdown; stdin is closed. Sends rejected. |
| `not_resumable` | `failed_precondition` | Child is not in `exited` status; cannot resume. |
| `not_exited` | `failed_precondition` | `Close` against a still-live child. |
| `session_file_missing` | `not_found` | `Resume` cannot find the recorded session. |
| `backpressure` | `resource_exhausted` | The child's command channel is full; client should retry. |
| `at_capacity` | `resource_exhausted` | A spawn refused because the parent's subtree is at its live-children cap (default 10); transient — retry once a child settles. The permanent limit refusals (depth, a zero cap) stay `invalid_args`. |
| `invalid_args` | `invalid_argument` | Request fields failed validation. |
| `spawn_failed` | `internal` | The child subprocess failed to start or exited immediately. |
| `auth_required` / `auth_invalid` | `unauthenticated` | The presented credential names no active user; identity failures are `unauthenticated`, never a silent downgrade. |
| `not_found` | `not_found` | Generic; e.g. `Resume` against an unknown id. |
| `permission_denied` | `permission_denied` | The authenticated caller addressed a resource that belongs to a different user — executor ownership scoping (`LabelExecutor`/`DisableExecutor`/`EnableExecutor`/`DeleteExecutor` against another user's executor). A MISSING row is `not_found`, never this; this code says "it exists and is not yours". |
| `internal` | `internal` | Unexpected daemon-side error. |
| `no_agent_db` | `unavailable` | The conversation/recall/pymodule backend is unwired (`RAFIKI_DB` unset). |
| `payload_too_large` | `invalid_argument` | A transcript or report exceeds its size cap. |
| `protocol_mismatch` | `failed_precondition` | A Connect request's `Rafiki-Protocol` header is missing or names a different epoch (`pkg/server.RequireEpoch`). Attached by the epoch gate, not the domain error table — see "The protocol epoch" above. |

**Reasons ride `google.rpc.ErrorInfo`** (`pkg/rpcreason`): `Attach` puts the
reason on a Connect error, `Reason` reads it back; the client reads the
reason, never parses message text. A `*connectapi.ControllerError` keeps its
authored message under its protocol code and carries the code as the detail.

**Redaction.** Any error this codebase did not author is `CodeInternal` with
the fixed text `internal error` — its raw text is never forwarded and no
detail is attached, because an unredacted dependency error names
infrastructure the caller has no business learning from a failed request (a
pgx failure names the database host, user and database). The handlers
practise **log-then-redact**: the cause is logged at the call site first, or
it is lost. `ConnectErr` itself does not log.

**Refusals name the credential, never the secret**: the provenance gate's
`CodePermissionDenied` names the procedure and the credential kind; the user
admin gate's names the missing authority; an unknown bearer token on the TLS
listener is the fixed "invalid auth token".

**Startup window.** A seam whose backend is not yet wired (the Controller is
built after the Connect server) fails closed with `CodeUnavailable` naming
the seam ("… not yet wired") — never a panic, never a hang.

**`Unimplemented`** is a wire fact, not an infrastructure failure: a
deliberately unimplemented RPC answers `CodeUnimplemented` directly, unmapped.

## 5. Multi-client semantics

- **Sends.** Any authorized caller may `Send` to any child in its scope.
  Messages queue at the child's inbox in arrival order at the daemon. No
  exclusive-control mechanism.
- **Subscribes.** Any number of independent `StreamEvents` subscribers per
  child. Each receives events in publish order. A slow subscriber may drop
  events (per-subscriber bounded channel) but never backpressures the child
  or other subscribers; the cursor replay covers the gap.
- **Extension UI.** A child's `extension_ui_request` events are forwarded to
  all matching subscribers. The first client to answer (via `SendFrame` for a
  child that speaks a frame protocol, or the child's own tool surface) wins;
  the runtime drops subsequent responses by id mismatch.

## 6. State machine

Nine states. Transitions are driven by the child runtime's published events
and the daemon's lifecycle actions.

```
              ┌──────────┐
              │ spawning │
              └────┬─────┘
                   │ first event/status from the child runtime
                   ▼
              ┌──────────┐    agent_start           ┌────────────┐
              │   idle   │ ────────────────────────► │ streaming  │
              └──────────┘ ◄────────────────────────  └─────┬──────┘
                   ▲       agent_end                        │
                   │                                        │ tool_execution_start
                   │                                        │ (activeTools 0→1)
                   │                                        ▼
                   │                                  ┌──────────────┐
                   │                                  │ tool_running │
                   │                                  └─────┬────────┘
                   │                                        │ tool_execution_end
                   │                                        │ (activeTools 1→0)
                   │                                        │
  modal (stack):                                           ▼
    compacting ─── on compaction_start, push; on compaction_end, pop.
    batch_wait ─── on batch_wait_start, push; on batch_wait_end, pop.
    blocked_ui ─── on extension_ui_request (dialog method), push;
                   on matching response, pop.

              shutting_down ──── on Kill or daemon shutdown. Closes stdin,
                                 then waits, escalates SIGTERM/SIGKILL.
                                 → exited on reap.

              exited ──── terminal except for Resume → spawning.
```

### 6.1 Transition table

| From                  | Trigger                                                     | To              |
|-----------------------|-------------------------------------------------------------|-----------------|
| (none)                | `Spawn` or `Resume`                                          | `spawning`      |
| `spawning`            | First event/status received from the child runtime           | `idle`          |
| `spawning`            | The child process exits before any event                     | `exited`        |
| `spawning`            | 5s elapsed without a first event (warning only)              | `spawning`      |
| `idle`                | `agent_start`                                               | `streaming`     |
| `streaming`           | `tool_execution_start`, activeTools 0→1                      | `tool_running`  |
| `tool_running`        | `tool_execution_start` (parallel), activeTools++             | `tool_running`  |
| `tool_running`        | `tool_execution_end`, activeTools 1→0                        | `streaming`     |
| `streaming`           | `agent_end`                                                  | `idle`          |
| `streaming` or `tool_running` or `idle` | `compaction_start` (push)                 | `compacting`    |
| `compacting`          | `compaction_end` (pop)                                       | previous state  |
| `streaming` or `tool_running` or `idle` | `batch_wait_start` (push)                 | `batch_wait`    |
| `batch_wait`          | `batch_wait_end` (pop)                                       | previous state  |
| `streaming` or `tool_running` or `idle` | `extension_ui_request` (dialog only) (push)| `blocked_ui`    |
| `blocked_ui`          | matching response (pop)                                      | previous state  |
| any except `exited` and `shutting_down` | `Kill` or daemon shutdown starts           | `shutting_down` |
| `shutting_down`       | the child process exits (reap)                               | `exited`        |
| `shutting_down`       | `Send` to the child                                          | rejected (`child_shutting_down`) |
| any non-exited        | the child process exits unexpectedly                         | `exited`        |
| `exited`              | `Resume`                                                     | `spawning`      |

Every transition is published as an `AgentStatus` event, carrying both the
new and previous state — that is what `GetChild`/`ListChildren` report and
what `StreamEvents` delivers.

**Script children bypass this table by design.** A `kind: script` child
enters `running` at spawn (skipping `spawning` — the table's first row is
the turn-based kinds' path), never passes through the turn states, and
leaves only at exit (`running → shutting_down → exited` on a kill, `running
→ exited` on its own exit). No output event is a transition trigger; the
state machine is configured for the kind before the child's stdout is read
(`pkg/child.StateMachine.ForScript`).

### Informational events (no transition)

`extension_error` (counts), `auto_retry_start`/`auto_retry_end` (retry
counters, last error), and `queue_update` (pending steer/follow-up counts)
update per-child counters without a status change; they are visible through
`GetChild`/`ListChildren` and the event stream.

### The modal stack

`compacting`, `batch_wait` and `blocked_ui` are *modal* states implemented
with a small state stack: push on entry, pop on the matching exit event,
handling arbitrary nesting (e.g. an extension UI dialog during compaction
during tool execution) without per-pair restoration logic. Defensive: if a
pop arrives with an empty stack (lost prior event, daemon restart mid-flight,
etc.), the current state is preserved and a warning is logged — the state
machine does not get stuck in a modal forever.

`batch_wait` is entered on `batch_wait_start` and left on
`batch_wait_end`. Emitted by a fundi child whose first call of a `:batch`
model is parked in the OpenRouter Batch API; can last hours.

Notes:

- **`tool_running` requires a counter** because children run tools in
  parallel. State enters `tool_running` on the *first* start and returns to
  `streaming` on the *last* end; `activeTools` resets on `agent_end` as a
  sanity guard.
- **`blocked_ui` is dialog-only**: the `extension_ui_request` event covers
  both blocking dialogs and fire-and-forget notifications; only dialogs
  enter the modal state. Pending dialog ids are matched in a capped map
  (default 64 entries).
- **Compaction is observed, never initiated** by the daemon: the child
  runtime compacts (manually or on threshold) and the daemon records the
  boundary.

## 7. Replay and persistence

### In-memory buffers per child

| Buffer | Default size | Source |
|--------|--------------|--------|
| `out` ring | 5000 events or 64 MB (LRU) | the child's published events |
| `in` buffer | 16 MB (drop-oldest on overflow) | daemon-to-child commands |
| `err` buffer | 4 MB (drop-oldest) | child stderr |

Each child owns a Bus that fans events out to its subscribers. Subscribers
each get a bounded channel (default 256 events). On a full channel, the
daemon drops the event for that subscriber and notes the gap — subscribers
resume through `StreamEvents`' `EventCursor` (per-child ordinals) rather than
polling. The child's producer publishes to the bus and appends to the ring in
a single critical section, so per-child event order is preserved across
subscribers and replay.

The durable store is `conversations.event_log`: a per-child gap-free ordinal
sequence starting at 0 (§2.3, "Two event tiers"), which is what
`StreamEvents`' cursor replays from and what makes at-least-once replay
exact.

### Disk persistence

Modes (global config):

| Mode           | Behavior                                                              |
|----------------|------------------------------------------------------------------------|
| `on_exit`      | Always dump the buffers on child exit. **Default for dev.**           |
| `on_failure`   | Dump only on a nonzero exit, a signal, or a final status of `error`.  |
| `never`        | Discard on exit; in-memory only.                                      |

Layout per dumped child (under the daemon's logs dir):

```
<LogsDir>/<childId>/
  in.jsonl.gz    commands sent to the child's stdin
  out.jsonl.gz   events received from the child
  err.log.gz     stderr
  meta.json      spawn args, timing, exit code, signal
```

Format details: gzip level 6, plain `.gz` — `zcat`, `zless`, `zgrep`,
`rg -z` all work; all three streams compressed for consistency, even tiny
ones; written once at exit. Persistence is independent of `Close`: closing a
child removes its in-memory entry and store row but never deletes disk
artifacts.

### Exit grace window

Default 7 days (`RAFIKI_GRACE_HOURS` overrides). Exited children stay in the
store with status `exited` so their history stays replayable via
`StreamEvents`/`GetHistory`, they can be `Resume`d explicitly, and a
background sweeper removes entries older than the window. `Close` and
`CloseAllExited` are the manual cleanup.

### State records (resume support)

For each known child the daemon maintains a persisted record (atomic-rename
writes, so a crash never leaves a half-written record): identity (`name`,
`cwd`), the resolved model/provider/thinking (an `apiKey` is NEVER persisted —
always null on disk), the session pointers, the full snapshot of spawn
options (tools, extensions, skills, prompts, pre-fill), and runtime metadata
(spawned-at, last-seen-alive, pid, last status).

- `apiKey` is always null on disk. A child originally spawned with a per-call
  key must re-supply it at `Resume`.
- `env` is **not** persisted: on `Resume` the daemon's current environment is
  re-inherited; per-spawn overrides do not survive. Callers that need stable
  env should set vars in the daemon's environment rather than via per-spawn
  `env`.

Lifecycle: written on spawn; `lastSeenAlive` bumped in batches; the session
pointer rewritten when the child's session changes; `lastStatus` rewritten on
every transition; deleted on `Close`/`CloseAllExited` and when the grace
window expires. On daemon startup every record is loaded with status
`exited`, an orphan's process is signalled (dead pipes cannot be reattached),
and the records that read ALIVE are auto-resumed: a fundi child's engine
re-issues an interrupted turn, a claude child re-attaches its session
(`--resume`) and gets a continuation prompt when the row says it was working,
and a daraja-hosted claude child whose pinned executor has not reconnected yet
waits for the executor-connection sweep instead of failing outright. A script
child is settled `failed (daemon restarted)` rather than resumed — its exit is
its result. Manual `Resume` stays explicit and interactive: it never
re-submits an interrupted turn.
