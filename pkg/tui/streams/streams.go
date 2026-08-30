// SPDX-License-Identifier: Apache-2.0

// Package streams owns the cockpit's two Connect subscriptions.
//
// They are split rather than being one stream with a widened filter: hopping
// must never interrupt rail coverage, and a slow focus consumer must not be
// able to degrade the rail.
package streams

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// Config is how to reach the daemon.
type Config struct {
	HTTPClient *http.Client
	BaseURL    string
}

func (c Config) client() rafikiv1connect.ControlClient {
	h := c.HTTPClient
	if h == nil {
		h = http.DefaultClient
	}
	return rafikiv1connect.NewControlClient(h, c.BaseURL)
}

// backoff is the reconnect schedule, capped. The cockpit is interactive: a user
// watching a frozen rail wants it back in seconds, and the daemon is on the
// other end of a unix socket, so there is no remote service to be polite to.
var backoff = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
}

func backoffFor(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(backoff) {
		return backoff[len(backoff)-1]
	}
	return backoff[attempt]
}

// StartRail opens the long-lived rail subscription and keeps it open.
//
// cursor is a FUNC, not a value, and is called fresh on every reconnect
// attempt: children spawn while you are disconnected, and a cursor captured at
// open time would resume from a map that does not mention them. It is called
// from this goroutine, which is why rail.Rail is mutex-guarded.
func StartRail(
	ctx context.Context,
	cfg Config,
	subject *rafikiv1.EventSubject,
	cursor func() *rafikiv1.EventCursor,
	out chan<- *rafikiv1.Event,
) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		client := cfg.client()
		attempt := 0
		for ctx.Err() == nil {
			req := &rafikiv1.StreamEventsRequest{
				Subject: subject,
				Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
				Types:   rail.Types(),
			}
			if cursor != nil {
				req.Cursor = cursor()
			}
			if pump(ctx, client, req, out) {
				attempt = 0 // delivered something; the connection was healthy
			} else {
				attempt++
			}
			if !sleepCtx(ctx, backoffFor(attempt)) {
				return
			}
		}
	}()
	return cancel
}

// StartFocus opens the focused child's full-fidelity subscription.
//
// Unlike the rail its cursor is a VALUE: a focus stream belongs to one hop, and
// the session it feeds owns the cursor for the next one.
func StartFocus(
	ctx context.Context,
	cfg Config,
	childID string,
	cursor *rafikiv1.EventCursor,
	out chan<- *rafikiv1.Event,
) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		client := cfg.client()
		req := &rafikiv1.StreamEventsRequest{
			Subject: &rafikiv1.EventSubject{
				Scope: &rafikiv1.EventSubject_Child{Child: childID},
			},
			Tier:   rafikiv1.EventTier_EVENT_TIER_ALL,
			Cursor: cursor,
		}
		attempt := 0
		for ctx.Err() == nil {
			if pump(ctx, client, req, out) {
				attempt = 0
			} else {
				attempt++
			}
			if !sleepCtx(ctx, backoffFor(attempt)) {
				return
			}
		}
	}()
	return cancel
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pump runs one StreamEvents to completion. It reports whether it delivered at
// least one event, which is what the caller uses to decide the connection was
// healthy enough to reset the backoff.
func pump(
	ctx context.Context,
	client rafikiv1connect.ControlClient,
	req *rafikiv1.StreamEventsRequest,
	out chan<- *rafikiv1.Event,
) bool {
	stream, err := client.StreamEvents(ctx, connect.NewRequest(req))
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("tui: stream open failed", "error", err)
		}
		return false
	}
	defer func() { _ = stream.Close() }()

	delivered := false
	for stream.Receive() {
		select {
		case <-ctx.Done():
			return delivered
		case out <- stream.Msg():
			delivered = true
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("tui: stream ended", "error", err)
	}
	return delivered
}
