// SPDX-License-Identifier: Apache-2.0
//! The live-daemon smoke test: a real `rafikid`, not a stub.
//!
//! It runs only when `RAFIKI_SDK_TEST_URL` is set — a client endpoint
//! (`unix:///…`, a bare path, or `https://…`) — with `RAFIKI_SDK_TEST_TOKEN`
//! as an optional bearer token. Unset means the test prints a SKIP line and
//! returns: an offline `cargo test` neither fails on a machine with no daemon
//! nor counts this test as a pass.

use std::time::Duration;

use futures_util::StreamExt as _;
use rafiki::{pb, Client};

const SKIP: &str = "SKIP a_live_daemon_lists_models_and_does_not_hang_on_a_quiet_stream: \
                    RAFIKI_SDK_TEST_URL is unset (this test needs a live rafikid)";

#[tokio::test]
async fn a_live_daemon_lists_models_and_does_not_hang_on_a_quiet_stream() {
    let Ok(url) = std::env::var("RAFIKI_SDK_TEST_URL") else {
        println!("{SKIP}");
        return;
    };
    let token = std::env::var("RAFIKI_SDK_TEST_TOKEN").ok();
    let client = Client::new(&url, token).expect("RAFIKI_SDK_TEST_URL is a client endpoint");

    let models = client
        .list_models(&pb::ListModelsRequest::default())
        .await
        .expect("list_models against a live daemon");
    println!("list_models: {} model(s)", models.models.len());

    let request = pb::StreamEventsRequest {
        subject: Some(pb::EventSubject {
            scope: Some(pb::event_subject::Scope::All(true)),
            ..Default::default()
        }),
        tier: pb::EventTier::All as i32,
        types: Vec::new(),
        cursor: None,
    };
    let idle = Duration::from_secs(2);
    let stream = client
        .stream_events(&request, idle)
        .await
        .expect("stream_events against a live daemon");
    futures_util::pin_mut!(stream);
    loop {
        match stream.next().await {
            Some(Ok(event)) => {
                println!("event {} ordinal {:?}", event.child_id, event.ordinal);
            }
            // A quiet daemon is the normal case: the idle window ends the
            // stream rather than the client hanging on it.
            Some(Err(err)) => {
                assert!(err.is_idle(), "a live daemon's quiet stream: {err}");
                println!("stream_events: idle after {idle:?}");
                break;
            }
            None => panic!("the stream ended without an idle window"),
        }
    }
}
