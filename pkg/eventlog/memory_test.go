// SPDX-License-Identifier: Apache-2.0

package eventlog_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/eventlog"
	"go.graveland.dev/rafiki/pkg/eventlog/eventlogtest"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestMemoryConformance(t *testing.T) {
	eventlogtest.RunConformance(t, func(t *testing.T) (eventlog.Store, string) {
		return eventlog.NewMemory(), "c_test"
	})
}

// TestNewEventRoundTripsTsAndDuration: the in-memory store round-trips a fresh
// event's Timestamp and Duration through append + read + decode, exactly as the
// Postgres store does.
func TestNewEventRoundTripsTsAndDuration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s := eventlog.NewMemory()

	ts := time.Date(2025, 5, 6, 7, 8, 9, 123000000, time.UTC)
	want := 1500 * time.Millisecond
	if _, err := s.Append(ctx, "c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Ts:      timestamppb.New(ts),
		Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{
			ToolUseId: "tu_1", Duration: durationpb.New(want), IsError: true,
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	recs, err := s.Read(ctx, "c_1", -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 1, "rows = %d, want 1", len(recs))
	ev, err := recs[0].Decode()
	c.NoError(err, "Decode")
	c.True(ev.GetTs().AsTime().Equal(ts), "ts = %v, want %v", ev.GetTs().AsTime(), ts)
	c.Eq(want, ev.GetToolExecutionEnd().GetDuration().AsDuration(), "duration")
}

// TestRetryResumeAtRoundTrips: the in-memory store round-trips a Retry's
// resume_at, and an unset resume_at stays nil.
func TestRetryResumeAtRoundTrips(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s := eventlog.NewMemory()

	resumeAt := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	if _, err := s.Append(ctx, "c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt: 1, WillRetry: true, Reason: "rate limited",
			ResumeAt: timestamppb.New(resumeAt), MaxAttempts: 3,
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.Append(ctx, "c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{Attempt: 1, Reason: "fired"}},
	}); err != nil {
		t.Fatalf("Append resolution: %v", err)
	}

	recs, err := s.Read(ctx, "c_1", -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 2, "rows = %d, want 2", len(recs))

	scheduled, err := recs[0].Decode()
	c.NoError(err, "Decode scheduled")
	c.True(scheduled.GetRetry().GetResumeAt().AsTime().Equal(resumeAt),
		"resume_at = %v, want %v", scheduled.GetRetry().GetResumeAt().AsTime(), resumeAt)

	resolved, err := recs[1].Decode()
	c.NoError(err, "Decode resolution")
	c.Nil(resolved.GetRetry().GetResumeAt(), "a resolution event must carry no resume_at")
}

// TestMemoryDecodeFallsBackToCreatedAt: an event appended with no ts decodes
// with the row's CreatedAt, the same fallback the Postgres reader applies to a
// row written before ts existed.
func TestMemoryDecodeFallsBackToCreatedAt(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s := eventlog.NewMemory()

	if _, err := s.Append(ctx, "c_1", &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: "idle"}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	recs, err := s.Read(ctx, "c_1", -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 1, "rows = %d, want 1", len(recs))
	ev, err := recs[0].Decode()
	c.NoError(err, "Decode")
	c.Require().NotNil(ev.GetTs(), "ts must fall back to the record's CreatedAt")
	c.True(ev.GetTs().AsTime().Equal(recs[0].CreatedAt),
		"ts = %v, want CreatedAt %v", ev.GetTs().AsTime(), recs[0].CreatedAt)
}
