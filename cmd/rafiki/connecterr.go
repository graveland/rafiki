// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/rpcreason"
)

// formatConnectErr renders a Connect error for stderr: "<reason>: <message>"
// when the daemon attached a rafiki reason (rpcreason.Reason — Connect codes
// are coarser than rafiki's reasons), else the Connect code and message in
// connect's own rendering ("<code>: <message>"). A non-Connect error falls
// back to its Error() text.
//
// Wrapped errors are handled: both rpcreason.Reason and errors.As unwrap.
func formatConnectErr(err error) string {
	if err == nil {
		return ""
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err.Error()
	}
	if reason := rpcreason.Reason(err); reason != "" {
		if msg := ce.Message(); msg != "" {
			return fmt.Sprintf("%s: %s", reason, msg)
		}
		return reason
	}
	return ce.Error()
}

// connectVerbErr renders a failed Connect RPC for a CLI verb's stderr,
// composing the two failure shapes every Connect-backed verb wants. The
// three infrastructure codes (Unimplemented/Unauthenticated/Unavailable)
// get diagnoseConnectError's advice — describe names the endpoint for the
// advice text; everything else renders through formatConnectErr, which
// prefers the daemon's rafiki reason over connect's coarser code name.
func connectVerbErr(err error, describe string) error {
	switch connect.CodeOf(err) {
	case connect.CodeUnimplemented, connect.CodeUnauthenticated, connect.CodeUnavailable:
		return diagnoseConnectError(err, describe)
	}
	return errors.New(formatConnectErr(err))
}
