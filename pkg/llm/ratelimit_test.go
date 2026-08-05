// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
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
	g := NewModelGate(10*time.Second, 300*time.Second)
	g.record429("kimi-k3", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := g.beforeSend(ctx, "kimi-k3")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("beforeSend should have blocked until context deadline")
	}
	if elapsed < 40*time.Millisecond {
		t.Errorf("beforeSend returned too quickly: %v", elapsed)
	}
}

func TestModelGateBeforeSendUnblocked(t *testing.T) {
	g := NewModelGate(10*time.Second, 300*time.Second)
	ctx := context.Background()
	err := g.beforeSend(ctx, "kimi-k3")
	if err != nil {
		t.Fatalf("unblocked model should pass immediately: %v", err)
	}
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
	if got < want-tolerance || got > want+tolerance {
		t.Errorf("%s blocked for %v, want ~%v", model, got, want)
	}
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
			gotRL, gotRA := isRateLimit(tt.err)
			if gotRL != tt.wantRL {
				t.Errorf("isRateLimit() = %v, want %v", gotRL, tt.wantRL)
			}
			if gotRA != tt.wantRA {
				t.Errorf("retryAfter = %v, want %v", gotRA, tt.wantRA)
			}
		})
	}
}

func TestRateLimitPolicyEffective(t *testing.T) {
	p := RateLimitPolicy{}.effective()
	if p.MaxRetries != 10 {
		t.Errorf("MaxRetries = %d, want 10", p.MaxRetries)
	}
	if p.BaseDelay != 10*time.Second {
		t.Errorf("BaseDelay = %v, want 10s", p.BaseDelay)
	}
	if p.MaxDelay != 300*time.Second {
		t.Errorf("MaxDelay = %v, want 300s", p.MaxDelay)
	}

	custom := RateLimitPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second}.effective()
	if custom.MaxRetries != 3 || custom.BaseDelay != time.Second || custom.MaxDelay != 10*time.Second {
		t.Error("custom values were overwritten")
	}
}

func TestIsRateLimitWithRetryAfterHelper(t *testing.T) {
	rl, ra := isRateLimit(rateLimitErrWithRetryAfter(45))
	if !rl {
		t.Error("expected 429 with Retry-After to be a rate limit")
	}
	if ra != 45*time.Second {
		t.Errorf("retryAfter = %v, want 45s", ra)
	}
}
