// SPDX-License-Identifier: Apache-2.0
//! A protobuf Connect client for rafiki's control plane.
//!
//! `rafikid` serves the RPCs in `proto/rafiki/v1/control.proto` over Connect
//! (`docs/reference/control-protocol.md` §2–2.3) on a unix socket and on an
//! optional TLS listener. This crate is the client for that plane: the same
//! shapes the Python SDK (`sdk/python/rafiki/connect.py`) and the Go CLI reach
//! it with, but with the **protobuf** codec rather than JSON.
//!
//! - [`Client::new`] takes an endpoint — `https://host:port[/prefix]` (TLS
//!   over rustls + ring), `http://host:port[/prefix]` (plain TCP), or a unix
//!   socket (`unix:///abs/path` or a bare `/abs/path`) — and an optional
//!   bearer token. The token is attached in the transport, once, for both
//!   unary and streaming calls.
//! - [`Client::unary`] and [`Client::server_stream`] are the two transports
//!   every typed verb rides; the verbs themselves (`get_history`,
//!   `stream_events`, `send`, `spawn`, …) take and return the generated
//!   messages in [`pb`].
//! - Errors are [`Error`] with a Connect [`Code`]. Nothing is retried: a
//!   caller that wants to retry `is_unavailable()` may, and a stream that ends
//!   with `is_idle()` resumes with its own cursor.
//!
//! The generated message types under `src/gen/` are produced by `make proto`
//! (via the `rafiki-gen` binary in this workspace) and committed. They are
//! never hand-edited.

// The generated docs mirror `control.proto`'s comments verbatim, and its ASCII
// tables contain tabs. Rewriting them in a generated file is not an option.
#![allow(clippy::tabs_in_doc_comments)]

// Generated: every message in the three protos is emitted, and only the ones
// the verbs reach are used here. Nothing in this wrapper is hand-written.
#[allow(dead_code)]
mod proto {
    pub mod rafiki {
        pub mod v1 {
            include!("gen/rafiki.v1.rs");
        }
        pub mod daraja {
            pub mod v1 {
                include!("gen/rafiki.daraja.v1.rs");
            }
        }
    }
}
/// Generated `rafiki.daraja.v1` messages, which Control's `Daraja*` RPCs carry.
pub use proto::rafiki::daraja::v1 as daraja_pb;
/// Generated `rafiki.v1` messages (control + event).
pub use proto::rafiki::v1 as pb;

mod client;
mod envelope;
mod error;
mod verbs;

pub use client::{Client, Streaming};
pub use error::{Code, Error};

/// The wire-protocol epoch this client speaks, mirroring `pkg/protocol.Epoch`
/// (Go). Every request carries it in [`EPOCH_HEADER`]; every response must too,
/// or the daemon is stale and the call fails with a terminal
/// [`Error::is_protocol_mismatch`] rather than a retryable `unavailable`.
pub const EPOCH: &str = "4";

/// The HTTP header carrying [`EPOCH`] (`protocol.EpochHeader` in Go).
pub const EPOCH_HEADER: &str = "Rafiki-Protocol";

#[cfg(test)]
mod tests {
    /// `pkg/protocol.Epoch` and [`EPOCH`](super::EPOCH) are the same fact
    /// written twice, so a bump in one place and not the other fails here
    /// instead of on the wire.
    #[test]
    fn epoch_matches_go() {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../pkg/protocol/epoch.go"
        );
        let source =
            std::fs::read_to_string(path).unwrap_or_else(|err| panic!("read {path}: {err}"));
        let wanted = format!("const Epoch = {}", super::EPOCH);
        assert!(
            source.lines().any(|line| line.trim() == wanted),
            "{path} has no `{wanted}` line: the Rust EPOCH and pkg/protocol.Epoch have drifted"
        );
    }
}
