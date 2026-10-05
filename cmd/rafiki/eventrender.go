// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/mattn/go-runewidth"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// eventSummaryMaxCols bounds the one-line summary an event's body renders to,
// so one wide event cannot produce an endless line. The raw modes (-r, -J)
// carry the full event; this bounds only the human line.
const eventSummaryMaxCols = 100

// eventRenderer renders native events into one line each. It is the ONE
// renderer for `logs` and `tail` — chosen by event type, never by command.
//
// It is deliberately pure (a clock is passed in), and it prefers an event's
// own ts over the render clock: replayed history then shows the
// original event times, and durations folded from a replay carry the real
// state-to-state gaps rather than the gap since the replay arrived.
type eventRenderer struct {
	names    map[string]string
	status   map[string]string
	statusAt map[string]time.Time
	bornAt   map[string]time.Time
	tools    map[string]string // tool_use_id → tool name, from tool_execution_start
}

func newEventRenderer() *eventRenderer {
	return &eventRenderer{
		names:    make(map[string]string),
		status:   make(map[string]string),
		statusAt: make(map[string]time.Time),
		bornAt:   make(map[string]time.Time),
		tools:    make(map[string]string),
	}
}

// rosterSource is the one RPC the roster seed needs — a real ControlClient
// satisfies it, and so does a test stub without the other fifty methods.
type rosterSource interface {
	ListChildren(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error)
}

// childSource is the one RPC the single-child seed needs.
type childSource interface {
	GetChild(ctx context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error)
}

// seed records the current roster from ListChildren — the multi-child case,
// where child_spawned does not replay (the cursor is absent and the events
// are in the past), so a reconnect would lose every name it had. Failures are
// noted on `notes` (stderr in production) and are not fatal: the stream still
// works, the rows just read "unnamed" until a child_spawned or a later
// reconnect names them.
func (r *eventRenderer) seed(ctx context.Context, notes io.Writer, client rosterSource, describe string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		fmt.Fprintf(notes, "# roster unavailable at %s: %s\n", describe, formatConnectErr(err))
		return
	}
	for _, ch := range resp.Msg.GetChildren() {
		r.noteChild(ch.GetChildId(), ch.GetName(), ch.GetStatus(), ch.GetStartedAt(), time.Now())
	}
}

// seedChild names and states one child from GetChild — the single-child case,
// where one precise RPC beats scanning a roster. Same failure posture as
// seed: noted, not fatal.
func (r *eventRenderer) seedChild(ctx context.Context, notes io.Writer, client childSource, childID, describe string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		fmt.Fprintf(notes, "# child %s unavailable at %s: %s\n", childID, describe, formatConnectErr(err))
		return
	}
	ch := resp.Msg.GetChild()
	r.noteChild(childID, ch.GetName(), ch.GetStatus(), ch.GetStartedAt(), time.Now())
}

// noteChild records a name and a first-known status without overwriting
// anything the stream itself has already taught us. bornAt prefers the
// child's started_at when the source carries one: an exit rendered from a
// seeded child then reports a real lifetime instead of "since connect".
func (r *eventRenderer) noteChild(id, name, status string, startedAtMs int64, now time.Time) {
	if id == "" {
		return
	}
	if name != "" {
		if _, known := r.names[id]; !known {
			r.names[id] = name
		}
	}
	if _, known := r.status[id]; known {
		return
	}
	if status != "" {
		r.status[id] = status
		r.statusAt[id] = now
	}
	if startedAtMs > 0 {
		r.bornAt[id] = time.UnixMilli(startedAtMs)
	} else {
		r.bornAt[id] = now
	}
}

// observe folds one event and returns its line, or "" when there is nothing
// worth printing (nil events, events with no child id, payloads with no
// lifecycle meaning). The raw modes print every event regardless; this
// decides only the human line.
func (r *eventRenderer) observe(ev *rafikiv1.Event, now time.Time) string {
	if ev == nil || ev.GetChildId() == "" {
		return ""
	}
	id := ev.GetChildId()
	t := now
	if ev.Ts != nil && ev.GetTs().AsTime().Unix() > 0 {
		t = ev.GetTs().AsTime().Local()
	}

	var detail string
	switch p := ev.GetPayload().(type) {
	case *rafikiv1.Event_ChildSpawned:
		if name := p.ChildSpawned.GetName(); name != "" {
			r.names[id] = name
		}
		if parent := p.ChildSpawned.GetParentId(); parent != "" {
			detail = " parent=" + parent
		}
		r.status[id] = "spawning"
		r.statusAt[id] = t
		if _, born := r.bornAt[id]; !born {
			r.bornAt[id] = t
		}
	case *rafikiv1.Event_AgentStatus:
		prev := r.status[id]
		st := p.AgentStatus.GetState()
		if prev != "" && prev != st {
			detail = fmt.Sprintf(" %s → %s (%s)", prev, st, fmtDur(t.Sub(r.statusAt[id])))
		} else {
			detail = " " + st
		}
		r.status[id] = st
		r.statusAt[id] = t
	case *rafikiv1.Event_ChildExited:
		detail = " " + exitSummary(p.ChildExited)
		if born, ok := r.bornAt[id]; ok {
			detail += fmt.Sprintf(" (lifetime %s)", fmtDur(t.Sub(born)))
		}
		r.status[id] = "exited"
	case *rafikiv1.Event_TurnEnd:
		te := p.TurnEnd
		// cost_usd is optional: absent reads as "not reported", while 0 is a
		// real reading worth printing on a free model.
		if te.CostUsd != nil {
			detail = fmt.Sprintf(" cost=$%.4f", te.GetCostUsd())
		}
		if te.GetStopReason() != rafikiv1.StopReason_STOP_REASON_UNSPECIFIED {
			// Trim the proto prefix; "stop=END_TURN" reads, "STOP_REASON_"
			// does not.
			detail += " stop=" + strings.TrimPrefix(te.GetStopReason().String(), "STOP_REASON_")
		}
		// The turn's duration, as far as status transitions can see it: how
		// long the child has been away from idle. Absent when it never
		// reported a working state.
		if st := r.status[id]; rail.Working(st) {
			detail += fmt.Sprintf(" (%s)", fmtDur(t.Sub(r.statusAt[id])))
		}
	case *rafikiv1.Event_Error:
		detail = fmt.Sprintf(" %s: %s", p.Error.GetCode(), p.Error.GetMessage())
	case *rafikiv1.Event_Retry:
		detail = fmt.Sprintf(" attempt=%d", p.Retry.GetAttempt())
		if p.Retry.GetWillRetry() {
			detail += " will-retry"
		} else {
			detail += " no-retry"
		}
		// The schedule instant renders in the viewer's local zone: the
		// producing daemon's clock zone is arbitrary (a container is UTC),
		// so it never embeds a formatted time in reason.
		if p.Retry.ResumeAt != nil {
			detail += " resumes " + p.Retry.GetResumeAt().AsTime().Local().Format("15:04:05")
		}
		if reason := p.Retry.GetReason(); reason != "" {
			detail += " " + reason
		}
	case *rafikiv1.Event_UserMessage:
		detail = " " + oneLine(contentSummary(p.UserMessage.GetContent()))
	case *rafikiv1.Event_AssistantMessage:
		detail = " " + oneLine(contentSummary(p.AssistantMessage.GetContent()))
	case *rafikiv1.Event_TurnStart:
		detail = " " + oneLine(turnStartSummary(p.TurnStart))
	case *rafikiv1.Event_ContentBlockDelta:
		detail = " " + oneLine(deltaSummary(p.ContentBlockDelta))
	case *rafikiv1.Event_ToolExecutionStart:
		name := p.ToolExecutionStart.GetName()
		if name != "" {
			r.tools[p.ToolExecutionStart.GetToolUseId()] = name
		}
		detail = " " + name
	case *rafikiv1.Event_ToolExecutionEnd:
		name := r.tools[p.ToolExecutionEnd.GetToolUseId()]
		if name != "" {
			detail = " " + name
		}
		detail += " " + fmtDur(p.ToolExecutionEnd.GetDuration().AsDuration())
		if p.ToolExecutionEnd.GetIsError() {
			detail += " error"
		}
	case *rafikiv1.Event_CompactionBoundary:
		cb := p.CompactionBoundary
		detail = " trigger=" + cb.GetTrigger()
		// Optional fields: absence reads "unknown", not zero.
		if cb.PreTokens != nil {
			detail += fmt.Sprintf(" pre=%d", cb.GetPreTokens())
		}
		if cb.PostTokens != nil {
			detail += fmt.Sprintf(" post=%d", cb.GetPostTokens())
		}
	case *rafikiv1.Event_ScriptReport:
		sr := p.ScriptReport
		detail = " " + sr.GetKind()
		if data := sr.GetDataJson(); data != "" {
			detail += " " + oneLine(data)
		}
	case *rafikiv1.Event_ScriptOutput:
		// The script's raw output. stdout renders verbatim (one line per
		// newline); stderr is prefixed `stderr| ` so a run's diagnostics are
		// distinguishable from its real output in the same stream of lines.
		// Pipes get plain text; the rendered view's styling rules carry no
		// colour here.
		text := p.ScriptOutput.GetText()
		if p.ScriptOutput.GetStream() == "stderr" {
			text = "stderr| " + text
		}
		detail = " " + oneLine(text)
	default:
		// An unrecognized payload still gets a line in the raw modes (which
		// dump the event before this runs); in the human format a type with
		// nothing to say is noise.
		return ""
	}
	return fmt.Sprintf("%s  %-6s %s  %s%s", t.Format("15:04:05"), typeColumn(ev), id, r.nameOf(id), detail)
}

// typeColumn is the second field of a rendered line: watch's short verbs for
// the six lifecycle types it named, the wire type name for everything else.
func typeColumn(ev *rafikiv1.Event) string {
	switch ev.GetPayload().(type) {
	case *rafikiv1.Event_AgentStatus:
		return "status"
	case *rafikiv1.Event_TurnEnd:
		return "turn"
	case *rafikiv1.Event_ChildSpawned:
		return "spawn"
	case *rafikiv1.Event_ChildExited:
		return "exit"
	case *rafikiv1.Event_Error:
		return "error"
	case *rafikiv1.Event_Retry:
		return "retry"
	default:
		return eventlog.TypeName(ev)
	}
}

func (r *eventRenderer) nameOf(id string) string {
	if name, ok := r.names[id]; ok {
		return name
	}
	// An unnamed child is itself signal — it is exactly what the rail looks
	// like when discovery misses — so say so rather than print an empty cell.
	return "[unnamed]"
}

// exitSummary renders a ChildExited. ExitCode is optional and absence is
// meaningful: a signalled child has no exit code, and 0 means success. Read
// the FIELD for presence — the generated getter collapses absent and 0 into
// the same int32.
func exitSummary(p *rafikiv1.ChildExited) string {
	if p.ExitCode != nil {
		return fmt.Sprintf("code=%d", p.GetExitCode())
	}
	if p.GetSignal() != "" {
		return "signal=" + p.GetSignal()
	}
	return "code=?"
}

// turnStartSummary names what a turn began with: the model and the turn id.
func turnStartSummary(ts *rafikiv1.TurnStart) string {
	var parts []string
	if m := ts.GetModel(); m != "" {
		parts = append(parts, "model="+m)
	}
	if id := ts.GetTurnId(); id != "" {
		parts = append(parts, "turn="+id)
	}
	return strings.Join(parts, " ")
}

// deltaSummary names which kind of fragment streamed and its content.
func deltaSummary(d *rafikiv1.ContentBlockDelta) string {
	var kind, frag string
	switch which := d.GetDelta().(type) {
	case *rafikiv1.ContentBlockDelta_Text:
		kind, frag = "text", which.Text
	case *rafikiv1.ContentBlockDelta_Thinking:
		kind, frag = "thinking", which.Thinking
	case *rafikiv1.ContentBlockDelta_InputJson:
		kind, frag = "input", which.InputJson
	}
	if kind == "" {
		return ""
	}
	return kind + "=" + frag
}

// contentSummary reduces a message's content blocks to one line: text as-is,
// everything else as a bracketed marker. The raw modes carry full blocks.
func contentSummary(blocks []*rafikiv1.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.GetText() != nil:
			parts = append(parts, b.GetText().GetText())
		case b.GetThinking() != nil:
			parts = append(parts, "(thinking)")
		case b.GetToolUse() != nil:
			parts = append(parts, "(tool "+b.GetToolUse().GetName()+")")
		case b.GetImage() != nil:
			parts = append(parts, "(image "+cmp.Or(b.GetImage().GetMediaType(), "?")+")")
		case b.GetToolResult() != nil:
			parts = append(parts, "(result)")
		}
	}
	return strings.Join(parts, " ")
}

// oneLine folds arbitrary text to a single bounded line: whitespace
// collapsed, width capped. Multi-line message text stays one line this way.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return runewidth.Truncate(s, eventSummaryMaxCols, "…")
}

// fmtDur renders a duration the way a status line wants it: 3.2s, 1m12s,
// 1h02m.
func fmtDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
