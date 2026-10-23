// SPDX-License-Identifier: Apache-2.0
//! `follow_events` against a scripted stub: each StreamEvents call gets the
//! next scripted answer, and every request is recorded so a test can check
//! the cursor each re-open sent.

use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use bytes::Bytes;
use futures_util::StreamExt as _;
use http::{Request, Response};
use http_body_util::{BodyExt, StreamBody};
use hyper::body::{Frame, Incoming};
use prost::Message as _;
use rafiki::{pb, Client, Code, FollowPolicy, Followed};

mod common;
use common::*;

/// One scripted StreamEvents answer.
type Answer = Box<dyn Fn() -> Response<TestBody> + Send + Sync>;

/// A stub that answers the Nth StreamEvents call with `answers[N]` (the last
/// one repeats) and GetChild with `latest_ordinal`, recording every
/// StreamEvents request.
fn scripted(
    answers: Vec<Answer>,
    latest_ordinal: Option<i32>,
) -> (Stub, Arc<Mutex<Vec<pb::StreamEventsRequest>>>) {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let answers = Arc::new(answers);
    let recorded = seen.clone();
    let stub: Stub = Arc::new(move |req: Request<Incoming>| {
        let recorded = recorded.clone();
        let answers = answers.clone();
        Box::pin(async move {
            if req.uri().path().ends_with("/GetChild") {
                let reply = pb::GetChildResponse {
                    child: Some(pb::ChildSummary {
                        latest_ordinal,
                        ..Default::default()
                    }),
                };
                return Ok(with_epoch(Response::new(full(reply.encode_to_vec()))));
            }
            let request = read_stream_request(req).await;
            let n = {
                let mut seen = recorded.lock().expect("lock");
                seen.push(request);
                seen.len() - 1
            };
            Ok(answers[n.min(answers.len() - 1)]())
        })
    });
    (stub, seen)
}

/// An event an hour in the future, so its ts is later than any `Date` the
/// stub sends and a test can tell which one set the floor.
fn future_event(child_id: &str, ordinal: i32) -> pb::Event {
    let now = SystemTime::now().duration_since(UNIX_EPOCH).expect("clock");
    pb::Event {
        ts: Some(prost_types::Timestamp {
            seconds: now.as_secs() as i64 + 3600 + i64::from(ordinal),
            nanos: 0,
        }),
        ..event(child_id, ordinal)
    }
}

/// Envelopes for `events`, then `tail`: an end-of-stream trailer, a bare EOF
/// (truncation), or silence.
enum Tail {
    End,
    Eof,
    Silent,
}

fn events(events: Vec<pb::Event>, tail: Tail) -> Answer {
    Box::new(move || {
        let mut parts: Vec<Bytes> = events
            .iter()
            .map(|event| envelop(0, &event.encode_to_vec()))
            .collect();
        if let Tail::End = tail {
            parts.push(end_of_stream(r#"{"error": null}"#));
        }
        let frames = futures_util::stream::iter(
            parts
                .into_iter()
                .map(|part| Ok::<_, std::io::Error>(Frame::data(part))),
        );
        let body = match tail {
            Tail::Silent => BodyExt::boxed(StreamBody::new(
                frames.chain(futures_util::stream::pending()),
            )),
            _ => BodyExt::boxed(StreamBody::new(frames)),
        };
        with_epoch(Response::new(body))
    })
}

fn fixed_answer(status: u16, body: &'static str, epoch: bool) -> Answer {
    Box::new(move || {
        let mut response = Response::new(full(body));
        *response.status_mut() = http::StatusCode::from_u16(status).expect("a status");
        if epoch {
            with_epoch(response)
        } else {
            response
        }
    })
}

fn quick(idle_ms: u64) -> FollowPolicy {
    FollowPolicy {
        idle: Duration::from_millis(idle_ms),
        initial_backoff: Duration::from_millis(10),
        max_backoff: Duration::from_millis(40),
        max_retries: None,
    }
}

fn all_events() -> pb::StreamEventsRequest {
    pb::StreamEventsRequest {
        subject: Some(pb::EventSubject {
            scope: Some(pb::event_subject::Scope::All(true)),
            ..Default::default()
        }),
        ..Default::default()
    }
}

fn ordinal_of(item: &Followed) -> Option<i32> {
    match item {
        Followed::Event(event) => event.ordinal,
        Followed::Interrupted { .. } => None,
    }
}

async fn next_item(stream: &mut rafiki::Following) -> Result<Followed, rafiki::Error> {
    tokio::time::timeout(Duration::from_secs(5), stream.next())
        .await
        .expect("the follower stalled")
        .expect("the follower ended")
}

/// A truncated stream is an interruption: the re-open carries a cursor naming
/// every child yielded and a floor at the newest server time seen.
#[tokio::test]
async fn a_truncated_stream_reopens_from_its_cursor() {
    let (stub, seen) = scripted(
        vec![
            events(
                vec![future_event("c_1", 1), future_event("c_1", 2)],
                Tail::Eof,
            ),
            events(vec![future_event("c_1", 3)], Tail::Silent),
        ],
        None,
    );
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let mut follow = client.follow_events(all_events(), quick(5_000));

    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("1")),
        Some(1)
    );
    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("2")),
        Some(2)
    );
    match next_item(&mut follow).await.expect("an interruption") {
        Followed::Interrupted {
            attempt,
            cause,
            retry_in,
        } => {
            assert_eq!(attempt, 1);
            assert!(cause.is_unavailable(), "{cause}");
            assert!(retry_in <= Duration::from_millis(12), "{retry_in:?}");
        }
        other => panic!("{other:?}"),
    }
    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("3")),
        Some(3)
    );

    let seen = seen.lock().expect("lock");
    assert_eq!(
        seen[0].cursor, None,
        "a live follower opens without a cursor"
    );
    let cursor = seen[1]
        .cursor
        .as_ref()
        .expect("the re-open carries a cursor");
    assert_eq!(cursor.ordinals.get("c_1"), Some(&2));
    assert_eq!(
        cursor.floor,
        future_event("c_1", 2).ts,
        "the floor is the newest server time seen"
    );
}

/// Idle means nothing was flowing, so nothing was lost: the re-open is silent.
#[tokio::test]
async fn an_idle_window_reopens_silently() {
    let (stub, seen) = scripted(
        vec![
            events(vec![], Tail::Silent),
            events(vec![event("c_1", 5)], Tail::Silent),
        ],
        None,
    );
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let mut follow = client.follow_events(all_events(), quick(100));

    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("an event")),
        Some(5),
        "no Interrupted before the re-opened stream's event"
    );
    let seen = seen.lock().expect("lock");
    assert_eq!(seen.len(), 2);
    let cursor = seen[1]
        .cursor
        .as_ref()
        .expect("a cursor after the first open");
    assert!(
        cursor.floor.is_some(),
        "the first open's Date floors the re-open"
    );
}

/// A daemon shutting down ends its streams cleanly; the follower outlives it.
#[tokio::test]
async fn a_clean_end_reopens() {
    let (stub, _seen) = scripted(
        vec![
            events(vec![event("c_1", 1)], Tail::End),
            events(vec![event("c_1", 2)], Tail::Silent),
        ],
        None,
    );
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let mut follow = client.follow_events(all_events(), quick(5_000));

    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("1")),
        Some(1)
    );
    assert!(matches!(
        next_item(&mut follow).await.expect("an interruption"),
        Followed::Interrupted { .. }
    ));
    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("2")),
        Some(2)
    );
}

/// An error no retry can fix is yielded once and ends the stream.
#[tokio::test]
async fn a_terminal_error_ends_the_stream() {
    let (stub, seen) = scripted(
        vec![fixed_answer(
            401,
            r#"{"code": "unauthenticated", "message": "invalid auth token"}"#,
            true,
        )],
        None,
    );
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let mut follow = client.follow_events(all_events(), quick(5_000));

    let err = next_item(&mut follow).await.expect_err("unauthenticated");
    assert_eq!(err.code, Code::Unauthenticated, "{err}");
    assert!(
        follow.next().await.is_none(),
        "nothing after a terminal error"
    );
    assert_eq!(seen.lock().expect("lock").len(), 1, "never retried");
}

/// max_retries bounds consecutive failures; the last one is yielded as the
/// error. Backoff doubles per attempt.
#[tokio::test]
async fn retries_give_up_after_max_retries() {
    let (stub, seen) = scripted(vec![fixed_answer(503, "<html>down</html>", false)], None);
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let mut policy = quick(5_000);
    policy.max_retries = Some(2);
    let mut follow = client.follow_events(all_events(), policy);

    let mut delays = Vec::new();
    for attempt in 1..=2 {
        match next_item(&mut follow).await.expect("an interruption") {
            Followed::Interrupted {
                attempt: got,
                retry_in,
                ..
            } => {
                assert_eq!(got, attempt);
                delays.push(retry_in);
            }
            other => panic!("{other:?}"),
        }
    }
    let err = next_item(&mut follow).await.expect_err("gave up");
    assert!(err.is_unavailable(), "{err}");
    assert!(follow.next().await.is_none());
    assert_eq!(seen.lock().expect("lock").len(), 3);
    assert!(
        delays[1] > delays[0],
        "backoff grows between attempts: {delays:?}"
    );
}

/// An event resets the failure count, so a later interruption starts over.
#[tokio::test]
async fn an_event_resets_the_failure_count() {
    let (stub, _seen) = scripted(
        vec![
            fixed_answer(503, "<html>down</html>", false),
            events(vec![event("c_1", 1)], Tail::Eof),
            events(vec![], Tail::Silent),
        ],
        None,
    );
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let mut follow = client.follow_events(all_events(), quick(5_000));

    let attempts: Vec<Option<u32>> = {
        let mut out = Vec::new();
        for _ in 0..3 {
            out.push(match next_item(&mut follow).await.expect("an item") {
                Followed::Interrupted { attempt, .. } => Some(attempt),
                Followed::Event(_) => None,
            });
        }
        out
    };
    assert_eq!(attempts, vec![Some(1), None, Some(1)]);
}

/// A live child subject opens from the child's latest ordinal: an unnamed
/// child subject replays its whole log, which a re-open must never do.
#[tokio::test]
async fn a_live_child_subject_opens_from_latest_ordinal() {
    let (stub, seen) = scripted(vec![events(vec![event("c_1", 8)], Tail::Silent)], Some(7));
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let request = pb::StreamEventsRequest {
        subject: Some(pb::EventSubject {
            scope: Some(pb::event_subject::Scope::Child("c_1".to_string())),
            ..Default::default()
        }),
        ..Default::default()
    };
    let mut follow = client.follow_events(request, quick(5_000));

    assert_eq!(
        ordinal_of(&next_item(&mut follow).await.expect("8")),
        Some(8)
    );
    let seen = seen.lock().expect("lock");
    let cursor = seen[0].cursor.as_ref().expect("a cursor on the first open");
    assert_eq!(cursor.ordinals.get("c_1"), Some(&7));
    assert_eq!(cursor.floor, None);
}

/// A caller's cursor seeds the first open unchanged.
#[tokio::test]
async fn a_caller_cursor_seeds_the_first_open() {
    let (stub, seen) = scripted(vec![events(vec![], Tail::Silent)], None);
    let client = Client::new(&serve(stub).await, None).expect("a client");
    let floor = prost_types::Timestamp {
        seconds: 1_700_000_000,
        nanos: 0,
    };
    let mut request = all_events();
    request.cursor = Some(pb::EventCursor {
        ordinals: [("c_9".to_string(), 4)].into_iter().collect(),
        floor: Some(floor),
    });
    let mut follow = client.follow_events(request.clone(), quick(5_000));

    // Nothing arrives; give the open a moment to land.
    assert!(
        tokio::time::timeout(Duration::from_millis(200), follow.next())
            .await
            .is_err(),
        "a silent stream yields nothing"
    );
    assert_eq!(seen.lock().expect("lock")[0].cursor, request.cursor);
}
