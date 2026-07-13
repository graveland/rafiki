package routing

import (
	"context"
	"errors"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
)

// ClassifyFailure reports whether a primary attempt is a retryable upstream
// failure worth failing over on: a 5xx or 429 status, or a transport error with
// no HTTP status — but NOT client cancellation (context.Canceled /
// DeadlineExceeded), which is not an upstream-health signal. It is the single
// definition shared by the SDK path (Core.callModel, via the error's embedded
// status) and the HTTP proxy path (ServeHTTP, via resp.StatusCode + err).
//
// statusCode is the HTTP status of a completed transport call (0 when the call
// itself errored before yielding a response); err is that call's error, if any.
func ClassifyFailure(statusCode int, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true // transport/timeout errors (no HTTP status) are retryable
	}
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

// Retryable classifies an Anthropic SDK error, mapping its embedded HTTP
// status onto ClassifyFailure — the SDK-error counterpart used by the llm
// package's per-upstream routing.
func Retryable(err error) bool { return retryable(err) }

// retryable classifies an Anthropic SDK error, mapping its embedded HTTP status
// onto ClassifyFailure. An *anthropic.Error with StatusCode 0 (SDK-side error
// before a response) is treated like a transport error and is retryable.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode != 0 {
		return ClassifyFailure(apiErr.StatusCode, nil)
	}
	return ClassifyFailure(0, err) // transport/timeout or status-less SDK error
}
