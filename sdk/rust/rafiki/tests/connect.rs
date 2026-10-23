// SPDX-License-Identifier: Apache-2.0
//! The wire, end to end against an in-process daemon stub on 127.0.0.1:0.
//!
//! Each test spawns its own hyper server, so nothing here needs a rafikid (the
//! live-daemon test is `tests/daemon.rs`). Everything is offline.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use bytes::{Bytes, BytesMut};
use futures_util::StreamExt as _;
use http::{HeaderValue, Request, Response};
use http_body_util::BodyExt;
use hyper::body::Incoming;
use prost::Message as _;
use rafiki::{pb, Client};

mod common;
use common::*;

#[tokio::test]
async fn unary_success() {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let recorded = seen.clone();
    let stub: Stub = Arc::new(move |req: Request<Incoming>| {
        let recorded = recorded.clone();
        Box::pin(async move {
            let path = req.uri().path().to_string();
            let content_type = header_of(&req, "content-type").unwrap_or_default();
            let epoch = header_of(&req, "rafiki-protocol").unwrap_or_default();
            let body = req
                .into_body()
                .collect()
                .await
                .expect("the request body")
                .to_bytes();
            let request = pb::GetHistoryRequest::decode(body).expect("a GetHistoryRequest");
            recorded
                .lock()
                .expect("lock")
                .push((path, content_type, epoch, request.child_id));
            let reply = pb::GetHistoryResponse {
                events: vec![event("c_1", 7)],
            };
            Ok(with_epoch(Response::new(full(reply.encode_to_vec()))))
        })
    });
    let url = serve(stub).await;

    let client = Client::new(&url, None).expect("a client");
    let request = pb::GetHistoryRequest {
        child_id: "c_1".to_string(),
        ..Default::default()
    };
    let response = client.get_history(&request).await.expect("get_history");
    assert_eq!(
        response.events,
        vec![event("c_1", 7)],
        "the response message decodes"
    );

    let seen = seen.lock().expect("lock");
    assert_eq!(seen.len(), 1);
    let (path, content_type, epoch, child_id) = &seen[0];
    assert_eq!(path, "/rafiki.v1.Control/GetHistory");
    assert_eq!(content_type, "application/proto");
    assert_eq!(epoch, "4");
    assert_eq!(child_id, "c_1", "the request message encodes");
}

#[tokio::test]
async fn unary_without_the_epoch_header_is_a_protocol_mismatch() {
    // A 200 with a valid body: only the missing epoch marks the daemon stale.
    let stub: Stub = Arc::new(|_req: Request<Incoming>| {
        Box::pin(async move {
            Ok(Response::new(full(
                pb::GetHistoryResponse::default().encode_to_vec(),
            )))
        })
    });
    let url = serve(stub).await;
    let client = Client::new(&url, None).expect("a client");
    let err = client
        .get_history(&pb::GetHistoryRequest::default())
        .await
        .expect_err("a stale daemon");
    assert!(err.is_protocol_mismatch(), "{err}");
    assert!(
        err.message.contains("this client speaks 4"),
        "our own message, not the daemon's: {}",
        err.message
    );
    assert!(err.message.contains("none"), "{}", err.message);
}

#[tokio::test]
async fn the_daemons_own_protocol_mismatch_is_not_masked() {
    let daemon_message =
        "this daemon speaks rafiki protocol 4; upgrade your rafiki client (peer sent none)";
    let stub: Stub = Arc::new(move |_req: Request<Incoming>| {
        Box::pin(async move {
            // ErrorInfo{reason: "protocol_mismatch"} as a Connect error body.
            let mut info = vec![0x0a, 17];
            info.extend_from_slice(b"protocol_mismatch");
            use base64::Engine as _;
            let value = base64::engine::general_purpose::STANDARD.encode(info);
            let body = serde_json::json!({
                "code": "failed_precondition",
                "message": daemon_message,
                "details": [{"type": "google.rpc.ErrorInfo", "value": value}],
            })
            .to_string();
            let mut response = Response::new(full(body));
            *response.status_mut() = http::StatusCode::BAD_REQUEST;
            Ok(with_epoch(response))
        })
    });
    let url = serve(stub).await;
    let client = Client::new(&url, None).expect("a client");
    let err = client
        .get_history(&pb::GetHistoryRequest::default())
        .await
        .expect_err("the daemon's refusal");
    assert!(err.is_protocol_mismatch(), "{err}");
    assert_eq!(err.message, daemon_message);
}

#[tokio::test]
async fn a_non_200_becomes_the_connect_error_it_names() {
    let stub: Stub = Arc::new(|_req: Request<Incoming>| {
        Box::pin(async move {
            let body = serde_json::json!({
                "code": "not_found",
                "message": "no such child: c_9",
            })
            .to_string();
            let mut response = Response::new(full(body));
            *response.status_mut() = http::StatusCode::NOT_FOUND;
            Ok(with_epoch(response))
        })
    });
    let url = serve(stub).await;
    let client = Client::new(&url, None).expect("a client");
    let err = client
        .get_child(&pb::GetChildRequest {
            child_id: "c_9".to_string(),
        })
        .await
        .expect_err("no such child");
    assert!(err.is_not_found(), "{err}");
    assert_eq!(err.message, "no such child: c_9");
    assert_eq!(err.http_status, Some(404));
}

/// A stub that answers a stream request with `body`.
fn stream_stub(body: impl Fn() -> TestBody + Send + Sync + 'static) -> Stub {
    Arc::new(move |req: Request<Incoming>| {
        let body = body();
        Box::pin(async move {
            read_stream_request(req).await;
            Ok(with_epoch(Response::new(body)))
        })
    })
}

/// `frames` as a body factory, cloned per request.
fn frames_body(parts: Vec<Bytes>) -> impl Fn() -> TestBody + Send + Sync + 'static {
    let parts = Arc::new(parts);
    move || frames(parts.as_ref().clone())
}

#[tokio::test]
async fn stream_yields_every_message_then_a_clean_end() {
    // Two envelopes in ONE frame, then the trailer in its own frame: the
    // reader must slice exactly rather than trust frame boundaries.
    let mut packed = BytesMut::from(&envelop(0, &event("c_1", 1).encode_to_vec())[..]);
    packed.extend_from_slice(&envelop(0, &event("c_1", 2).encode_to_vec()));
    let url = serve(stream_stub(frames_body(vec![
        packed.freeze(),
        end_of_stream(r#"{"error": null}"#),
    ])))
    .await;
    let client = Client::new(&url, None).expect("a client");
    let stream = client
        .stream_events(&pb::StreamEventsRequest::default(), Duration::from_secs(5))
        .await
        .expect("stream_events");
    futures_util::pin_mut!(stream);
    assert_eq!(
        stream.next().await.expect("first").expect("an event"),
        event("c_1", 1)
    );
    assert_eq!(
        stream.next().await.expect("second").expect("an event"),
        event("c_1", 2)
    );
    assert!(
        stream.next().await.is_none(),
        "a null-error trailer ends the stream"
    );
}

/// A Connect server stream ends with a flags-`0x02` envelope. A connection
/// that just stops after a complete message is a TRUNCATION: the message
/// arrives, then an error — never a clean `None`, which a consumer would read
/// as "settled" instead of "resume from my cursor".
#[tokio::test]
async fn stream_truncated_after_a_message_is_unavailable() {
    let url = serve(stream_stub(frames_body(vec![envelop(
        0,
        &event("c_3", 4).encode_to_vec(),
    )])))
    .await;
    let client = Client::new(&url, None).expect("a client");
    let stream = client
        .stream_events(&pb::StreamEventsRequest::default(), Duration::from_secs(5))
        .await
        .expect("stream_events");
    futures_util::pin_mut!(stream);
    assert_eq!(
        stream.next().await.expect("first").expect("an event"),
        event("c_3", 4)
    );
    let err = stream
        .next()
        .await
        .expect("an error, never a clean end")
        .expect_err("a truncated stream");
    assert!(err.is_unavailable(), "{err}");
    assert_eq!(
        err.message,
        "stream closed without an end-of-stream envelope"
    );
    assert!(stream.next().await.is_none(), "the error ends the stream");
}

/// The same truncation with nothing at all after the headers.
#[tokio::test]
async fn stream_ending_with_no_envelope_at_all_is_unavailable() {
    let url = serve(stream_stub(frames_body(Vec::new()))).await;
    let client = Client::new(&url, None).expect("a client");
    let stream = client
        .stream_events(&pb::StreamEventsRequest::default(), Duration::from_secs(5))
        .await
        .expect("stream_events");
    futures_util::pin_mut!(stream);
    let err = stream
        .next()
        .await
        .expect("an error, never a clean end")
        .expect_err("a truncated stream");
    assert!(err.is_unavailable(), "{err}");
    assert_eq!(
        err.message,
        "stream closed without an end-of-stream envelope"
    );
    assert!(stream.next().await.is_none(), "the error ends the stream");
}

#[tokio::test]
async fn stream_ending_in_an_error_trailer_yields_that_error() {
    let trailer = serde_json::json!({
        "error": {"code": "unauthenticated", "message": "invalid auth token"},
    })
    .to_string();
    let url = serve(stream_stub(frames_body(vec![
        envelop(0, &event("c_2", 3).encode_to_vec()),
        end_of_stream(&trailer),
    ])))
    .await;
    let client = Client::new(&url, None).expect("a client");
    let stream = client
        .stream_events(&pb::StreamEventsRequest::default(), Duration::from_secs(5))
        .await
        .expect("stream_events");
    futures_util::pin_mut!(stream);
    assert_eq!(
        stream.next().await.expect("first").expect("an event"),
        event("c_2", 3)
    );
    let err = stream
        .next()
        .await
        .expect("an error")
        .expect_err("an error");
    assert!(err.is_unauthenticated(), "{err}");
    assert_eq!(err.message, "invalid auth token");
    assert!(stream.next().await.is_none(), "the error ends the stream");
}

#[tokio::test]
async fn a_silent_stream_is_idle_within_the_budget() {
    let url = serve(stream_stub(silent)).await;
    let client = Client::new(&url, None).expect("a client");
    let idle = Duration::from_millis(150);
    let started = Instant::now();
    let stream = client
        .stream_events(&pb::StreamEventsRequest::default(), idle)
        .await
        .expect("the headers arrive; the body is silent");
    futures_util::pin_mut!(stream);
    let err = stream.next().await.expect("an error").expect_err("idle");
    let elapsed = started.elapsed();
    assert!(err.is_idle(), "{err}");
    assert_eq!(err.message, "stream idle");
    assert!(
        elapsed < idle + Duration::from_millis(500),
        "the idle budget bounded the wait: {elapsed:?}"
    );
}

#[tokio::test]
async fn a_stream_with_no_headers_yet_is_idle_within_the_budget() {
    // A stream's headers arrive with its first message: a daemon with nothing
    // to say yet sends no bytes at all, so the open itself is bounded.
    let stub: Stub = Arc::new(|_req: Request<Incoming>| {
        Box::pin(async move {
            tokio::time::sleep(Duration::from_secs(30)).await;
            Ok(with_epoch(Response::new(full(Vec::new()))))
        })
    });
    let url = serve(stub).await;
    let client = Client::new(&url, None).expect("a client");
    let idle = Duration::from_millis(150);
    let started = Instant::now();
    let err = client
        .stream_events(&pb::StreamEventsRequest::default(), idle)
        .await
        .expect_err("idle");
    let elapsed = started.elapsed();
    assert!(err.is_idle(), "{err}");
    assert!(
        elapsed < idle + Duration::from_millis(500),
        "the idle budget bounded the open: {elapsed:?}"
    );
}

/// Serve both codecs and record every request's Authorization header.
async fn auth_stub() -> (String, Arc<Mutex<Vec<(String, Option<String>)>>>) {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let recorded = seen.clone();
    let stub: Stub = Arc::new(move |req: Request<Incoming>| {
        let recorded = recorded.clone();
        Box::pin(async move {
            let streaming =
                header_of(&req, "content-type").as_deref() == Some("application/connect+proto");
            recorded.lock().expect("lock").push((
                if streaming {
                    "stream".to_string()
                } else {
                    "unary".to_string()
                },
                header_of(&req, "authorization"),
            ));
            let response = if streaming {
                Response::new(frames(vec![end_of_stream(r#"{"error": null}"#)]))
            } else {
                Response::new(full(pb::ListModelsResponse::default().encode_to_vec()))
            };
            Ok(with_epoch(response))
        })
    });
    (serve(stub).await, seen)
}

async fn call_both(client: &Client) {
    client
        .list_models(&pb::ListModelsRequest::default())
        .await
        .expect("list_models");
    let stream = client
        .stream_events(&pb::StreamEventsRequest::default(), Duration::from_secs(5))
        .await
        .expect("stream_events");
    futures_util::pin_mut!(stream);
    assert!(stream.next().await.is_none(), "the stub ends the stream");
}

#[tokio::test]
async fn the_token_rides_both_unary_and_stream_requests() {
    let (url, seen) = auth_stub().await;
    let client = Client::new(&url, Some("t0ken".to_string())).expect("a client");
    call_both(&client).await;
    let seen = seen.lock().expect("lock");
    assert_eq!(seen.len(), 2, "{seen:?}");
    for (kind, authorization) in seen.iter() {
        assert_eq!(authorization.as_deref(), Some("Bearer t0ken"), "{kind}");
    }
}

#[tokio::test]
async fn no_token_means_no_authorization_header() {
    let (url, seen) = auth_stub().await;
    let client = Client::new(&url, None).expect("a client");
    call_both(&client).await;
    let seen = seen.lock().expect("lock");
    assert_eq!(seen.len(), 2, "{seen:?}");
    for (kind, authorization) in seen.iter() {
        assert_eq!(authorization, &None, "{kind}");
    }
}

/// The bounds a consumer may rely on: a stream is a `futures_core::Stream`
/// that can be held across an await, and the client is shareable between tasks.
#[test]
fn the_public_bounds_hold() {
    fn stream_bounds<T>()
    where
        T: futures_core::Stream<Item = Result<pb::Event, rafiki::Error>> + Send + Unpin + 'static,
    {
    }
    fn client_bounds<T: Send + Sync + 'static>() {}
    stream_bounds::<rafiki::Streaming<pb::Event>>();
    client_bounds::<Client>();
}

/// A stub that answers every request with `status`, `body` and, when given,
/// that `Rafiki-Protocol`.
fn fixed(status: u16, body: &'static str, epoch: Option<&'static str>) -> Stub {
    Arc::new(move |_req: Request<Incoming>| {
        Box::pin(async move {
            let mut response = Response::new(full(body));
            *response.status_mut() = http::StatusCode::from_u16(status).expect("a status");
            if let Some(epoch) = epoch {
                response
                    .headers_mut()
                    .insert("rafiki-protocol", HeaderValue::from_static(epoch));
            }
            Ok(response)
        })
    })
}

/// A reverse proxy in front of a restarting daemon answers 502 with its own
/// page and no rafiki header. That is the retryable outage it looks like,
/// never a terminal protocol mismatch — on either codec.
#[tokio::test]
async fn a_proxy_502_without_the_epoch_header_is_unavailable() {
    let url = serve(fixed(502, "<html>bad gateway</html>", None)).await;
    let client = Client::new(&url, None).expect("a client");

    let err = client
        .get_history(&pb::GetHistoryRequest::default())
        .await
        .expect_err("a proxy outage");
    assert!(err.is_unavailable(), "unary: {err}");
    assert!(!err.is_protocol_mismatch(), "unary: {err}");

    let err = client
        .stream_events(&pb::StreamEventsRequest::default(), Duration::from_secs(5))
        .await
        .expect_err("a proxy outage");
    assert!(err.is_unavailable(), "stream: {err}");
    assert!(!err.is_protocol_mismatch(), "stream: {err}");
}

/// A Connect error body with no rafiki header is a daemon from before the
/// epoch existed: a mismatch, not the error it names.
#[tokio::test]
async fn a_connect_error_without_the_epoch_header_is_a_protocol_mismatch() {
    let body = r#"{"code": "unimplemented", "message": "no such procedure"}"#;
    let url = serve(fixed(404, body, None)).await;
    let client = Client::new(&url, None).expect("a client");
    let err = client
        .get_history(&pb::GetHistoryRequest::default())
        .await
        .expect_err("a pre-epoch daemon");
    assert!(err.is_protocol_mismatch(), "{err}");
    assert!(
        err.message.contains("upgrade the daemon"),
        "{}",
        err.message
    );
}

/// A daemon NEWER than this client must not be told to upgrade: the client is
/// the one behind.
#[tokio::test]
async fn a_newer_daemon_says_upgrade_this_client() {
    let url = serve(fixed(200, "", Some("5"))).await;
    let client = Client::new(&url, None).expect("a client");
    let err = client
        .get_history(&pb::GetHistoryRequest::default())
        .await
        .expect_err("a newer daemon");
    assert!(err.is_protocol_mismatch(), "{err}");
    assert!(
        err.message.contains("upgrade this client"),
        "{}",
        err.message
    );
    assert!(err.message.contains("protocol 5"), "{}", err.message);
}

/// A unary timeout leaves the call's outcome unknown, so it is
/// `deadline_exceeded` but never `is_idle()` — a caller resuming on idle must
/// not resend a `send` whose fate it does not know.
#[tokio::test]
async fn a_unary_timeout_is_not_idle() {
    let stub: Stub = Arc::new(|_req: Request<Incoming>| {
        Box::pin(async move {
            tokio::time::sleep(Duration::from_secs(30)).await;
            Ok(with_epoch(Response::new(full(Vec::new()))))
        })
    });
    let url = serve(stub).await;
    let client = Client::new(&url, None)
        .expect("a client")
        .with_timeout(Duration::from_millis(150));
    let err = client
        .send(&pb::SendRequest::default())
        .await
        .expect_err("a timeout");
    assert_eq!(err.code, rafiki::Code::DeadlineExceeded, "{err}");
    assert!(!err.is_idle(), "{err}");
}
