// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// RateLimitPolicy is a per-conversation tuning knob for rate-limit retries.
// Zero values mean "use defaults": MaxRetries=10, BaseDelay=10s, MaxDelay=300s.
type RateLimitPolicy struct {
	MaxRetries int           // 0 = use default (10)
	BaseDelay  time.Duration // 0 = use default (10s)
	MaxDelay   time.Duration // 0 = use default (300s)
}

func (p RateLimitPolicy) effective() RateLimitPolicy {
	if p.MaxRetries <= 0 {
		p.MaxRetries = 10
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 10 * time.Second
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 300 * time.Second
	}
	return p
}

// ModelGate is a per-model, process-wide backpressure gate. When any send
// receives a 429 for a model, that model is blocked until the backoff expires.
// All sends for that model — across all conversations — sleep until unblocked.
// A successful send resets the consecutive 429 counter.
type ModelGate struct {
	mu           sync.Mutex
	blockedUntil map[string]time.Time
	consecutive  map[string]int
	baseDelay    time.Duration
	maxDelay     time.Duration
}

// NewModelGate returns a gate with the given base and max backoff delays.
func NewModelGate(baseDelay, maxDelay time.Duration) *ModelGate {
	return &ModelGate{
		blockedUntil: map[string]time.Time{},
		consecutive:  map[string]int{},
		baseDelay:    baseDelay,
		maxDelay:     maxDelay,
	}
}

func (g *ModelGate) backoff(consecutive int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > g.maxDelay {
			return g.maxDelay
		}
		return retryAfter
	}
	d := time.Duration(float64(g.baseDelay) * math.Pow(2, float64(consecutive-1)))
	if d > g.maxDelay {
		d = g.maxDelay
	}
	return d
}

// beforeSend blocks until the model is unblocked (or ctx is done).
func (g *ModelGate) beforeSend(ctx context.Context, model string) error {
	g.mu.Lock()
	until, ok := g.blockedUntil[model]
	g.mu.Unlock()
	if !ok {
		return nil
	}
	wait := time.Until(until)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// record429 records a 429 for model. retryAfter is from the Retry-After
// response header, or zero when absent.
func (g *ModelGate) record429(model string, retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consecutive[model]++
	d := g.backoff(g.consecutive[model], retryAfter)
	g.blockedUntil[model] = time.Now().Add(d)
}

// recordSuccess resets the consecutive 429 counter for model.
func (g *ModelGate) recordSuccess(model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.consecutive, model)
	delete(g.blockedUntil, model)
}

// isRateLimit reports whether err is a rate-limit rejection (HTTP 429) and
// returns the parsed Retry-After duration, or (false, 0).
func isRateLimit(err error) (bool, time.Duration) {
	var apiErr *anthropic.Error
	if err == nil {
		return false, 0
	}
	if !errors.As(err, &apiErr) {
		return false, 0
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		return false, 0
	}
	var retryAfter time.Duration
	if apiErr.Response != nil {
		if v := apiErr.Response.Header.Get("Retry-After"); v != "" {
			retryAfter = parseRetryAfter(v)
		}
	}
	return true, retryAfter
}

// parseRetryAfter parses an RFC 7231 §7.1.1.1 Retry-After value.
func parseRetryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := time.Parse(time.RFC1123, v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
