// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/multigres/testkit/assert"
)

func TestModelGateBackoff(t *testing.T) {
	g := NewModelGate(10*time.Second, 300*time.Second)

	g.record429("kimi-k3", 0)
	checkBlocked(t, g, "kimi-k3", 10*time.Second)

	g.record429("kimi-k3", 0)
	checkBlocked(t, g, "kimi-k3", 20*time.Second)

	g.record429("kimi-k3", 0)
	checkBlocked(t, g, "kimi-k3", 40*time.Second)

	g.recordSuccess("kimi-k3")
	g.record429("kimi-k3", 0)
	checkBlocked(t, g, "kimi-k3", 10*time.Second)
}

func TestModelGateRetryAfterOverrides(t *testing.T) {
	g := NewModelGate(10*time.Second, 300*time.Second)
	g.record429("kimi-k3", 5*time.Second)
	checkBlocked(t, g, "kimi-k3", 5*time.Second)
}

func TestModelGateRetryAfterClamped(t *testing.T) {
	g := NewModelGate(10*time.Second, 60*time.Second)
	g.record429("kimi-k3", 300*time.Second)
	checkBlocked(t, g, "kimi-k3", 60*time.Second)
}

func TestModelGateMaxDelayCapsExponential(t *testing.T) {
	g := NewModelGate(10*time.Second, 30*time.Second)
	for i := 0; i < 5; i++ {
		g.record429("kimi-k3", 0)
	}
	checkBlocked(t, g, "kimi-k3", 30*time.Second)
}

func TestModelGateBeforeSendBlocks(t *testing.T) {
	c := assert.NewCollecting(t)
	g := NewModelGate(10*time.Second, 300*time.Second)
	g.record429("kimi-k3", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := g.beforeSend(ctx, "kimi-k3")
	elapsed := time.Since(start)

	c.Require().Error(err, "beforeSend should have blocked until context deadline")
	c.GreaterOrEqual(40*time.Millisecond, elapsed, "beforeSend returned too quickly")
}

func TestModelGateBeforeSendUnblocked(t *testing.T) {
	g := NewModelGate(10*time.Second, 300*time.Second)
	ctx := context.Background()
	err := g.beforeSend(ctx, "kimi-k3")
	assert.NewAborting(t).NoError(err, "unblocked model should pass immediately")
}

func TestModelGateSeparateModels(t *testing.T) {
	g := NewModelGate(10*time.Second, 300*time.Second)
	g.record429("kimi-k3", 0)
	g.record429("deepseek-v4", 0)

	checkBlocked(t, g, "kimi-k3", 10*time.Second)
	checkBlocked(t, g, "deepseek-v4", 10*time.Second)

	g.recordSuccess("kimi-k3")
	checkBlocked(t, g, "deepseek-v4", 10*time.Second)
}

func checkBlocked(t *testing.T, g *ModelGate, model string, want time.Duration) {
	t.Helper()
	g.mu.Lock()
	until := g.blockedUntil[model]
	g.mu.Unlock()
	got := time.Until(until)
	tolerance := 50 * time.Millisecond
	assert.NewCollecting(t).False(got < want-tolerance || got > want+tolerance, "%s blocked for %v, want ~%v", model, got, want)
}

func TestIsRateLimit(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		wantRL bool
		wantRA time.Duration
	}{
		{
			name:   "nil",
			err:    nil,
			wantRL: false,
		},
		{
			name:   "not an API error",
			err:    context.DeadlineExceeded,
			wantRL: false,
		},
		{
			name: "429 with Retry-After seconds",
			err: &anthropic.Error{
				StatusCode: http.StatusTooManyRequests,
				Response: &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": {"30"}},
				},
			},
			wantRL: true,
			wantRA: 30 * time.Second,
		},
		{
			name: "429 without Retry-After",
			err: &anthropic.Error{
				StatusCode: http.StatusTooManyRequests,
				Response: &http.Response{
					StatusCode: http.StatusTooManyRequests,
				},
			},
			wantRL: true,
			wantRA: 0,
		},
		{
			name: "529 not rate limit",
			err: &anthropic.Error{
				StatusCode: 529,
			},
			wantRL: false,
		},
		{
			name: "400 not rate limit",
			err: &anthropic.Error{
				StatusCode: http.StatusBadRequest,
			},
			wantRL: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			gotRL, gotRA := isRateLimit(tt.err)
			c.Eq(tt.wantRL, gotRL, "isRateLimit()")
			c.Eq(tt.wantRA, gotRA, "retryAfter")
		})
	}
}

func TestRateLimitPolicyEffective(t *testing.T) {
	c := assert.NewCollecting(t)
	p := RateLimitPolicy{}.effective()
	c.Eq(10, p.MaxRetries, "MaxRetries")
	c.Eq(10*time.Second, p.BaseDelay, "BaseDelay")
	c.Eq(300*time.Second, p.MaxDelay, "MaxDelay")

	custom := RateLimitPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second}.effective()
	c.False(custom.MaxRetries != 3 || custom.BaseDelay != time.Second || custom.MaxDelay != 10*time.Second, "custom values were overwritten")
}

func TestIsRateLimitWithRetryAfterHelper(t *testing.T) {
	c := assert.NewCollecting(t)
	rl, ra := isRateLimit(rateLimitErrWithRetryAfter(45))
	c.True(rl, "expected 429 with Retry-After to be a rate limit")
	c.Eq(45*time.Second, ra, "retryAfter")
}
