# Rust SDK

`sdk/rust` is the Rust client for rafiki's Connect control plane — the same
plane the Go CLI, the cockpit and the Python SDK reach
(`docs/reference/control-protocol.md` §2–2.3), over the **protobuf** codec
rather than JSON (Python uses JSON; nothing here needs protojson).

It exists so a Rust program can drive a daemon without shelling out to
`rafiki` and without parsing CLI output: list and spawn children, read a
child's transcript, send a prompt, follow the event feed.

```rust
use std::time::Duration;
use futures_util::StreamExt;
use rafiki::{pb, Client};

let client = Client::new("unix:///run/user/1000/rafiki/controller.sock", None)?;
let children = client.list_children(&pb::ListChildrenRequest::default()).await?;

let request = pb::StreamEventsRequest {
    subject: Some(pb::EventSubject { scope: Some(pb::event_subject::Scope::All(true)), ..Default::default() }),
    tier: pb::EventTier::All as i32,
    ..Default::default()
};
// A quiet stream sends no headers, so the idle window can end the open too.
let mut events = match client.stream_events(&request, Duration::from_secs(30)).await {
    Ok(events) => events,
    Err(err) if err.is_idle() => return Ok(()), // nothing yet: re-open later with your cursor
    Err(err) => return Err(err.into()),
};
while let Some(event) = events.next().await {
    match event {
        Ok(event) => println!("{} {:?}", event.child_id, event.ordinal),
        // A quiet daemon is normal: resume with your own cursor, don't hang.
        Err(err) if err.is_idle() => break,
        Err(err) => return Err(err.into()),
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
idle)`, `send`, `spawn`, `get_child`, `list_children`, `kill`, `close`,
`resume`, `rename`, `set_budget`, `list_presets`, `list_models`,
`list_executors`. `unary` and `server_stream` are public, so an RPC added to
`control.proto` is callable immediately (regenerate first). `pb` holds the
`rafiki.v1` messages and `daraja_pb` the `rafiki.daraja.v1` ones the `Daraja*`
RPCs carry.

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

## Idle and resume

A server stream's headers arrive with its **first message**, not before: a
stream with nothing to say yet sends no bytes at all. `server_stream(method,
req, idle)` therefore bounds two waits with `idle` — the response headers, and
each next body frame. On expiry the stream yields
`deadline_exceeded` / "stream idle" (`is_idle()`) and ends — and because a quiet
stream sends no headers, that expiry usually ends `stream_events(...).await`
itself, before any `Streaming` exists. Handle `is_idle()` at the open as well
as on each item.

That is not a failure, and the SDK never re-opens the stream itself: only the
caller knows the cursor. A dropped connection is the same kind of event, and so
is any end without the Connect end-of-stream envelope ("stream closed without
an end-of-stream envelope"): a truncation, never a clean close. Treat
`is_idle()` and `is_unavailable()` alike — re-open from your own cursor. Only
`None` from the stream, after a flags-`0x02` end-of-stream envelope, is a clean
end.

Resume by calling `stream_events` again with `EventCursor` set from the events
already seen (`ordinals` for children you have seen, `floor` for a child you
have not). Never invent a cursor of 0 — 0 is a real ordinal.

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
its cursor without a repeat, backward `GetHistory` paging checked against one
unbounded read, and the daemon's own epoch refusal on both codecs (sent through
`Client::with_wire_epoch`, which exists only under the `test-hooks` feature —
never enable it in a consumer). The daemon needs a child that is not working
and has at least two history events. A daemon NEWER than the client cannot be
provoked live; the stub tests cover it.

`cargo tree -i aws-lc-rs` must keep reporting nothing: the crate builds TLS
with rustls + ring only, so it can be a dependency of a consumer that forbids
aws-lc-rs and native-tls.
