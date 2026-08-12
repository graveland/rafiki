// SPDX-License-Identifier: Apache-2.0

package agentloop // same package as agentloop_test.go — access to unexported helpers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestIsRetryable(t *testing.T) {
	liveCtx := context.Background()
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name  string
		err   error
		ctx   context.Context
		retry bool
	}{
		// Nil / cancelled context.
		{"nil error", nil, liveCtx, false},
		{"cancelled context", errors.New("any"), cancelledCtx, false},

		// Network errors.
		{"net.OpError (connection reset)", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, liveCtx, true},
		{"net.OpError (connection refused)", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, liveCtx, true},
		{"net.OpError (timeout)", &net.OpError{Op: "read", Err: &timedoutErr{}}, liveCtx, true},

		// Syscall errors.
		{"syscall.ECONNRESET", syscall.ECONNRESET, liveCtx, true},
		{"syscall.ECONNREFUSED", syscall.ECONNREFUSED, liveCtx, true},
		{"syscall.EPIPE", syscall.EPIPE, liveCtx, true},
		{"syscall.ETIMEDOUT", syscall.ETIMEDOUT, liveCtx, true},
		{"wrapped syscall error", fmt.Errorf("wrap: %w", syscall.ECONNRESET), liveCtx, true},

		// HTTP errors.
		{"HTTP 502", &anthropic.Error{StatusCode: 502}, liveCtx, true},
		{"HTTP 503", &anthropic.Error{StatusCode: 503}, liveCtx, true},
		{"HTTP 504", &anthropic.Error{StatusCode: 504}, liveCtx, true},
		{"HTTP 400", &anthropic.Error{StatusCode: 400}, liveCtx, false},
		{"HTTP 401", &anthropic.Error{StatusCode: 401}, liveCtx, false},
		{"HTTP 404", &anthropic.Error{StatusCode: 404}, liveCtx, false},
		{"HTTP 429", &anthropic.Error{StatusCode: 429}, liveCtx, false}, // already handled

		// String fallback.
		{"contains 'connection reset'", fmt.Errorf("read: connection reset by peer"), liveCtx, true},
		{"contains 'broken pipe'", fmt.Errorf("write: broken pipe"), liveCtx, true},
		{"contains 'i/o timeout'", fmt.Errorf("i/o timeout"), liveCtx, true},
		{"contains 'EOF'", fmt.Errorf("unexpected EOF"), liveCtx, true},
		{"contains 'tls:'", fmt.Errorf("tls: handshake failure"), liveCtx, true},

		// Non-transient.
		{"plain error", errors.New("something went wrong"), liveCtx, false},
		{"deadline exceeded (own ctx)", context.DeadlineExceeded, liveCtx, true}, // not from our ctx
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryable(tt.err, tt.ctx)
			if got != tt.retry {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.retry)
			}
		})
	}
}

// timedoutErr implements net.Error with Timeout()==true.
type timedoutErr struct{}

func (e *timedoutErr) Error() string   { return "i/o timeout" }
func (e *timedoutErr) Timeout() bool   { return true }
func (e *timedoutErr) Temporary() bool { return true }
