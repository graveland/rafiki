// SPDX-License-Identifier: Apache-2.0

package agentloop

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// isRetryable reports whether err is a transient error worth retrying.
// ctx must be checked first: if ctx.Err() != nil the parent is shutting down
// and no retry should be attempted regardless of the error shape.
func isRetryable(err error, ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if err == nil {
		return false
	}

	// Already-handled retry paths: don't double-retry.
	if llm.IsPromptTooLarge(err) {
		return false
	}

	// A deadline exceeded on some other context (an HTTP client's own
	// request timeout, a sub-operation's timeout) is a transient read/write
	// stall, not our caller giving up — that case was already rejected by
	// the ctx.Err() check above.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// HTTP-level errors from the SDK.
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		code := apiErr.StatusCode
		if code == 429 {
			return false // handled by sendStreaming's own retry loop
		}
		return code >= 500 && code < 600
	}

	// Network/syscall errors.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if isSyscallErr(err) {
		return true
	}

	// String-based fallback for wrapped errors that lose type info.
	s := err.Error()
	for _, frag := range []string{"connection reset", "broken pipe", "i/o timeout", "EOF"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	// TLS errors often wrap net.OpError but some handshake failures surface
	// only as string "tls:" — catch those too.
	if strings.Contains(s, "tls:") {
		return true
	}

	return false
}

// isSyscallErr checks whether err wraps a known transient syscall error.
func isSyscallErr(err error) bool {
	for {
		if errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EPIPE) ||
			errors.Is(err, syscall.ETIMEDOUT) {
			return true
		}
		err = errors.Unwrap(err)
		if err == nil {
			return false
		}
	}
}
