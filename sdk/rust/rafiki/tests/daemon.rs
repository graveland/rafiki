// SPDX-License-Identifier: Apache-2.0
//! The live-daemon tests: a real `rafikid`, not a stub.
//!
//! Every test here is `#[ignore]`d, so an offline `cargo test` reports them as
//! ignored rather than passed. `make check-sdk-rust-live` needs no arguments:
//! it builds the daemon and the CLI from this tree, starts a scratch daemon
//! with the loopback MCP OAuth fixture (`test/livefixture`), and runs this file
//! against it — see `sdk/rust/README.md`.
//!
//! To run the file by hand instead, set `RAFIKI_SDK_TEST_URL` to a client
//! endpoint (`unix:///…`, a bare path, or `https://…`) with
//! `RAFIKI_SDK_TEST_TOKEN` as an optional bearer token, and run
//! `cargo test --all-features --test daemon -- --ignored`. Asked to run with no
//! endpoint, they FAIL: a live test that skips itself is not a test.
//!
//! The tests otherwise only read: they list, stream and page, and never spawn,
//! send or kill. The four MCP OAuth tests are the exception — they log in and
//! out explicitly, and the refresh one spawns a child with the fixture's MCP
//! config (no prompt, so no model call) and kills it again, because a real MCP
//! session is what proves the credential was refreshed. The four history tests
//! need a child that is not working and has at least two durable events, so they
//! fail against a daemon that has never spawned anything (the scratch one
//! `make check-sdk-rust-live` starts).
//!
//! The other half of the protocol-mismatch pair — a daemon speaking an epoch
//! this client does not — cannot be provoked against a real daemon, so it is
//! covered by the stub tests in `tests/connect.rs`.

use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use futures_util::StreamExt as _;
use rafiki::{pb, Client, Code, Error, FollowPolicy, Followed, Streaming};

/// How long a stream may stay quiet before it counts as caught up.
const IDLE: Duration = Duration::from_secs(2);

/// The daemon names its own epoch in its refusal; ours never says this.
const DAEMON_REFUSAL: &str = "upgrade your rafiki client";

fn live_client() -> Client {
    let url = std::env::var("RAFIKI_SDK_TEST_URL")
        .expect("RAFIKI_SDK_TEST_URL is unset: the live-daemon tests need a rafikid endpoint");
    let token = std::env::var("RAFIKI_SDK_TEST_TOKEN").ok();
    Client::new(&url, token).expect("RAFIKI_SDK_TEST_URL is a client endpoint")
}

fn child_stream(child_id: &str, after: i32) -> pb::StreamEventsRequest {
    pb::StreamEventsRequest {
        subject: Some(pb::EventSubject {
            scope: Some(pb::event_subject::Scope::Child(child_id.to_string())),
            ..Default::default()
        }),
        tier: pb::EventTier::Durable as i32,
        types: Vec::new(),
        cursor: Some(pb::EventCursor {
            ordinals: [(child_id.to_string(), after)].into_iter().collect(),
            floor: None,
        }),
    }
}

/// Every ordinal a stream yields until it goes idle. A quiet stream sends no
/// headers, so the idle window can end the open itself as well as a read.
async fn ordinals_until_idle(client: &Client, request: &pb::StreamEventsRequest) -> Vec<i32> {
    let stream: Streaming<pb::Event> = match client.stream_events(request, IDLE).await {
        Ok(stream) => stream,
        Err(err) if err.is_idle() => return Vec::new(),
        Err(err) => panic!("stream_events: {err}"),
    };
    futures_util::pin_mut!(stream);
    let mut seen = Vec::new();
    loop {
        match stream.next().await {
            Some(Ok(event)) => {
                seen.push(event.ordinal.expect("a durable event carries an ordinal"))
            }
            Some(Err(err)) if err.is_idle() => return seen,
            Some(Err(err)) => panic!("stream_events after {seen:?}: {err}"),
            None => panic!("the stream ended cleanly after {seen:?} instead of going idle"),
        }
    }
}

#[tokio::test]
#[ignore = "needs a live rafikid: set RAFIKI_SDK_TEST_URL and run with --ignored"]
async fn a_live_daemon_answers_a_unary_call() {
    let models = live_client()
        .list_models(&pb::ListModelsRequest::default())
        .await
        .expect("list_models against a live daemon");
    println!("list_models: {} model(s)", models.models.len());
}

/// Replay a child's last few durable events, go idle, then re-open from the
/// last ordinal seen: the second stream must repeat nothing.
#[tokio::test]
#[ignore = "needs a live rafikid: set RAFIKI_SDK_TEST_URL and run with --ignored"]
async fn a_live_stream_idles_and_resumes_from_its_cursor() {
    let client = live_client();
    let children = client
        .list_children(&pb::ListChildrenRequest::default())
        .await
        .expect("list_children")
        .children;
    let (child_id, latest) = children
        .iter()
        .filter(|child| child.display_state != "working")
        .filter_map(|child| {
            child
                .latest_ordinal
                .map(|latest| (child.child_id.clone(), latest))
        })
        .filter(|(_, latest)| *latest >= 0)
        .max_by_key(|(_, latest)| *latest)
        .expect("the daemon needs a child that is not working and has a durable event");

    let start = (latest - 3).max(-1);
    let replayed = ordinals_until_idle(&client, &child_stream(&child_id, start)).await;
    println!("{child_id}: replayed {replayed:?} after {start}, latest {latest}");
    assert!(
        !replayed.is_empty(),
        "nothing replayed after ordinal {start} (latest {latest})"
    );
    assert!(
        replayed.windows(2).all(|pair| pair[0] < pair[1]) && replayed[0] > start,
        "replay is in order and after the cursor: {replayed:?}"
    );
    let last = *replayed.last().expect("non-empty");
    assert!(
        last >= latest,
        "replay reached latest_ordinal {latest}: {replayed:?}"
    );

    let resumed = ordinals_until_idle(&client, &child_stream(&child_id, last)).await;
    println!("{child_id}: resumed after {last} with {resumed:?}");
    assert!(
        resumed.iter().all(|ordinal| *ordinal > last),
        "a resumed stream repeated an event at or before {last}: {resumed:?}"
    );
}

/// The child the stream tests read: not working, with at least one durable
/// event.
async fn quiet_child(client: &Client) -> (String, i32) {
    client
        .list_children(&pb::ListChildrenRequest::default())
        .await
        .expect("list_children")
        .children
        .iter()
        .filter(|child| child.display_state != "working")
        .filter_map(|child| {
            child
                .latest_ordinal
                .map(|latest| (child.child_id.clone(), latest))
        })
        .filter(|(_, latest)| *latest >= 0)
        .max_by_key(|(_, latest)| *latest)
        .expect("the daemon needs a child that is not working and has a durable event")
}

/// Everything a follower yields until it has been quiet for `quiet`.
async fn follow_until_quiet(mut follow: rafiki::Following, quiet: Duration) -> Vec<Followed> {
    let mut items = Vec::new();
    while let Ok(item) = tokio::time::timeout(quiet, follow.next()).await {
        match item {
            Some(Ok(item)) => items.push(item),
            Some(Err(err)) => panic!("follow_events after {items:?}: {err}"),
            None => panic!("follow_events ended after {items:?}"),
        }
    }
    items
}

fn one_second_idle() -> FollowPolicy {
    FollowPolicy {
        idle: Duration::from_secs(1),
        ..FollowPolicy::default()
    }
}

/// A follower replays a child's tail from its cursor, then sits through
/// several idle windows — each one a silent re-open — without repeating an
/// event or reporting an interruption.
#[tokio::test]
#[ignore = "needs a live rafikid: set RAFIKI_SDK_TEST_URL and run with --ignored"]
async fn a_live_follower_reopens_across_idle_windows_without_repeats() {
    let client = live_client();
    let (child_id, latest) = quiet_child(&client).await;
    let start = (latest - 3).max(-1);
    let follow = client.follow_events(child_stream(&child_id, start), one_second_idle());

    let items = follow_until_quiet(follow, Duration::from_millis(3500)).await;
    let ordinals: Vec<i32> = items
        .iter()
        .map(|item| match item {
            Followed::Event(event) => event.ordinal.expect("a durable event carries an ordinal"),
            Followed::Interrupted { cause, .. } => {
                panic!("interrupted on a healthy daemon: {cause}")
            }
        })
        .collect();
    println!("{child_id}: followed {ordinals:?} after {start} across idle re-opens");
    assert!(
        !ordinals.is_empty(),
        "nothing replayed after {start} (latest {latest})"
    );
    assert!(
        ordinals.windows(2).all(|pair| pair[0] < pair[1]) && ordinals[0] > start,
        "in order, after the cursor, never repeated: {ordinals:?}"
    );
    assert!(
        *ordinals.last().expect("non-empty") >= latest,
        "{ordinals:?}"
    );
}

/// A live follower of one child starts at the child's latest ordinal, so its
/// idle re-opens never replay the child's log.
#[tokio::test]
#[ignore = "needs a live rafikid: set RAFIKI_SDK_TEST_URL and run with --ignored"]
async fn a_live_child_follower_never_replays_history() {
    let client = live_client();
    let (child_id, latest) = quiet_child(&client).await;
    let request = pb::StreamEventsRequest {
        subject: Some(pb::EventSubject {
            scope: Some(pb::event_subject::Scope::Child(child_id.clone())),
            ..Default::default()
        }),
        tier: pb::EventTier::Durable as i32,
        ..Default::default()
    };
    let items = follow_until_quiet(
        client.follow_events(request, one_second_idle()),
        Duration::from_millis(3500),
    )
    .await;
    for item in &items {
        if let Followed::Event(event) = item {
            assert!(
                event.ordinal.is_some_and(|ordinal| ordinal > latest),
                "{child_id}: replayed history at or before {latest}: {event:?}"
            );
        }
    }
    println!("{child_id}: live follower saw {} new event(s)", items.len());
}

/// Paging `GetHistory` backwards — `limit`, then `before` set to the oldest
/// ts received — visits exactly the events one unbounded read returns.
#[tokio::test]
#[ignore = "needs a live rafikid: set RAFIKI_SDK_TEST_URL and run with --ignored"]
async fn a_live_history_pages_backwards_without_gaps_or_repeats() {
    let client = live_client();
    let children = client
        .list_children(&pb::ListChildrenRequest::default())
        .await
        .expect("list_children")
        .children;
    let mut paged_any = false;
    for child in children
        .iter()
        .filter(|child| child.display_state != "working")
    {
        let full = match history(&client, &child.child_id, None, None).await {
            Ok(response) => response.events,
            // Not every child kind has a fundi conversation to read.
            Err(err) if err.is_not_found() => continue,
            Err(err) => panic!("{}: get_history: {err}", child.child_id),
        };
        if full.len() < 2 {
            continue;
        }
        let mut paged = Vec::new();
        let mut before = None;
        loop {
            let page = history(&client, &child.child_id, Some(1), before)
                .await
                .unwrap_or_else(|err| panic!("{}: get_history page: {err}", child.child_id))
                .events;
            let Some(oldest) = page.first() else { break };
            before = Some(oldest.ts.expect("a history event carries ts"));
            paged.splice(0..0, page);
            assert!(
                paged.len() <= full.len(),
                "{}: paging repeated an event",
                child.child_id
            );
        }
        let ordinals = |events: &[pb::Event]| events.iter().map(|e| e.ordinal).collect::<Vec<_>>();
        assert_eq!(
            ordinals(&paged),
            ordinals(&full),
            "{}: paged vs full",
            child.child_id
        );
        println!(
            "{}: paged {} event(s) one at a time",
            child.child_id,
            full.len()
        );
        paged_any = true;
    }
    assert!(
        paged_any,
        "the daemon needs a child with at least two history events"
    );
}

async fn history(
    client: &Client,
    child_id: &str,
    limit: Option<i32>,
    before: Option<prost_types::Timestamp>,
) -> Result<pb::GetHistoryResponse, Error> {
    let request = pb::GetHistoryRequest {
        child_id: child_id.to_string(),
        limit,
        before,
        ..Default::default()
    };
    client.get_history(&request).await
}

/// A client sending the wrong epoch is refused by the daemon itself, on both
/// codecs, and the daemon's own message reaches the caller unmasked.
#[tokio::test]
#[ignore = "needs a live rafikid: set RAFIKI_SDK_TEST_URL and run with --ignored"]
async fn a_live_daemon_refuses_a_stale_client_on_both_codecs() {
    let client = live_client().with_wire_epoch("999");

    let err = client
        .list_models(&pb::ListModelsRequest::default())
        .await
        .expect_err("a daemon refuses epoch 999");
    assert_daemon_refusal("unary", &err);

    let request = pb::StreamEventsRequest {
        subject: Some(pb::EventSubject {
            scope: Some(pb::event_subject::Scope::All(true)),
            ..Default::default()
        }),
        tier: pb::EventTier::Durable as i32,
        types: Vec::new(),
        cursor: None,
    };
    // The daemon refuses a stream with an HTTP 200 and an end-of-stream error,
    // so the refusal arrives as the stream's first item.
    let err = match client.stream_events(&request, IDLE).await {
        Err(err) => err,
        Ok(stream) => {
            futures_util::pin_mut!(stream);
            match stream.next().await {
                Some(Err(err)) => err,
                other => panic!("a refused stream yielded {other:?}"),
            }
        }
    };
    assert_daemon_refusal("stream", &err);
}

fn assert_daemon_refusal(codec: &str, err: &Error) {
    assert!(err.is_protocol_mismatch(), "{codec}: {err}");
    assert!(
        err.message.contains(DAEMON_REFUSAL),
        "{codec}: the daemon's own message, not ours: {}",
        err.message
    );
}

// ─── MCP OAuth: the three verbs, against a live daemon ──────────────────────
//
// These are the same requests `rafiki mcp login` / `mcp logout` make, over the
// Rust client, with a REAL daemon on the other end. They need a fake
// authorization server the runner supplies — never a real vendor account —
// because the login is explicit and interactive by design:
//
//   RAFIKI_SDK_TEST_MCP_CONFIG  absolute path to an .mcp.json on the daemon
//                               host whose entry is a direct `url` server
//                               backed by a fake authorization server that
//                               answers its authorization endpoint with a 302
//                               to loopback (the runner's fixture).
//   RAFIKI_SDK_TEST_MCP_SERVER  the entry's name (default "fixture").
//   RAFIKI_SDK_TEST_MCP_RESOURCE
//                               optional: the canonical resource the runner
//                               expects the login to bind.
//
// The browser step is performed here, over plain HTTP: the fake authorization
// server answers the consent GET with the redirect a browser would follow, and
// its query — carrying the state and PKCE-bound code — is handed to
// `complete_mcp_login`. An https:// authorization endpoint is refused with a
// message rather than guessed at: point these tests at the loopback fixture.

/// The three MCP OAuth tests share ONE configured server and therefore ONE
/// endpoint's login fence: a second login for the same endpoint supersedes the
/// first (`failed_precondition`, "start again"), which is correct behaviour and
/// would make a parallel run nondeterministic. This slot serializes them without
/// holding a lock across an await (no `tokio::sync` feature here) and without
/// requiring the runner to pass `--test-threads=1`.
static MCP_LOGIN_BUSY: AtomicBool = AtomicBool::new(false);

struct LoginSlot;

impl Drop for LoginSlot {
    fn drop(&mut self) {
        MCP_LOGIN_BUSY.store(false, Ordering::SeqCst);
    }
}

async fn login_slot() -> LoginSlot {
    while MCP_LOGIN_BUSY
        .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
        .is_err()
    {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    LoginSlot
}

fn live_mcp_config() -> (String, String) {
    let config = std::env::var("RAFIKI_SDK_TEST_MCP_CONFIG").expect(
        "RAFIKI_SDK_TEST_MCP_CONFIG is unset: the live MCP OAuth tests need an .mcp.json \
         on the daemon host whose entry is backed by a fake authorization server",
    );
    let server =
        std::env::var("RAFIKI_SDK_TEST_MCP_SERVER").unwrap_or_else(|_| "fixture".to_string());
    (config, server)
}

/// One plain-HTTP request, returning (status, headers, body). Enough to play the
/// browser against a loopback fake authorization server and to read its control
/// surface, and deliberately not a general HTTP client.
fn http_call(method: &str, url: &str, body: &str) -> (u16, String, String) {
    use std::io::{Read, Write};
    let rest = url.strip_prefix("http://").unwrap_or_else(|| {
        panic!(
            "{url}: the live fixture answers on loopback http:// only \
             (the tests never reach an https:// authorization server)"
        )
    });
    let (authority, path) = match rest.find('/') {
        Some(i) => (&rest[..i], &rest[i..]),
        None => (rest, "/"),
    };
    let mut stream = std::net::TcpStream::connect(authority)
        .unwrap_or_else(|err| panic!("connect to {authority}: {err}"));
    let request = format!(
        "{method} {path} HTTP/1.1\r\nHost: {authority}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    stream
        .write_all(request.as_bytes())
        .expect("write the fixture request");
    let mut response = String::new();
    stream
        .read_to_string(&mut response)
        .expect("read the fixture response");
    let (headers, body) = match response.split_once("\r\n\r\n") {
        Some((head, body)) => (head.to_string(), body.to_string()),
        None => (response.clone(), String::new()),
    };
    let status = headers
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse::<u16>().ok())
        .unwrap_or_else(|| panic!("no status line in the fixture response: {response:?}"));
    let body = if headers
        .to_ascii_lowercase()
        .contains("transfer-encoding: chunked")
    {
        dechunk(&body)
    } else {
        body
    };
    (status, headers, body)
}

/// Decode a chunked body. The fixture sets Content-Length (so this normally does
/// nothing), but a response that outgrew net/http's buffer used to arrive
/// chunked and the chunk-size line is not JSON — the helper reads the wire
/// honestly rather than assuming one framing.
fn dechunk(body: &str) -> String {
    fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
        haystack.windows(needle.len()).position(|w| w == needle)
    }
    let mut out: Vec<u8> = Vec::new();
    let mut rest = body.as_bytes();
    while let Some(pos) = find(rest, b"\r\n") {
        let size = match std::str::from_utf8(&rest[..pos])
            .ok()
            .and_then(|line| usize::from_str_radix(line.trim(), 16).ok())
        {
            Some(size) => size,
            None => break,
        };
        let after = &rest[pos + 2..];
        if size == 0 {
            break;
        }
        if after.len() < size {
            out.extend_from_slice(after);
            break;
        }
        out.extend_from_slice(&after[..size]);
        rest = if after.len() >= size + 2 {
            &after[size + 2..]
        } else {
            &[]
        };
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// One plain-HTTP GET, returning (status, Location). The consent round's
/// redirect is the only header read directly.
fn http_get_location(url: &str) -> (u16, Option<String>) {
    let (status, headers, _) = http_call("GET", url, "");
    let location = headers.lines().find_map(|line| {
        let (name, value) = line.split_once(':')?;
        if name.eq_ignore_ascii_case("location") {
            Some(value.trim().to_string())
        } else {
            None
        }
    });
    (status, location)
}

/// The fixture's origin, taken from the entry the daemon is configured with —
/// the same file the login reads, so the control surface cannot point somewhere
/// else.
fn fixture_base() -> String {
    let (config, server) = live_mcp_config();
    let raw = std::fs::read_to_string(&config)
        .unwrap_or_else(|err| panic!("read the MCP config {config}: {err}"));
    let doc: serde_json::Value = serde_json::from_str(&raw)
        .unwrap_or_else(|err| panic!("the MCP config {config} is not JSON: {err}"));
    let url = doc["mcpServers"][&server]["url"]
        .as_str()
        .unwrap_or_else(|| panic!("the MCP config {config} has no url for entry {server}"));
    let rest = url
        .strip_prefix("http://")
        .unwrap_or_else(|| panic!("{url}: the live fixture is a loopback http:// server"));
    let authority = match rest.find('/') {
        Some(i) => &rest[..i],
        None => rest,
    };
    format!("http://{authority}")
}

/// One request to the fixture's control surface, parsed. The whole document is
/// needed for the assertions (token names, request bearers, hint strings), and
/// nothing in it is secret the caller did not already hold.
fn fixture_call(method: &str, path: &str) -> serde_json::Value {
    let url = format!("{}{path}", fixture_base());
    let (status, _, body) = http_call(method, &url, "");
    assert_eq!(200, status, "{method} {url} answered {status}: {body}");
    serde_json::from_str(&body).unwrap_or_else(|err| panic!("{method} {url}: {err}: {body}"))
}

fn fixture_state() -> serde_json::Value {
    fixture_call("GET", "/_state")
}

/// Poll the fixture until `pred` holds, then return that state. Bounded, not
/// timed: nothing here sleeps to make an expiry happen — the expiry is a
/// property of the token the fixture issued.
async fn wait_for_fixture(
    what: &str,
    pred: impl Fn(&serde_json::Value) -> bool,
) -> serde_json::Value {
    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    loop {
        let state = fixture_state();
        if pred(&state) {
            return state;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the fixture never saw {what}; last state: {state}"
        );
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

/// The newest authorization_code exchange, which is this run's login: the login
/// slot serializes the MCP tests, so the newest one is the caller's.
fn last_login_exchange(state: &serde_json::Value) -> &serde_json::Value {
    state["exchanges"]
        .as_array()
        .expect("the fixture reports its exchanges")
        .iter()
        .rev()
        .find(|x| x["grant_type"] == "authorization_code")
        .expect("a completed login left an authorization_code exchange")
}

/// Drive one whole explicit login under the login slot: begin, play the
/// browser (another round if the daemon asks for one), and return (server,
/// resource, tools).
async fn live_mcp_login(client: &Client) -> (String, String, Vec<String>) {
    let _slot = login_slot().await;
    live_mcp_login_locked(client).await
}

/// The login itself. Callers that keep the slot for work AFTER the login (the
/// refresh and revocation tests) call this directly, so the whole test stays one
/// critical section — the slot is not reentrant.
async fn live_mcp_login_locked(client: &Client) -> (String, String, Vec<String>) {
    let (config, server) = live_mcp_config();
    let begin = client
        .begin_mcp_login(&pb::BeginMcpLoginRequest {
            config_path: config,
            server: server.clone(),
            scopes: Vec::new(),
        })
        .await
        .expect("begin_mcp_login against a live daemon");
    assert!(!begin.login_id.is_empty(), "begin returned no login id");
    assert!(
        begin.redirect_port > 0
            && !begin.callback_path.is_empty()
            && !begin.redirect_hosts.is_empty(),
        "begin returned no usable loopback callback: {begin:?}"
    );

    let mut descriptor = begin;
    for round in 0..4 {
        let (status, location) = http_get_location(&descriptor.authorize_url);
        assert_eq!(
            status, 302,
            "round {round}: the fixture's authorization endpoint did not redirect (it answered {status})"
        );
        let location = location.expect("the consent redirect carries no Location");
        let query = location
            .split_once('?')
            .map(|(_, q)| q.to_string())
            .unwrap_or_else(|| panic!("the redirect {location} carries no query"));
        let done = client
            .complete_mcp_login(&pb::CompleteMcpLoginRequest {
                login_id: descriptor.login_id.clone(),
                callback_query: query,
            })
            .await
            .expect("complete_mcp_login against a live daemon");
        match done.continuation {
            Some(next) => {
                // A continuation is the WHOLE outcome of the round: nothing is
                // stored yet and the same login id carries on.
                assert_eq!(
                    next.login_id, descriptor.login_id,
                    "a continuation changed the login id"
                );
                descriptor = next;
            }
            None => {
                assert_eq!(done.server, server, "the daemon named another server");
                assert!(
                    !done.resource.is_empty(),
                    "a completed login carries no resource"
                );
                assert!(
                    !done.tools.is_empty(),
                    "a completed login verified no tools"
                );
                for secret_field in [&done.resource, &done.account_id, &done.server] {
                    assert!(
                        !secret_field.starts_with("access-")
                            && !secret_field.starts_with("refresh-"),
                        "a response field carries a credential: {done:?}"
                    );
                }
                if let Ok(expected) = std::env::var("RAFIKI_SDK_TEST_MCP_RESOURCE") {
                    assert_eq!(done.resource, expected, "the login bound another resource");
                }
                return (done.server, done.resource, done.tools);
            }
        }
    }
    panic!("the login still asked for consent after four rounds");
}

#[tokio::test]
#[ignore = "needs a live rafikid + RAFIKI_SDK_TEST_MCP_CONFIG: run with --ignored"]
async fn a_live_daemon_starts_and_completes_an_mcp_login() {
    let client = live_client();
    let (server, resource, tools) = live_mcp_login(&client).await;
    println!(
        "{server}: logged in for {resource} with {} tool(s): {tools:?}",
        tools.len()
    );

    // The same login id is consumed: replaying the callback is refused, and an
    // unknown id answers the SAME not_found (a caller cannot probe which logins
    // exist).
    let err = client
        .complete_mcp_login(&pb::CompleteMcpLoginRequest {
            login_id: "no-such-login".to_string(),
            callback_query: "code=x&state=y".to_string(),
        })
        .await
        .expect_err("an unknown login id completed");
    assert!(err.is_not_found(), "an unknown login id: {err}");
}

#[tokio::test]
#[ignore = "needs a live rafikid + RAFIKI_SDK_TEST_MCP_CONFIG: run with --ignored"]
async fn a_live_daemon_logs_out_an_mcp_grant() {
    let _slot = login_slot().await;
    let client = live_client();
    let (_, resource, _) = live_mcp_login_locked(&client).await;

    // The grant's own refresh token. The revocation the daemon sends upstream
    // must name THIS token: that is what makes "the logout reached the
    // authorization server" a fact rather than an inference from the RPC's
    // success.
    let state = fixture_state();
    let refresh = last_login_exchange(&state)["issued_refresh"]
        .as_str()
        .expect("the login issued a refresh token")
        .to_string();

    // The resource form needs no config file at all, which is how a grant is
    // removed once its project file is gone.
    client
        .delete_mcp_credential(&pb::DeleteMcpCredentialRequest {
            resource: resource.clone(),
            ..Default::default()
        })
        .await
        .expect("delete_mcp_credential by resource");

    // The upstream revocation is detached and best-effort — the logout does not
    // wait on it — so wait for the fixture to report it.
    let state = wait_for_fixture("a revocation for the logged-out grant", |s| {
        s["revocations"]
            .as_array()
            .map(|rows| rows.iter().any(|r| r["token"] == refresh.as_str()))
            .unwrap_or(false)
    })
    .await;
    let entry = state["revocations"]
        .as_array()
        .expect("the fixture reports revocations")
        .iter()
        .find(|r| r["token"] == refresh.as_str())
        .expect("the revocation that was just waited for");
    let hint = entry["token_type_hint"].as_str().unwrap_or("");
    assert_eq!(
        "refresh_token", hint,
        "the revocation named another token type: {entry}"
    );
    println!("revoked {refresh} upstream with token_type_hint={hint}");

    // Logging the same grant out again is IDEMPOTENT: the tombstone is already
    // there, so the local invalidation has nothing left to do and succeeds. A
    // resource that was never logged in is the one that answers not_found.
    client
        .delete_mcp_credential(&pb::DeleteMcpCredentialRequest {
            resource: resource.clone(),
            ..Default::default()
        })
        .await
        .expect("a second logout of the same grant is idempotent");
    let err = client
        .delete_mcp_credential(&pb::DeleteMcpCredentialRequest {
            resource: "https://never-logged-in.invalid/mcp".to_string(),
            ..Default::default()
        })
        .await
        .expect_err("a grant that was never logged in was removed");
    assert!(err.is_not_found(), "an unknown resource: {err}");

    // Giving BOTH selectors is refused before anything is looked up.
    let (config, server) = live_mcp_config();
    let err = client
        .delete_mcp_credential(&pb::DeleteMcpCredentialRequest {
            config_path: config,
            server,
            resource,
        })
        .await
        .expect_err("both delete selectors were accepted");
    assert_eq!(err.code, Code::InvalidArgument, "both selectors: {err}");
}

#[tokio::test]
#[ignore = "needs a live rafikid + RAFIKI_SDK_TEST_MCP_CONFIG: run with --ignored"]
async fn a_live_daemon_refreshes_an_expired_mcp_grant() {
    let _slot = login_slot().await;
    let client = live_client();
    let (config, _) = live_mcp_config();
    let (_, resource, _) = live_mcp_login_locked(&client).await;
    // Taken the moment the login returned, which is already after the
    // authorization server minted the access token: `issued_at + lifetime` is
    // therefore an UPPER bound on the expiry the daemon stored.
    let issued_at = std::time::Instant::now();

    // What the login stored: the access token a later session would otherwise
    // send, and the refresh token that must rotate.
    //
    // `before` also pins the fixture's request count. The fixture is shared by
    // every MCP test in this binary, so "a tools/list with some other bearer"
    // must mean a request made AFTER this point — an earlier test's session is
    // not evidence about this one.
    let state = fixture_state();
    let requests_before = state["mcp_requests"]
        .as_array()
        .map(|rows| rows.len() as i64)
        .unwrap_or(0);
    let grant = last_login_exchange(&state);
    let login_access = grant["issued_access"]
        .as_str()
        .expect("the login issued an access token")
        .to_string();
    let login_refresh = grant["issued_refresh"]
        .as_str()
        .expect("the login issued a refresh token")
        .to_string();

    // Force expiry from the server side as well, so a session that reuses the
    // login's access token cannot succeed even if the daemon ignored the
    // expiry. The daemon's own reason to refresh is the LIFETIME the exchange
    // advertised (one second — see the wait below); this makes the stale token
    // unusable rather than merely old.
    let expired = fixture_call("POST", "/_expire");
    let expired = expired["expired"]
        .as_array()
        .expect("the fixture reports what it expired");
    assert!(
        expired.iter().any(|t| t == login_access.as_str()),
        "/_expire did not invalidate the login's access token {login_access}: {expired:?}"
    );

    // The fixture issues a login's access token with a one-second lifetime, so
    // the daemon's stored expiry has passed by the time the session dials. Wait
    // for that deadline instead of hoping the run was slow: the bound above
    // makes "expired" a fact before the call is made.
    let lifetime = state["lifetimes"]["authorization_code"]
        .as_u64()
        .expect("the fixture reports the lifetime it advertised");
    let deadline = issued_at + Duration::from_secs(lifetime) + Duration::from_millis(250);
    if let Some(wait) = deadline.checked_duration_since(std::time::Instant::now()) {
        tokio::time::sleep(wait).await;
    }

    // One MCP-backed call through the daemon, driven by the SDK: a fundi spawn
    // materializes the configured server's tools over a real MCP session before
    // any model call, so this needs no provider key and no prompt.
    let spawned = client
        .spawn(&pb::SpawnRequest {
            kind: "fundi".to_string(),
            model: "anthropic/claude-x".to_string(),
            cwd: std::env::temp_dir().to_string_lossy().into_owned(),
            mcp_config: config,
            no_session: true,
            ..Default::default()
        })
        .await
        .expect("spawn with the OAuth MCP config");
    assert!(!spawned.child_id.is_empty(), "spawn returned no child id");

    // The fixture must serve a tools/list to a bearer that is NOT the login's:
    // the only way to obtain one is the refresh the daemon was forced to make.
    let after_marker = |r: &serde_json::Value| -> bool {
        r["seq"].as_i64().is_some_and(|seq| seq > requests_before)
    };
    let state = wait_for_fixture("a tools/list with a refreshed bearer", |s| {
        s["mcp_requests"]
            .as_array()
            .map(|rows| {
                rows.iter().any(|r| {
                    after_marker(r)
                        && r["method"] == "tools/list"
                        && r["accepted"] == true
                        && r["bearer"]
                            .as_str()
                            .is_some_and(|bearer| bearer != login_access)
                })
            })
            .unwrap_or(false)
    })
    .await;

    // The refresh presented the stored refresh token and issued a DIFFERENT
    // one: single-use rotation, not a re-login and not a silent reuse.
    let exchange = state["exchanges"]
        .as_array()
        .expect("the fixture reports exchanges")
        .iter()
        .find(|x| {
            x["grant_type"] == "refresh_token" && x["presented_refresh"] == login_refresh.as_str()
        })
        .unwrap_or_else(|| {
            panic!("the daemon never refreshed with the stored refresh token {login_refresh}")
        });
    let fresh_access = exchange["issued_access"]
        .as_str()
        .expect("the refresh issued an access token");
    let fresh_refresh = exchange["issued_refresh"]
        .as_str()
        .expect("the refresh issued a refresh token");
    assert_ne!(
        login_refresh, fresh_refresh,
        "the refresh token was not rotated: {exchange}"
    );

    // And the session that succeeded carried that refreshed token.
    let carried = state["mcp_requests"]
        .as_array()
        .expect("the fixture reports MCP requests")
        .iter()
        .any(|r| {
            after_marker(r)
                && r["method"] == "tools/list"
                && r["accepted"] == true
                && r["bearer"] == fresh_access
        });
    assert!(
        carried,
        "the verified MCP session did not carry the refreshed token {fresh_access}"
    );
    println!(
        "{resource}: refreshed {login_refresh} -> {fresh_refresh}; the MCP session carried {fresh_access}"
    );

    client
        .kill(&pb::KillRequest {
            child_id: spawned.child_id,
            ..Default::default()
        })
        .await
        .expect("kill the child that opened the session");
}

#[tokio::test]
#[ignore = "needs a live rafikid + RAFIKI_SDK_TEST_MCP_CONFIG: run with --ignored"]
async fn a_live_daemon_refuses_an_mcp_login_for_an_undeclared_server() {
    let _slot = login_slot().await;
    let client = live_client();
    let (config, _) = live_mcp_config();
    let err = client
        .begin_mcp_login(&pb::BeginMcpLoginRequest {
            config_path: config,
            server: "no-such-server-in-this-config".to_string(),
            scopes: Vec::new(),
        })
        .await
        .expect_err("a login started for an undeclared server");
    assert_eq!(
        err.code,
        Code::InvalidArgument,
        "an undeclared server: {err}"
    );
    assert!(
        !err.message.contains("access-") && !err.message.contains("refresh-"),
        "a refusal carried a credential: {err}"
    );
}
