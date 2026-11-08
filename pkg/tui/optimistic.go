// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui/session"
)

// The optimistic-send ledger holds messages until the receiving harness accepts
// custody. Claude's UUID receipt retires its row immediately, including images;
// capture independently supplies the canonical transcript later. Fundi keeps
// its persisted-user-row text acknowledgement. This overlay is deliberately
// separate from session.Session's canonical transcript state.

// ackMode says which acknowledgement mechanism a child's runtime provides, and
// is captured ONCE, when the send is issued, before the RPC returns.
//
// It is captured before the response on purpose: a claude send's inbox id only
// arrives with SendResponse, and until it does the row must already be
// committed to the receipt path. A row that fell back to the legacy text
// matcher in the meantime would be retired by that matcher's oldest-send
// fallback on any unrelated echo — exactly how a claude steer's ghost used to
// clear while the message the agent actually read stood.
type ackMode int

const (
	// ackReceipt is the zero value and the mode of every child that is not
	// known to be fundi. Only a message_delivered receipt naming the send's
	// inbox id retires such a row.
	//
	// Unknown is receipt, not text, because the two mistakes are not
	// symmetrical: treating a claude row as fundi can retire a message the
	// agent never saw, while treating a fundi row as claude merely leaves a
	// ghost standing — which textAckFor then retires as soon as the rail knows
	// the child's kind, so the asymmetry costs a delayed ghost rather than a
	// standing one.
	ackReceipt ackMode = iota

	// ackText is a fundi child, whose echo is the persisted user row and whose
	// acknowledgement is that row's text — the behaviour this cockpit has
	// always had, unchanged.
	ackText
)

const (
	// earlyReceiptCap bounds one child's buffer of receipts that arrived
	// BEFORE the Send RPC that minted their id returned. A receipt can beat
	// its own response: the daemon publishes it from the child's echo, which
	// for an idle child can land before the RPC's reply reaches this process.
	// The buffer is bounded and oldest-first because it is a reordering
	// tolerance, not a mailbox — an unbounded one is a memory leak fed by a
	// chatty daemon, and dropping the OLDEST is right because a receipt is
	// only useful for a row whose response is still outstanding.
	earlyReceiptCap = 256

	// earlyReceiptTTL is how long a buffered receipt stays usable. Past it the
	// RPC is presumed lost or the id was never ours, and guessing is worse
	// than leaving the row pending: a wrong join would remove a send that
	// nothing acknowledged.
	earlyReceiptTTL = 60 * time.Second
)

// optimisticSend is one message this client sent and has not seen acknowledged.
type optimisticSend struct {
	// attempt is this process's monotonic id for the Send RPC. Removing an
	// attempt by text would remove a different send that happens to say the
	// same thing — two "continue"s are two sends.
	attempt uint64
	// mode is the runtime acknowledgement mode, captured at send time.
	mode ackMode
	// text and images retain the original send for the pending display.
	text   string
	images []stagedAttachment
	// inboxID is the row id SendResponse returned, "" until the RPC settles.
	inboxID string
}

// displayText is what a PENDING row draws: the message, or a count of its
// attachments when it had no text at all.
func (s optimisticSend) displayText() string {
	if s.text != "" {
		return s.text
	}
	return "(" + itoa(int64(len(s.images))) + " attached)"
}

// earlyReceipt is a delivery receipt that arrived before its Send RPC settled.
type earlyReceipt struct {
	id string
	at time.Time
}

// optimisticState is one child's optimistic-send ledger.
type optimisticState struct {
	// sends is every optimistic send to this child, oldest first. Order is the
	// order used to draw pending rows.
	sends []optimisticSend
	// early buffers receipts whose inbox id no settled send claims yet.
	early []earlyReceipt
}

// optimisticLedger holds every child's optimistic-send state.
//
// It lives on the Cockpit rather than on a session or a pane because a send
// must survive both: the pane is evicted on LRU, and the session is re-fetched
// on a hop, but a message the user sent has not stopped being sent.
type optimisticLedger struct {
	// now is the clock the early-receipt TTL is measured against. Injectable
	// so a test can expire a receipt without sleeping.
	now func() time.Time
	// nextAttempt mints local attempt ids.
	nextAttempt uint64
	children    map[string]*optimisticState
}

func newOptimisticLedger(now func() time.Time) optimisticLedger {
	if now == nil {
		now = time.Now
	}
	return optimisticLedger{now: now, children: map[string]*optimisticState{}}
}

// clock is the ledger's time source, defaulting to the wall clock so a
// zero-valued ledger (a test fixture that never called newOptimisticLedger)
// cannot nil-panic on a TTL check.
func (l *optimisticLedger) clock() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

// get returns child's state, or nil when the child has none. It never creates
// one: a lookup must not allocate state for a child nobody sent to.
func (l *optimisticLedger) get(child string) *optimisticState {
	return l.children[child]
}

// state returns child's state, creating it on first use.
func (l *optimisticLedger) state(child string) *optimisticState {
	if l.children == nil {
		l.children = map[string]*optimisticState{}
	}
	st := l.children[child]
	if st == nil {
		st = &optimisticState{}
		l.children[child] = st
	}
	return st
}

// add records a send and returns its local attempt id. It runs BEFORE the RPC
// is issued, so an echo (or a receipt) that beats the response still finds the
// row it belongs to.
func (l *optimisticLedger) add(child string, mode ackMode, text string, images []stagedAttachment) uint64 {
	l.nextAttempt++
	st := l.state(child)
	st.sends = append(st.sends, optimisticSend{
		attempt: l.nextAttempt,
		mode:    mode,
		text:    text,
		images:  slices.Clone(images),
	})
	return l.nextAttempt
}

// settle records the inbox id a Send RPC returned and applies any receipt that
// arrived before it.
func (l *optimisticLedger) settle(child string, attempt uint64, inboxID string) {
	st := l.get(child)
	if st == nil {
		return
	}
	i := st.indexOf(attempt)
	if i < 0 {
		return
	}
	st.sends[i].inboxID = inboxID
	if st.sends[i].mode == ackReceipt && inboxID != "" && st.consumeEarly(inboxID) {
		st.sends = slices.Delete(st.sends, i, i+1)
	}
	st.dropSettledEarly()
}

// fail removes exactly the attempt whose RPC failed. Its siblings are other
// sends, however identical they look.
func (l *optimisticLedger) fail(child string, attempt uint64) {
	st := l.get(child)
	if st == nil {
		return
	}
	if i := st.indexOf(attempt); i >= 0 {
		st.sends = slices.Delete(st.sends, i, i+1)
	}
	st.dropSettledEarly()
}

// deliver applies a message_delivered receipt naming inboxID.
//
// It is idempotent by construction: the rail and focus subscriptions overlap on
// the durable tier, so the same receipt arrives twice, and a replayed receipt
// arrives again after a reconnect. The second application finds the row already
// retired and does nothing.
func (l *optimisticLedger) deliver(child, inboxID string) {
	if inboxID == "" {
		return
	}
	st := l.get(child)
	if st == nil {
		return
	}
	if i := st.indexOfInbox(inboxID); i >= 0 {
		if st.sends[i].mode == ackReceipt {
			st.sends = slices.Delete(st.sends, i, i+1)
			st.dropSettledEarly()
		}
		return
	}
	// Nothing claims it yet. Buffer it only while an RPC is still outstanding
	// for this child: otherwise the id belongs to a send that has already
	// settled (and was retired), or to another client entirely.
	if st.unresolved() {
		st.buffer(inboxID, l.clock())
	}
}

// expire drops buffered receipts past their TTL. The cockpit's existing 250ms
// tick calls it; there is no separate poller.
func (l *optimisticLedger) expire() {
	if len(l.children) == 0 {
		return
	}
	now := l.clock()
	for _, st := range l.children {
		if len(st.early) == 0 {
			continue
		}
		kept := st.early[:0]
		for _, e := range st.early {
			if now.Sub(e.at) <= earlyReceiptTTL {
				kept = append(kept, e)
			}
		}
		st.early = kept
	}
}

// forget drops everything about one child. Called when the child leaves the
// rail, because nothing will ever acknowledge a send to a child the daemon no
// longer has.
func (l *optimisticLedger) forget(child string) {
	delete(l.children, child)
}

func (st *optimisticState) indexOf(attempt uint64) int {
	return slices.IndexFunc(st.sends, func(s optimisticSend) bool { return s.attempt == attempt })
}

func (st *optimisticState) indexOfInbox(id string) int {
	return slices.IndexFunc(st.sends, func(s optimisticSend) bool { return s.inboxID == id })
}

// unresolved reports whether any send to this child is still waiting on its
// Send RPC, which is the only state in which a receipt can be early.
func (st *optimisticState) unresolved() bool {
	return slices.ContainsFunc(st.sends, func(s optimisticSend) bool { return s.inboxID == "" })
}

func (st *optimisticState) buffer(id string, now time.Time) {
	for _, e := range st.early {
		if e.id == id {
			return
		}
	}
	if len(st.early) >= earlyReceiptCap {
		st.early = st.early[1:]
	}
	st.early = append(st.early, earlyReceipt{id: id, at: now})
}

// consumeEarly takes id out of the buffer, reporting whether it was there.
func (st *optimisticState) consumeEarly(id string) bool {
	i := slices.IndexFunc(st.early, func(e earlyReceipt) bool { return e.id == id })
	if i < 0 {
		return false
	}
	st.early = slices.Delete(st.early, i, i+1)
	return true
}

// dropSettledEarly clears what is left of the buffer once no RPC is
// outstanding: a receipt that has not been claimed by then names an id this
// client cannot use.
func (st *optimisticState) dropSettledEarly() {
	if st.unresolved() {
		return
	}
	st.early = nil
}

// sig is the overlay's cache signature: what changed about the rows without
// their rendered bytes.
func (st *optimisticState) sig() string {
	var b strings.Builder
	for _, s := range st.sends {
		b.WriteString(strconv.FormatUint(s.attempt, 10))
		b.WriteString(strconv.Itoa(len(s.images)))
		b.WriteByte(':')
		b.WriteString(s.text)
		b.WriteByte(0)
	}
	return b.String()
}

// ── Cockpit glue ────────────────────────────────────────────────────────────

// ackModeFor is the acknowledgement mode a send to child is captured with.
//
// Only an explicitly fundi child gets the legacy text matcher. See ackMode for
// why unknown is the receipt path — and textAckFor for the one way a row
// captured under that default can still retire on its text echo.
func (c *Cockpit) ackModeFor(child string) ackMode {
	if n, ok := c.rail.Get(child); ok && n.Kind == protocol.KindFundi {
		return ackText
	}
	return ackReceipt
}

// textAckFor reports whether a text echo may retire a pending send to child.
//
// The send-time mode is one answer, but not the only one, and on its own it is
// not enough: a send can be issued before the rail knows the child's kind — a
// row introduced by a daemon that predates ChildSpawned.kind, or by traffic
// from a child this client never saw spawn — which captures the row on the
// conservative receipt path, and a fundi child never sends the receipt that
// path waits for. The kind is static for the child's life, so the kind the rail
// knows NOW is just as good an answer, and it is what lets such a row retire on
// its echo instead of standing until the child leaves the rail.
func (c *Cockpit) textAckFor(child string) bool {
	return c.ackModeFor(child) == ackText
}

// kindKnown reports whether the rail knows child's runtime kind. "" means
// unknown -- a daemon that predates ChildSpawned.kind, or a row this client has
// not re-listed yet -- and never a kind.
func (c *Cockpit) kindKnown(child string) bool {
	n, ok := c.rail.Get(child)
	return ok && n.Kind != ""
}

// overlayFor builds child's overlay: the signature that says when it changed,
// and the builder the pane calls once it has applied its display flags. Both
// are needed — the rows are what the pane draws, and the signature is what
// makes a receipt removing a row rebuild a pane whose block list did not
// move — and the rows are deferred for the reason pane.go's overlay type gives.
// Empty when the child has no optimistic sends.
func (c *Cockpit) overlayFor(child string) overlay {
	st := c.optimistic.get(child)
	if st == nil || len(st.sends) == 0 {
		return overlay{}
	}
	return overlay{
		sig:  st.sig(),
		rows: func(width int) []string { return c.optimisticRows(st, width) },
	}
}

// optimisticRows draws pending sends as grey ghosts, oldest first.
func (c *Cockpit) optimisticRows(st *optimisticState, width int) []string {
	var rows []string
	for _, s := range st.sends {
		rows = append(rows, "")
		// Sanitized: an optimistic row is drawn from text the
		// user typed, which can carry an escape sequence like anything else
		// that reaches the transcript.
		for _, line := range strings.Split(sanitizeControlChars(s.displayText()), "\n") {
			for _, row := range wrapTo("▌ ", line, width) {
				rows = append(rows, styleGhost.Width(width).Render(row))
			}
		}
	}
	return rows
}

// ackPending retires the pending sends to child that the echoed text answers.
// Sends made before the agent's next turn can be merged into ONE user message
// (the API forbids consecutive user turns), so one echo may answer several:
// every pending send, oldest first, that the echo reads as a consecutive run
// of is retired. With no such run an exact match wins, else the oldest send
// (an image-only message echoes no text to match).
//
// Only rows a text echo may answer are candidates: ackText rows, and any row of
// a child the rail knows is fundi NOW (see textAckFor). A claude row is
// acknowledged by its receipt and retired immediately — never by text, whose
// oldest-send fallback is precisely the wrong-ghost defect this path used to
// have.
func (c *Cockpit) ackPending(child, text string) {
	st := c.optimistic.get(child)
	if st == nil {
		return
	}
	if c.ackPendingRun(child, text) {
		return
	}
	textAck := c.textAckFor(child)
	first := -1
	for i, p := range st.sends {
		if p.mode != ackText && !textAck {
			continue
		}
		if p.text == text {
			st.sends = slices.Delete(st.sends, i, i+1)
			return
		}
		if first < 0 {
			first = i
		}
	}
	if first >= 0 {
		st.sends = slices.Delete(st.sends, first, first+1)
	}
}

// ackPendingRun is ackPending's matching half, and its only exacting one: it
// retires the pending sends to child that text reads as a consecutive run of,
// oldest first, reporting whether it retired any. One echo answers several
// sends when the daemon merged them into one user message, and that message's
// text is exactly the sends joined in order.
//
// It never falls back to "retire something anyway": a caller that matches
// against HISTORY must not retire a ghost on an unrelated block, so the
// oldest-send fallback lives in ackPending, where the text is known to be the
// message that just arrived.
func (c *Cockpit) ackPendingRun(child, text string) bool {
	st := c.optimistic.get(child)
	if st == nil {
		return false
	}
	rest := strings.TrimSpace(text)
	textAck := c.textAckFor(child)
	retired := false
	for rest != "" {
		i := slices.IndexFunc(st.sends, func(p optimisticSend) bool {
			t := strings.TrimSpace(p.text)
			return (p.mode == ackText || textAck) && t != "" && strings.HasPrefix(rest, t)
		})
		if i < 0 {
			break
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, strings.TrimSpace(st.sends[i].text)))
		st.sends = slices.Delete(st.sends, i, i+1)
		retired = true
	}
	return retired
}

// ackPendingFromHistory retires pending sends to child that a fetched
// transcript already holds. The echo is not necessarily the last block -- the
// agent may have answered before the fetch landed -- so every user block is
// checked, newest first.
//
// The newest user block gets ackPendingRun, because it is still the likeliest
// echo and one row can carry several merged sends ("a\nb"). An OLDER user
// block only ever matches exactly: a prefix match against history would retire
// the ghost of a message that merely starts the same way. The trailing block
// alone keeps ackPending's oldest-send fallback, which is what an image-only
// message (no text to match) needs.
//
// A claude child's rows are untouched here: history cannot establish that a row
// it returns is this send's capture rather than one the client had not loaded,
// and a wrong match would remove a message from the display.
func (c *Cockpit) ackPendingFromHistory(child string, blocks []session.Block) {
	c.ackPendingBlocks(child, blocks, true)
}

// ackPendingBlocks is ackPendingFromHistory with the oldest-send fallback made
// optional.
//
// The fallback belongs to a FETCHED transcript, where the trailing user block is
// still the likeliest echo. A rescue that re-runs the matcher over a transcript
// this client already holds (the seed that finally teaches a child's kind -- see
// textAckFor) passes false: that text is history, not the echo that just
// arrived, so a row may only go on a match.
func (c *Cockpit) ackPendingBlocks(child string, blocks []session.Block, fallback bool) {
	st := c.optimistic.get(child)
	if st == nil || len(blocks) == 0 {
		return
	}
	if i := trailingUserIndex(blocks); i >= 0 {
		if fallback {
			c.ackPending(child, blocks[i].Text)
		} else {
			c.ackPendingRun(child, blocks[i].Text)
		}
		blocks = slices.Delete(slices.Clone(blocks), i, i+1)
	}
	newest := -1
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Kind == session.KindUser {
			newest = i
			break
		}
	}
	textAck := c.textAckFor(child)
	for i := len(blocks) - 1; i >= 0 && c.hasTextAck(child); i-- {
		if blocks[i].Kind != session.KindUser {
			continue
		}
		if i == newest {
			c.ackPendingRun(child, blocks[i].Text)
			continue
		}
		for j, p := range st.sends {
			if (p.mode == ackText || textAck) && p.text == blocks[i].Text {
				st.sends = slices.Delete(st.sends, j, j+1)
				break
			}
		}
	}
}

// hasTextAck reports whether child still has a send a text echo may act on.
func (c *Cockpit) hasTextAck(child string) bool {
	st := c.optimistic.get(child)
	if st == nil || len(st.sends) == 0 {
		return false
	}
	if c.textAckFor(child) {
		return true
	}
	return slices.ContainsFunc(st.sends, func(p optimisticSend) bool { return p.mode == ackText })
}

// forgetOptimistic drops every trace of a child's optimistic sends. Called when
// the child leaves the rail: nothing will ever acknowledge them.
func (c *Cockpit) forgetOptimistic(child string) {
	c.optimistic.forget(child)
}
