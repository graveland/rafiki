// SPDX-License-Identifier: Apache-2.0

package agentloop

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"syscall"
	"time"

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

	// HTTP errors from the SDK.
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		code := apiErr.StatusCode
		if code == 429 {
			return false // handled by sendStreaming's own retry loop
		}
		return code >= 500 && code < 600
	}

	// An in-band SSE rate_limit_error mirrors the typed 429 above exactly —
	// it is the same rejection arriving inside the stream instead of as a
	// response status — and stays false here for the same reason: sendStreaming's
	// own rate-limit loop owns it (its ModelGate blocks every conversation's
	// sends for that model while backing off), and stacking this loop's generic
	// backoff on top would multiply the wait without adding anything.
	if llm.IsRateLimitStreamError(err) {
		return false
	}

	// An in-band SSE "error" event reaches here stripped of all type info:
	// the SDK wraps the raw JSON body in a plain fmt.Errorf (see
	// llm.sseStreamErrPrefix), so the anthropic.Error check above and the
	// net/syscall checks below both miss it. Look inside the payload — this
	// is how an OpenRouter upstream timeout mid-turn ("timeout_error", "The
	// operation was aborted") is recognized as transient instead of ending
	// the whole turn.
	if llm.IsTransientStreamError(err) {
		return true
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

// retryDelays defines the exponential backoff sequence for LLM retries.
// 7 attempts total (initial + 6 retries), ~127s worst case.
var retryDelays = []time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second,
	8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second,
}

// backoffFor returns the backoff delay for retry attempt N (0-indexed)
// with ±25% uniform jitter.
func backoffFor(attempt int) time.Duration {
	if attempt >= len(retryDelays) {
		return retryDelays[len(retryDelays)-1]
	}
	d := retryDelays[attempt]
	jitter := time.Duration(float64(d) * 0.25 * (rand.Float64()*2 - 1))
	return d + jitter
}

// continueWithRetry wraps conv.Continue with transient-error retries.
// On each retry it logs a warning and sleeps the backoff delay.
// On recovery it logs an info line. On exhaustion it returns the last error.
func continueWithRetry(ctx context.Context, conv *llm.Conversation, opts ...llm.SendOption) (*anthropic.Message, error) {
	for attempt := 0; ; attempt++ {
		resp, err := conv.Continue(ctx, opts...)
		if err == nil || !isRetryable(err, ctx) {
			if attempt > 0 && err == nil {
				slog.Info("agentloop: turn recovered",
					"attempts", attempt+1,
					"total_backoff", time.Duration(attempt)*time.Second) // approximate
			}
			return resp, err
		}
		if attempt >= len(retryDelays) {
			return resp, err
		}
		d := backoffFor(attempt)
		slog.Warn("agentloop: retrying turn",
			"attempt", attempt+1, "max", len(retryDelays)+1,
			"backoff", d.Round(100*time.Millisecond), "error", err)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
