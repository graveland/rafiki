// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func spawnedFor(id, name, parent string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{
			ChildId: id, ParentId: parent, Name: name}}}
}

func statusFor(id, state string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}}}
}

func exitedFor(id string, code *int32, signal string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{
			ExitCode: code, Signal: signal}}}
}

func turnEndForCost(id string, cost float64) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{
			CostUsd:    &cost,
			StopReason: rafikiv1.StopReason_STOP_REASON_END_TURN,
		}}}
}

func atClock(h, m int, sec float64) time.Time {
	// Local, not UTC: the renderer formats event times the way a terminal
	// wants them — in the process's zone — and a Timestamp renders .Local().
	base := time.Date(2026, 9, 26, h, m, 0, 0, time.Local)
	return base.Add(time.Duration(sec * float64(time.Second)))
}

// ── the golden lines ─────────────────────────────────────────────────────────

// The two lines the event commands must produce for a status transition and a
// turn end, byte for byte — cmd_watch.go's line format carried over whole.
// The events carry their own ts: replayed history renders with the
// original event times, which is what makes the durations here real.
func TestEventRendererGoldenStatusAndTurnLines(t *testing.T) {
	c := assert.NewCollecting(t)
	id := "c_01M3F2M6AMA3W09HD7R6Z58Q8B"
	const name = "1.2-review-r2"

	r := newEventRenderer()
	_ = r.observe(spawnedFor(id, name, ""), atClock(8, 44, 30.0))

	// The first status folds silently only for an unseen child; after a spawn
	// it renders the spawning→streaming transition.
	_ = r.observe(withTS(statusFor(id, "streaming"), atClock(8, 44, 34.3)), atClock(8, 44, 34.3))

	got := r.observe(withTS(statusFor(id, "tool_running"), atClock(8, 44, 58.0)), atClock(8, 44, 58.0))
	want := "08:44:58  status c_01M3F2M6AMA3W09HD7R6Z58Q8B  1.2-review-r2 streaming → tool_running (23.7s)"
	c.Eq(want, got, "status line:\n got")

	// A status back to streaming at 08:44:58.6, then the turn ends at
	// 08:45:07: the working duration is the 8.4s between them.
	_ = r.observe(withTS(statusFor(id, "streaming"), atClock(8, 44, 58.6)), atClock(8, 44, 58.6))
	got = r.observe(withTS(turnEndForCost(id, 0.0122), atClock(8, 45, 7.0)), atClock(8, 45, 7.0))
	want = "08:45:07  turn   c_01M3F2M6AMA3W09HD7R6Z58Q8B  1.2-review-r2 cost=$0.0122 stop=END_TURN (8.4s)"
	c.Eq(want, got, "turn line:\n got")
}

// Every other event type gets the same prefix shape — HH:MM:SS, the type
// column padded to six, the child id, two spaces, the name — then a one-line
// summary of the body. The six lifecycle types keep watch's short verbs; the
// rest carry their wire type name.
func TestEventRendererPrefixIsStableAcrossTypes(t *testing.T) {
	id := "c_prefix"
	r := newEventRenderer()
	_ = r.observe(spawnedFor(id, "impl", ""), atClock(9, 0, 0.0))

	cases := []struct {
		name string
		ev   *rafikiv1.Event
		want string
	}{
		{"user message", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{Index: 0, Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hello\nwide\t world"}}}},
		}}}, atClock(9, 0, 1.0)),
			"09:00:01  user_message c_prefix  impl hello wide world"},
		{"assistant message", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{
			Content: []*rafikiv1.ContentBlock{
				{Index: 0, Block: &rafikiv1.ContentBlock_Thinking{Thinking: &rafikiv1.ThinkingBlock{Thinking: "hmm"}}},
				{Index: 1, Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "answer"}}},
				{Index: 2, Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{Id: "tu_1", Name: "bash"}}},
			},
		}}}, atClock(9, 0, 2.0)),
			"09:00:02  assistant_message c_prefix  impl (thinking) answer (tool bash)"},
		{"turn start", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{TurnId: "t_1", Model: "m1"}}}, atClock(9, 0, 3.0)),
			"09:00:03  turn_start c_prefix  impl model=m1 turn=t_1"},
		{"tool start", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: &rafikiv1.ToolExecutionStart{ToolUseId: "tu_1", Name: "bash"}}}, atClock(9, 0, 4.0)),
			"09:00:04  tool_execution_start c_prefix  impl bash"},
		{"tool end", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{ToolUseId: "tu_1", Duration: durationpb.New(1200 * time.Millisecond), IsError: true}}}, atClock(9, 0, 5.2)),
			"09:00:05  tool_execution_end c_prefix  impl bash 1.2s error"},
		{"compaction", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_CompactionBoundary{CompactionBoundary: &rafikiv1.CompactionBoundary{Trigger: "auto", PreTokens: proto32(12000), PostTokens: proto32(4000)}}}, atClock(9, 0, 6.0)),
			"09:00:06  compaction_boundary c_prefix  impl trigger=auto pre=12000 post=4000"},
		{"script report", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_ScriptReport{ScriptReport: &rafikiv1.ScriptReport{Kind: "progress", DataJson: `{"step": 3}`}}}, atClock(9, 0, 7.0)),
			"09:00:07  script_report c_prefix  impl progress {\"step\": 3}"},
		{"delta", withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{TurnId: "t_1", Delta: &rafikiv1.ContentBlockDelta_Text{Text: "partial text"}}}}, atClock(9, 0, 8.0)),
			"09:00:08  content_block_delta c_prefix  impl text=partial text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NewCollecting(t).Eq(tc.want, r.observe(tc.ev, atClock(9, 0, 59.0)), "line:\n got")
		})
	}
}

// A tool end with no remembered start (an --all-types stream joining late)
// still renders its duration.
func TestEventRendererToolEndWithoutStart(t *testing.T) {
	r := newEventRenderer()
	got := r.observe(withTS(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{ToolUseId: "tu_late", Duration: durationpb.New(2500 * time.Millisecond)}}}, atClock(9, 1, 0.0)), atClock(9, 1, 0.0))
	assert.NewCollecting(t).False(!strings.Contains(got, "2.5s") || strings.Contains(got, "bash"), "late tool end = %q, want a duration and no invented name", got)
}

// A summary longer than the column cap is truncated, never wrapped.
func TestEventRendererSummaryClampsToOneLine(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newEventRenderer()
	long := strings.Repeat("x", 300)
	got := r.observe(withTS(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
		Content: []*rafikiv1.ContentBlock{{Index: 0, Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: long}}}},
	}}}, atClock(9, 2, 0.0)), atClock(9, 2, 0.0))
	c.NotStrContains(got, "\n", "summary wrapped")
	// Prefix (8+2+12+1+3+2+9+1) + the capped summary (99 ASCII chars + the
	// 3-byte ellipsis, 100 columns wide).
	n, want := len(got), 8+2+12+1+3+2+9+1+99+3
	c.Eq(want, n, "summary length = %d, want %d: %q", n, want, got)
	c.True(strings.HasSuffix(got, "…"), "clamped summary lost its ellipsis: %q", got)
}

// ── the tracker, carried over from watch ─────────────────────────────────────

// A full lifecycle in five lines, with the durations that make the status
// lines worth reading: how long each state lasted, and the child's lifetime.
func TestEventRendererFullLifecycle(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newEventRenderer()
	t0 := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	got := r.observe(spawnedFor("c_1", "impl-auth", "c_root"), t0)
	for _, want := range []string{"spawn", "impl-auth", "parent=c_root"} {
		c.StrContains(got, want, "spawn line")
	}

	got = r.observe(statusFor("c_1", "streaming"), t0.Add(2*time.Second))
	c.StrContains(got, "spawning → streaming", "first status line should show spawning→streaming, got")

	got = r.observe(statusFor("c_1", "idle"), t0.Add(6*time.Second))
	c.StrContains(got, "streaming → idle (4.0s)", "idle transition should carry the working duration, got")

	got = r.observe(turnEndForCost("c_1", 0.0142), t0.Add(7*time.Second))
	c.False(!strings.Contains(got, "cost=$0.0142") || !strings.Contains(got, "stop=END_TURN"), "turn line = %q", got)
	// idle is not Working, so no duration is invented on the turn line.
	c.NotStrContains(got, "(7.0s)", "turn line after idle invented a duration")

	got = r.observe(exitedFor("c_1", proto32(0), ""), t0.Add(3*time.Minute))
	c.False(!strings.Contains(got, "code=0") || !strings.Contains(got, "(lifetime 3m00s)"), "exit line = %q", got)
}

// A child whose exit carried no code was signalled; the line must say so
// rather than print code=0, which means success.
func TestEventRendererSignalledExit(t *testing.T) {
	r := newEventRenderer()
	t0 := time.Now()
	_ = r.observe(spawnedFor("c_1", "impl-auth", ""), t0)

	got := r.observe(exitedFor("c_1", nil, "SIGKILL"), t0.Add(time.Second))
	assert.NewCollecting(t).False(!strings.Contains(got, "signal=SIGKILL") || strings.Contains(got, "code="), "signalled exit line = %q", got)
}

// An event for a child the renderer has never met reads [unnamed].
func TestEventRendererNamesAnUnmetChild(t *testing.T) {
	r := newEventRenderer()
	got := r.observe(statusFor("c_stranger", "idle"), time.Now())
	assert.NewCollecting(t).False(!strings.Contains(got, "[unnamed]") || !strings.Contains(got, "c_stranger"), "unknown-child line = %q", got)
}

// A duplicate status event (same state twice) must not render a phantom "x →
// x" transition, and a turn_end that arrives while the child is already idle
// carries no duration rather than a bogus one. A zero cost still prints.
func TestEventRendererKeepsDuplicateStatusAndTurnSane(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newEventRenderer()
	t0 := time.Now()
	_ = r.observe(statusFor("c_1", "idle"), t0)

	got := r.observe(statusFor("c_1", "idle"), t0.Add(time.Second))
	c.NotStrContains(got, "→", "duplicate status rendered a transition")

	got = r.observe(turnEndForCost("c_1", 0), t0.Add(2*time.Second))
	c.NotStrContains(got, "(", "turn_end after idle invented a duration")
	c.StrContains(got, "cost=$0.0000", "zero cost dropped")
}

// The roster seed names children the stream never announced — the reconnect
// case, where child_spawned is in the past and will not replay.
func TestEventRendererSeedSuppliesNamesAfterReconnect(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newEventRenderer()
	var notes bytes.Buffer
	client := stubbedRosterSource{listChildren: func(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
		return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: []*rafikiv1.ChildSummary{
			{ChildId: "c_1", Name: "coordinator", Status: "streaming"},
		}}), nil
	}}

	r.seed(context.Background(), &notes, client, "test")
	c.StrContains(r.observe(statusFor("c_1", "idle"), time.Now()), "coordinator", "seeded name missing from line")
	c.Eq(0, notes.Len(), "successful seed wrote notes: %q", notes.String())
}

// A failed roster fetch is a note, not an error: the stream still works.
func TestEventRendererSeedFailureIsANote(t *testing.T) {
	r := newEventRenderer()
	var notes bytes.Buffer
	client := stubbedRosterSource{listChildren: func(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
		return nil, connect.NewError(connect.CodeUnavailable, context.DeadlineExceeded)
	}}

	r.seed(context.Background(), &notes, client, "describe-here")
	assert.NewCollecting(t).StrContains(notes.String(), "# roster unavailable at describe-here", "seed failure not noted")
}

// The single-child seed names the one child from GetChild and takes its
// lifetime from started_at, so a later exit reports a real lifetime.
func TestEventRendererSeedChildNamesAndTimes(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newEventRenderer()
	var notes bytes.Buffer
	born := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	client := stubbedChildSource{getChild: func(ctx context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
		return connect.NewResponse(&rafikiv1.GetChildResponse{Child: &rafikiv1.ChildSummary{
			ChildId: "c_1", Name: "solo", Status: "streaming", StartedAt: timestamppb.New(born),
		}}), nil
	}}

	r.seedChild(context.Background(), &notes, client, "c_1", "test")
	c.Require().Eq(0, notes.Len(), "successful seed wrote notes: %q", notes.String())
	got := r.observe(exitedFor("c_1", proto32(0), ""), born.Add(90*time.Second))
	c.False(!strings.Contains(got, "solo") || !strings.Contains(got, "(lifetime 1m30s)"), "exit after child seed = %q", got)
}

func TestEventRendererIgnoresEmptyAndNilEvents(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newEventRenderer()
	c.Eq("", r.observe(nil, time.Now()), "nil event rendered")
	c.Eq("", r.observe(&rafikiv1.Event{}, time.Now()), "child-less event rendered")
}

// The retry schedule instant renders in the viewer's local zone — the
// producing daemon's clock zone is arbitrary (a container runs UTC), which is
// why it travels as resume_at rather than inside reason — next to a
// line prefix that is local for the same reason.
func TestEventRendererRetryScheduleLineIsViewerLocal(t *testing.T) {
	c := assert.NewCollecting(t)
	id := "c_01M3F2M6AMA3W09HD7R6Z58Q8B"

	r := newEventRenderer()
	_ = r.observe(spawnedFor(id, "impl-auth", ""), atClock(12, 0, 0.0))

	ev := &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt: 1, WillRetry: true, Reason: "rate limited (HTTP 429)",
			MaxAttempts: 3, ResumeAt: timestamppb.New(atClock(19, 10, 30.0)),
		}}}
	got := r.observe(withTS(ev, atClock(19, 9, 58.0)), atClock(19, 9, 58.0))
	want := "19:09:58  retry  c_01M3F2M6AMA3W09HD7R6Z58Q8B  impl-auth attempt=1 will-retry resumes 19:10:30 rate limited (HTTP 429)"
	c.Eq(want, got, "retry schedule line:\n got")

	// A resolution event names no schedule: no "resumes" token.
	got = r.observe(withTS(&rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt: 1, Reason: "auto-resume 1 firing",
		}}}, atClock(19, 10, 31.0)), atClock(19, 10, 31.0))
	want = "19:10:31  retry  c_01M3F2M6AMA3W09HD7R6Z58Q8B  impl-auth attempt=1 no-retry auto-resume 1 firing"
	c.Eq(want, got, "retry resolution line:\n got")
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{900 * time.Millisecond, "0.9s"},
		{3200 * time.Millisecond, "3.2s"},
		{72 * time.Second, "1m12s"},
		{62 * time.Minute, "1h02m"},
	}
	for _, tc := range cases {
		got := fmtDur(tc.d)
		assert.NewCollecting(t).Eq(tc.want, got, "fmtDur(%v) = %q, want", tc.d, got)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func withTS(ev *rafikiv1.Event, t time.Time) *rafikiv1.Event {
	ev.Ts = timestamppb.New(t)
	return ev
}

func proto32(v int32) *int32 { return &v }

// stubbedRosterSource serves the one RPC the roster seed makes.
type stubbedRosterSource struct {
	listChildren func(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error)
}

func (s stubbedRosterSource) ListChildren(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	return s.listChildren(ctx, req)
}

// stubbedChildSource serves the one RPC the single-child seed makes.
type stubbedChildSource struct {
	getChild func(ctx context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error)
}

func (s stubbedChildSource) GetChild(ctx context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	return s.getChild(ctx, req)
}

func TestContentSummaryNamesImage(t *testing.T) {
	c := assert.NewCollecting(t)
	blocks := []*rafikiv1.ContentBlock{
		{Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{MediaType: "image/png"}}},
		{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "look"}}},
	}
	c.Eq("(image image/png) look", contentSummary(blocks), "contentSummary with an image")

	empty := []*rafikiv1.ContentBlock{
		{Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{}}},
	}
	c.StrContains(contentSummary(empty), "(image ?)", "unnamed media type")
}
