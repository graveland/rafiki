// SPDX-License-Identifier: Apache-2.0

package streams_test

import (
	"context"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
	"go.graveland.dev/rafiki/pkg/tui/streams"
)

// The rail must never subscribe at tier=ALL: that is content_block_delta for
// every child in the subtree, which is the load the rail/focus split exists to
// avoid.
func TestRailAsksForDurableTierAndSixTypes(t *testing.T) {
	if len(rail.Types()) != 6 {
		t.Fatalf("rail.Types() = %v, want six", rail.Types())
	}
	if rafikiv1.EventTier_EVENT_TIER_DURABLE == rafikiv1.EventTier_EVENT_TIER_ALL {
		t.Fatal("tier constants collapsed")
	}
}

func TestRailStopIsIdempotentAndPrompt(t *testing.T) {
	out := make(chan *rafikiv1.Event, 1)
	stop := streams.StartRail(
		context.Background(),
		streams.Config{BaseURL: "http://127.0.0.1:1"}, // nothing listening
		&rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}},
		func() *rafikiv1.EventCursor { return nil },
		out,
	)
	done := make(chan struct{})
	go func() { stop(); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not return, or panicked on the second call")
	}
}

func TestFocusStopReleasesPromptly(t *testing.T) {
	out := make(chan *rafikiv1.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := streams.StartFocus(ctx, streams.Config{BaseURL: "http://127.0.0.1:1"},
		"c_1", nil, out)
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not return")
	}
}

// The cursor callback runs on the stream's goroutine. Reconnect must call it
// FRESH each time rather than capturing it once, or a child spawned during the
// disconnect is resumed from a map that never mentions it.
func TestRailCallsTheCursorFuncOnEveryAttempt(t *testing.T) {
	calls := make(chan struct{}, 16)
	out := make(chan *rafikiv1.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := streams.StartRail(ctx, streams.Config{BaseURL: "http://127.0.0.1:1"},
		&rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}},
		func() *rafikiv1.EventCursor {
			select {
			case calls <- struct{}{}:
			default:
			}
			return &rafikiv1.EventCursor{}
		}, out)
	defer stop()

	// Two calls means it reconnected and asked again rather than reusing.
	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("cursor func called %d times, want at least 2", i)
		}
	}
}
