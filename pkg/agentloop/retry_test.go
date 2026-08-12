// SPDX-License-Identifier: Apache-2.0

package agentloop // same package as agentloop_test.go — access to unexported helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
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

func TestContinueWithRetry_NonTransientError_ReturnsImmediately(t *testing.T) {
	conv := newMemConv(t, &erroringSender{err: errors.New("permanent failure")})
	seedUser(t, conv)
	_, err := continueWithRetry(context.Background(), conv)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "permanent failure") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestContinueWithRetry_TransientError_Recovers(t *testing.T) {
	// First call fails transient, second succeeds.
	fails := 1
	sender := &conditionalSender{
		fn: func() (*anthropic.Message, error) {
			if fails > 0 {
				fails--
				return nil, &net.OpError{Op: "read", Err: syscall.ECONNRESET}
			}
			var m anthropic.Message
			if err := json.Unmarshal([]byte(respEndTurn), &m); err != nil {
				panic(err)
			}
			return &m, nil
		},
	}
	conv := newMemConv(t, sender)
	seedUser(t, conv)
	_, err := continueWithRetry(context.Background(), conv)
	if err != nil {
		t.Fatalf("expected recovery, got error: %v", err)
	}
}

func TestContinueWithRetry_ContextCanceled_StopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sender := &conditionalSender{
		fn: func() (*anthropic.Message, error) {
			cancel() // cancel after first failure
			return nil, &net.OpError{Op: "read", Err: syscall.ECONNRESET}
		},
	}
	conv := newMemConv(t, sender)
	seedUser(t, conv)
	_, err := continueWithRetry(ctx, conv)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestContinueWithRetry_ExhaustedRetries_ReturnsLastError(t *testing.T) {
	// The real retryDelays sequence sums to 127s (1+2+4+...+64); shrink it
	// to milliseconds so exhausting all 7 attempts stays a fast unit test.
	orig := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = orig })

	connErr := &net.OpError{Op: "read", Err: syscall.ECONNRESET}
	sender := &erroringSender{err: connErr}
	conv := newMemConv(t, sender)
	seedUser(t, conv)
	_, err := continueWithRetry(context.Background(), conv)
	if err == nil {
		t.Fatal("expected error after exhausted retries, got nil")
	}
}

// seedUser appends a user message so conv.Continue (Continue sends AS
// STORED, unlike Send/Run) has something to send.
func seedUser(t *testing.T, conv *llm.Conversation) {
	t.Helper()
	if err := conv.AppendUser(context.Background(), llm.UserText("hi")); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
}

// conditionalSender calls fn on each New call.
type conditionalSender struct {
	mu sync.Mutex
	fn func() (*anthropic.Message, error)
}

func (s *conditionalSender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fn()
}
