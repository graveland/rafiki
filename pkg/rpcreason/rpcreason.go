// SPDX-License-Identifier: Apache-2.0

// Package rpcreason carries rafiki's precise error reason (a protocol.Err*
// value) on a Connect error as a google.rpc.ErrorInfo detail, because
// Connect codes are coarser than rafiki's reasons.
//
// The package is deliberately pgx-free: the rafiki CLI reads reasons off
// Connect errors, and the client must never link Postgres (pinned by
// TestClientDoesNotLinkPostgres in cmd/rafiki). pkg/connectapi links pgx
// transitively, so the CLI reads reasons through this package instead of
// importing connectapi.
package rpcreason

import (
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// Domain is the google.rpc.ErrorInfo domain rafiki writes every reason under;
// Reason ignores ErrorInfo details from any other domain.
const Domain = "rafiki"

// Attach adds reason to e as ErrorInfo{Reason: reason, Domain: Domain}.
// A nil e stays nil; if the detail somehow cannot be marshalled (ErrorInfo
// always can) e is returned unchanged rather than half-decorated.
func Attach(e *connect.Error, reason string) *connect.Error {
	if e == nil {
		return nil
	}
	detail, derr := connect.NewErrorDetail(&errdetails.ErrorInfo{Reason: reason, Domain: Domain})
	if derr != nil {
		return e
	}
	e.AddDetail(detail)
	return e
}

// Reason returns the rafiki reason on err, or "" if it carries none (or err
// is not a *connect.Error). ErrorInfo details from other domains, and details
// that are not ErrorInfo at all, are skipped; the first rafiki-domain reason
// wins.
func Reason(err error) string {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return ""
	}
	for _, detail := range ce.Details() {
		msg, derr := detail.Value()
		if derr != nil {
			continue
		}
		info, ok := msg.(*errdetails.ErrorInfo)
		if !ok || info.Domain != Domain {
			continue
		}
		return info.Reason
	}
	return ""
}
