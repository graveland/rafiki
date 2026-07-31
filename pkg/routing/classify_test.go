// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestRetryableExcludesContextCancellation(t *testing.T) {
	// Context cancellation should not be retryable.
	if retryable(context.Canceled) {
		t.Error("retryable(context.Canceled) = true, want false")
	}
	if retryable(context.DeadlineExceeded) {
		t.Error("retryable(context.DeadlineExceeded) = true, want false")
	}
	// But 5xx errors should still be retryable.
	if !retryable(&anthropic.Error{StatusCode: 529}) {
		t.Error("retryable(500 error) = false, want true")
	}
}
