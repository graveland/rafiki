// SPDX-License-Identifier: Apache-2.0

package protocol

// Epoch is the rafiki wire-protocol version. It is incremented on every
// wire-breaking change; a peer that sends no EpochHeader is epoch 1.
//
// The epoch exists so a stale peer fails with a clear message instead of
// silently exchanging fields the two sides no longer agree on: the Connect
// control plane (both mounts) and the executor/daraja upgrade links refuse a
// mismatched peer, and the clients refuse a daemon that answers with the wrong
// epoch.
const Epoch = 2

// EpochHeader is the HTTP header carrying Epoch: every Connect request and
// response on the control plane, and the executor/daraja upgrade handshake.
// A request without it is an epoch-1 peer.
const EpochHeader = "Rafiki-Protocol"

// ErrProtocolMismatch is the google.rpc.ErrorInfo reason a daemon attaches to
// the FailedPrecondition it answers a peer whose EpochHeader is missing or
// wrong (see pkg/rpcreason). It is deliberately NOT part of the general error
// vocabulary in types.go: it names a transport-level refusal, not a domain
// outcome.
const ErrProtocolMismatch = "protocol_mismatch"
