// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui/session"
)

// This file is the cockpit's optimistic-send ledger: the messages this client
// sent and has not yet seen captured, the receipts that acknowledge them, and
// the capture evidence used to retire a row the transcript already shows.
//
// It is deliberately NOT part of session.Session. The session is canonical
// transcript state built from events; an optimistic row is a claim about a
// message that may not be in the transcript at all yet, and folding it in
// would make a local guess indistinguishable from a stored row.
//
// The three display states, and what each one claims:
//
//   - pending (a grey ghost): rafiki has the message — the inbox row is
//     committed, but the child's harness has not taken it into a turn;
//   - sent (normal user decoration): the child's harness has it — a
//     message_delivered receipt named this send's inbox id;
//   - captured (the real transcript row): the model was sent it. The row
//     disappears from this overlay in the same Update that draws the real one.
//
// A row must never go from pending straight to gone: the sent state exists so
// the message stays visible for the whole window between custody and capture.
// For a claude child that window is a turn long, which is why it is drawn with
// the real row's decoration rather than as a footnote.

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
	// inbox id moves such a row to sent, and only capture evidence retires it.
	//
	// Unknown is receipt, not text, because the two mistakes are not
	// symmetrical: treating a claude row as fundi can retire a message the
	// agent never saw, while treating a fundi row as claude merely leaves a
	// ghost standing (and a fundi send always has an explicit Kind on its rail
	// row by the time a user can type into it).
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
	// than leaving the row pending: a wrong join would mark a send sent that
	// nothing acknowledged.
	earlyReceiptTTL = 60 * time.Second
)

// optimisticSend is one message this client sent and has not seen captured.
type optimisticSend struct {
	// attempt is this process's monotonic id for the Send RPC. Removing an
	// attempt by text would remove a different send that happens to say the
	// same thing — two "continue"s are two sends.
	attempt uint64
	// mode is the runtime acknowledgement mode, captured at send time.
	mode ackMode
	// text is the ORIGINAL text, exactly as typed: it is what the row draws and
	// what the evidence matcher normalizes. Displayed content is never
	// normalized, so a row reads back the way it was written.
	text string
	// images are the attachments sent with it, in wire order.
	images []stagedAttachment
	// inboxID is the row id SendResponse returned, "" until the RPC settles.
	inboxID string
	// sent records that a message_delivered receipt named inboxID: the
	// child's harness took the message into a turn.
	sent bool
	// gen is the evidence generation at send time. Only capture evidence
	// ingested AFTER this point can retire the row, so a message already in
	// the transcript when the send was issued is never mistaken for its echo.
	gen uint64
}

// displayText is what a PENDING row draws: the message, or a count of its
// attachments when it had no text at all.
func (s optimisticSend) displayText() string {
	if s.text != "" {
		return s.text
	}
	return "(" + itoa(int64(len(s.images))) + " attached)"
}

// block is the row a SENT send draws, in the transcript's own vocabulary, so
// the provisional row and the captured row that replaces it have one shape.
func (s optimisticSend) block() session.Block {
	b := session.Block{Kind: session.KindUser, Text: s.text}
	for _, img := range s.images {
		b.Images = append(b.Images, &rafikiv1.ImageBlock{MediaType: img.mediaType, Data: img.data})
	}
	return b
}

// captureEvidence is one captured user message, retained as proof that a send
// reached the transcript.
type captureEvidence struct {
	// key is the message's identity: its ORIGINAL timestamp plus a digest of
	// the whole UserMessage payload. Two copies of one stored message — a
	// stream replay and a history read — share a key, so they are one piece of
	// evidence, and an occurrence they carry can only be spent once.
	key string
	// gen is the ledger generation this evidence was ingested at.
	gen uint64
	// norm is the whole message's top-level user text, whitespace-normalized.
	norm string
	// used counts the occurrences of each normalized send text already spent
	// out of this message. Shared across sends and across retries, because
	// consumption is a property of the EVIDENCE, not of the attempt.
	used map[string]int
}

// earlyReceipt is a delivery receipt that arrived before its Send RPC settled.
type earlyReceipt struct {
	id string
	at time.Time
}

// optimisticState is one child's optimistic-send ledger.
type optimisticState struct {
	// sends is every optimistic send to this child, oldest first. Order is the
	// allocation order for evidence occurrences.
	sends []optimisticSend
	// early buffers receipts whose inbox id no settled send claims yet.
	early []earlyReceipt
	// baseline holds the evidence keys of the authoritative history loaded for
	// this child. A capture whose key is in the baseline was already in the
	// transcript before the ledger saw it — a replay, or a row the client had
	// simply never loaded — and is never evidence.
	baseline map[string]bool
	// evidence is the retained capture evidence, oldest first.
	evidence []*captureEvidence
	// gen counts evidence ingested for this child.
	gen uint64
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
		st = &optimisticState{baseline: map[string]bool{}}
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
		gen:     st.gen,
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
	if inboxID != "" && st.consumeEarly(inboxID) {
		st.sends[i].sent = true
		st.reconcile()
	}
	st.dropSettledEarly()
	l.releaseIfIdle(child)
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
	l.releaseIfIdle(child)
}

// deliver applies a message_delivered receipt naming inboxID.
//
// It is idempotent by construction: the rail and focus subscriptions overlap on
// the durable tier, so the same receipt arrives twice, and a replayed receipt
// arrives again after a reconnect. The second application finds the row already
// sent (or already retired) and does nothing.
func (l *optimisticLedger) deliver(child, inboxID string) {
	if inboxID == "" {
		return
	}
	st := l.get(child)
	if st == nil {
		return
	}
	if i := st.indexOfInbox(inboxID); i >= 0 {
		if st.sends[i].sent {
			return
		}
		st.sends[i].sent = true
		st.reconcile()
		l.releaseIfIdle(child)
		return
	}
	// Nothing claims it yet. Buffer it only while an RPC is still outstanding
	// for this child: otherwise the id belongs to a send that has already
	// settled (and was retired), or to another client entirely.
	if st.unresolved() {
		st.buffer(inboxID, l.clock())
	}
}

// capture ingests one captured user message as evidence, and reconciles the
// rows already sent. Evidence arriving while a send is still PENDING is kept
// and changes nothing: a capture cannot discharge a claim about custody.
func (l *optimisticLedger) capture(child string, ev *rafikiv1.Event) {
	st := l.get(child)
	if st == nil || len(st.sends) == 0 {
		// No send outstanding means no evidence bookkeeping is retained at
		// all; evidence is only ever consulted by a sent row.
		return
	}
	e, ok := evidenceOf(ev)
	if !ok {
		// No timestamp, or a payload that cannot be identified: retaining the
		// optimistic row is the conservative answer, never guessing it away.
		return
	}
	if e.norm == "" {
		// No top-level user text (a tool-result turn, an image-only message):
		// it can never carry a send's text, and retaining it would only grow
		// the evidence list.
		return
	}
	if st.baseline[e.key] || st.hasEvidence(e.key) {
		return
	}
	st.gen++
	e.gen = st.gen
	e.used = map[string]int{}
	st.evidence = append(st.evidence, e)
	st.reconcile()
	l.releaseIfIdle(child)
}

// baseline records the evidence keys of an authoritative history read.
//
// History is NEVER evidence. The API cannot say whether a row it returns is new
// or merely one this client had not loaded, so treating a fetch as proof would
// let a paginated page of old messages retire a fresh send. It is only ever a
// baseline: a capture whose key is here was already in the transcript before the
// ledger looked, and a stream replay of it supplies nothing.
func (l *optimisticLedger) baseline(child string, evs []*rafikiv1.Event) {
	if len(evs) == 0 {
		return
	}
	st := l.state(child)
	for _, ev := range evs {
		e, ok := evidenceOf(ev)
		if !ok {
			continue
		}
		// A key already held as evidence stays evidence: it arrived on the
		// stream first, and it was new then.
		if st.hasEvidence(e.key) {
			continue
		}
		st.baseline[e.key] = true
	}
}

// expire drops buffered receipts past their TTL. The cockpit's existing 250ms
// tick calls it; there is no separate poller.
func (l *optimisticLedger) expire() {
	if len(l.children) == 0 {
		return
	}
	now := l.clock()
	for child, st := range l.children {
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
		l.releaseIfIdle(child)
	}
}

// forget drops everything about one child. Called when the child leaves the
// rail, because nothing will ever acknowledge a send to a child the daemon no
// longer has.
func (l *optimisticLedger) forget(child string) {
	delete(l.children, child)
}

// releaseIfIdle releases evidence bookkeeping once nothing is outstanding.
//
// The baseline is deliberately kept: it is what makes a replay of an already
// loaded capture ineligible, and it is a few hashes per user message. It goes
// with the child.
func (l *optimisticLedger) releaseIfIdle(child string) {
	st := l.get(child)
	if st == nil {
		return
	}
	if len(st.sends) > 0 || len(st.early) > 0 {
		return
	}
	st.evidence = nil
	st.gen = 0
}

// reconcile retires SENT rows whose text the retained evidence proves is
// already in the transcript, in local send order and earliest occurrence first.
func (st *optimisticState) reconcile() {
	if len(st.evidence) == 0 || len(st.sends) == 0 {
		return
	}
	kept := st.sends[:0]
	for _, s := range st.sends {
		if st.retireByEvidence(s) {
			continue
		}
		kept = append(kept, s)
	}
	st.sends = kept
}

// retireByEvidence reports whether captured evidence already carries this send,
// spending one non-overlapping occurrence if so.
//
// Only an ACKNOWLEDGED row is eligible: a pending row is a claim about custody,
// and no capture can discharge it. Only a claude row is eligible: a fundi row is
// retired by its echo's text, which is its runtime's own acknowledgement. An
// attachment-bearing row is never eligible — retiring it would drop the images
// from the display, and the text of a message with a screenshot is the least
// reliable half of its identity.
func (st *optimisticState) retireByEvidence(s optimisticSend) bool {
	if !s.sent || s.mode != ackReceipt || len(s.images) > 0 {
		return false
	}
	norm := normalizeText(s.text)
	if norm == "" {
		return false
	}
	for _, e := range st.evidence {
		// Evidence older than the send cannot be its capture.
		if e.gen <= s.gen {
			continue
		}
		if e.used[norm] >= countOccurrences(e.norm, norm) {
			continue
		}
		e.used[norm]++
		return true
	}
	return false
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

func (st *optimisticState) hasEvidence(key string) bool {
	return slices.ContainsFunc(st.evidence, func(e *captureEvidence) bool { return e.key == key })
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
		if s.sent {
			b.WriteByte('s')
		} else {
			b.WriteByte('p')
		}
		b.WriteString(strconv.Itoa(len(s.images)))
		b.WriteByte(':')
		b.WriteString(s.text)
		b.WriteByte(0)
	}
	return b.String()
}

// evidenceOf derives a captured message's identity and normalized text.
//
// The key is the captured EVENT's own timestamp (seconds/nanos, from the stored
// node's created_at) plus a digest of the whole UserMessage payload. It is
// neither ordinal namespace on purpose: the event log's per-child ordinal and
// the conversation's message ordinal are unrelated spaces, and publishEvent
// rewrites the former after the fact.
//
// A missing or invalid timestamp means the message cannot be identified, and a
// caller must retain its optimistic row rather than guess.
func evidenceOf(ev *rafikiv1.Event) (*captureEvidence, bool) {
	um := ev.GetUserMessage()
	if um == nil {
		return nil, false
	}
	ts := ev.GetTs()
	if ts == nil || ts.CheckValid() != nil {
		return nil, false
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(um)
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return &captureEvidence{
		key:  fmt.Sprintf("%d.%09d:%x", ts.GetSeconds(), ts.GetNanos(), sum[:16]),
		norm: normalizeText(userText(um)),
	}, true
}

// userText is the top-level text a user message carries — its CONTENT blocks
// only. Injected daemon notices live in UserMessage.injected, and tool results
// are not text blocks at all, so neither can be mistaken for something the user
// typed.
func userText(um *rafikiv1.UserMessage) string {
	var parts []string
	for _, cb := range um.GetContent() {
		if t := cb.GetText(); t != nil {
			parts = append(parts, t.GetText())
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeText folds a message's whitespace so a wrapper's line breaks and
// indentation cannot hide its text. Only MATCHING input is normalized; what a
// row draws is the original.
func normalizeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// countOccurrences counts the NON-OVERLAPPING occurrences of needle in hay.
// Non-overlapping is what lets one echoed "continue continue" answer two sends
// and never more.
func countOccurrences(hay, needle string) int {
	if needle == "" {
		return 0
	}
	n := 0
	for off := 0; off+len(needle) <= len(hay); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			break
		}
		n++
		off += i + len(needle)
	}
	return n
}

// ── Cockpit glue ────────────────────────────────────────────────────────────

// ackModeFor is the acknowledgement mode a send to child is captured with.
//
// Only an explicitly fundi child gets the legacy text matcher. See ackMode for
// why unknown is the receipt path.
func (c *Cockpit) ackModeFor(child string) ackMode {
	if n, ok := c.rail.Get(child); ok && n.Kind == protocol.KindFundi {
		return ackText
	}
	return ackReceipt
}

// overlay is the optimistic rows drawn after the transcript, plus the
// signature that says when they changed. Both are needed: the rows are what the
// pane draws, and the signature is what makes a state change (pending → sent)
// rebuild a pane whose block list did not move.
type overlay struct {
	rows []string
	sig  string
}

// overlayFor builds child's overlay at width. Empty when the child has no
// optimistic sends.
func (c *Cockpit) overlayFor(child string, width int) overlay {
	st := c.optimistic.get(child)
	if st == nil || len(st.sends) == 0 {
		return overlay{}
	}
	return overlay{rows: c.optimisticRows(child, st, width), sig: st.sig()}
}

// optimisticRows draws one child's optimistic sends, oldest first: a pending
// send as the grey ghost it has always been, a sent one with the same
// decoration the captured row will carry.
func (c *Cockpit) optimisticRows(child string, st *optimisticState, width int) []string {
	var rows []string
	r := c.pane(child).renderer
	for _, s := range st.sends {
		if s.sent {
			rows = append(rows, r.userBlockLines(s.block(), width)...)
			continue
		}
		rows = append(rows, "")
		// Sanitized in both states: an optimistic row is drawn from text the
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
// Only ackText rows are candidates. A claude row is acknowledged by its receipt
// and retired by capture evidence — never by text, whose oldest-send fallback
// is precisely the wrong-ghost defect this path used to have.
func (c *Cockpit) ackPending(child, text string) {
	st := c.optimistic.get(child)
	if st == nil {
		return
	}
	if c.ackPendingRun(child, text) {
		return
	}
	first := -1
	for i, p := range st.sends {
		if p.mode != ackText {
			continue
		}
		if p.text == text {
			st.sends = slices.Delete(st.sends, i, i+1)
			c.optimistic.releaseIfIdle(child)
			return
		}
		if first < 0 {
			first = i
		}
	}
	if first >= 0 {
		st.sends = slices.Delete(st.sends, first, first+1)
		c.optimistic.releaseIfIdle(child)
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
	retired := false
	for rest != "" {
		i := slices.IndexFunc(st.sends, func(p optimisticSend) bool {
			t := strings.TrimSpace(p.text)
			return p.mode == ackText && t != "" && strings.HasPrefix(rest, t)
		})
		if i < 0 {
			break
		}
		rest = strings.TrimSpace(strings.TrimPrefix(rest, strings.TrimSpace(st.sends[i].text)))
		st.sends = slices.Delete(st.sends, i, i+1)
		retired = true
	}
	if retired {
		c.optimistic.releaseIfIdle(child)
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
	st := c.optimistic.get(child)
	if st == nil || len(blocks) == 0 {
		return
	}
	if i := trailingUserIndex(blocks); i >= 0 {
		c.ackPending(child, blocks[i].Text)
		blocks = slices.Delete(slices.Clone(blocks), i, i+1)
	}
	newest := -1
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Kind == session.KindUser {
			newest = i
			break
		}
	}
	for i := len(blocks) - 1; i >= 0 && c.hasTextAck(child); i-- {
		if blocks[i].Kind != session.KindUser {
			continue
		}
		if i == newest {
			c.ackPendingRun(child, blocks[i].Text)
			continue
		}
		for j, p := range st.sends {
			if p.mode == ackText && p.text == blocks[i].Text {
				st.sends = slices.Delete(st.sends, j, j+1)
				c.optimistic.releaseIfIdle(child)
				break
			}
		}
	}
}

// hasTextAck reports whether child still has a send the legacy text matcher
// may act on.
func (c *Cockpit) hasTextAck(child string) bool {
	st := c.optimistic.get(child)
	return st != nil && slices.ContainsFunc(st.sends, func(p optimisticSend) bool { return p.mode == ackText })
}

// forgetOptimistic drops every trace of a child's optimistic sends. Called when
// the child leaves the rail: nothing will ever acknowledge them.
func (c *Cockpit) forgetOptimistic(child string) {
	c.optimistic.forget(child)
}
