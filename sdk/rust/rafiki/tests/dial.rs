// SPDX-License-Identifier: Apache-2.0
//! `Client::new_with_dial`: TCP goes to the dial address while TLS SNI,
//! certificate verification and `Host` still come from the URL.
//!
//! The server's certificate is not in the native roots, so these tests install
//! it through `new_with_dial_and_roots` (the `test-hooks` feature).

use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use http::{Request, Response};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::TokioIo;
use prost::Message as _;
use rafiki::{pb, Client, Code};
use tokio_rustls::rustls;

mod common;
use common::*;

const NAME: &str = "rafiki.test.invalid";

#[derive(Default)]
struct Seen {
    sni: Mutex<Vec<Option<String>>>,
    hosts: Mutex<Vec<String>>,
    /// Handshake failures the server observed, delivered as they happen.
    handshake_errors: Mutex<Option<std::sync::mpsc::Sender<String>>>,
}

fn ok_stub(seen: Arc<Seen>) -> Stub {
    Arc::new(move |req: Request<Incoming>| {
        let seen = seen.clone();
        Box::pin(async move {
            seen.hosts
                .lock()
                .expect("lock")
                .push(header_of(&req, "host").unwrap_or_default());
            let reply = pb::GetHistoryResponse {
                events: vec![event("c_1", 1)],
            };
            Ok(with_epoch(Response::new(full(reply.encode_to_vec()))))
        })
    })
}

/// A TLS server on 127.0.0.1:0 with a certificate for `NAME`; returns its
/// address and the certificate's DER.
async fn tls_server(seen: Arc<Seen>) -> (SocketAddr, Vec<u8>) {
    let certified = rcgen::generate_simple_self_signed(vec![NAME.to_string()]).expect("a cert");
    let der = certified.cert.der().to_vec();
    let key = rustls::pki_types::PrivateKeyDer::try_from(certified.key_pair.serialize_der())
        .expect("a key");
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let config = rustls::ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .expect("versions")
        .with_no_client_auth()
        .with_single_cert(vec![certified.cert.der().clone()], key)
        .expect("server config");
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(config));
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0))
        .await
        .expect("bind");
    let addr = listener.local_addr().expect("addr");
    let stub = ok_stub(seen.clone());
    tokio::spawn(async move {
        while let Ok((tcp, _)) = listener.accept().await {
            let acceptor = acceptor.clone();
            let stub = stub.clone();
            let seen = seen.clone();
            tokio::spawn(async move {
                let tls = match acceptor.accept(tcp).await {
                    Ok(tls) => tls,
                    Err(err) => {
                        if let Some(tx) = seen.handshake_errors.lock().expect("lock").as_ref() {
                            let _ = tx.send(err.to_string());
                        }
                        return;
                    }
                };
                seen.sni
                    .lock()
                    .expect("lock")
                    .push(tls.get_ref().1.server_name().map(str::to_string));
                let service = service_fn(move |req: Request<Incoming>| {
                    let stub = stub.clone();
                    async move { stub(req).await }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(tls), service)
                    .await;
            });
        }
    });
    (addr, der)
}

async fn history(client: &Client) -> Result<pb::GetHistoryResponse, rafiki::Error> {
    client
        .get_history(&pb::GetHistoryRequest {
            child_id: "c_1".to_string(),
            ..Default::default()
        })
        .await
}

#[tokio::test]
async fn dial_https_verifies_url_host_not_dial_addr() {
    let seen = Arc::new(Seen::default());
    let (addr, der) = tls_server(seen.clone()).await;
    let client = Client::new_with_dial_and_roots(&format!("https://{NAME}"), None, addr, &[der])
        .expect("a client");
    let response = history(&client).await.expect("a call through the dial");
    assert_eq!(response.events, vec![event("c_1", 1)]);
    assert_eq!(
        *seen.sni.lock().expect("lock"),
        vec![Some(NAME.to_string())],
        "SNI comes from the URL host"
    );
    assert_eq!(
        *seen.hosts.lock().expect("lock"),
        vec![NAME.to_string()],
        "Host comes from the URL"
    );
}

#[tokio::test]
async fn dial_https_wrong_name_fails_verification() {
    let seen = Arc::new(Seen::default());
    let (tx, rx) = std::sync::mpsc::channel();
    *seen.handshake_errors.lock().expect("lock") = Some(tx);
    let (addr, der) = tls_server(seen.clone()).await;
    let client = Client::new_with_dial_and_roots("https://other.test.invalid", None, addr, &[der])
        .expect("a client");
    history(&client).await.expect_err("verification must fail");
    // The client's own error hides the TLS cause behind "client error
    // (connect)", so the server is the witness: TCP reached it (the dial
    // worked) and the handshake died on the client's certificate alert.
    let handshake =
        tokio::task::spawn_blocking(move || rx.recv_timeout(std::time::Duration::from_secs(5)))
            .await
            .expect("join")
            .expect("the server saw a failed handshake");
    assert!(
        handshake.to_lowercase().contains("alert"),
        "the client rejected the certificate: {handshake}"
    );
    assert!(
        seen.hosts.lock().expect("lock").is_empty(),
        "no request reached the server"
    );
}

#[tokio::test]
async fn dial_ignores_uri_port() {
    let seen = Arc::new(Seen::default());
    let (addr, der) = tls_server(seen.clone()).await;
    let client = Client::new_with_dial_and_roots(&format!("https://{NAME}:9"), None, addr, &[der])
        .expect("a client");
    history(&client)
        .await
        .expect("the dial address wins over port 9");
    assert_eq!(
        *seen.hosts.lock().expect("lock"),
        vec![format!("{NAME}:9")],
        "Host keeps the URL's authority"
    );
}

#[tokio::test]
async fn dial_http_plain() {
    let seen = Arc::new(Seen::default());
    let url = serve(ok_stub(seen.clone())).await;
    let addr: SocketAddr = url.trim_start_matches("http://").parse().expect("an addr");
    let client = Client::new_with_dial(&format!("http://{NAME}"), None, addr).expect("a client");
    history(&client)
        .await
        .expect("plain TCP to the dial address");
    assert_eq!(*seen.hosts.lock().expect("lock"), vec![NAME.to_string()]);
}

#[test]
fn dial_rejects_unix_endpoint() {
    let addr: SocketAddr = "127.0.0.1:1".parse().expect("an addr");
    let err = Client::new_with_dial("unix:///tmp/x.sock", None, addr).expect_err("refused");
    assert_eq!(err.code, Code::InvalidArgument);
}
