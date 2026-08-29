// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/inboxdb"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// inboxStore returns a durable inbox store, or an in-memory one when there is
// no database. Mirrors eventLogStore.
func inboxStore(pool *pgxpool.Pool) inbox.Store {
	if pool == nil {
		return inbox.NewMemory()
	}
	return inboxdb.New(pool)
}

// sentFrame is one written-but-unconfirmed injection: the rows it accounts for
// and the child that owes an ack.
//
// It is deliberately NOT persisted. Losing it means the rows stay 'sent' and a
// restart returns them to pending, which is exactly the behaviour we want —
// the process that held the map is the process the child died with.
type sentFrame struct {
	childID string
	rowIDs  []string
}

// inboxBatchConfig reads the caps on one delivered batch.
//
// They are read here rather than into eventbuf.Config because coalescing
// happens at DELIVERY now, over the persisted rows: the buffer schedules, the
// rows decide what is in the batch. RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT is
// the one cap that still belongs to the buffer, because it applies on the way
// in — an oversized fragment must never reach the store at all.
func inboxBatchConfig() inbox.BatchConfig {
	return inbox.BatchConfig{
		MaxFragments:     envInt("RAFIKI_EVENTBUF_MAX_FRAGMENTS", 30),
		MaxBytesPerFlush: envInt("RAFIKI_EVENTBUF_MAX_BYTES_PER_FLUSH", 65536),
	}
}

// newInboxQueue builds the controller's queue. Validation runs before persist
// so a send to a dead child is still a clean error rather than a row nobody
// will consume.
func (c *Controller) newInboxQueue(store inbox.Store) *inbox.Queue {
	return inbox.NewQueue(inbox.QueueConfig{
		Store:    store,
		Validate: c.validateSendTarget,
		Deliver:  c.deliverInbox,
		Batch:    c.inboxBatch,
	})
}

// validateSendTarget is the command-plane check: does this child exist and can
// it still be sent to. Shared by the queue (before persisting) and sendFrame
// (before writing), so the two cannot drift.
func (c *Controller) validateSendTarget(childID string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	switch snap.Status {
	case protocol.StatusShuttingDown:
		return &control.ControllerError{Code: protocol.ErrChildShuttingDown, Message: "child is shutting down"}
	case protocol.StatusExited:
		return &control.ControllerError{Code: protocol.ErrChildExited, Message: "child has exited"}
	}
	return nil
}

// inboundFromFrame recognises the three frames that mean "work for a turn".
// Everything else — get_state, extension_ui_response, new_session — is a
// control frame and must keep going straight to the child.
func inboundFromFrame(childID string, frame json.RawMessage) (inbox.Inbound, bool) {
	var hdr struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return inbox.Inbound{}, false
	}
	switch hdr.Type {
	case "prompt":
		return inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Text: hdr.Message}, true
	case "steer":
		return inbox.Inbound{ChildID: childID, Mode: inbox.ModeSteer, Text: hdr.Message}, true
	case "abort":
		return inbox.Inbound{ChildID: childID, Mode: inbox.ModeAbort}, true
	}
	return inbox.Inbound{}, false
}

// buildInjectionFrame renders one batch as the child-facing frame.
//
// The id is the FRAME id, not a row id: a coalesced batch is many rows and one
// frame, and the child can only echo one identifier. The controller maps it
// back through sentFrames.
//
// An empty frameID is omitted rather than written as "": it means nothing acks
// this frame, which is the orphan path (no rows to retire) and the pre-durable
// behaviour for any runtime that does not speak the ack.
//
// An abort carries no id because nothing acks it — there is no turn for it to
// enter, only one to cancel.
func buildInjectionFrame(b inbox.Batch, frameID string) (json.RawMessage, error) {
	if b.Mode == inbox.ModeAbort {
		return json.RawMessage(`{"type":"abort"}`), nil
	}
	kind := "prompt"
	if b.Mode == inbox.ModeSteer {
		kind = "steer"
	}
	body := strings.Join(b.Frags, "\n")
	if b.Source != "" {
		body = "<rafiki-event source=\"" + b.Source + "\">\n" + body + "\n</rafiki-event>"
	}
	out, err := json.Marshal(struct {
		Type    string `json:"type"`
		ID      string `json:"id,omitempty"`
		Message string `json:"message"`
	}{kind, frameID, body})
	if err != nil {
		return nil, fmt.Errorf("inbox: marshal injection frame: %w", err)
	}
	return out, nil
}

// deliverInbox writes one batch to its child and reports whether an ack is
// coming.
//
// Only a fundi child confirms consumption: it is the only runtime that speaks
// a protocol with the seam. A claude child's rows are consumed on the write,
// which is exactly today's guarantee and no worse — but it is also why a
// claude child's queue does not survive a restart.
func (c *Controller) deliverInbox(ctx context.Context, b inbox.Batch) (bool, error) {
	// Honour the caller's deadline. Every deadline on this path is generous
	// and sendFrame itself never blocks, so this only ever fires part-way
	// through a multi-batch delivery whose store round trips already ran out
	// of time -- where writing one more frame is exactly the wrong move.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	// A stored abort aimed at a claude child is retired here rather than
	// written. sendFrame routes such a frame into handleClaudeAbort, which
	// SIGINTs, polls for the exit and respawns -- all while Queue.deliver
	// holds this child's lock, which handleChildExit needs to reset the same
	// child's unconfirmed rows. The abort would spin out its whole wait for a
	// cm.Remove that cannot happen until it lets go, then resume a child the
	// blocked exit path is about to remove from the manager.
	//
	// Controller.Send and connectAccepter both classify before persisting, so
	// no such row is created today. This is the guard on the one point every
	// delivery funnels through, which is where it survives a third submitter
	// -- and replaying a stored cancellation into an unrelated later turn was
	// never wanted regardless of the lock.
	if b.Mode == inbox.ModeAbort && c.isClaudeAbortTarget(b.ChildID) {
		slog.Warn("inbox: retiring a stored abort for a claude child rather than replaying an interrupt",
			"childId", b.ChildID, "rows", len(b.IDs))
		return false, nil
	}

	awaitAck := b.Mode != inbox.ModeAbort
	if snap, ok := c.st.Get(b.ChildID); !ok || snap.Kind != protocol.KindFundi {
		awaitAck = false
	}

	var frameID string
	if awaitAck {
		id, err := inbox.NewID()
		if err != nil {
			return false, err
		}
		frameID = id
	}

	frame, err := buildInjectionFrame(b, frameID)
	if err != nil {
		return false, err
	}

	// Record the frame BEFORE the write. The ack arrives on the child's own
	// goroutine and, for an in-process fundi child, can land before sendFrame
	// has returned; a map written afterwards would be missing exactly the
	// entry that ack needs, and the rows would sit 'sent' until a restart.
	if awaitAck {
		c.sentMu.Lock()
		if c.sentFrames == nil {
			c.sentFrames = make(map[string]sentFrame)
		}
		c.sentFrames[frameID] = sentFrame{childID: b.ChildID, rowIDs: b.IDs}
		c.sentMu.Unlock()
	}

	if err := c.sendFrame(b.ChildID, frame); err != nil {
		if awaitAck {
			c.sentMu.Lock()
			delete(c.sentFrames, frameID)
			c.sentMu.Unlock()
		}
		return false, err
	}
	return awaitAck, nil
}

// consumeFrames retires the rows behind frames the child confirmed it took
// into a turn.
//
// Row ids are grouped by CHILD before being retired. Queue.Consume takes the
// per-child lock on the childID it is given but hands the whole id slice to
// the store, which does not check ownership — so a childID borrowed from the
// first frame would take child A's lock while mutating child B's rows, the
// one cross-child mutation this design forbids everywhere. In practice an ack
// callback is bound to one child, but nothing in this signature enforces it.
func (c *Controller) consumeFrames(frameIDs []string) {
	if c.inbox == nil || len(frameIDs) == 0 {
		return
	}
	byChild := make(map[string][]string)
	var order []string
	c.sentMu.Lock()
	for _, f := range frameIDs {
		sf, ok := c.sentFrames[f]
		if !ok {
			continue
		}
		if _, seen := byChild[sf.childID]; !seen {
			order = append(order, sf.childID)
		}
		byChild[sf.childID] = append(byChild[sf.childID], sf.rowIDs...)
		delete(c.sentFrames, f)
	}
	c.sentMu.Unlock()
	if len(byChild) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, childID := range order {
		if err := c.inbox.Consume(ctx, childID, byChild[childID]); err != nil {
			slog.Warn("inbox: consume failed; rows stay sent and will replay",
				"childId", childID, "error", err)
		}
	}
}

// forgetFrames drops childID's unconfirmed frame bookkeeping. The ROWS are not
// touched: whether they are reset or dropped is the caller's decision.
func (c *Controller) forgetFrames(childID string) {
	c.sentMu.Lock()
	defer c.sentMu.Unlock()
	for id, sf := range c.sentFrames {
		if sf.childID == childID {
			delete(c.sentFrames, id)
		}
	}
}

// acceptAndDeliver persists a message and attempts immediate delivery.
//
// A delivery failure is NOT the caller's error: the message is durably
// accepted, and the child's next idle transition drains it. Only a validation
// or persistence failure is reported back.
func (c *Controller) acceptAndDeliver(ctx context.Context, in inbox.Inbound) (string, error) {
	id, err := c.inbox.Accept(ctx, in)
	if err != nil {
		return "", err
	}
	if derr := c.inbox.Deliver(ctx, in.ChildID, in.Source); derr != nil {
		slog.Warn("inbox: immediate delivery failed; message stays queued",
			"childId", in.ChildID, "messageId", id, "error", derr)
	}
	return id, nil
}

// flushInboxSource is eventbuf's FlushFn: the buffer decided it is time, the
// rows decide what goes.
func (c *Controller) flushInboxSource(childID, source string, orphans []inbox.Inbound) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if c.inbox != nil {
		if err := c.inbox.Deliver(ctx, childID, source); err != nil {
			slog.Warn("eventbuf: flush failed; fragments stay queued",
				"childId", childID, "source", source, "error", err)
		}
	}
	c.deliverOrphans(childID, orphans)
}

// deliverOrphans injects fragments whose durable write failed. They have no
// rows, so they are never marked sent or consumed and never await an ack:
// delivered and forgotten.
//
// They go through inbox.Coalesce and buildInjectionFrame — the SAME code the
// durable path uses — rather than a hand-built frame. That is what keeps a
// PushSteer that lost its database a steer: Mode travels on the row, and a
// second frame builder with a hardcoded mode would silently downgrade it to a
// prompt exactly when the news was urgent enough to interrupt a turn.
func (c *Controller) deliverOrphans(childID string, orphans []inbox.Inbound) {
	for _, b := range inbox.Coalesce(orphans, c.inboxBatch) {
		frame, err := buildInjectionFrame(b, "")
		if err != nil {
			slog.Warn("eventbuf: marshal orphan frame", "childId", childID, "error", err)
			continue
		}
		if err := c.sendFrame(b.ChildID, frame); err != nil {
			slog.Warn("eventbuf: orphan injection failed",
				"childId", b.ChildID, "source", b.Source, "error", err)
		}
	}
}

// isClaudeAbortTarget reports whether an abort aimed at childID is the claude
// interrupt+resume cycle rather than a message.
//
// claude -p has no in-band abort frame: the only way to stop it is to signal
// the process and resume the session. That is a SIGNAL plus a lifecycle
// operation, not something with a place in a queue — which is why every entry
// point must recognise it BEFORE anything is persisted. One predicate, so the
// framed path and the Connect path cannot come to different conclusions.
func (c *Controller) isClaudeAbortTarget(childID string) bool {
	snap, ok := c.st.Get(childID)
	return ok && snap.Kind == protocol.KindClaude
}

// connectAccepter is the inbox seam the Connect face holds.
//
// It is NOT the bare queue. Controller.Send classifies before it persists, and
// a Connect Send that went straight to Queue.Accept skipped that: a claude
// abort became a row, and a crash or a failed delivery between Accept and
// delivery would leave it PENDING for the idle drain to replay — delivering a
// stored cancellation as SIGINT+resume into an unrelated later turn.
//
// It also keeps handleClaudeAbort out of Queue.deliver, which holds the
// per-child lock while it runs. handleClaudeAbort signals, polls for the exit,
// then respawns; the exit path takes that same per-child lock to reset the
// child's unconfirmed rows, from monitorChild's goroutine. Keeping claude
// aborts out of the queue makes that mutual wait unreachable by construction
// rather than survivable by timeout.
//
// The Controller is the right place for this: it is what knows child kinds.
// connectapi sees only inbox.Accepter and still cannot deliver or ack.
type connectAccepter struct{ c *Controller }

// Accept classifies, then delegates.
//
// A claude abort returns an EMPTY message id. SendResponse.message_id names a
// durable row the caller may quote back, and there is no row: the abort
// happened synchronously and completely, and there is nothing left to track.
// Minting an id here would hand the caller a handle that resolves to nothing
// in every store, which is worse than saying nothing.
func (a connectAccepter) Accept(ctx context.Context, in inbox.Inbound) (string, error) {
	if in.Mode == inbox.ModeAbort && a.c.isClaudeAbortTarget(in.ChildID) {
		return "", a.c.handleClaudeAbort(in.ChildID)
	}
	// acceptAndDeliver, not a bare Accept: the framed path gives a submitter
	// persist-then-deliver, and a Connect submitter whose prompt only got
	// persisted would wait for the child's next idle transition to see it —
	// which for an already-idle child may never come.
	return a.c.acceptAndDeliver(ctx, in)
}

// connectInbox returns the accepter to hand to connectapi.Server.SetInbox.
func (c *Controller) connectInbox() inbox.Accepter { return connectAccepter{c: c} }

// inboxRetention is how long a consumed or dropped row stays readable. Long
// enough to answer "what did that agent receive this morning", short enough
// that the table does not become a second conversation store.
const inboxRetention = 24 * time.Hour

// drainInbox delivers anything still pending for childID. Called on the
// child's idle transition: it is the retry path for an immediate delivery
// that failed, and it costs one indexed read when there is nothing to do.
//
// Only on the transition INTO idle, never on every status change. eventbuf's
// fragments are inbox rows, persisted on Push and held pending while the
// buffer debounces and while the child is mid-turn; a drain on any other
// transition would deliver them the moment the child started working, which
// bypasses the debounce and the busy gate together.
//
// The queue is a PARAMETER rather than a field read, because this is the one
// hook that runs detached: `go c.drainInbox(c.inbox, childID)` evaluates the
// argument on the goroutine that made the decision, so the queue this drain
// uses is the one that existed when the child went idle.
func (c *Controller) drainInbox(q *inbox.Queue, childID string) {
	if q == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := q.DeliverAll(ctx, childID); err != nil {
		slog.Warn("inbox: idle drain failed", "childId", childID, "error", err)
	}
}

// releaseInboxOnExit returns childID's unconfirmed rows to pending.
//
// NOT a drop: an exited child can be resumed, and its queue is exactly what a
// resume should run. Dropping here would silently discard work a `rafiki
// resume` was about to perform. Scoped to this child — rows are shared across
// daemons, so an unscoped transition reaches into another daemon's traffic.
//
// forgetFrames goes with it, and covers more than the rows this call resets:
// a frame whose MarkSent failed is recorded in sentFrames and its rows stay
// pending, so nothing else would ever retire that entry. The child's death is
// the one moment every one of its entries is known to be dead.
func (c *Controller) releaseInboxOnExit(childID string) {
	if c.inbox == nil {
		return
	}
	c.forgetFrames(childID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := c.inbox.Reset(ctx, childID)
	if err != nil {
		slog.Warn("inbox: reset on exit failed", "childId", childID, "error", err)
		return
	}
	if n > 0 {
		slog.Info("inbox: unconfirmed messages returned to the queue", "childId", childID, "count", n)
	}
}

// dropInboxForForgotten terminates childID's queue and records it in the
// durable event log.
//
// Forgetting a child is the one moment there will never be a turn to inject
// into. It is also the only moment a row can be terminated safely: a row for a
// forgotten child is never pending-for-a-live-child again and never terminal
// on its own, so the retention sweep — which deletes terminal rows only —
// could never reach it, and it would leak permanently.
//
// The event log is where a dropped message goes on the record: an operator
// asking "what happened to the prompt I sent" gets an answer with an ordinal
// rather than a log line that has already rotated away.
func (c *Controller) dropInboxForForgotten(childID, reason string) {
	if c.inbox == nil {
		return
	}
	c.forgetFrames(childID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := c.inbox.Drop(ctx, childID, reason)
	if err != nil {
		slog.Warn("inbox: drop failed", "childId", childID, "error", err)
		return
	}
	if n == 0 {
		return
	}
	slog.Warn("inbox: dropped undelivered messages", "childId", childID, "count", n, "reason", reason)
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
			Code:    "inbox_dropped",
			Message: fmt.Sprintf("%d undelivered message(s) dropped: %s", n, reason),
		}},
	})
}

// replayInbox returns childID's unconfirmed rows to pending and delivers
// everything queued for it.
//
// Called once per child THIS DAEMON RESUMED, never as a sweep over the table.
// Child rows are shared across daemons, so a global reset would resurrect
// another live daemon's in-flight messages and deliver every one of them
// twice.
func (c *Controller) replayInbox(ctx context.Context, childID string) {
	if c.inbox == nil {
		return
	}
	n, err := c.inbox.Reset(ctx, childID)
	if err != nil {
		slog.Warn("inbox: replay reset failed", "childId", childID, "error", err)
		return
	}
	if n > 0 {
		slog.Info("inbox: replaying messages the previous daemon never confirmed",
			"childId", childID, "count", n)
	}
	if err := c.inbox.DeliverAll(ctx, childID); err != nil {
		slog.Warn("inbox: replay delivery failed; messages stay queued",
			"childId", childID, "error", err)
	}
}

// sweepInbox deletes terminal rows older than inboxRetention. Called from the
// existing 5-minute sweeper tick, so it needs no timer of its own.
func (c *Controller) sweepInbox() {
	if c.inbox == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := c.inbox.Sweep(ctx, time.Now().Add(-inboxRetention))
	if err != nil {
		slog.Warn("inbox: sweep failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("inbox: swept terminal rows", "count", n)
	}
}
