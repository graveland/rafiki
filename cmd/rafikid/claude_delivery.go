// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"slices"
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

// claudeDelivery is one registered delivery: the inbox row it names and what
// that row carried. The text and image count are kept because a captured user
// message has to be matched back to the row that delivered it, and text is what
// the frame said rather than what the client typed.
type claudeDelivery struct {
	rowID  string
	text   string
	images int
}

// acceptedDelivery is one delivery Claude Code has accepted and the capture
// that stores its message has not yet reported. It outlives take (the receipt
// publishes immediately) because the captured message arrives later, on the
// request that carried it.
//
// responseID names that request: it is the message id of the first main-thread
// assistant frame printed after the echo ("" until that frame is seen), which
// is the same id the request's capture reports. Binding is by stdout order
// alone — see bind.
type acceptedDelivery struct {
	rowID      string
	text       string
	images     int
	gen        uint64
	responseID string
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
	// insts is the generation each live Child INSTANCE was launched under. The
	// exit path needs it because it is handed a Child, not a generation, and
	// "whatever is current now" is not the same thing: a client can Resume the
	// child while the old exit is still finishing (MarkExited is near the top of
	// handleChildExit, endLaunch near the bottom), and retiring the successor's
	// generation would silently stop every receipt for the resumed launch.
	insts map[*child.Child]uint64
	rows  map[claudeDeliveryKey]claudeDelivery
	// accepted is the echo-ordered queue of deliveries accepted but not yet
	// linked to a captured message, per child. A row leaves it when its
	// response's capture reports (link), when its response had already reported
	// by the time it was bound (bind), when its launch retires, or when the
	// queue overflows.
	accepted map[string][]acceptedDelivery
	// lastResponse is the response id the child's latest main-thread assistant
	// frame carried, per child. bind acts only on a CHANGE of id: every content
	// block of one response prints its own frame, and an echo printed between
	// two of them belongs to a later request, not to the response in progress.
	lastResponse map[string]string
	// reported is the per-child ring of response ids whose capture has already
	// linked, newest last, capped at claudeLinkQueueCap. A delivery bound to one
	// of them missed its capture (the report beat the stdout frame), and nothing
	// will ever report that id again.
	reported map[string][]string
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
		r.rows = make(map[claudeDeliveryKey]claudeDelivery)
	}
	r.gens[childID] = gen
	for k := range r.rows {
		if k.childID == childID {
			delete(r.rows, k)
		}
	}
	delete(r.accepted, childID)
	delete(r.lastResponse, childID)
	delete(r.reported, childID)
	return gen
}

// register records rowID as awaiting deliveryUUID's echo on childID's CURRENT
// launch, and reports the generation it landed in. text and images are what the
// row carried, kept for the eventual link to its captured message.
//
// The generation is read and the entry inserted under one lock hold, so a
// launch that begins mid-delivery retires this entry with the rest of its
// generation instead of leaving it to answer a late echo. ok is false when the
// child has no live launch (a daemon that never launched it, or one whose exit
// already tore the generation down): nothing would ever acknowledge it, so the
// caller writes no uuid.
func (r *claudeDeliveryRegistry) register(childID, deliveryUUID, rowID, text string, images int) (gen uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen, ok = r.gens[childID]
	if !ok {
		return 0, false
	}
	if r.rows == nil {
		r.rows = make(map[claudeDeliveryKey]claudeDelivery)
	}
	r.rows[claudeDeliveryKey{childID: childID, gen: gen, uuid: deliveryUUID}] = claudeDelivery{rowID: rowID, text: text, images: images}
	return gen, true
}

// take removes deliveryUUID's entry, reports the row it named, and appends the
// accepted delivery to the child's link queue. dropped is the row an overflowing
// queue pushed out ("" when nothing did), which the caller logs once it has
// released the lock.
//
// Removal, the current-generation check and the queue append are ONE critical
// section: a late echo from a retired launch finds its entry already gone, and
// two echoes for the same uuid (claude may replay, and the frame is at-least-once)
// yield one receipt and one queued delivery. A miss is the ordinary case, not an
// error — an unknown uuid, another child's uuid, or a replay of a message from a
// previous launch all publish nothing rather than guessing.
func (r *claudeDeliveryRegistry) take(childID string, gen uint64, deliveryUUID string) (rowID, dropped string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gens[childID] != gen {
		return "", "", false
	}
	k := claudeDeliveryKey{childID: childID, gen: gen, uuid: deliveryUUID}
	d, ok := r.rows[k]
	if !ok {
		return "", "", false
	}
	delete(r.rows, k)
	if r.accepted == nil {
		r.accepted = make(map[string][]acceptedDelivery)
	}
	q := append(r.accepted[childID], acceptedDelivery{
		rowID: d.rowID, text: d.text, images: d.images, gen: gen,
	})
	if len(q) > claudeLinkQueueCap {
		dropped = q[0].rowID
		q = q[1:]
	}
	r.accepted[childID] = q
	return d.rowID, dropped, true
}

// bind attaches every unbound accepted delivery of childID's launch gen to
// responseID, the message id of a main-thread assistant frame, and reports the
// rows it had to drop.
//
// It runs on the child's stdout goroutine (OnAssistantMessage), in line order
// with the echoes take consumed, and Claude Code prints a message's echo before
// the first assistant frame of the response to the request that carried it. So
// the deliveries still unbound when a NEW id appears are exactly the ones that
// response's request carried — an ordering, not a time window. A repeat of the
// latest id (the next content block of the same response) binds nothing.
//
// A delivery bound to an id whose capture already reported is dropped and
// logged: its capture published without it, and no other capture can name it.
func (r *claudeDeliveryRegistry) bind(childID string, gen uint64, responseID string) (logs []linkLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if responseID == "" || r.gens[childID] != gen || r.lastResponse[childID] == responseID {
		return nil
	}
	if r.lastResponse == nil {
		r.lastResponse = make(map[string]string)
	}
	r.lastResponse[childID] = responseID
	late := slices.Contains(r.reported[childID], responseID)
	q := r.accepted[childID]
	kept := q[:0]
	for _, a := range q {
		if a.gen == gen && a.responseID == "" {
			if late {
				logs = append(logs, linkLog{
					msg:    "claude delivery never linked",
					rowID:  a.rowID,
					sent:   linkTruncate(a.text),
					reason: "capture published first",
				})
				continue
			}
			a.responseID = responseID
		}
		kept = append(kept, a)
	}
	r.setAcceptedLocked(childID, kept)
	return logs
}

// setAcceptedLocked stores childID's queue, deleting the entry when it is empty.
func (r *claudeDeliveryRegistry) setAcceptedLocked(childID string, q []acceptedDelivery) {
	if len(q) == 0 {
		delete(r.accepted, childID)
		return
	}
	r.accepted[childID] = q
}

// drop forgets ONE delivery whose write definitively failed. Its row stays
// pending (deliverInbox's caller never marks it consumed), so a redelivery
// mints a new uuid and this one must not be able to publish a receipt.
func (r *claudeDeliveryRegistry) drop(childID string, gen uint64, deliveryUUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, claudeDeliveryKey{childID: childID, gen: gen, uuid: deliveryUUID})
}

// attach records which generation the live Child instance belongs to, so the
// exit path can retire exactly the launch that died. A zero generation (a
// non-claude launch) records nothing.
func (r *claudeDeliveryRegistry) attach(ch *child.Child, gen uint64) {
	if ch == nil || gen == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insts == nil {
		r.insts = make(map[*child.Child]uint64)
	}
	r.insts[ch] = gen
}

// endLaunch retires the generation the DYING Child instance was launched under:
// its pending entries go, and the child stops accepting registrations until the
// next launch begins.
//
// It is the instance's generation, never "whatever is current": a client can
// Resume a child as soon as its row reads exited, which happens near the TOP of
// handleChildExit while this runs near the bottom — so the current generation
// may already belong to the resumed process. When it does, this is a no-op and
// the successor's generation (and any entries already registered under it)
// survives; the same instance cannot be torn down twice, so a second call finds
// nothing.
func (r *claudeDeliveryRegistry) endLaunch(childID string, ch *child.Child) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen, ok := r.insts[ch]
	if !ok {
		return
	}
	delete(r.insts, ch)
	if r.gens[childID] != gen {
		return
	}
	delete(r.gens, childID)
	for k := range r.rows {
		if k.childID == childID && k.gen == gen {
			delete(r.rows, k)
		}
	}
	r.retireAccepted(childID, gen)
	delete(r.lastResponse, childID)
	delete(r.reported, childID)
}

// abandonLaunch retires childID's current generation for a spawn that never
// produced a Child, so a refused claude spawn leaves no generation behind.
//
// "Current" is safe HERE and only here: abortSpawn runs on an id that was minted
// moments ago for this one spawn, so no other launch can hold a generation for
// it, and there is no instance to key on because child.Spawn never returned one.
// The exit path uses endLaunch, which is instance-scoped for exactly the reason
// this one may not be.
func (r *claudeDeliveryRegistry) abandonLaunch(childID string) {
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
	r.retireAccepted(childID, gen)
	delete(r.lastResponse, childID)
	delete(r.reported, childID)
}

// retireAccepted drops childID's queued deliveries from gen, keeping any a later
// launch already registered: a captured message is linked only to the launch
// that accepted it, and a replayed user frame is indistinguishable from a fresh
// acceptance.
func (r *claudeDeliveryRegistry) retireAccepted(childID string, gen uint64) {
	q := r.accepted[childID]
	kept := q[:0]
	for _, a := range q {
		if a.gen != gen {
			kept = append(kept, a)
		}
	}
	r.setAcceptedLocked(childID, kept)
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
		rowID, dropped, ok := c.claudeDeliveries.take(childID, gen, deliveryUUID)
		if !ok {
			return
		}
		if dropped != "" {
			slog.Warn("claude delivery never linked", "child", childID, "message_id", dropped, "reason", "accepted queue full")
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

// claudeAssistantHook is the OnAssistantMessage callback for one claude launch:
// it binds the launch's accepted deliveries to the response id the child just
// printed. Synchronous on the child's stdout goroutine, like claudeReplayHook,
// and for the same reason it only takes the registry's lock and logs after.
func (c *Controller) claudeAssistantHook(childID string, gen uint64) func(string) {
	return func(messageID string) {
		logLinks(childID, c.claudeDeliveries.bind(childID, gen, messageID))
	}
}

// tracksClaudeDelivery reports whether batch b is one message whose acceptance
// claude will echo back, and therefore carries a delivery uuid.
//
// Only a DIRECT prompt or steer qualifies. A batch with a Source is a
// notification the daemon injected (a job exit, a subagent report, a schedule);
// an abort is a signal, not a message, and claude has no abort frame at all.
// Exactly one row, because the echo names ONE message and a coalesced batch
// would have to guess which.
//
// The daemon's OWN nudges (the restart and rate-limit continuations,
// sendClaudeNudge) are a known exception: they are injected with a Source but
// Controller.Send persists the wrapped text with the row's Source EMPTY, so they
// arrive here as direct messages and do get a uuid and a receipt. That is
// harmless rather than wrong — no client is waiting on that row id, and the
// event contract says an unknown id is a no-op — so the predicate stays the
// plain one instead of growing a second "is this really the user" signal.
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
	gen, ok = c.claudeDeliveries.register(b.ChildID, u.String(), b.IDs[0], strings.Join(b.Frags, "\n"), len(b.Attachments))
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
// A rejected FLAG is reported on both launch shapes, because the upgrade path
// runs through both: a fresh spawn has no session yet, while a RESUMED child
// always has one (that is what --resume uses) and is the shape most existing
// children take when the daemon starts launching with a flag their binary does
// not know. So the hint branch is decided by the stderr, not by the session.
//
// Everything else is reported only for a child that never established a
// session: a claude process reports its id on the first turn's system/init, so
// exiting without one means the binary rejected its argv, could not
// authenticate, or died on startup — a launch failure. With a session, a
// nonzero exit and unrelated stderr is an ordinary turn or auth failure, which
// is not this event's business.
//
// A signal is a kill (an operator's, or the abort ladder), never a launch
// failure, and exit 0 is a clean end. The stderr is carried verbatim (bounded)
// rather than summarised, because the binary's own words are the actionable
// part. A launch failure that printed NOTHING on stderr is still reported, with
// that stated: the operator otherwise gets a bare `child_exited` and no way to
// tell "silent failure" from "no diagnostics exist".
//
// BOTH launch paths carry stderr here. A daraja-hosted child's stderr used to
// be drained to Discard on the executor host, so an executor-routed child
// reported only `child_exited` and its cause was unreadable from the daemon —
// the exact shape of "claude children aren't starting and there is nothing in
// the log". pkg/daraja/host.go now relays it like a script's (the relay already
// had a stderr frame, so this needed no wire change), and
// darajapool.Runner's stderr pipe feeds pkg/child's readStderr on that path
// too. handleChildExit also logs the exit itself.
func claudeLaunchFailure(kind, sessionID string, res child.ShutdownResult, stderr []byte) *rafikiv1.ErrorEvent {
	if kind != protocol.KindClaude {
		return nil
	}
	if res.Signal != "" || res.ExitCode == 0 {
		return nil
	}
	tail := strings.TrimSpace(lastStderrTail(stderr, claudeLaunchStderrBytes))
	rejected := rejectsAFlag(tail)
	if sessionID != "" && !rejected {
		return nil
	}
	detail := tail
	if detail == "" {
		detail = "(the process printed nothing on stderr)"
	}
	var msg string
	if sessionID == "" {
		msg = fmt.Sprintf("claude exited (%d) before establishing a session:\n%s", res.ExitCode, detail)
	} else {
		msg = fmt.Sprintf("claude exited (%d) while resuming session %s:\n%s", res.ExitCode, sessionID, detail)
	}
	if rejected {
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
