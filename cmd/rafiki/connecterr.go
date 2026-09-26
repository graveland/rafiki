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
