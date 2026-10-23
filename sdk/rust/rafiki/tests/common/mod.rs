// SPDX-License-Identifier: Apache-2.0
//! The in-process daemon stub every offline test file shares: a hyper server
//! on 127.0.0.1:0 answering each request through a `Stub`.
#![allow(dead_code)] // each test crate uses a different subset

use std::convert::Infallible;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use bytes::Bytes;
use http::{HeaderValue, Request, Response};
use http_body_util::{combinators::BoxBody, BodyExt, Full, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::service::service_fn;
use hyper_util::rt::TokioIo;
use prost::Message as _;
use rafiki::pb;

/// What the stub server answers with.
pub type TestBody = BoxBody<Bytes, std::io::Error>;

/// What a stub answers, once it has read the request.
pub type StubFuture = Pin<Box<dyn Future<Output = Result<Response<TestBody>, Infallible>> + Send>>;

/// One stub handler.
pub type Stub = Arc<dyn Fn(Request<Incoming>) -> StubFuture + Send + Sync>;

pub fn full(bytes: impl Into<Bytes>) -> TestBody {
    Full::new(bytes.into())
        .map_err(|never| match never {})
        .boxed()
}

/// A body made of the given frames, in order.
pub fn frames(parts: Vec<Bytes>) -> TestBody {
    let stream = futures_util::stream::iter(
        parts
            .into_iter()
            .map(|part| Ok::<_, std::io::Error>(Frame::data(part))),
    );
    BodyExt::boxed(StreamBody::new(stream))
}

/// A body that never yields: headers, then silence.
pub fn silent() -> TestBody {
    BodyExt::boxed(StreamBody::new(futures_util::stream::pending::<
        Result<Frame<Bytes>, std::io::Error>,
    >()))
}

/// One Connect streaming envelope.
pub fn envelop(flags: u8, payload: &[u8]) -> Bytes {
    let mut out = vec![flags];
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    Bytes::from(out)
}

pub fn end_of_stream(json: &str) -> Bytes {
    envelop(0x02, json.as_bytes())
}

pub fn with_epoch(mut response: Response<TestBody>) -> Response<TestBody> {
    response
        .headers_mut()
        .insert("rafiki-protocol", HeaderValue::from_static("4"));
    response
}

pub fn header_of(req: &Request<Incoming>, name: &str) -> Option<String> {
    req.headers()
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::to_string)
}

/// Serve `handler` on 127.0.0.1:0 and return its base URL.
pub async fn serve(handler: Stub) -> String {
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0))
        .await
        .expect("bind loopback");
    let addr = listener.local_addr().expect("a bound address");
    tokio::spawn(async move {
        while let Ok((stream, _peer)) = listener.accept().await {
            let handler = handler.clone();
            tokio::spawn(async move {
                let service = service_fn(move |req: Request<Incoming>| {
                    let handler = handler.clone();
                    async move { handler(req).await }
                });
                if let Err(err) = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(stream), service)
                    .await
                {
                    // A client that hangs up mid-stream is a test's own doing.
                    eprintln!("stub connection ended: {err}");
                }
            });
        }
    });
    format!("http://{addr}")
}

/// One distinct event, so a decoded message is identifiable.
pub fn event(child_id: &str, ordinal: i32) -> pb::Event {
    pb::Event {
        child_id: child_id.to_string(),
        ordinal: Some(ordinal),
        ts: None,
        payload: None,
    }
}

/// Check a streaming request's shape and decode it: ONE envelope, flags 0, a
/// length that covers the rest of the body.
pub async fn read_stream_request(req: Request<Incoming>) -> pb::StreamEventsRequest {
    assert_eq!(
        header_of(&req, "content-type").as_deref(),
        Some("application/connect+proto")
    );
    let body = req
        .into_body()
        .collect()
        .await
        .expect("the request body")
        .to_bytes();
    assert_eq!(body[0], 0);
    let length = u32::from_be_bytes([body[1], body[2], body[3], body[4]]) as usize;
    assert_eq!(body.len(), 5 + length);
    pb::StreamEventsRequest::decode(&body[5..]).expect("a StreamEventsRequest")
}
