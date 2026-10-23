// SPDX-License-Identifier: Apache-2.0
//! The transport: a Connect client over TCP/TLS or a unix socket, with the
//! protocol headers and the epoch check in exactly one place.
//!
//! Wire facts this module owns (`docs/reference/control-protocol.md` §2.3):
//!
//! - unary: POST `{base}/rafiki.v1.Control/{Method}`, `Content-Type:
//!   application/proto`; a 200 body is the response message, anything else is a
//!   Connect error body.
//! - server-streaming: same path, `Content-Type: application/connect+proto`,
//!   the request body ONE envelope, the response a sequence of envelopes whose
//!   flags byte's bit 1 marks the end-of-stream trailer.
//! - a stream's headers arrive with its FIRST message, not before: a stream
//!   with nothing to say yet sends no bytes at all, so a client budgets an
//!   idle window and surfaces its expiry as `deadline_exceeded` — a quiet
//!   stream is the normal case, and the caller decides whether quiet means
//!   resume or give up.
//!
//! Nothing here retries. `unavailable` is the daemon-restarting signal a caller
//! may retry on; `deadline_exceeded` is re-opened with the caller's cursor.

use std::fmt;
use std::future::Future;
use std::marker::PhantomData;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use bytes::Bytes;
use http::{header, HeaderName, HeaderValue, Method, Request, Response, Uri, Version};
use http_body_util::{BodyExt, Full};
use hyper::body::{Body as _, Incoming};
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::client::legacy::Client as HttpClient;
use hyper_util::rt::{TokioExecutor, TokioIo};
use tokio::time::{sleep, Instant, Sleep};
use tower_service::Service;

use crate::envelope::{self, Envelope, EnvelopeDecoder, FLAG_COMPRESSED, FLAG_END};
use crate::error;
use crate::{Code, Error, EPOCH, EPOCH_HEADER};

/// The Connect service every verb addresses.
const SERVICE: &str = "rafiki.v1.Control";

/// The unary codec: protobuf, not JSON.
const CONTENT_UNARY: &str = "application/proto";

/// The enveloped server-streaming codec.
const CONTENT_STREAM: &str = "application/connect+proto";

/// Connect's own protocol version, sent on every request.
const CONNECT_PROTOCOL_VERSION: &str = "1";

/// Header names are case-insensitive; these are the lowercase spellings used
/// to write them. [`EPOCH_HEADER`]'s `Rafiki-Protocol` is the same header.
const CONNECT_PROTOCOL_VERSION_HEADER: &str = "connect-protocol-version";
const EPOCH_HEADER_LOWER: &str = "rafiki-protocol";

/// The reserved invalid host a unix-socket endpoint addresses, matching the
/// Python SDK and the Go client's `connectUDSBaseURL`: a misconfigured dial
/// fails loudly instead of reaching a real host.
const UDS_BASE: &str = "http://connect.rafiki.invalid";

/// The default unary budget; [`Client::with_timeout`] overrides it.
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// The request body type both codecs use.
type ReqBody = Full<Bytes>;

/// A parsed endpoint: where requests dial, and the base a request path is
/// appended to.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Endpoint {
    /// `https://…` (rustls + ring) or `http://…` (plain TCP).
    Tcp { base: String, tls: bool },
    /// `unix:///abs/path` or a bare `/abs/path`.
    Unix { base: String, path: PathBuf },
}

fn invalid_endpoint(endpoint: &str) -> Error {
    Error::new(
        Code::InvalidArgument,
        format!(
            "endpoint {endpoint:?}: not a unix socket path, unix:/// or http(s):// URL"
        ),
    )
}

fn parse_endpoint(endpoint: &str) -> Result<Endpoint, Error> {
    if let Some(path) = endpoint.strip_prefix("unix://") {
        if !path.starts_with('/') {
            return Err(invalid_endpoint(endpoint));
        }
        return Ok(Endpoint::Unix {
            base: UDS_BASE.to_string(),
            path: PathBuf::from(path),
        });
    }
    if endpoint.starts_with('/') {
        return Ok(Endpoint::Unix {
            base: UDS_BASE.to_string(),
            path: PathBuf::from(endpoint),
        });
    }
    let (tls, rest) = if let Some(rest) = endpoint.strip_prefix("https://") {
        (true, rest)
    } else if let Some(rest) = endpoint.strip_prefix("http://") {
        (false, rest)
    } else {
        return Err(invalid_endpoint(endpoint));
    };
    // `host[:port]` before the optional `/prefix`, and nothing may make the
    // authority empty or carry a query into every request path.
    let authority_end = rest.find('/').unwrap_or(rest.len());
    if authority_end == 0 {
        return Err(invalid_endpoint(endpoint));
    }
    // A prefix is allowed (`https://host/rafiki`); a trailing slash is not
    // carried into every request path.
    let base = endpoint.trim_end_matches('/').to_string();
    let uri: Uri = base.parse().map_err(|_| invalid_endpoint(endpoint))?;
    let host_ok = uri.host().map(|host| !host.is_empty()).unwrap_or(false);
    if !host_ok || uri.authority().is_none() || uri.query().is_some() || base.contains('#') {
        return Err(invalid_endpoint(endpoint));
    }
    Ok(Endpoint::Tcp { base, tls })
}

/// The dialer for a unix-socket endpoint: it ignores the URI it is handed (the
/// reserved `http://connect.rafiki.invalid` base) and dials the path.
#[derive(Debug, Clone)]
struct UnixConnector {
    path: Arc<PathBuf>,
}

impl Service<Uri> for UnixConnector {
    type Response = TokioIo<tokio::net::UnixStream>;
    type Error = std::io::Error;
    type Future = Pin<Box<dyn Future<Output = Result<Self::Response, Self::Error>> + Send>>;

    fn poll_ready(&mut self, _cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, _uri: Uri) -> Self::Future {
        let path = self.path.clone();
        Box::pin(async move {
            let stream = tokio::net::UnixStream::connect(&*path).await?;
            Ok(TokioIo::new(stream))
        })
    }
}

/// Which client dials. One variant per endpoint form, so an `http://` endpoint
/// never loads root certificates and a unix endpoint never resolves a host.
enum Transport {
    Https(HttpClient<hyper_rustls::HttpsConnector<HttpConnector>, ReqBody>),
    Http(HttpClient<HttpConnector, ReqBody>),
    Unix(HttpClient<UnixConnector, ReqBody>),
}

impl Transport {
    async fn request(
        &self,
        request: Request<ReqBody>,
    ) -> Result<Response<Incoming>, hyper_util::client::legacy::Error> {
        match self {
            Transport::Https(client) => client.request(request).await,
            Transport::Http(client) => client.request(request).await,
            Transport::Unix(client) => client.request(request).await,
        }
    }
}

/// A client for one daemon: a unix socket, or a TCP listener with or without
/// TLS.
pub struct Client {
    transport: Transport,
    base: String,
    token: Option<String>,
    timeout: Duration,
}

impl fmt::Debug for Client {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        // The token is a credential: never in a log line or a Debug dump.
        f.debug_struct("Client")
            .field("base", &self.base)
            .field("token", &self.token.as_ref().map(|_| "<redacted>"))
            .field("timeout", &self.timeout)
            .finish()
    }
}

impl Client {
    /// A client for `endpoint`, presenting `token` when one is given.
    ///
    /// Accepted spellings: `https://host[:port][/prefix]` (TLS over rustls +
    /// ring), `http://host[:port][/prefix]` (plain TCP, for a `rafikid --dev`
    /// loopback), `unix:///abs/path` and a bare `/abs/path`. Anything else is
    /// [`Code::InvalidArgument`].
    pub fn new(endpoint: &str, token: Option<String>) -> Result<Client, Error> {
        let (transport, base) = match parse_endpoint(endpoint)? {
            Endpoint::Tcp { base, tls: true } => (tls_transport()?, base),
            Endpoint::Tcp { base, tls: false } => (
                Transport::Http(HttpClient::builder(TokioExecutor::new()).build(HttpConnector::new())),
                base,
            ),
            Endpoint::Unix { base, path } => (
                Transport::Unix(
                    HttpClient::builder(TokioExecutor::new()).build(UnixConnector {
                        path: Arc::new(path),
                    }),
                ),
                base,
            ),
        };
        Ok(Client {
            transport,
            base,
            token,
            timeout: DEFAULT_TIMEOUT,
        })
    }

    /// The unary budget, covering connect + response for one call (default
    /// 30s). Exceeding it is [`Code::DeadlineExceeded`].
    pub fn with_timeout(mut self, timeout: Duration) -> Client {
        self.timeout = timeout;
        self
    }

    /// The base requests are addressed to — the endpoint, minus a trailing
    /// slash (`http://connect.rafiki.invalid` for a unix socket).
    pub fn base(&self) -> &str {
        &self.base
    }

    /// One unary call: `method` is the RPC name (`"GetHistory"`).
    pub async fn unary<Req, Resp>(&self, method: &str, req: &Req) -> Result<Resp, Error>
    where
        Req: prost::Message,
        Resp: prost::Message + Default,
    {
        let request = self.build_request(method, CONTENT_UNARY, Bytes::from(req.encode_to_vec()))?;
        let call = async {
            let response = self
                .transport
                .request(request)
                .await
                .map_err(|err| self.unavailable(method, &err))?;
            let status = response.status().as_u16();
            let peer = peer_epoch(response.headers());
            let body = response
                .into_body()
                .collect()
                .await
                .map_err(|err| self.unavailable(method, &err))?
                .to_bytes();
            let failure = (status != 200).then(|| error::from_body(status, &body));
            check_epoch(peer.as_deref(), failure.as_ref(), status)?;
            match failure {
                Some(err) => Err(err),
                // An empty 200 body is the empty message, as Connect defines.
                None if body.is_empty() => Ok(Resp::default()),
                None => Resp::decode(body).map_err(|err| {
                    Error::new(
                        Code::Internal,
                        format!("{method} against {}: malformed response body: {err}", self.base),
                    )
                    .with_status(200)
                }),
            }
        };
        match tokio::time::timeout(self.timeout, call).await {
            Ok(result) => result,
            Err(_) => Err(Error::new(
                Code::DeadlineExceeded,
                format!(
                    "{method} against {}: timed out after {:?}",
                    self.base, self.timeout
                ),
            )),
        }
    }

    /// One server-streaming call: `method` is the RPC name (`"StreamEvents"`).
    ///
    /// `idle` bounds waiting for the response headers and, separately, waiting
    /// for each next body frame. Its expiry is [`Code::DeadlineExceeded`] with
    /// the message `stream idle`; the SDK never re-opens — callers resume with
    /// their own cursor.
    pub async fn server_stream<Req, Resp>(
        &self,
        method: &str,
        req: &Req,
        idle: Duration,
    ) -> Result<Streaming<Resp>, Error>
    where
        Req: prost::Message,
        Resp: prost::Message + Default,
    {
        let body = envelope::envelop(&req.encode_to_vec());
        let request = self.build_request(method, CONTENT_STREAM, body)?;
        let response = match tokio::time::timeout(idle, self.transport.request(request)).await {
            Ok(Ok(response)) => response,
            Ok(Err(err)) => return Err(self.unavailable(method, &err)),
            Err(_) => return Err(idle_error()),
        };
        let status = response.status().as_u16();
        let peer = peer_epoch(response.headers());
        if status != 200 {
            let body = match tokio::time::timeout(idle, response.into_body().collect()).await {
                Ok(Ok(collected)) => collected.to_bytes(),
                Ok(Err(err)) => return Err(self.unavailable(method, &err)),
                Err(_) => return Err(idle_error()),
            };
            let err = error::from_body(status, &body);
            check_epoch(peer.as_deref(), Some(&err), status)?;
            return Err(err);
        }
        check_epoch(peer.as_deref(), None, status)?;
        Ok(Streaming::new(response.into_body(), idle))
    }

    /// The ONE place a request is built, so every header — the Connect
    /// version, the epoch, and the bearer token — is attached for unary and
    /// streaming calls alike and never at an individual call site.
    fn build_request(
        &self,
        method: &str,
        content_type: &'static str,
        body: Bytes,
    ) -> Result<Request<ReqBody>, Error> {
        let uri: Uri = format!("{}/{SERVICE}/{method}", self.base)
            .parse()
            .map_err(|err| {
                Error::new(
                    Code::Internal,
                    format!("{}: invalid request URI: {err}", self.base),
                )
            })?;
        let mut request = Request::new(Full::new(body));
        *request.method_mut() = Method::POST;
        *request.uri_mut() = uri;
        *request.version_mut() = Version::HTTP_11;
        let headers = request.headers_mut();
        // Header names are case-insensitive, and these are the lowercase
        // spellings of the same two the wire documents.
        headers.insert(header::CONTENT_TYPE, HeaderValue::from_static(content_type));
        headers.insert(
            HeaderName::from_static(CONNECT_PROTOCOL_VERSION_HEADER),
            HeaderValue::from_static(CONNECT_PROTOCOL_VERSION),
        );
        headers.insert(
            HeaderName::from_static(EPOCH_HEADER_LOWER),
            // `EPOCH_HEADER`'s value is this client's epoch.
            HeaderValue::from_static(EPOCH),
        );
        if let Some(token) = &self.token {
            let value = HeaderValue::from_str(&format!("Bearer {token}")).map_err(|_| {
                Error::new(Code::InvalidArgument, "token is not a valid HTTP header value")
            })?;
            headers.insert(header::AUTHORIZATION, value);
        }
        Ok(request)
    }

    /// A dial or read failure: the daemon was not reachable. Never retried
    /// here; the endpoint stays in the message so the caller can see which one.
    fn unavailable(&self, method: &str, err: &dyn fmt::Display) -> Error {
        Error::new(
            Code::Unavailable,
            format!("{method} against {}: {err}", self.base),
        )
    }
}

fn tls_transport() -> Result<Transport, Error> {
    // rustls + ring, explicitly: the process default provider is never
    // consulted, so an aws-lc-rs build anywhere else cannot change the
    // handshake we run.
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let connector = hyper_rustls::HttpsConnectorBuilder::new()
        .with_provider_and_native_roots(provider)
        .map_err(|err| {
            Error::new(
                Code::Internal,
                format!("loading native root certificates: {err}"),
            )
        })?
        .https_or_http()
        .enable_http1()
        .build();
    Ok(Transport::Https(
        HttpClient::builder(TokioExecutor::new()).build(connector),
    ))
}

/// The `Rafiki-Protocol` a response carried, if any.
fn peer_epoch(headers: &http::HeaderMap) -> Option<String> {
    headers
        .get(EPOCH_HEADER)
        .and_then(|value| value.to_str().ok())
        .map(str::to_string)
}

/// Refuse a response from a daemon that speaks another epoch.
///
/// The daemon's OWN `protocol_mismatch` is never masked: when `daemon_error`
/// already carries that reason the caller sees the daemon's message, which is
/// the more specific one ("upgrade your rafiki client"), not ours.
fn check_epoch(
    peer: Option<&str>,
    daemon_error: Option<&Error>,
    status: u16,
) -> Result<(), Error> {
    if peer == Some(EPOCH) {
        return Ok(());
    }
    if let Some(err) = daemon_error {
        if err.is_protocol_mismatch() {
            return Err(err.clone());
        }
    }
    Err(Error {
        code: Code::FailedPrecondition,
        message: format!(
            "daemon speaks rafiki protocol {}; this client speaks {EPOCH} — upgrade the daemon",
            peer.unwrap_or("none")
        ),
        reason: Some(error::PROTOCOL_MISMATCH_REASON.to_string()),
        http_status: Some(status),
    })
}

fn idle_error() -> Error {
    Error::new(Code::DeadlineExceeded, "stream idle")
}

/// The body ended without an end-of-stream envelope. A Connect server stream
/// terminates with a flags-`0x02` envelope, so EOF at an envelope boundary is
/// a TRUNCATED stream, never a clean close: reporting it as a clean end would
/// make a dropped connection look like a daemon that finished talking.
fn truncated_error() -> Error {
    Error::new(
        Code::Unavailable,
        "stream closed without an end-of-stream envelope",
    )
}

/// One decoded envelope's outcome.
enum Next<T> {
    Item(Result<T, Error>),
    /// The end-of-stream trailer said the stream closed cleanly.
    End,
    /// Nothing to yield (an empty payload).
    Continue,
}

/// A server stream's messages, as `futures_core::Stream<Item = Result<T,
/// Error>>`.
///
/// Dropping it closes the stream. An idle expiry ends it with
/// [`Error::is_idle`]; the SDK never re-opens it. It ends with `None` ONLY
/// after a clean end-of-stream envelope: a transport EOF without one is
/// [`Code::Unavailable`], because a truncated stream must never look
/// complete.
pub struct Streaming<T> {
    body: Pin<Box<Incoming>>,
    decoder: EnvelopeDecoder,
    idle: Duration,
    sleep: Pin<Box<Sleep>>,
    /// Whether `sleep` is running: armed when a frame is awaited, disarmed
    /// when one arrives, so every wait gets its own full window.
    armed: bool,
    /// The body has ended.
    eof: bool,
    /// A flags-`0x02` end-of-stream envelope said the stream closed. Until one
    /// has been consumed, a body EOF is a truncation.
    ended: bool,
    done: bool,
    _message: PhantomData<fn() -> T>,
}

impl<T> fmt::Debug for Streaming<T> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Streaming")
            .field("idle", &self.idle)
            .field("eof", &self.eof)
            .field("ended", &self.ended)
            .field("done", &self.done)
            .finish()
    }
}

impl<T> Streaming<T> {
    fn new(body: Incoming, idle: Duration) -> Streaming<T> {
        Streaming {
            body: Box::pin(body),
            decoder: EnvelopeDecoder::new(),
            idle,
            sleep: Box::pin(sleep(idle)),
            armed: false,
            eof: false,
            ended: false,
            done: false,
            _message: PhantomData,
        }
    }

    /// What one envelope means for the stream.
    fn take(&mut self, envelope: Envelope) -> Next<T>
    where
        T: prost::Message + Default,
    {
        if envelope.flags & FLAG_COMPRESSED != 0 {
            return Next::Item(Err(
                Error::new(Code::Unimplemented, "compressed envelopes are not supported")
                    .with_status(200),
            ));
        }
        if envelope.flags & FLAG_END != 0 {
            return match error::from_trailer(&envelope.payload) {
                Some(err) => Next::Item(Err(err)),
                None => Next::End,
            };
        }
        if envelope.payload.is_empty() {
            return Next::Continue;
        }
        Next::Item(T::decode(envelope.payload).map_err(|err| {
            Error::new(
                Code::Internal,
                format!("malformed stream payload: {err}"),
            )
            .with_status(200)
        }))
    }
}

impl<T: prost::Message + Default> futures_core::Stream for Streaming<T> {
    type Item = Result<T, Error>;

    fn poll_next(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        let this = self.get_mut();
        if this.done {
            return Poll::Ready(None);
        }
        loop {
            match this.decoder.next() {
                Ok(Some(envelope)) => {
                    this.armed = false;
                    match this.take(envelope) {
                        Next::Item(item) => {
                            if item.is_err() {
                                this.done = true;
                            }
                            return Poll::Ready(Some(item));
                        }
                        Next::End => {
                            this.ended = true;
                            this.done = true;
                            return Poll::Ready(None);
                        }
                        Next::Continue => continue,
                    }
                }
                Ok(None) => {
                    if this.eof {
                        this.done = true;
                        // No end-of-stream envelope: the connection ended
                        // mid-stream, and saying `None` would call a truncated
                        // stream complete.
                        return if this.ended {
                            Poll::Ready(None)
                        } else {
                            Poll::Ready(Some(Err(truncated_error())))
                        };
                    }
                }
                Err(err) => {
                    this.done = true;
                    return Poll::Ready(Some(Err(err)));
                }
            }
            if !this.armed {
                this.sleep.as_mut().reset(Instant::now() + this.idle);
                this.armed = true;
            }
            match this.body.as_mut().poll_frame(cx) {
                Poll::Ready(Some(Ok(frame))) => {
                    this.armed = false;
                    // Data frames carry envelope bytes. A trailing frame is
                    // not an error channel here (Connect reports errors in the
                    // end-of-stream envelope), so it has nothing to read.
                    if let Ok(data) = frame.into_data() {
                        this.decoder.feed(&data);
                    }
                }
                Poll::Ready(Some(Err(err))) => {
                    this.done = true;
                    return Poll::Ready(Some(Err(Error::new(
                        Code::Unavailable,
                        format!("reading stream: {err}"),
                    ))));
                }
                Poll::Ready(None) => {
                    this.armed = false;
                    this.eof = true;
                    this.decoder.finish();
                }
                Poll::Pending => {
                    if this.sleep.as_mut().poll(cx).is_ready() {
                        this.done = true;
                        return Poll::Ready(Some(Err(idle_error())));
                    }
                    return Poll::Pending;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tcp(endpoint: &str) -> (String, bool) {
        match parse_endpoint(endpoint).expect("a tcp endpoint") {
            Endpoint::Tcp { base, tls } => (base, tls),
            other => panic!("{endpoint} parsed as {other:?}"),
        }
    }

    fn unix_path(endpoint: &str) -> (String, PathBuf) {
        match parse_endpoint(endpoint).expect("a unix endpoint") {
            Endpoint::Unix { base, path } => (base, path),
            other => panic!("{endpoint} parsed as {other:?}"),
        }
    }

    #[test]
    fn endpoint_parsing() {
        assert_eq!(
            tcp("https://daemon.example:8036"),
            ("https://daemon.example:8036".to_string(), true)
        );
        assert_eq!(
            tcp("https://daemon.example:8036/"),
            ("https://daemon.example:8036".to_string(), true)
        );
        assert_eq!(
            tcp("https://daemon.example/rafiki/"),
            ("https://daemon.example/rafiki".to_string(), true)
        );
        assert_eq!(
            tcp("http://127.0.0.1:8035"),
            ("http://127.0.0.1:8035".to_string(), false)
        );
        assert_eq!(
            unix_path("unix:///run/rafiki/controller.sock"),
            (UDS_BASE.to_string(), PathBuf::from("/run/rafiki/controller.sock"))
        );
        assert_eq!(
            unix_path("/home/u/.local/state/rafiki/controller.sock"),
            (
                UDS_BASE.to_string(),
                PathBuf::from("/home/u/.local/state/rafiki/controller.sock")
            )
        );
        for garbage in [
            "",
            "daemon.example:8036",
            "ftp://daemon.example",
            "unix://relative.sock",
            "unix://",
            "https://",
            "http:///no-host",
            "https://daemon.example?x=1",
            "RAFIKI_SOCKET",
        ] {
            let err = parse_endpoint(garbage).expect_err(garbage);
            assert_eq!(err.code, Code::InvalidArgument, "{garbage:?}");
        }
    }

    #[test]
    fn a_client_never_prints_its_token() {
        let client = Client::new("http://127.0.0.1:9", Some("s3cret".to_string()))
            .expect("a plain endpoint needs no certificate store");
        let rendered = format!("{client:?}");
        assert!(!rendered.contains("s3cret"), "{rendered}");
        assert!(rendered.contains("<redacted>"), "{rendered}");
    }
}
