// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/base64"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// osc7501 is iTerm2 3.7.5+'s Program Status Protocol (its own "OSC 7501"): one
// record per top-level child, so the terminal tab shows a per-agent status and
// the Session Status tool lists each record. Workers roll up into their
// coordinator and never reach the terminal -- see rail.TopLevelDisplays.
//
// The wire grammar is normative and enforced here rather than trusted, because
// a record that breaks a limit is discarded WHOLE by the terminal (silently, off
// to the side of the tty), so a malformed sequence is not a wrong pixel, it is
// nothing at all.
const (
	// osc7501MaxSeq bounds the whole escape sequence. Every other limit is
	// chosen to fit inside it, but the cap is enforced anyway: a future field
	// must not be the thing that silently voids a record.
	osc7501MaxSeq = 4096
	// osc7501MaxMsg is the byte limit on the DECODED `msg` value.
	osc7501MaxMsg = 2048
	// osc7501MaxTitle is the byte limit on the DECODED `title` value.
	osc7501MaxTitle = 192

	// osc7501DoneDelay is how long a would-be `done` is held before it is
	// emitted. An idle that lands in the gap between a fact flushing (or a
	// schedule firing) and the child's status flipping to working would
	// otherwise flash `done` for a coordinator that is waking: the hold lets
	// any following working/blocked/error supersede it, and only a child that
	// stays idle for the whole delay settles to done.
	osc7501DoneDelay = 1500 * time.Millisecond
)

// The state= values a record can carry. They are deliberately NOT
// protocol.DisplayState: several display states collapse onto one OSC state
// (idle and a non-operator wait both become OSC "done"/"idle" at different
// points), and a display state with no transition emits nothing at all.
const (
	osc7501Working = "working"
	osc7501Blocked = "blocked"
	osc7501Error   = "error"
	osc7501Done    = "done"
	osc7501Idle    = "idle"
	osc7501Clear   = "clear"
)

// osc7501Query asks a supporting terminal to report the states and kinds it
// implements. The reply is what establishes support (step 0): a terminal that
// does not know OSC 7501 ignores it silently, so absence of a reply is the
// only signal that the emitter must stay off.
const osc7501Query = "\x1b]7501;?\x1b\\"

// osc7501IDPattern is the record-id grammar: segment("/"segment)* with each
// segment [A-Za-z0-9_.+-]{1,32}. A rafiki child id is "c_" + a 26-character
// ULID and always fits one segment; anything that does not match is
// unrepresentable and its child emits nothing.
var osc7501IDPattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}(/[A-Za-z0-9_.+-]{1,32})*$`)

// kv is one key=value pair appended to a record after app=rafiki.
type kv struct{ k, v string }

// osc7501 builds one record: "\x1b]7501;state=<state>:id=<id>:app=rafiki"
// plus each extra as ":<key>=<value>", terminated by ST (ESC \). It returns ""
// when the assembled sequence exceeds the whole-record limit, discarding it
// whole rather than shipping a record the terminal would drop.
func osc7501(state, id string, extra ...kv) string {
	var b strings.Builder
	b.WriteString("\x1b]7501;state=")
	b.WriteString(state)
	b.WriteString(":id=")
	b.WriteString(id)
	b.WriteString(":app=rafiki")
	for _, e := range extra {
		b.WriteByte(':')
		b.WriteString(e.k)
		b.WriteByte('=')
		b.WriteString(e.v)
	}
	b.WriteString("\x1b\\")
	if b.Len() > osc7501MaxSeq {
		return ""
	}
	return b.String()
}

// osc7501Value base64-encodes a sanitised, length-capped value. base64 standard
// alphabet is a subset of the value grammar ([A-Za-z0-9_.,+/=-]), so the
// encoded text never needs escaping.
func osc7501Value(s string, limit int) string {
	return base64.StdEncoding.EncodeToString([]byte(osc7501Sanitise(s, limit)))
}

// osc7501Sanitise makes a human string safe for a single-line record: every
// control character becomes a space, runs of spaces collapse, the whole thing
// trims, and it is then truncated on a RUNE boundary to limit bytes. A byte
// cut through a multi-byte rune would produce invalid UTF-8, which base64
// would faithfully carry to the terminal as mojibake.
func osc7501Sanitise(s string, limit int) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if osc7501Control(r) {
			r = ' '
		}
		if r == ' ' {
			// Drop leading spaces and collapse a run into one, without ever
			// writing the space a trailing run would leave behind.
			if b.Len() == 0 || prevSpace {
				prevSpace = true
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return osc7501Truncate(strings.TrimRight(b.String(), " "), limit)
}

// osc7501Control reports whether r is a control character the record grammar
// forbids: C0 (U+0000-U+001F), DEL (U+007F) and C1 (U+0080-U+009F). That
// includes newline and tab.
func osc7501Control(r rune) bool {
	return r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// osc7501Truncate cuts s to at most limit bytes on a rune boundary.
func osc7501Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// osc7501StateOf recovers the state= value from a built sequence, so the
// emitter can apply the "only after working or blocked" rule without keeping
// the state in a second place.
func osc7501StateOf(seq string) string {
	const p = "\x1b]7501;state="
	if !strings.HasPrefix(seq, p) {
		return ""
	}
	rest := seq[len(p):]
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// pendingDone is a held `done`: the exact sequence to emit and the earliest
// time it may go out. A later Sync that maps the child to anything else drops
// it, so a transient idle never reaches the terminal as done.
type pendingDone struct {
	seq string
	at  time.Time
}

// osc7501Emitter owns the edge-triggered per-child state. It is not safe for
// concurrent use: the cockpit drives it from the bubbletea goroutine only.
type osc7501Emitter struct {
	// tmux disables the emitter outright: tmux mangles the reply, so support
	// could never be established reliably, and a half-written sequence would
	// be rendered as garbage on the screen.
	tmux bool
	// supported is set once the terminal answers the ? query.
	supported bool

	// lastEmitted is the last sequence written for an id; a mapped sequence
	// identical to it is not re-sent. It is the whole sequence rather than a
	// state so that a blocked record whose msg changed is re-sent (each report
	// replaces its record completely).
	lastEmitted map[string]string
	// lastState is the state= value of lastEmitted, for the one rule that
	// needs the prior state rather than the prior bytes.
	lastState map[string]string
	// worked is set whenever a child is seen working and cleared when its
	// done is emitted, so an idle child that never worked produces no record
	// and a done is never re-sent without new work.
	worked map[string]bool

	// doneDelay is how long a would-be done is held before emission. A
	// non-positive value emits done immediately (the edge semantics the
	// golden tests pin); production uses osc7501DoneDelay.
	doneDelay time.Duration
	// now returns the current time. It is a field rather than a direct
	// time.Now call so tests can tick the settle deterministically.
	now func() time.Time
	// pending holds a would-be done per id until its deadline, so an idle in
	// the flush-to-working gap is not shown as done.
	pending map[string]pendingDone
}

// newOSC7501Emitter builds the emitter. getenv is os.Getenv in production and a
// map lookup in tests.
func newOSC7501Emitter(getenv func(string) string) *osc7501Emitter {
	return &osc7501Emitter{
		tmux:        getenv("TMUX") != "",
		lastEmitted: map[string]string{},
		lastState:   map[string]string{},
		worked:      map[string]bool{},
		doneDelay:   osc7501DoneDelay,
		now:         time.Now,
		pending:     map[string]pendingDone{},
	}
}

// query returns the support-probe sequence, or "" when the emitter is off
// (under tmux there is nothing to learn and no reason to write).
func (e *osc7501Emitter) query() string {
	if e.tmux {
		return ""
	}
	return osc7501Query
}

// observe folds one incoming message into the support decision. It establishes
// support when an OSC 7501 reply arrives, and does nothing else: every other
// message, and every message once support is known, is ignored.
//
// Step 0 finding: ultraviolet's OSC parser returns an unknown command as a
// uv.UnknownOscEvent carrying the whole raw sequence, and bubbletea forwards it
// to Update unchanged (it has no case for it), so the reply is exactly what
// lands here.
func (e *osc7501Emitter) observe(msg tea.Msg) {
	if e.tmux || e.supported {
		return
	}
	osc, ok := msg.(uv.UnknownOscEvent)
	if !ok {
		return
	}
	if osc7501Reply(string(osc)) {
		e.supported = true
	}
}

// osc7501Reply reports whether raw is an OSC whose command number is 7501. raw
// is the decoded event body: an ESC ] or 8-bit OSC introducer followed by the
// command digits.
func osc7501Reply(raw string) bool {
	switch {
	case strings.HasPrefix(raw, "\x1b]"):
		raw = raw[2:]
	case strings.HasPrefix(raw, "\x9d"):
		raw = raw[1:]
	default:
		return false
	}
	i := 0
	for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
		i++
	}
	return raw[:i] == "7501"
}

// Sync maps every top-level display to a record and returns the concatenated
// sequences that changed, or "" when there are none. It is the whole write
// side: the cockpit sends the result as one tea.Raw.
func (e *osc7501Emitter) Sync(displays []rail.TopLevelDisplay) string {
	if e.tmux || !e.supported {
		return ""
	}
	var out strings.Builder
	for _, d := range displays {
		if s := e.record(d); s != "" {
			out.WriteString(s)
		}
	}
	e.prune(displays)
	return out.String()
}

// pendingDelay reports how long until the earliest held done may be emitted,
// or (0, false) when none is held. The cockpit arms a tea.Tick for it so a done
// that settles with no further state still reaches the terminal.
func (e *osc7501Emitter) pendingDelay() (time.Duration, bool) {
	now := e.now()
	var best time.Duration
	found := false
	for _, p := range e.pending {
		d := p.at.Sub(now)
		if d < 0 {
			d = 0
		}
		if !found || d < best {
			best, found = d, true
		}
	}
	return best, found
}

// prune drops bookkeeping for ids no longer in the display set. A child removed
// from the rail while its done is held would otherwise leave a stale pending
// entry whose expired deadline pendingDelay clamps to 0, re-arming a zero-delay
// settle tick on every sync -- an unbounded tick/Update/re-render loop.
// lastState, lastEmitted and worked are pruned the same way, so a vanished child
// leaves nothing behind its record.
func (e *osc7501Emitter) prune(displays []rail.TopLevelDisplay) {
	if len(e.lastState) == 0 && len(e.lastEmitted) == 0 && len(e.pending) == 0 && len(e.worked) == 0 {
		return
	}
	present := make(map[string]bool, len(displays))
	for _, d := range displays {
		present[d.ChildID] = true
	}
	for id := range e.lastEmitted {
		if !present[id] {
			delete(e.lastEmitted, id)
		}
	}
	for id := range e.lastState {
		if !present[id] {
			delete(e.lastState, id)
		}
	}
	for id := range e.pending {
		if !present[id] {
			delete(e.pending, id)
		}
	}
	for id := range e.worked {
		if !present[id] {
			delete(e.worked, id)
		}
	}
}

// record maps one display to a record, or "" when it maps to nothing or maps
// to the same bytes it last emitted. It is edge-triggered per id.
func (e *osc7501Emitter) record(d rail.TopLevelDisplay) string {
	// An id the grammar cannot express means the terminal could not address
	// the record; emit nothing for this child rather than a broken sequence.
	if !osc7501IDPattern.MatchString(d.ChildID) {
		return ""
	}
	id := d.ChildID
	s := e.sequence(d)

	// Hold a would-be done for the settle delay (F1): a fact flushing or a
	// schedule firing drops the pending fact to zero before the child's status
	// flips to working, so an idle that lands in that gap would otherwise flash
	// done for a coordinator that is waking. Any later mapping supersedes the
	// held done; only an idle that stays idle for the whole delay is emitted.
	if osc7501StateOf(s) == osc7501Done && e.doneDelay > 0 {
		p, held := e.pending[id]
		if !held || p.seq != s {
			e.pending[id] = pendingDone{seq: s, at: e.now().Add(e.doneDelay)}
			return ""
		}
		if e.now().Before(p.at) {
			return ""
		}
		delete(e.pending, id)
	} else {
		// Any other mapping supersedes a held done.
		delete(e.pending, id)
	}

	if s == "" || s == e.lastEmitted[id] {
		return ""
	}
	e.lastEmitted[id] = s
	e.lastState[id] = osc7501StateOf(s)
	// Clear worked only once the done actually goes out, so the held record
	// still maps to done on the timer re-sync.
	if osc7501StateOf(s) == osc7501Done {
		e.worked[id] = false
	}
	return s
}

// sequence maps one top-level display to the record it should carry, or ""
// when the display implies no transition. D7:
//
//   - waiting on the operator  -> blocked, kind=question, msg = the question
//   - failed                   -> error, msg = the error
//   - working                  -> working
//   - any other wait           -> idle, but ONLY if the last record was working
//     or blocked, so a spinner or badge is dropped and nothing else is said
//   - idle                     -> done, but only if the child has worked since
//     its last done
//   - exited                   -> clear, only if a record was ever emitted
//
// A `done` is never overwritten by a later `idle`: each report replaces its
// record completely, so a stale idle update would erase a result the operator
// has not seen yet. The next working/blocked/error replaces it naturally.
func (e *osc7501Emitter) sequence(d rail.TopLevelDisplay) string {
	id := d.ChildID
	title := d.Name
	if title == "" {
		title = id
	}
	switch protocol.DisplayState(d.DisplayState) {
	case protocol.DisplayWorking:
		e.worked[id] = true
		return osc7501(osc7501Working, id)
	case protocol.DisplayWaiting:
		if protocol.WaitingOn(d.WaitingOn) == protocol.WaitingOnOperator {
			return osc7501(osc7501Blocked, id,
				kv{"kind", "question"},
				kv{"msg", osc7501Value(d.DisplayMsg, osc7501MaxMsg)},
				kv{"title", osc7501Value(title, osc7501MaxTitle)},
			)
		}
		switch e.lastState[id] {
		case osc7501Working, osc7501Blocked:
			return osc7501(osc7501Idle, id)
		}
		return ""
	case protocol.DisplayFailed:
		return osc7501(osc7501Error, id,
			kv{"msg", osc7501Value(d.DisplayMsg, osc7501MaxMsg)},
			kv{"title", osc7501Value(title, osc7501MaxTitle)},
		)
	case protocol.DisplayIdle:
		if !e.worked[id] {
			return ""
		}
		// worked is cleared by record when the done is actually emitted, not
		// here, so a done held for the settle delay still maps to done.
		return osc7501(osc7501Done, id)
	case protocol.DisplayExited:
		if _, emitted := e.lastEmitted[id]; !emitted {
			return ""
		}
		return osc7501(osc7501Clear, id)
	}
	// The zero DisplayState ("") is "no state computed" and must never render
	// as a record.
	return ""
}
