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
`list_executors`. `unary` and `server_stream` are public, so an RPC added to
`control.proto` is callable immediately (regenerate first). `pb` holds the
`rafiki.v1` messages and `daraja_pb` the `rafiki.daraja.v1` ones the `Daraja*`
RPCs carry.

`get_child` and `list_children` return `ChildSummary`; its `parent_child_id` names the
child that spawned it and is empty for a top-level child.

`get_history` pages backwards: ask with `limit` for the newest N, then again
with `before` set to the oldest `ts` you hold. A page never splits events that
share a `ts`, so it can hold more than `limit`; an empty page is the start of
the conversation.

## Errors: nothing is retried

Every failure is an `Error` carrying a Connect `Code` (snake_case on the wire,
`Code::as_str()`), the peer's message, and — when the peer attached a
`google.rpc.ErrorInfo` — its `reason`. Nothing in this crate retries.
`is_unavailable()` is the daemon-restarting signal a caller MAY retry on;
`is_unauthenticated()`, `is_not_found()`, `is_protocol_mismatch()` and
`permission_denied` are terminal. A unary call is bounded by
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

A durable `AssistantMessage` or `UserMessage` event carries in `Event.ts` the
`created_at` of the `conversation_message` row that message was persisted as,
the same value `get_history` reports for it, so a message is identified across
a live stream and a history read by its `ts`. Fundi children meet this today;
a claude child's live `ts` is still the claude stream's time, not the row's.
Rows written in one transaction share a `ts`, so match on `ts` plus position
and role. A fundi tool result is its own `UserMessage` (a `tool_result` block),
published in tool_use order; steers drained together are one merged event.

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
make check-sdk-rust-live  # tests/daemon.rs against RAFIKI_SDK_TEST_URL
```

`make check-sdk-rust` is offline: the transport tests run against an
in-process hyper server on `127.0.0.1:0`, and the live-daemon tests in
`tests/daemon.rs` report as ignored. `make check-sdk-rust-live` runs them
against the daemon at `RAFIKI_SDK_TEST_URL` (with `RAFIKI_SDK_TEST_TOKEN` as an
optional bearer token), and FAILS when it is unset. They only read: a unary
call, a stream that replays a child's recent events, idles and resumes from
its cursor without a repeat, a follower that does the same across several
silent idle re-opens, a live child follower that never replays history, backward `GetHistory` paging checked against one
unbounded read, and the daemon's own epoch refusal on both codecs (sent through
`Client::with_wire_epoch`, which exists only under the `test-hooks` feature —
never enable it in a consumer). The daemon needs a child that is not working
and has at least two history events. A daemon NEWER than the client cannot be
provoked live; the stub tests cover it.

`cargo tree -i aws-lc-rs` must keep reporting nothing: the crate builds TLS
with rustls + ring only, so it can be a dependency of a consumer that forbids
aws-lc-rs and native-tls.
