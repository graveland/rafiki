// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/claudeargv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// A claude child acknowledges a message by ECHOING it: launched with
// --replay-user-messages, Claude Code writes every user message it accepts into
// a turn back onto stdout with the uuid the message carried
// (pkg/child/provider_claude.go, SpawnSpec.OnUserReplay). That echo is the only
// acceptance signal claude offers, and it names the message by the uuid rafiki
// wrote on it — so the daemon has to remember which uuid was which row, in
// memory, for exactly as long as an echo could still arrive.
//
// It is a UI receipt and nothing more. Consume-on-write is unchanged: the row
// is marked consumed when the frame is written (deliverInbox), never on the
// echo, so a claude child's queue still does not survive a restart. See
// docs/reference/control-protocol.md "message_delivered".

// claudeDeliveryKey names one delivery awaiting its echo: the child, the LAUNCH
// that wrote it, and the uuid the message carried.
//
// The launch generation is what makes a stale echo harmless. A resume or a
// respawn starts a new process whose --resume replays the old conversation, and
// a replayed user frame is indistinguishable from a fresh acceptance — so the
// registry is scoped to the launch that minted the uuid, and a callback from a
// retired launch publishes nothing.
type claudeDeliveryKey struct {
	childID string
	gen     uint64
	uuid    string
}

// claudeDeliveryRegistry maps a delivery uuid to the inbox row it names.
//
// Deliberately separate from sentFrames: that map is fundi's, keyed by FRAME id,
// and an entry there means "these rows are written but unconfirmed, so reset
// them on exit". These rows are already consumed on the write, so nothing here
// may touch inbox state, consumeFrames, or awaitAck.
//
// The zero value is usable, and every method takes the lock only for the map
// operation: no caller holds it across a write, a callback, an event publish or
// a database call.
type claudeDeliveryRegistry struct {
	mu      sync.Mutex
	nextGen uint64
	// gens is the CURRENT launch generation per child. A child absent from it
	// has no launch this daemon can acknowledge against, so a delivery to it
	// carries no uuid at all.
	gens map[string]uint64
	rows map[claudeDeliveryKey]string
}

// beginLaunch starts a new launch generation for childID and retires every
// delivery the previous one left outstanding.
//
// Called once per claude launch — spawn, resume and respawn alike — BEFORE the
// process exists, so the callback installed for the launch and the entries
// registered against it agree on the generation.
func (r *claudeDeliveryRegistry) beginLaunch(childID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextGen++
	gen := r.nextGen
	if r.gens == nil {
		r.gens = make(map[string]uint64)
	}
	if r.rows == nil {
		r.rows = make(map[claudeDeliveryKey]string)
	}
	r.gens[childID] = gen
	for k := range r.rows {
		if k.childID == childID {
			delete(r.rows, k)
		}
	}
	return gen
}

// register records rowID as awaiting deliveryUUID's echo on childID's CURRENT
// launch, and reports the generation it landed in.
//
// The generation is read and the entry inserted under one lock hold, so a
// launch that begins mid-delivery retires this entry with the rest of its
// generation instead of leaving it to answer a late echo. ok is false when the
// child has no live launch (a daemon that never launched it, or one whose exit
// already tore the generation down): nothing would ever acknowledge it, so the
// caller writes no uuid.
func (r *claudeDeliveryRegistry) register(childID, deliveryUUID, rowID string) (gen uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen, ok = r.gens[childID]
	if !ok {
		return 0, false
	}
	if r.rows == nil {
		r.rows = make(map[claudeDeliveryKey]string)
	}
	r.rows[claudeDeliveryKey{childID: childID, gen: gen, uuid: deliveryUUID}] = rowID
	return gen, true
}

// take removes deliveryUUID's entry and reports the row it named.
//
// Removal and the current-generation check are ONE critical section: a late
// echo from a retired launch finds its entry already gone, and two echoes for
// the same uuid (claude may replay, and the frame is at-least-once) yield one
// receipt. A miss is the ordinary case, not an error — an unknown uuid, another
// child's uuid, or a replay of a message from a previous launch all publish
// nothing rather than guessing.
func (r *claudeDeliveryRegistry) take(childID string, gen uint64, deliveryUUID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gens[childID] != gen {
		return "", false
	}
	k := claudeDeliveryKey{childID: childID, gen: gen, uuid: deliveryUUID}
	rowID, ok := r.rows[k]
	if !ok {
		return "", false
	}
	delete(r.rows, k)
	return rowID, true
}

// drop forgets ONE delivery whose write definitively failed. Its row stays
// pending (deliverInbox's caller never marks it consumed), so a redelivery
// mints a new uuid and this one must not be able to publish a receipt.
func (r *claudeDeliveryRegistry) drop(childID string, gen uint64, deliveryUUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, claudeDeliveryKey{childID: childID, gen: gen, uuid: deliveryUUID})
}

// endLaunch retires childID's current launch generation: its pending entries go
// and the child stops accepting registrations until the next launch begins.
//
// Generation-scoped by construction — only the generation the child is on NOW
// is touched, and only that generation's entries. Called from handleChildExit,
// which runs before cm.Remove, and every relaunch (Resume, RespawnChild) waits
// for cm.Remove first, so the generation this retires is the one that just
// died rather than a successor's.
func (r *claudeDeliveryRegistry) endLaunch(childID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen, ok := r.gens[childID]
	if !ok {
		return
	}
	delete(r.gens, childID)
	for k := range r.rows {
		if k.childID == childID && k.gen == gen {
			delete(r.rows, k)
		}
	}
}

// claudeReplayHook is the OnUserReplay callback for one claude launch.
//
// The Child fires it SYNCHRONOUSLY on its stdout goroutine, outside the message
// gate and outside the state machine, so it must be cheap and must not block on
// anything that waits for that child. It takes the entry under the registry's
// own lock (dropped before the publish) and then publishes through
// Controller.publishEvent — the one path that appends to the durable event log
// before the native bus, so a receipt survives a resume. Never native.Publish
// directly, and never through the message gate: the echo arrives BEFORE the
// capture persists the row that gate is holding.
func (c *Controller) claudeReplayHook(childID string, gen uint64) func(string) {
	return func(deliveryUUID string) {
		rowID, ok := c.claudeDeliveries.take(childID, gen, deliveryUUID)
		if !ok {
			return
		}
		c.publishEvent(childID, &rafikiv1.Event{
			ChildId: childID,
			Ts:      timestamppb.Now(),
			Payload: &rafikiv1.Event_MessageDelivered{MessageDelivered: &rafikiv1.MessageDelivered{
				MessageId: rowID,
			}},
		})
	}
}

// tracksClaudeDelivery reports whether batch b is one message whose acceptance
// claude will echo back, and therefore carries a delivery uuid.
//
// Only a DIRECT prompt or steer qualifies. A batch with a Source is a
// notification the daemon injected (a job exit, a subagent report, a schedule)
// and no client is waiting on it; an abort is a signal, not a message, and
// claude has no abort frame at all. Exactly one row, because the echo names ONE
// message and a coalesced batch would have to guess which.
func tracksClaudeDelivery(kind string, b inbox.Batch) bool {
	if kind != protocol.KindClaude {
		return false
	}
	if b.Source != "" {
		return false
	}
	if b.Mode != inbox.ModePrompt && b.Mode != inbox.ModeSteer {
		return false
	}
	return len(b.IDs) == 1
}

// claudeDeliveryFor mints the delivery uuid for one batch and registers it
// against the child's current launch. It returns "" (and no generation) for a
// batch that is not tracked, which is every batch but a direct claude message.
//
// The uuid is a v7, minted here and never persisted: it exists to name one
// write to one process, and the design allows the time-ordered simplification
// (a deterministic id derived from the row is NOT required). Nothing is
// invented on error — an unmintable uuid fails the delivery, because a frame
// with a fabricated or empty uuid would be one whose echo can never be matched
// and whose receipt could name the wrong message.
func (c *Controller) claudeDeliveryFor(b inbox.Batch) (deliveryUUID string, gen uint64, err error) {
	snap, ok := c.st.Get(b.ChildID)
	if !ok || !tracksClaudeDelivery(snap.Kind, b) {
		return "", 0, nil
	}
	u, err := uuid.NewV7()
	if err != nil {
		return "", 0, fmt.Errorf("inbox: mint a claude delivery uuid: %w", err)
	}
	gen, ok = c.claudeDeliveries.register(b.ChildID, u.String(), b.IDs[0])
	if !ok {
		// No live launch to acknowledge against. Deliver untracked rather than
		// write a uuid nothing will ever resolve.
		return "", 0, nil
	}
	return u.String(), gen, nil
}

// claudeLaunchStderrBytes caps how much of a failed launch's stderr the error
// event carries — the same bound a failed script settles with
// (scriptStderrTailBytes): enough for the binary's own usage/error text, small
// enough to ride an event.
const claudeLaunchStderrBytes = scriptStderrTailBytes

// claudeUnsupportedFlagHint is the actionable half of a claude launch failure
// whose stderr says the binary rejected a flag.
//
// rafiki launches every headless claude child with claudeargv.ReplayUserMessages
// — the echo is the only acceptance signal Claude Code offers — and there is
// deliberately no probe, no version floor and no relaunch without it (see that
// constant's doc comment). So an older binary fails at launch and the fix is to
// upgrade it, which is what this says.
const claudeUnsupportedFlagHint = "rafiki launches every headless claude child with " +
	claudeargv.ReplayUserMessages + " (the message-acceptance echo). " +
	"Upgrade Claude Code: rafiki never relaunches a child without that flag."

// claudeLaunchFailure builds the error event for a claude child that died at
// LAUNCH, or nil for a child that ran and then stopped.
//
// The distinction is the session: a claude process reports its session id on
// the first turn's system/init, so a child that exits with none never got that
// far — the binary rejected its argv, could not authenticate, or died on
// startup. A signal is a kill (an operator's, or the abort ladder), not a
// launch failure, and exit 0 is a clean end. The stderr is carried verbatim
// (bounded) rather than summarised, because the binary's own words are the
// actionable part; the upgrade hint is added only when that text says a flag
// was rejected, so it never claims a cause the child did not name.
func claudeLaunchFailure(kind, sessionID string, res child.ShutdownResult, stderr []byte) *rafikiv1.ErrorEvent {
	if kind != protocol.KindClaude || sessionID != "" {
		return nil
	}
	if res.Signal != "" || res.ExitCode == 0 {
		return nil
	}
	tail := strings.TrimSpace(lastStderrTail(stderr, claudeLaunchStderrBytes))
	if tail == "" {
		return nil
	}
	msg := fmt.Sprintf("claude exited (%d) before establishing a session:\n%s", res.ExitCode, tail)
	if rejectsAFlag(tail) {
		msg += "\n\n" + claudeUnsupportedFlagHint
	}
	return &rafikiv1.ErrorEvent{Code: "claude_launch_failed", Message: msg}
}

// rejectsAFlag reports whether stderr is a CLI's rejection of an argument.
//
// Only the phrasings the launch actually produces are matched — Claude Code is a
// commander-based CLI and answers an unsupported flag with "error: unknown
// option '--x'". Anything else (an auth failure, a missing config) carries its
// stderr without the upgrade hint, which would be a wrong diagnosis there.
func rejectsAFlag(stderr string) bool {
	low := strings.ToLower(stderr)
	for _, phrase := range []string{
		"unknown option",
		"unknown argument",
		"unrecognized option",
		"unrecognised option",
		"invalid option",
	} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}
