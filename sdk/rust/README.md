# Rust SDK

`sdk/rust` is the Rust client for rafiki's Connect control plane — the same
plane the Go CLI, the cockpit and the Python SDK reach
(`docs/reference/control-protocol.md` §2–2.3), over the **protobuf** codec
rather than JSON (Python uses JSON; nothing here needs protojson).

It exists so a Rust program can drive a daemon without shelling out to
`rafiki` and without parsing CLI output: list and spawn children, read a
child's transcript, send a prompt, follow the event feed.

```rust
use futures_util::StreamExt;
use rafiki::{pb, Client, FollowPolicy, Followed};

let client = Client::new("unix:///run/user/1000/rafiki/controller.sock", None)?;
let children = client.list_children(&pb::ListChildrenRequest::default()).await?;

let request = pb::StreamEventsRequest {
    subject: Some(pb::EventSubject { scope: Some(pb::event_subject::Scope::All(true)), ..Default::default() }),
    tier: pb::EventTier::All as i32,
    ..Default::default()
};
// Re-opens itself across idle windows, dropped connections and daemon restarts.
let mut events = client.follow_events(request, FollowPolicy::default());
while let Some(item) = events.next().await {
    match item? {
        Followed::Event(event) => println!("{} {:?}", event.child_id, event.ordinal),
        // Deltas from the gap are gone; durable events will be replayed.
        Followed::Interrupted { cause, retry_in, .. } => eprintln!("{cause}; retrying in {retry_in:?}"),
    }
}
```

## Endpoints

`Client::new(endpoint, token)` accepts exactly these forms:

| endpoint | transport |
| --- | --- |
| `https://host[:port][/prefix]` | TLS over hyper + rustls, **ring** provider, native roots, HTTP/1.1 only |
| `http://host[:port][/prefix]` | plain TCP (a loopback listener) |
| `unix:///abs/path` | unix socket |
| `/abs/path` | unix socket |

Anything else is `Code::InvalidArgument`. A unix-socket endpoint addresses the
reserved base `http://connect.rafiki.invalid` and a `tower_service::Service`
connector dials the socket, ignoring the URI — a misconfigured dial fails
loudly instead of reaching a real host.

`Client::new_with_dial(endpoint, token, addr)` is `Client::new` with the TCP
layer pinned to `addr`: it never resolves or dials the URL's host or port. For
`https://` the TLS layer still takes SNI and certificate verification from the
URL's host, and `Host` still comes from the URL, so a caller can reach one
specific backend address while the certificate is checked against the name in
the URL. For `http://` it is plain TCP to `addr`. A unix-socket endpoint is
`Code::InvalidArgument`.

## The token is attached in the transport

Every request — unary and streaming alike — carries `Connect-Protocol-Version:
1`, `Rafiki-Protocol: 4` and, when a token was given, `Authorization: Bearer
<token>`. That happens in ONE place, the request builder both codecs go
through, and never at an individual call site: a token attached per call is a
token some call forgets, and the stream the cockpit holds would be the one
that fails, after the alt screen is up. The token is never printed: `Debug`
for `Client` shows `<redacted>`.

A unix socket is the credential (0600 in a 0700 directory), so `None` is the
normal token for a local daemon.

## Verbs

Typed wrappers over `Client::unary` and `Client::server_stream`, taking and
returning the generated `pb::` types: `get_history`, `stream_events(req,
idle)`, `follow_events(req, policy)` (see below), `send`, `spawn`, `get_child`, `list_children`, `kill`, `close`,
`resume`, `rename`, `set_budget`, `list_presets`, `list_models`,
`list_model_aliases`, `put_model_alias`, `delete_model_alias`, `tag`, `untag`, `list_tags`, `list_executors`, `begin_provider_login`, `complete_provider_login`,
`delete_provider_credential`, `begin_mcp_login`, `complete_mcp_login`,
`delete_mcp_credential`. `unary` and `server_stream` are public, so an
RPC added to `control.proto` is callable immediately (regenerate first). `pb`
holds the `rafiki.v1` messages and `daraja_pb` the `rafiki.daraja.v1` ones the
`Daraja*` RPCs carry.

`list_model_aliases` / `put_model_alias` / `delete_model_alias` manage the
calling connection OWNER's model aliases: a slash-free `name` the owner
resolves through, and a `target` whose base names a provider (so an alias never
chains to another alias). `put` creates or replaces and answers
`invalid_argument` for a bad name or target; `delete` answers `not_found` when
the owner has no such alias. A child credential may read them and never write.

```rust
client.put_model_alias(&pb::PutModelAliasRequest {
    name: "ds".into(),
    target: "openrouter/~deepseek/deepseek-flash-latest".into(),
}).await?;
let rows = client.list_model_aliases(&pb::ListModelAliasesRequest::default()).await?.rows;
client.delete_model_alias(&pb::DeleteModelAliasRequest { name: "ds".into() }).await?;
```

`SpawnRequest.notify` is the child's wake policy for its spawner: empty or
`"all"` wakes it on every settle and heartbeat, `"result"` only for a recorded
result, a pending question, a turn error, the child's own budget cap, or
exit/failure. Anything else is refused `invalid_argument`. Set `notify` on the
generated `SpawnRequest` (`notify: "result".into()`); no wrapper is added.

`tag` / `untag` / `list_tags` manage the calling connection OWNER's tags. A tag
is a named, scoped handle to a node: naming it is the grant to fork from it,
and `scope` says who holds it — lineage (the default), tree or owner. Anyone
who can read a node may tag it; only the tag's creator, that creator's
ancestors or a user credential may move or untag it. `tag` creates when
`tag_id` is empty and moves when it is set (compare-and-swap on
`expect_node_id`; a stale expectation is `failed_precondition`), and a move
appends a row, so a tag's history is never overwritten. `list_tags` answers the
visible tags nearest scope first, or one tag's whole history when `tag_id` is
set. An unknown tag id and another owner's look the same: `not_found`.

`SpawnRequest.fork_from` starts a child from a point in another child's history
and `from_tag` names a tag instead, resolved once at spawn, nearest scope first
(a tag rooted at one of your descendants ranks last). They are mutually
exclusive, and only `kind = "fundi"` can be forked to. A child may fork only
what it could already read, and a foreign or unknown tag answers the same
`permission_denied` as one it cannot see.

```rust
// Start from a point in another child's history: `cursor` is "m:<ordinal>",
// empty = the source's last completed turn.
let forked = client.spawn(&pb::SpawnRequest {
    cwd: "/work/repo".into(),
    fork_from: Some(pb::spawn_request::ForkFrom {
        child_id: child.clone(),
        cursor: "m:12".into(),
    }),
    ..Default::default()
}).await?.child_id;

// Or from whatever a tag names today, resolved once here.
let tagged = client.spawn(&pb::SpawnRequest {
    cwd: "/work/repo".into(),
    from_tag: "review-checkpoint".into(),
    ..Default::default()
}).await?.child_id;

let tag = client.tag(&pb::TagRequest {
    name: "checkpoint".into(),
    child_id: child.clone(),
    cursor: "m:12".into(),
    scope: pb::TagScope::Tree as i32,
    note: "before the rewrite".into(),
    ..Default::default()
}).await?.tag.unwrap();

// Move: compare-and-swap on the node the tag points at; name, scope and
// replace must be unset. A stale expect_node_id is failed_precondition.
client.tag(&pb::TagRequest {
    tag_id: tag.tag_id.clone(),
    expect_node_id: tag.node_id.clone(),
    child_id: child.clone(),
    cursor: "m:20".into(),
    ..Default::default()
}).await?;

let rows = client.list_tags(&pb::ListTagsRequest {
    name_prefix: "check".into(),
    ..Default::default()
}).await?.rows; // nearest scope first; limit 0 = 50 (max 500)
let history = client.list_tags(&pb::ListTagsRequest {
    tag_id: tag.tag_id.clone(),
    ..Default::default()
}).await?.rows; // one tag's whole history, oldest first
client.untag(&pb::UntagRequest { tag_id: tag.tag_id }).await?; // tombstone
```

`get_child` and `list_children` return `ChildSummary`; its `parent_child_id` names the
child that spawned it and is empty for a top-level child.

`list_children` takes the raw `ListChildrenRequest`, so `scope` is whatever the
caller sets on it: `ListScope::Subtree` (a child credential's own subtree, the
default for `Unspecified`), or `ListScope::Owner` (every agent its owner owns —
siblings and its owner's other trees included). Both are no-ops for a user,
admin or unix-socket caller, who has no position in the tree. `ChildSummary.in_subtree`
is `Some` only on a child credential's `Owner` list, where it says whether the
row is inside the caller's own subtree.

A child credential READS any agent its owner owns — its subtree, its siblings
and its owner's other trees alike — so `get_child`, `get_history` and the
conversation reads answer for them, while acting (`send`, `kill`, `close`,
`set_budget`) stays on the tree: a target outside the caller's own subtree is
refused even when it may read it. A foreign owner, an unknown id and an agent
whose lineage has no owner all answer the one identical `permission_denied`; a
caller with no owner of its own keeps subtree-only reads.

`get_history` pages backwards: ask with `limit` for the newest N, then again
with `before` set to the oldest `ts` you hold. A page never splits events that
share a `ts`, so it can hold more than `limit`; an empty page is the start of
the conversation. A stored `system`-role row comes back as `SystemMessage`, not
`UserMessage`. A `UserMessage` splits the text its child's harness injected
(Claude Code's `<system-reminder>` context) out of `content` into `injected`,
with the wrapper removed; a client that ignores the field shows the user's own
text.

`begin_provider_login` / `complete_provider_login` / `delete_provider_credential`
manage the CALLING USER's own provider credentials. `provider` is the NAME of a
`providers.toml` entry whose KIND authenticates by login (e.g.
`openai-responses-chatgpt`): begin returns the `login_id`, the `authorize_url`
to open, and the loopback port, callback path and hosts to listen on (the
provider's registered redirect URI is fixed), complete hands back the raw
callback query and answers the `provider` and `account_id`. Tokens never cross
the wire: the daemon stores them, and a child resolves its owner's credential
at spawn. Each needs a user credential on the client (`failed_precondition`
otherwise, and `permission_denied` for a child's credential). `begin` and
`complete` answer `invalid_argument` for a provider that is unknown or does not
login-authenticate; `delete` never resolves the provider entry, so it answers
`not_found` for a provider with no stored credential whether or not it is
configured (a login stored under a name since removed from `providers.toml`
can still be cleared). A login id that is unknown, expired or was begun by
another user is `not_found` too (one identical answer, so no caller can probe
which logins exist).

`begin_mcp_login` / `complete_mcp_login` / `delete_mcp_credential` are the same
shape for a remote MCP server that authenticates with OAuth. They are explicit
and interactive — nothing prompts during a spawn — and the grant belongs to the
CALLING USER: an admin gets only their own grants, and a child credential cannot
start, finish or delete one (`permission_denied`; `failed_precondition` for a
connection with no user identity at all). `config_path` is an ABSOLUTE path ON
THE DAEMON HOST: the daemon reads that exact file and that exact `mcpServers`
entry, so a client on another machine cannot upload its project config here, and
the SDK reads nothing client-side. A path the daemon cannot read, an entry it
does not declare, and an entry that cannot log in here (a stdio server, a
`via_executor` server, an entry with its own `Authorization` header) are
`invalid_argument`.

`complete_mcp_login` has EXACTLY ONE outcome: when `continuation` is set the
login is not finished — nothing is stored, the same `login_id` carries on, and
the continuation is another Begin-shaped descriptor to drive another round (a
server may ask for a wider scope once it has seen the tool list). Only a
continuation-free answer is success, and then `server`, `resource`,
`account_id` (empty unless a validated source names one) and the sorted tool
NAMES it serves are set. A callback code is single use; a replayed one, a
refused consent round and a mismatched `state`/`iss` are all `invalid_argument`
("start again"). A login id that is unknown, expired or was begun by another
user is `not_found`.

`delete_mcp_credential` takes two exclusive forms: `config_path` + `server`
resolves the configured entry and invalidates its endpoint (fencing a first
login still in flight), or `resource` names the canonical resource and touches
no config at all, which is how a grant is removed after its project file is
gone. Both or neither is `invalid_argument`; a grant the caller does not have is
`not_found`. It returns the generated `pb::DeleteMcpCredentialResponse` (an
empty message), matching `delete_provider_credential` rather than `()`. No token,
client secret or callback code ever appears in a response — the daemon holds
every credential.

## Errors: nothing is retried

Every failure is an `Error` carrying a Connect `Code` (snake_case on the wire,
`Code::as_str()`), the peer's message, and — when the peer attached a
`google.rpc.ErrorInfo` — its `reason`. Nothing in this crate retries.
`is_unavailable()` is the daemon-restarting signal a caller MAY retry on;
`is_unauthenticated()`, `is_not_found()`, `is_protocol_mismatch()` and
`permission_denied` are terminal.

A daemon authenticating several users isolates agents per owner: a non-admin
token gets `permission_denied` naming the child for any id it does not own
(foreign and unknown ids answer identically, so ids cannot be probed), and
`list_children`/`status`/`search`/`close_all_exited` answer for the caller's
own agents only. Admin tokens and the local unix socket are unrestricted. A unary call is bounded by
`Client::with_timeout` (30s default) and its expiry is `deadline_exceeded` —
but NOT `is_idle()`: the call's outcome is unknown, so never resend a `send` on
it (`Send` has no idempotency key; a resend can queue the message twice).

A daemon speaking another wire epoch is refused on EVERY response, unary or
stream (the stream's response headers included): a different
`Rafiki-Protocol` is `failed_precondition` with reason `protocol_mismatch` —
"daemon speaks rafiki protocol N; this client speaks 4 — upgrade the daemon",
or "— upgrade this client" when N is the newer one. A MISSING header is a
mismatch only when a Connect server answered (a 200, or an error body naming a
Connect code: a daemon from before the epoch). A non-200 that is neither came
from something in front of the daemon — a reverse proxy's 502 while rafikid
restarts — and is that status's error (`unavailable` for 502–504), so an outage
stays retryable instead of looking terminal. The daemon's own
`protocol_mismatch` refusal is passed through unchanged, since its message
("upgrade your rafiki client") is the more specific one.

A response body, or one stream envelope, over 64 MiB is `resource_exhausted`;
a length field is never trusted past that.

## Following the event feed

`follow_events(req, policy)` is the event feed a long-running consumer wants:
a `Following` stream of `Followed::Event(..)` and `Followed::Interrupted {
attempt, cause, retry_in }` that re-opens itself.

- **Cursor.** It records the highest ordinal yielded per child, and on every
  re-open sends them as `ordinals` with a `floor` at the newest server time it
  has seen (the `Date` of its last open, or a later event's `ts`), so the
  daemon replays exactly the durable events it missed — including those of a
  child born during the gap. `req.cursor` seeds the first open; unset means
  live. A live `child` subject reads `GetChild.latest_ordinal` first and opens
  from it, because an unnamed child subject replays its whole log.
- **Idle** (`policy.idle`, default 30s) re-opens at once and silently: a quiet
  stream lost nothing.
- **Interruptions** — `unavailable`, a truncated stream, a clean end (a daemon
  shutting down) — yield `Interrupted`, then re-open after a jittered
  exponential backoff (`initial_backoff` 500ms doubling to `max_backoff` 30s).
  Ephemeral events from the gap (`content_block_delta`) are gone; the
  `AssistantMessage` that follows carries the full text, so drop partial text
  on `Interrupted`. `max_retries` (default unlimited) bounds consecutive
  failures; an event resets the count.
- **Anything else is terminal**: `unauthenticated`, a protocol mismatch,
  `not_found`, `permission_denied`. It is yielded once and the stream ends.

`Client` is cheap to clone (it shares its connection pool), and the follower
owns a clone, so a `Following` is `'static` and `Send`.

## Raw streams: idle and resume

`stream_events(req, idle)` is one open, and nothing re-opens it. `idle` bounds
two waits — the response headers, and each next body frame. On expiry the
stream yields `deadline_exceeded` / "stream idle" (`is_idle()`) and ends. A
current daemon flushes `StreamEvents` headers at open, but an older one sends
them with the first message, so handle `is_idle()` at the open as well as on
each item. Other server streams still send headers with their first message.

A dropped connection, and any end without the Connect end-of-stream envelope
("stream closed without an end-of-stream envelope"), is `unavailable`: a
truncation, never a clean close. Only `None` from the stream, after a
flags-`0x02` end-of-stream envelope, is a clean end.

A durable `AssistantMessage`, `UserMessage` or `SystemMessage` event carries
in `Event.ts` the `created_at` of the `conversation_message` row that message
was persisted as, the same value `get_history` reports for it, so a message is
identified across a live stream and a history read by its `ts`. A `system`-role
row — a notice the child's harness injected (a heartbeat nudge, an environment
update), not something the user typed — arrives as `SystemMessage`, from the
stream and from `get_history` alike: it is history, and hiding it is a display
choice. Fundi children meet this today;
a claude child's live `ts` is still the claude stream's time, not the row's.
Rows written in one transaction share a `ts`, so match on `ts` plus position
and role. A fundi tool result is its own `UserMessage` (a `tool_result` block),
published in tool_use order; steers drained together are one merged event.

`message_delivered` (`pb::event::Payload::MessageDelivered`) is a durable
ACCEPTANCE RECEIPT, and it is not a message: `message_id` is the inbox row id
the send was given (`SendResponse.message_id`), and the event says the child's
harness took that message into a turn. It is not proof the model read it, not a
durability guarantee for the message itself, and it changes no inbox state.
Because it is durable, `follow_events` REPLAYS it after a gap, so the same
receipt can arrive twice: key any effect on `message_id`, and treat an unknown
id — or a payload arm this build does not know — as a no-op rather than an
error or a turn boundary. It is an ordinary `Followed::Event`, never an
interruption; a daemon that predates the event publishes nothing.

`get_history` returns the transcript along the conversation head's history, not
every stored row: a prompt retracted before it was answered, and the superseded
sibling reply a retry left behind, are omitted. A live `stream_events` feed
still shows such a branch as it happens, and a refetch drops it. Ordinals stay
strictly increasing along the returned page. A conversation id the daemon does
not hold is an error (`not_found`), never an empty page.

Resuming by hand means calling `stream_events` again with `EventCursor` set
from the events already seen (`ordinals` for children you have seen, `floor`
for a child you have not). Never invent a cursor of 0 — 0 is a real ordinal.

Every TCP connection the client dials has keepalive on (30s, probing every
10s), so the OS notices a dead peer without protocol traffic. There is no
keepalive frame on the wire: Connect defines none, and every rafikid face is
HTTP/1.1.

`is_idle()` is a flag only the stream's own idle window sets: an
`Error::new(Code::DeadlineExceeded, …)` is a unary timeout and is never idle.
A consumer whose tests fake `stream_events` builds the real one with
`Error::stream_idle_for_tests()`, under the `test-util` feature, enabled from
`[dev-dependencies]` only:

```toml
[dev-dependencies]
rafiki = { git = "https://github.com/graveland/rafiki.git", tag = "…", features = ["test-util"] }
```

Two ordinal spaces exist and must never be mixed: `GetHistory` events are
`conversation_message` ordinals, the event feed's `Event.ordinal` is the
event-log ordinal.

## Generated types: `make proto`

`sdk/rust/rafiki/src/gen/` is generated by the `rafiki-gen` binary (this
workspace) from `proto/rafiki/v1/{control,event}.proto` and
`proto/rafiki/daraja/v1/daraja.proto`, and is **committed**. `make proto`
regenerates it together with the Go code and the Python types, so no SDK can
lag the wire. Never hand-edit a generated file: `make proto` followed by
`git diff --exit-code -- sdk/rust/rafiki/src/gen` is the check that they match
the protos.

## Checking

```bash
make check-sdk-rust       # cargo fmt --check, then test + clippy -D warnings (--locked --all-features)
make check-sdk-rust-live  # one command: a scratch daemon plus the loopback MCP OAuth fixture
```

`make check-sdk-rust` is offline: the transport tests run against an
in-process hyper server on `127.0.0.1:0`, and the live-daemon tests in
`tests/daemon.rs` report as ignored. Neither it nor `make check` needs anything
below.

### The live run: `make check-sdk-rust-live`

One command reproduces the live-daemon tests from a clean checkout. It builds
`rafikid`, `rafiki` and `test/livefixture` **from this tree** (never a `$PATH`
binary), isolates `HOME` and every XDG base under a temp dir, migrates the
database named by `RAFIKI_TEST_DSN` (override with `RAFIKI_SDK_LIVE_DSN`),
mints a user token with `rafikid user create --token`, writes a scratch
`rafikid.toml` carrying the operator `[mcp_oauth]` allowance loopback and
cleartext need, starts the daemon and the fixture, waits for the control
socket by making one authenticated Connect call, enrolls one executor for the
run's own machine (the executor pool is on, and a spawn needs a live member),
then runs `cargo test --locked --all-features --test daemon -- --ignored`.
Everything it started is torn down on every exit path; `RAFIKI_SDK_LIVE_KEEP=1`
keeps the scratch tree (with the daemon and fixture logs) for debugging.

It needs cargo, Go and the disposable database. Each missing piece is a loud
failure naming the fix, never a skip: a live run that silently passes nothing
is the defect this command exists to prevent.

`test/livefixture/main.go` is a loopback origin that is both a protected MCP
resource and its own authorization server: RFC 9728 protected-resource
metadata, RFC 8414 authorization-server metadata, RFC 7591 dynamic client
registration, a scripted `authorize` that answers with the 302 a consenting
browser would follow (so no browser or person is involved), a code exchange
with a rotating refresh token, RFC 7009 revocation, and a real MCP SDK server
with one tool. **No real vendor account is involved and no request leaves the
loopback interface.** It is checked in as a command (not a throwaway file) so
this run is reproducible from a clean checkout.

It also answers a small test control surface on the same origin, because two of
the claims the run makes are only observable from the authorization server's
side: `GET /_state` reports the token exchanges it served (which refresh token
was presented and what replaced it), the revocations it received with their
`token_type_hint`, every MCP request with the bearer it carried, and which
access tokens are still valid; `POST /_expire` invalidates every access token it
has issued. A login's access token is issued with a ONE-SECOND lifetime — the
only lever a fixture has on the daemon's refresh decision, since a 401 is
answered with the login hint rather than a refresh — so the next MCP-backed call
must refresh, and the run asserts the rotation and the refreshed bearer.

The follower and history tests are the reason the driver also provisions state:
four of them follow, page and replay CHILD HISTORY, which needs a child that is
not working and has at least two durable events. The driver spawns a fundi child
detached, drives two SCRIPTED turns through it (`--fake-turns` swaps the sender,
so no provider is called and no key is needed), waits for both to land, and kills
it — the child then sits `exited` with an ordinal. If that cannot be done the run
FAILS; it does not run those tests against a daemon that cannot satisfy them and
call the result an outcome.

So every ignored test passes in this run, and the driver requires exactly that:

- the four MCP OAuth tests: `a_live_daemon_starts_and_completes_an_mcp_login`,
  `a_live_daemon_logs_out_an_mcp_grant` (which also asserts the upstream
  revocation arrived, naming the token type),
  `a_live_daemon_refreshes_an_expired_mcp_grant` (a fundi spawn materializes the
  server's tools through the stored grant after the token was forced to expire,
  and the refreshed bearer is the one the fixture saw), and
  `a_live_daemon_refuses_an_mcp_login_for_an_undeclared_server`;
- `a_live_daemon_answers_a_unary_call` and
  `a_live_daemon_refuses_a_stale_client_on_both_codecs`;
- the four history tests, against the provisioned child:
  `a_live_stream_idles_and_resumes_from_its_cursor`,
  `a_live_follower_reopens_across_idle_windows_without_repeats`,
  `a_live_child_follower_never_replays_history`, and
  `a_live_history_pages_backwards_without_gaps_or_repeats`.

There is no allowlist and nothing is expected to fail. The driver requires the
passed set to equal the expected ten EXACTLY, the harness summary to agree with
the per-test lines, no test to fail, and cargo's exit status to be 0 — so an
extra or missing test, a harness that ran nothing, a build error and a
regression all fail the run. `test/livefixture/run.sh --check-outcome
<cargo-log> [exit-status]` runs that classification alone — no database, no
daemon, no cargo — so the rule itself is testable.

For a daemon of your own, or to run one test alone, use the plain `cargo`
invocation the driver makes:

```bash
cd sdk/rust
RAFIKI_SDK_TEST_URL=unix:///…/controller.sock RAFIKI_SDK_TEST_TOKEN=… \
  cargo test --locked --all-features --test daemon -- --ignored
```

The tests otherwise only read: a unary call, a stream that replays a child's
recent events, idles and resumes from its cursor without a repeat, a follower
that does the same across several silent idle re-opens, a live child follower
that never replays history, backward `GetHistory` paging checked against one
unbounded read, and the daemon's own epoch refusal on both codecs (sent through
`Client::with_wire_epoch`, which exists only under the `test-hooks` feature —
never enable it in a consumer). The MCP OAuth tests are the exception: they log
in and out explicitly, and one of them spawns a fundi child with the fixture's
MCP config and kills it again — with no prompt, a spawn does no model call, and
the tools it materializes are what prove the refresh. A daemon NEWER than the
client cannot be provoked live; the stub tests cover it.

`cargo tree -i aws-lc-rs` must keep reporting nothing: the crate builds TLS
with rustls + ring only, so it can be a dependency of a consumer that forbids
aws-lc-rs and native-tls.
