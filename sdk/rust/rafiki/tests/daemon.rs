// SPDX-License-Identifier: Apache-2.0
//! The live-daemon tests: a real `rafikid`, not a stub.
//!
//! Every test here is `#[ignore]`d, so an offline `cargo test` reports them as
//! ignored rather than passed. Run them with `make check-sdk-rust-live` (or
//! `cargo test --all-features --test daemon -- --ignored`) and
//! `RAFIKI_SDK_TEST_URL` set to a client endpoint (`unix:///…`, a bare path,
//! or `https://…`), with `RAFIKI_SDK_TEST_TOKEN` as an optional bearer token.
//! Asked to run with no endpoint, they FAIL: a live test that skips itself is
//! not a test.
//!
//! The tests only read: they list, stream and page, and never spawn, send or
//! kill. The resume test needs one child that is not working and has at least
//! one durable event.
//!
//! The other half of the protocol-mismatch pair — a daemon speaking an epoch
//! this client does not — cannot be provoked against a real daemon, so it is
//! covered by the stub tests in `tests/connect.rs`.

use std::time::Duration;

use futures_util::StreamExt as _;
use rafiki::{pb, Client, Error, FollowPolicy, Followed, Streaming};

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
