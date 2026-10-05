// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// This file is the daemon-side hub for the three script-child verbs — the
// implementation behind connectapi.ScriptHub (Report / Receive / SetResult).
// The Connect handler (pkg/connectapi script.go) resolves the caller from the
// credential and enforces the size caps; everything past that lives here,
// because only the Controller holds the event buffer a report is pushed to,
// the inbox a Receive stream pulls from, and the childstore a result is
// written to.
//
// All three verbs act on the CALLER'S OWN position in the tree, resolved from
// the credential — never from a request field. Report pushes upward to the
// caller's own parent; Receive and SetResult are strictly self-only.

// scriptEventSource is the event-buffer source name for script progress
// reports. One source per concern keeps a coordinator's injected frame
// readable, the same rule as subagentEventSource: the buffer coalesces per
// (child, source), so a script's reports land in their own batches rather than
// being interleaved with subagent settles or budget warnings.
const scriptEventSource = "script"

// scriptReceiveInterval is how often an open Receive stream polls its child's
// inbox and status. The inbox has no push channel for a stream consumer, so
// the stream polls; the interval is a package var so the hub tests can
// shorten it.
var scriptReceiveInterval = 500 * time.Millisecond

// connectScriptHub returns the hub to hand to connectapi.Server.SetScriptHub.
func (c *Controller) connectScriptHub() connectapi.ScriptHub { return scriptHub{c: c} }

// scriptHub implements connectapi.ScriptHub against one Controller.
type scriptHub struct{ c *Controller }

var _ connectapi.ScriptHub = scriptHub{}

// Report publishes one progress report from child callerID — any child may
// report, not only a script.
//
// A PARENTED child's report is pushed into its parent's event buffer: a
// `progress` report is keyed on callerID, so each tick supersedes the last
// (last write wins), while every other kind accumulates — see the push site
// below. Either way it coalesces and defers exactly like a subagent settle
// (notifySubagentSettled): several reports between the parent's turns cost one
// injected frame, not one per report. The push is fire-and-forget — the parent
// reads the coalesced frame on its next turn boundary, and nothing in the
// Report round trip waits for that.
//
// A TOP-LEVEL script (no parent) has nobody to push to, so its report is
// appended to its OWN durable event log instead (script_report is a durable
// type, so it gets an ordinal and is resumable). Same fallback when no event
// buffer is configured: an append to the child's own log is worth having even
// when the coalescing buffer is not.
func (h scriptHub) Report(ctx context.Context, callerID, kind, dataJSON string) error {
	snap, ok := h.c.st.Get(callerID)
	if !ok {
		return connect.NewError(connect.CodeNotFound,
			fmt.Errorf("unknown child %q", callerID))
	}
	parent, hasParent := h.c.st.ParentOf(callerID)
	if hasParent && parent != "" && h.c.evbuf != nil {
		// A progress report is a tick on work the parent already knows is
		// running: keyed on the caller, each new tick supersedes the last, so a
		// chatty reporter costs one fragment rather than one per tick. Every
		// other kind is a message — a warning, an error, a result — that must
		// not be overwritten by whatever reports next, so it accumulates
		// (key "" — eventbuf.Buffer.Push: "key is last-write-wins WITHIN
		// (childID, source); key == \"\" accumulates").
		pushKey := ""
		if kind == "progress" {
			pushKey = callerID
		}
		h.c.evbuf.Push(parent, scriptEventSource, pushKey,
			scriptReportFragment(callerID, snap.Name, kind, dataJSON))
		return nil
	}
	h.c.publishEvent(callerID, &rafikiv1.Event{
		ChildId: callerID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ScriptReport{ScriptReport: &rafikiv1.ScriptReport{
			Kind:     kind,
			DataJson: dataJSON,
		}},
	})
	return nil
}

// scriptReportFragment is the wording a parent's injected frame carries for
// one report. ANY child may report through the Report verb — a script's
// progress tick, a subagent's warning, a note from any child in the tree — so
// the wording is kind-neutral ("agent", not "script"). The fragment
// deliberately does NOT summarise the payload: like settleFragment, it says
// what happened and renders the source through renderJSONPayload (a JSON
// string decoded, any other JSON value verbatim) — a digest that tried to
// interpret a JSON payload it knows nothing about would be a lossy copy of it.
func scriptReportFragment(childID, name, kind, dataJSON string) string {
	if name == "" {
		name = "unnamed"
	}
	return fmt.Sprintf("agent %s (%s) reported %s: %s", childID, name, kind, renderJSONPayload(dataJSON))
}

// SetResult stores the calling child's final result. Last write wins:
// every call replaces the stored value, and the value present when the child
// settles rides the settle fragment (notifySubagentSettled) and GetChild.
// For a NON-SCRIPT child the stored result is per turn: when its next turn
// starts, handleStatusChange clears it, so a result from an earlier turn never
// rides a later settle fragment. A script child's result is the work product
// of its whole run and is never cleared this way.
//
// The in-memory session is updated first, then the durable row is written from
// the same snapshot — the same order every other persistent field write uses,
// so a crash between the two leaves at worst a stale row, never a phantom
// result the session never held.
func (h scriptHub) SetResult(ctx context.Context, callerID, resultJSON string) error {
	if err := h.c.st.Update(callerID, func(s *childstore.Session) {
		s.Result = resultJSON
	}); err != nil {
		if errors.Is(err, childstore.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound,
				fmt.Errorf("unknown child %q", callerID))
		}
		return connect.NewError(connect.CodeInternal,
			fmt.Errorf("store result: %w", err))
	}
	if err := h.c.writeRecord(callerID); err != nil {
		// The session holds the value; the row write is the durability half.
		// Warn, don't fail: the caller's result IS recorded in memory, and the
		// next ordinary writeRecord (status change, exit) carries it.
		slog.Warn("persist script result", "childId", callerID, "error", err)
	}
	return nil
}

// Receive opens the calling script child's message stream.
//
// Everything addressed to the child's inbox from now on is streamed as a Text
// message — one inbox row per message, its durable row id on message_ids — and
// the lifecycle events a script must react to arrive as the stop variant: an
// inbox abort row, the child entering shutting_down (the operator's kill path),
// and the child's exit. Each stop ENDS the stream, because to a script all
// three mean "stop what you are doing" and no further delivery is defined
// after one.
//
// Delivery here is at-most-once, in the same family as the claude children's
// contract but slightly weaker, and the difference is worth stating: a claude
// child's failed frame write leaves its rows pending for the idle-drain retry,
// while this stream consumes rows at the PULL and a stream that dies between
// the pull and the wire has nothing left to retry — the batch is lost. What is
// NOT lost is a row held in the retained batch below: the stream delivers
// every row it pulled. Pull takes the same per-child lock Queue.deliver takes,
// so the immediate delivery an agent_send triggers and this stream can never
// both read the same rows.
//
// A row is also only ever pending here because no engine consumed it on the
// write: today that means a child with no working stdin writer, which is
// exactly the shape the script runtime (wave 3) spawns. A fundi child's rows
// leave pending inside deliverInbox and never reach this stream — its inbox is
// its stdin, not this face.
func (h scriptHub) Receive(ctx context.Context, callerID string) (connectapi.ScriptStream, error) {
	if _, ok := h.c.st.Get(callerID); !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("unknown child %q", callerID))
	}
	if h.c.inbox == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("inbox not configured"))
	}
	return &scriptStream{c: h.c, childID: callerID}, nil
}

// scriptStream is one open Receive stream. It polls the child's inbox and
// lifecycle state on scriptReceiveInterval and hands each delivery to Recv's
// caller.
type scriptStream struct {
	c       *Controller
	childID string
	// pending is the batch the last pull consumed and this stream has not
	// delivered yet. Pull consumes EVERY pending row at once, so the stream
	// retains the rest of the batch here and drains it across Recv calls
	// before polling again — a pull that returned N rows delivers N messages,
	// never fewer (see pollOnce).
	pending []inbox.Inbound
	// done latches the terminal stop: after it, every Recv returns io.EOF.
	done bool
}

// Recv blocks until the next message is available. It returns io.EOF after the
// stream's terminal stop, and the context's error when the caller cancels.
func (s *scriptStream) Recv(ctx context.Context) (*rafikiv1.ScriptMessage, error) {
	// A terminal stop from a previous call ends the stream for good — Recv is
	// not required to be idempotent past EOF, but it must not resurrect a
	// stream it already ended.
	if s.done {
		return nil, io.EOF
	}
	ticker := time.NewTicker(scriptReceiveInterval)
	defer ticker.Stop()
	for {
		m, stop, err := s.pollOnce(ctx)
		if err != nil {
			return nil, err
		}
		if m != nil {
			if stop {
				s.done = true
			}
			return m, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollOnce delivers work first and stops last: the inbox is drained (the
// retained batch first, then a fresh poll) before the lifecycle state is
// consulted, so a batch and a stop arriving in the same tick deliver the work
// before ending the stream.
func (s *scriptStream) pollOnce(ctx context.Context) (*rafikiv1.ScriptMessage, bool, error) {
	if len(s.pending) == 0 {
		rows, err := s.c.inbox.Pull(ctx, s.childID)
		if err != nil {
			return nil, false, connect.NewError(connect.CodeInternal,
				fmt.Errorf("pull inbox: %w", err))
		}
		s.pending = rows
	}
	// Work first, stop last. The text rows AHEAD of an abort row are delivered
	// before the abort's stop; the rows BEHIND it are discarded — a stop was
	// requested, and text accepted after a stop is not defined work. With no
	// abort in the batch, every pulled row is delivered, one message per row.
	work := s.pending
	for i, row := range s.pending {
		if row.Mode == inbox.ModeAbort {
			work = s.pending[:i]
			break
		}
	}
	if len(work) > 0 {
		row := work[0]
		s.pending = s.pending[1:]
		return textMessage(row), false, nil
	}
	if len(s.pending) > 0 {
		// Only an abort row remains: an abort is delivered as the stop variant,
		// never as text — to a script both mean "stop what you are doing".
		s.pending = nil
		return stopMessage("abort requested"), true, nil
	}

	snap, ok := s.c.st.Get(s.childID)
	if !ok {
		// The child left the store (forgotten/closed): nothing further will
		// ever be delivered.
		return stopMessage("child forgotten"), true, nil
	}
	switch snap.Status {
	case protocol.StatusShuttingDown:
		// The operator's kill path. The process-group signal this precedes is
		// the backstop that still reaps a script that ignores the stream.
		return stopMessage("stopping"), true, nil
	case protocol.StatusExited:
		return stopMessage("child exited"), true, nil
	}
	return nil, false, nil
}

func textMessage(row inbox.Inbound) *rafikiv1.ScriptMessage {
	mode := rafikiv1.SendMode_SEND_MODE_PROMPT
	if row.Mode == inbox.ModeSteer {
		mode = rafikiv1.SendMode_SEND_MODE_STEER
	}
	blocks := make([]*rafikiv1.ImageBlock, 0, len(row.Attachments))
	for _, a := range row.Attachments {
		blocks = append(blocks, &rafikiv1.ImageBlock{MediaType: a.MediaType, Data: a.Data})
	}
	return &rafikiv1.ScriptMessage{
		Body: &rafikiv1.ScriptMessage_Text_{Text: &rafikiv1.ScriptMessage_Text{
			Text:        row.Text,
			Mode:        mode,
			MessageIds:  []string{row.ID},
			Attachments: blocks,
		}},
	}
}

func stopMessage(reason string) *rafikiv1.ScriptMessage {
	return &rafikiv1.ScriptMessage{
		Body: &rafikiv1.ScriptMessage_Stop_{Stop: &rafikiv1.ScriptMessage_Stop{
			Reason: reason,
		}},
	}
}
