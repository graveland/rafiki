// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"errors"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
)

// ControllerError carries a machine-readable protocol error code from a
// Controller method, letting ConnectErr translate it to the correct Connect
// code without inspecting error strings.
//
// A ControllerError's Message is a promise: this codebase wrote it, and
// ConnectErr forwards it to the caller unexamined.
//
// Building one from another error's text is therefore fine when that error is
// OURS — a domain sentinel or a message from our own machinery, whose wording
// we control (translateInsightsErr on insights.ErrNotFound, the spawn-plan
// errors). It is NOT fine for an error from a dependency, whose text we do not
// control and cannot audit: `&ControllerError{Message: err.Error()}` over a
// driver or network error re-opens exactly the hole ConnectErr's redaction
// closes, and errors.As finds it however deeply it is wrapped. That is what
// the executor enroll/list paths used to do with pgx errors, which name the
// database host, user and database on a connection failure.
//
// The test is not "did I write this line" but "do I control every string that
// can reach it". If a dependency's error can flow into the Message, promote a
// known sentinel instead, or let it through to be logged and generalised.
type ControllerError struct {
	Code    string
	Message string
}

func (e *ControllerError) Error() string { return e.Message }

// errCodeTable maps every protocol.Err* code onto its Connect code. The code
// the daemon attached at the source IS the classification; Connect codes are
// coarser than rafiki's reasons, so the precise reason also rides the error as
// a google.rpc.ErrorInfo detail (see pkg/rpcreason).
//
// TestConnectErrCoversEveryErrConstant in errmap_test.go parses
// pkg/protocol/types.go and fails if a new Err* constant lands unmapped.
var errCodeTable = map[string]connect.Code{
	protocol.ErrInvalidArgs:        connect.CodeInvalidArgument,
	protocol.ErrPayloadTooLarge:    connect.CodeInvalidArgument,
	protocol.ErrChildNotFound:      connect.CodeNotFound,
	protocol.ErrNotFound:           connect.CodeNotFound,
	protocol.ErrPermissionDenied:   connect.CodePermissionDenied,
	protocol.ErrSessionFileMissing: connect.CodeNotFound,
	protocol.ErrChildExited:        connect.CodeFailedPrecondition,
	protocol.ErrChildInGrace:       connect.CodeFailedPrecondition,
	protocol.ErrChildShuttingDown:  connect.CodeFailedPrecondition,
	protocol.ErrNotResumable:       connect.CodeFailedPrecondition,
	protocol.ErrNotExited:          connect.CodeFailedPrecondition,
	protocol.ErrBackpressure:       connect.CodeResourceExhausted,
	protocol.ErrAtCapacity:         connect.CodeResourceExhausted,
	protocol.ErrAuthRequired:       connect.CodeUnauthenticated,
	protocol.ErrAuthInvalid:        connect.CodeUnauthenticated,
	protocol.ErrNoAgentDB:          connect.CodeUnavailable,
	protocol.ErrSpawnFailed:        connect.CodeInternal,
	protocol.ErrInternal:           connect.CodeInternal,
}

// internalErrText is what a genuinely uncoded error says on the wire through
// ConnectErr. Its raw text is never forwarded: text this codebase did not
// author can name infrastructure the caller has no business learning from a
// failed request (a pgx failure names the database host, user and database).
// The same allowlist discipline applies on every plane this package serves.
const internalErrText = "internal error"

// ConnectErr converts a daemon error into a *connect.Error. A
// *ControllerError keeps its authored message, gets the Connect code for its
// protocol.Err* code, and carries that code as a google.rpc.ErrorInfo{Reason:
// <code>, Domain: "rafiki"} detail so a client can branch on the precise
// reason (Connect codes are coarser).
//
// Any other error is CodeInternal with the fixed text internalErrText: its raw
// text is never forwarded and no detail is attached — matching the redaction
// precedent of queryError in conversations.go, which keeps a pgx failure from
// naming the database through this surface. ConnectErr does not log; a caller
// that needs the cause preserved logs it at the call site. A nil error returns
// nil. An unknown ControllerError code is CodeInternal with the reason still
// attached.
func ConnectErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *ControllerError
	if !errors.As(err, &ce) {
		return connect.NewError(connect.CodeInternal, errors.New(internalErrText))
	}
	code, ok := errCodeTable[ce.Code]
	if !ok {
		code = connect.CodeInternal
	}
	return rpcreason.Attach(connect.NewError(code, ce), ce.Code)
}
