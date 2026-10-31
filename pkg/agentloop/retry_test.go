// SPDX-License-Identifier: Apache-2.0

package agentloop // same package as agentloop_test.go — access to unexported helpers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/batch"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/llm/llmtest"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
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
		// Nil / cancelled context. The cancelled case carries an error that is
		// otherwise retryable, so only the ctx guard can make it false.
		{"nil error", nil, liveCtx, false},
		{"cancelled context", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, cancelledCtx, false},

		// Network errors.
		{"net.OpError (connection reset)", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, liveCtx, true},
		{"net.OpError (connection refused)", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, liveCtx, true},
		{"net.OpError (timeout)", &net.OpError{Op: "read", Err: &timedoutErr{}}, liveCtx, true},
		// An OpError wrapping nothing the syscall or string arms recognize: only
		// the net.OpError branch can answer true.
		{"net.OpError (opaque cause)", &net.OpError{Op: "read", Err: errors.New("socket closed")}, liveCtx, true},

		// Syscall errors.
		{"syscall.ECONNRESET", syscall.ECONNRESET, liveCtx, true},
		{"syscall.ECONNREFUSED", syscall.ECONNREFUSED, liveCtx, true},
		{"syscall.EPIPE", syscall.EPIPE, liveCtx, true},
		{"syscall.ETIMEDOUT", syscall.ETIMEDOUT, liveCtx, true},
		{"wrapped syscall error", fmt.Errorf("wrap: %w", syscall.ECONNRESET), liveCtx, true},

		// HTTP errors from the native senders.
		{"HTTP 502", &msg.APIError{Status: 502}, liveCtx, true},
		{"HTTP 503", &msg.APIError{Status: 503}, liveCtx, true},
		{"HTTP 504", &msg.APIError{Status: 504}, liveCtx, true},
		{"HTTP 529", &msg.APIError{Status: 529, Kind: msg.ErrOverloaded}, liveCtx, true},
		{"HTTP 400", &msg.APIError{Status: 400}, liveCtx, false},
		{"HTTP 401", &msg.APIError{Status: 401}, liveCtx, false},
		{"HTTP 404", &msg.APIError{Status: 404}, liveCtx, false},
		{"HTTP 429", &msg.APIError{Status: 429, Kind: msg.ErrRateLimit}, liveCtx, false}, // already handled
		{"prompt too large", &msg.APIError{Status: 400, Kind: msg.ErrPromptTooLarge}, liveCtx, false},
		{"HTTP 500 wrapped", fmt.Errorf("llm: post: %w", &msg.APIError{Status: 500}), liveCtx, true},

		// In-band SSE "error" events (a status-0 APIError the codec classified).
		// The first is the exact payload a live OpenRouter gemini turn aborted
		// with mid-iteration; without recognition it killed the whole turn.
		{"sse timeout_error", inBandErr(msg.ErrTransient, `{"type":"error","error":{"type":"timeout_error","message":"The operation was aborted","error_type":"timeout"}}`), liveCtx, true},
		{"sse overloaded_error", inBandErr(msg.ErrOverloaded, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), liveCtx, true},
		// The exact payload a live OpenRouter gemini turn aborted with at
		// iteration 41 — a provider rate limit relayed in-band. False for the
		// same reason the typed 429 above is: sendStreaming's rate-limit loop
		// owns it (ModelGate backpressure); this loop must not stack a second
		// backoff on top.
		{"sse rate_limit_error", inBandErr(msg.ErrRateLimit, `{"type":"error","error":{"type":"rate_limit_error","message":"Provider returned error","error_type":"rate_limit_exceeded"}}`), liveCtx, false},
		{"sse invalid_request_error", inBandErr(msg.ErrOther, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`), liveCtx, false},

		// String fallback.
		{"contains 'connection reset'", fmt.Errorf("read: connection reset by peer"), liveCtx, true},
		{"contains 'broken pipe'", fmt.Errorf("write: broken pipe"), liveCtx, true},
		{"contains 'i/o timeout'", fmt.Errorf("i/o timeout"), liveCtx, true},
		{"contains 'EOF'", fmt.Errorf("unexpected EOF"), liveCtx, true},
		{"contains 'tls:'", fmt.Errorf("tls: handshake failure"), liveCtx, true},

		// Parked-batch terminal outcomes (*batch.Error, pkg/batch): the text is
		// provider-controlled, so fragments like "connection reset"/"EOF" must
		// not satisfy the string fallback above — a retry would resubmit the
		// batch up to 7 times (the global constraint's double-billing path).
		{"batch.Error text contains 'connection reset'", &batch.Error{Msg: "upstream said: connection reset, giving up"}, liveCtx, false},
		{"batch.Error text contains 'EOF'", &batch.Error{Msg: "upstream said unexpected EOF"}, liveCtx, false},
		{"batch.Error whole-batch failure", &batch.Error{Msg: "batch failed: upstream exploded"}, liveCtx, false},

		// Non-transient.
		{"plain error", errors.New("something went wrong"), liveCtx, false},
		{"deadline exceeded (own ctx)", context.DeadlineExceeded, liveCtx, true}, // not from our ctx
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryable(tt.err, tt.ctx)
			assert.NewCollecting(t).Eq(tt.retry, got, "isRetryable(%v) = %v, want", tt.err, got)
		})
	}
}

// timedoutErr implements net.Error with Timeout()==true.
type timedoutErr struct{}

func (e *timedoutErr) Error() string   { return "i/o timeout" }
func (e *timedoutErr) Timeout() bool   { return true }
func (e *timedoutErr) Temporary() bool { return true }

// inBandErr builds the error a native sender surfaces for an in-band SSE
// "error" event: a status-0 APIError carrying the event's own JSON and the kind
// its codec derived from it.
func inBandErr(kind msg.ErrorKind, payload string) error {
	return &msg.APIError{Status: 0, Body: []byte(payload), Kind: kind, Dialect: msg.DialectAnthropic}
}

func TestContinueWithRetry_NonTransientError_ReturnsImmediately(t *testing.T) {
	c := assert.NewAborting(t)
	conv := newMemConv(t, &erroringSender{err: errors.New("permanent failure")})
	seedUser(t, conv)
	_, err := continueWithRetry(context.Background(), conv)
	c.Error(err, "expected error, got nil")
	c.StrContains(err.Error(), "permanent failure", "unexpected error: %v", err)
}

func TestContinueWithRetry_TransientError_Recovers(t *testing.T) {
	// First call fails transient, second succeeds.
	fails := 1
	sender := &conditionalSender{
		fn: func() (*msg.Response, error) {
			if fails > 0 {
				fails--
				return nil, &net.OpError{Op: "read", Err: syscall.ECONNRESET}
			}
			return llmtest.FromAnthropicJSON(respEndTurn), nil
		},
	}
	conv := newMemConv(t, sender)
	seedUser(t, conv)
	_, err := continueWithRetry(context.Background(), conv)
	assert.NewAborting(t).NoError(err, "expected recovery, got error")
}

func TestContinueWithRetry_SSEStreamTimeout_Recovers(t *testing.T) {
	c := assert.NewAborting(t)
	// An OpenRouter upstream timeout arriving as an in-band SSE "error" event
	// (the exact payload from the live gemini abort that used to end the
	// turn) must be retried like any other transient failure, not kill the
	// loop. retryDelays are shrunk so the recovery backoff stays in
	// milliseconds.
	orig := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = orig })

	fails := 1
	sender := &conditionalSender{
		fn: func() (*msg.Response, error) {
			if fails > 0 {
				fails--
				return nil, inBandErr(msg.ErrTransient, `{"type":"error","error":{"type":"timeout_error",`+
					`"message":"The operation was aborted","error_type":"timeout"}}`)
			}
			return llmtest.FromAnthropicJSON(respEndTurn), nil
		},
	}
	conv := newMemConv(t, sender)
	seedUser(t, conv)
	resp, err := continueWithRetry(context.Background(), conv)
	c.NoError(err, "expected recovery from sse timeout_error, got error")
	c.False(resp == nil || resp.StopReason.String() != "end_turn", "expected an end_turn message after recovery, got %+v", resp)
	c.Eq(0, fails, "sender still has")
}

func TestContinueWithRetry_ContextCanceled_StopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sender := &conditionalSender{
		fn: func() (*msg.Response, error) {
			cancel() // cancel after first failure
			return nil, &net.OpError{Op: "read", Err: syscall.ECONNRESET}
		},
	}
	conv := newMemConv(t, sender)
	seedUser(t, conv)
	_, err := continueWithRetry(ctx, conv)
	assert.NewAborting(t).Error(err, "expected error from cancelled context, got nil")
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
	assert.NewAborting(t).Error(err, "expected error after exhausted retries, got nil")
}

// seedUser appends a user message so conv.Continue (Continue sends AS
// STORED, unlike Send/Run) has something to send.
func seedUser(t *testing.T, conv *llm.Conversation) {
	t.Helper()
	_, aerr := conv.AppendUser(context.Background(), llm.UserText("hi"))
	assert.NewAborting(t).NoError(aerr, "seed user message")
}

// conditionalSender calls fn on each New call.
type conditionalSender struct {
	mu sync.Mutex
	fn func() (*msg.Response, error)
}

func (s *conditionalSender) Send(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fn()
}

func (s *conditionalSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := s.Send(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return llmtest.NewDecoder(llmtest.Events(resp)), nil
}

// TestUsageLimitErrorNotRetried pins a subscription usage limit as terminal: the
// reset is hours or days away, so a retry loop against it only burns budget. The
// type is deliberately not a *msg.APIError, and its text avoids every string
// fragment the fallback below looks for, so nothing in isRetryable can rescue it
// into a retry.
func TestUsageLimitErrorNotRetried(t *testing.T) {
	c := assert.NewCollecting(t)
	limit := &llm.UsageLimitError{
		Code:     "usage_limit_reached",
		PlanType: "plus",
		ResetsAt: time.Now().Add(5 * time.Hour),
	}
	c.False(isRetryable(limit, context.Background()), "isRetryable(UsageLimitError)")
	// Also when wrapped, and when the reset time is unknown.
	c.False(isRetryable(fmt.Errorf("wrapped: %w", limit), context.Background()), "isRetryable(wrapped UsageLimitError)")
	c.False(isRetryable(&llm.UsageLimitError{Code: "insufficient_quota"}, context.Background()), "isRetryable(UsageLimitError with no reset time)")
	// The rendered text names the code and the plan, and never looks transient.
	text := limit.Error()
	c.StrContains(text, "usage_limit_reached", "error text")
	c.StrContains(text, "plan plus", "error text")
	for _, forbidden := range []string{"EOF", "connection reset", "broken pipe", "i/o timeout", "tls:"} {
		c.NotStrContains(text, forbidden, "error text must not contain %q", forbidden)
	}
	c.False(strings.HasPrefix(text, "batch: "), "error text must not look like a parked batch")
}
