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
use std::net::SocketAddr;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::{Duration, SystemTime};

use bytes::Bytes;
use http::{header, HeaderName, HeaderValue, Method, Request, Response, Uri, Version};
use http_body_util::{BodyExt, Full, LengthLimitError, Limited};
use hyper::body::{Body as _, Incoming};
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::client::legacy::Client as HttpClient;
use hyper_util::rt::{TokioExecutor, TokioIo};
use tokio::time::{sleep, Instant, Sleep};
use tower_service::Service;

use crate::envelope::{self, Envelope, EnvelopeDecoder, FLAG_COMPRESSED, FLAG_END, MAX_PAYLOAD};
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

/// TCP keepalive on every dialled connection: the OS notices a dead peer (a
/// laptop that slept, expired NAT state) with no protocol traffic at all. A
/// reverse proxy's read timeout counts data, not keepalive probes, so this
/// complements a stream's idle window rather than replacing it.
const TCP_KEEPALIVE: Duration = Duration::from_secs(30);
const TCP_KEEPALIVE_INTERVAL: Duration = Duration::from_secs(10);

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
        format!("endpoint {endpoint:?}: not a unix socket path, unix:/// or http(s):// URL"),
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

/// The dialer behind [`Client::new_with_dial`]: it ignores the URI it is handed
/// and always connects to `addr`. The TLS layer, when present, wraps this and
/// still takes SNI and verification from the URI's host.
#[derive(Clone)]
struct DialConnector {
    tcp: HttpConnector,
    target: Uri,
}

impl DialConnector {
    fn new(addr: SocketAddr) -> DialConnector {
        DialConnector {
            tcp: tcp_connector(),
            target: Uri::try_from(format!("http://{addr}"))
                .expect("a socket address is a valid URI authority"),
        }
    }
}

impl Service<Uri> for DialConnector {
    type Response = TokioIo<tokio::net::TcpStream>;
    type Error = <HttpConnector as Service<Uri>>::Error;
    type Future = Pin<Box<dyn Future<Output = Result<Self::Response, Self::Error>> + Send>>;

    fn poll_ready(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        self.tcp.poll_ready(cx)
    }

    fn call(&mut self, _uri: Uri) -> Self::Future {
        Box::pin(self.tcp.call(self.target.clone()))
    }
}

/// Which client dials. One variant per endpoint form, so an `http://` endpoint
/// never loads root certificates and a unix endpoint never resolves a host.
#[derive(Clone)]
enum Transport {
    Https(HttpClient<hyper_rustls::HttpsConnector<HttpConnector>, ReqBody>),
    Http(HttpClient<HttpConnector, ReqBody>),
    HttpsDial(HttpClient<hyper_rustls::HttpsConnector<DialConnector>, ReqBody>),
    HttpDial(HttpClient<DialConnector, ReqBody>),
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
            Transport::HttpsDial(client) => client.request(request).await,
            Transport::HttpDial(client) => client.request(request).await,
            Transport::Unix(client) => client.request(request).await,
        }
    }
}

/// A client for one daemon: a unix socket, or a TCP listener with or without
/// TLS. Cloning is cheap and shares the connection pool.
#[derive(Clone)]
pub struct Client {
    transport: Transport,
    base: String,
    token: Option<String>,
    timeout: Duration,
    /// The epoch sent in `Rafiki-Protocol`: always [`EPOCH`] outside the
    /// `test-hooks` feature.
    wire_epoch: &'static str,
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
    /// ring), `http://host[:port][/prefix]` (plain TCP, for a loopback
    /// listener), `unix:///abs/path` and a bare `/abs/path`. Anything else is
    /// [`Code::InvalidArgument`].
    pub fn new(endpoint: &str, token: Option<String>) -> Result<Client, Error> {
        let (transport, base) = match parse_endpoint(endpoint)? {
            Endpoint::Tcp { base, tls: true } => (tls_transport()?, base),
            Endpoint::Tcp { base, tls: false } => (
                Transport::Http(HttpClient::builder(TokioExecutor::new()).build(tcp_connector())),
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
            wire_epoch: EPOCH,
        })
    }

    /// Like [`Client::new`], but the TCP layer always connects to `dial` and
    /// never resolves or dials the URL's host or port.
    ///
    /// For `https://`, TLS still takes SNI and certificate verification from
    /// the URL's host, and `Host` / `:authority` still come from the URL. For
    /// `http://` this is plain TCP to `dial`. A unix-socket endpoint has no
    /// use for a dial override and is [`Code::InvalidArgument`].
    pub fn new_with_dial(
        endpoint: &str,
        token: Option<String>,
        dial: SocketAddr,
    ) -> Result<Client, Error> {
        Client::dialed(endpoint, token, dial, &[])
    }

    /// [`Client::new_with_dial`] that additionally trusts `extra_roots` (DER
    /// certificates), so a test can talk to a server whose certificate is not
    /// in the native roots. Used ONLY by `tests/dial.rs`; the production
    /// constructor delegates to the same builder with no extra roots.
    #[cfg(feature = "test-hooks")]
    pub fn new_with_dial_and_roots(
        endpoint: &str,
        token: Option<String>,
        dial: SocketAddr,
        extra_roots: &[Vec<u8>],
    ) -> Result<Client, Error> {
        Client::dialed(endpoint, token, dial, extra_roots)
    }

    fn dialed(
        endpoint: &str,
        token: Option<String>,
        dial: SocketAddr,
        extra_roots: &[Vec<u8>],
    ) -> Result<Client, Error> {
        let dial = DialConnector::new(dial);
        let (transport, base) = match parse_endpoint(endpoint)? {
            Endpoint::Tcp { base, tls: true } => (
                Transport::HttpsDial(
                    HttpClient::builder(TokioExecutor::new())
                        .build(https_connector(dial, extra_roots)?),
                ),
                base,
            ),
            Endpoint::Tcp { base, tls: false } => (
                Transport::HttpDial(HttpClient::builder(TokioExecutor::new()).build(dial)),
                base,
            ),
            Endpoint::Unix { .. } => {
                return Err(Error::new(
                    Code::InvalidArgument,
                    "a dial override has no meaning for a unix socket endpoint",
                ))
            }
        };
        Ok(Client {
            transport,
            base,
            token,
            timeout: DEFAULT_TIMEOUT,
            wire_epoch: EPOCH,
        })
    }

    /// Send `epoch` instead of [`EPOCH`], so a live-daemon test can provoke
    /// the daemon's own `protocol_mismatch` refusal. Test builds only.
    #[cfg(feature = "test-hooks")]
    pub fn with_wire_epoch(mut self, epoch: &'static str) -> Client {
        self.wire_epoch = epoch;
        self
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
        let request =
            self.build_request(method, CONTENT_UNARY, Bytes::from(req.encode_to_vec()))?;
        let call = async {
            let response = self
                .transport
                .request(request)
                .await
                .map_err(|err| self.unavailable(method, &err))?;
            let status = response.status().as_u16();
            let peer = peer_epoch(response.headers());
            let body = Limited::new(response.into_body(), MAX_PAYLOAD)
                .collect()
                .await
                .map_err(|err| self.body_error(method, err))?
                .to_bytes();
            let failure = (status != 200).then(|| error::from_body(status, &body));
            let from_connect = failure.as_ref().is_none_or(|(_, connect)| *connect);
            let failure = failure.map(|(err, _)| err);
            check_epoch(peer.as_deref(), failure.as_ref(), from_connect, status)?;
            match failure {
                Some(err) => Err(err),
                // An empty 200 body is the empty message, as Connect defines.
                None if body.is_empty() => Ok(Resp::default()),
                None => Resp::decode(body).map_err(|err| {
                    Error::new(
                        Code::Internal,
                        format!(
                            "{method} against {}: malformed response body: {err}",
                            self.base
                        ),
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
    /// the message `stream idle`. This call never re-opens;
    /// [`Client::follow_events`] does.
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
            Err(_) => return Err(Error::stream_idle()),
        };
        let status = response.status().as_u16();
        let peer = peer_epoch(response.headers());
        if status != 200 {
            let collect = Limited::new(response.into_body(), MAX_PAYLOAD).collect();
            let body = match tokio::time::timeout(idle, collect).await {
                Ok(Ok(collected)) => collected.to_bytes(),
                Ok(Err(err)) => return Err(self.body_error(method, err)),
                Err(_) => return Err(Error::stream_idle()),
            };
            let (err, from_connect) = error::from_body(status, &body);
            check_epoch(peer.as_deref(), Some(&err), from_connect, status)?;
            return Err(err);
        }
        check_epoch(peer.as_deref(), None, true, status)?;
        let opened_at = response
            .headers()
            .get(header::DATE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| httpdate::parse_http_date(value).ok());
        Ok(Streaming::new(response.into_body(), idle, opened_at))
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
            HeaderValue::from_static(self.wire_epoch),
        );
        if let Some(token) = &self.token {
            let value = HeaderValue::from_str(&format!("Bearer {token}")).map_err(|_| {
                Error::new(
                    Code::InvalidArgument,
                    "token is not a valid HTTP header value",
                )
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

    /// A failure collecting a size-[`Limited`] body: over the cap is
    /// `resource_exhausted`, anything else a read failure.
    fn body_error(&self, method: &str, err: Box<dyn std::error::Error + Send + Sync>) -> Error {
        if err.is::<LengthLimitError>() {
            return Error::new(
                Code::ResourceExhausted,
                format!(
                    "{method} against {}: response body exceeds {MAX_PAYLOAD} bytes",
                    self.base
                ),
            );
        }
        self.unavailable(method, &err)
    }
}

/// The TCP dialer both TCP transports wrap, with keepalive on.
fn tcp_connector() -> HttpConnector {
    let mut connector = HttpConnector::new();
    connector.set_keepalive(Some(TCP_KEEPALIVE));
    connector.set_keepalive_interval(Some(TCP_KEEPALIVE_INTERVAL));
    connector
}

fn tls_transport() -> Result<Transport, Error> {
    let mut tcp = tcp_connector();
    // The TLS layer owns the scheme check; the TCP layer must accept the
    // https:// URIs it is handed.
    tcp.enforce_http(false);
    Ok(Transport::Https(
        HttpClient::builder(TokioExecutor::new()).build(https_connector(tcp, &[])?),
    ))
}

/// The TLS layer over `inner`: rustls + ring, SNI and verification from the
/// URI's host. `extra_roots` (DER) are trusted instead of the native roots
/// when non-empty; only the test-only dial constructor ever passes any.
fn https_connector<T>(
    inner: T,
    extra_roots: &[Vec<u8>],
) -> Result<hyper_rustls::HttpsConnector<T>, Error> {
    // rustls + ring, explicitly: the process default provider is never
    // consulted, so an aws-lc-rs build anywhere else cannot change the
    // handshake we run.
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let tls_error =
        |what: &str, err: &dyn fmt::Display| Error::new(Code::Internal, format!("{what}: {err}"));
    let builder = if extra_roots.is_empty() {
        hyper_rustls::HttpsConnectorBuilder::new()
            .with_provider_and_native_roots(provider)
            .map_err(|err| tls_error("loading native root certificates", &err))?
    } else {
        let mut roots = rustls::RootCertStore::empty();
        for der in extra_roots {
            roots
                .add(rustls::pki_types::CertificateDer::from(der.clone()))
                .map_err(|err| tls_error("adding extra root certificate", &err))?;
        }
        let config = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .map_err(|err| tls_error("selecting TLS protocol versions", &err))?
            .with_root_certificates(roots)
            .with_no_client_auth();
        hyper_rustls::HttpsConnectorBuilder::new().with_tls_config(config)
    };
    Ok(builder.https_only().enable_http1().wrap_connector(inner))
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
///
/// A missing header is a stale daemon only when a Connect server answered —
/// a 200, or an error body naming a Connect code (`from_connect`). A non-200
/// with neither came from something in front of the daemon (a reverse proxy,
/// a load balancer), so it is that status's error: a 502 while rafikid
/// restarts must stay a retryable `unavailable`, never a terminal mismatch.
fn check_epoch(
    peer: Option<&str>,
    daemon_error: Option<&Error>,
    from_connect: bool,
    status: u16,
) -> Result<(), Error> {
    if peer == Some(EPOCH) {
        return Ok(());
    }
    if let Some(err) = daemon_error {
        if err.is_protocol_mismatch() || (peer.is_none() && !from_connect) {
            return Err(err.clone());
        }
    }
    // Which side is behind decides the advice; an unparseable or missing peer
    // epoch is an old daemon (a peer without the header is epoch 1).
    let newer_daemon = matches!(
        (peer.and_then(|p| p.parse::<u32>().ok()), EPOCH.parse::<u32>().ok()),
        (Some(theirs), Some(ours)) if theirs > ours
    );
    let advice = if newer_daemon {
        "upgrade this client"
    } else {
        "upgrade the daemon"
    };
    Err(Error::new(
        Code::FailedPrecondition,
        format!(
            "daemon speaks rafiki protocol {}; this client speaks {EPOCH} — {advice}",
            peer.unwrap_or("none")
        ),
    )
    .with_reason(error::PROTOCOL_MISMATCH_REASON)
    .with_status(status))
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
/// [`Error::is_idle`], and nothing re-opens it ([`Client::follow_events`]
/// is the stream that does). It ends with `None` ONLY
/// after a clean end-of-stream envelope: a transport EOF without one is
/// [`Code::Unavailable`], because a truncated stream must never look
/// complete.
pub struct Streaming<T> {
    body: Pin<Box<Incoming>>,
    /// The server's `Date` when the stream opened.
    opened_at: Option<SystemTime>,
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
    fn new(body: Incoming, idle: Duration, opened_at: Option<SystemTime>) -> Streaming<T> {
        Streaming {
            body: Box::pin(body),
            opened_at,
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

    /// The server's clock when the stream opened, from its `Date` header (to
    /// the second), if it sent one. It is the server's time, not ours, which
    /// is what a replay `floor` is measured against.
    pub fn opened_at(&self) -> Option<SystemTime> {
        self.opened_at
    }

    /// What one envelope means for the stream.
    fn take(&mut self, envelope: Envelope) -> Next<T>
    where
        T: prost::Message + Default,
    {
        if envelope.flags & FLAG_COMPRESSED != 0 {
            return Next::Item(Err(Error::new(
                Code::Unimplemented,
                "compressed envelopes are not supported",
            )
            .with_status(200)));
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
            Error::new(Code::Internal, format!("malformed stream payload: {err}")).with_status(200)
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
                        return Poll::Ready(Some(Err(Error::stream_idle())));
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
            (
                UDS_BASE.to_string(),
                PathBuf::from("/run/rafiki/controller.sock")
            )
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
