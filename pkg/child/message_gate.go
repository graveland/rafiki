package child

import (
	"log/slog"
	"slices"
	"sync"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// messageGated is the optional provider capability that turns the gate on.
// GatedMessageID returns the message id of a main-thread assistant frame,
// "" for every other line.
type messageGated interface {
	GatedMessageID(line []byte) string
}

const (
	persistedRingSize = 64
	// gateStallTimeout bounds how long ONE message id may hold the gate. The
	// report that releases a wait is sent after its request's capture tail, and
	// every step of that tail is bounded by the proxy's 60s contexts, so this
	// sits comfortably above any healthy tail: it fires only for a response
	// nothing will ever report — a request the proxy could not correlate to this
	// child, a daemon with no capture sink, a response that stored no rows. The
	// release is deliberately loud (see stalled): it publishes the held turn and
	// tool events without the message event they follow, which is a visible
	// reorder, so trading that for perpetual silence is never right.
	gateStallTimeout = 2 * time.Minute
)

type heldEvent struct {
	tag string // the awaited message id when held; "" = releasable
	ev  *rafikiv1.Event
}

// messageGate holds native events behind the response message they follow
// until that message's row is persisted. Every method runs, and calls sink,
// under mu: that is what keeps events from the supervise goroutine, the
// stdout goroutine and the daemon's report path in one order.
type messageGate struct {
	mu        sync.Mutex
	sink      func(*rafikiv1.Event)
	childID   string // names the child in the stall warning; "" in a bare gate
	key       string // the latest awaited id not yet persisted; "" = open
	held      []heldEvent
	persisted []string // last persistedRingSize persisted ids, oldest first
	// stall is the bounded wait armed for key. It is the belt, not the release
	// path: a report and the child's exit both stop it.
	stall *time.Timer
	// stallAfter overrides gateStallTimeout when positive. It is read under mu
	// and exists so a test can shrink the wait: the mechanism under test is
	// "an unreported message cannot silence the child forever", not "two
	// minutes elapse".
	stallAfter time.Duration
}

func (g *messageGate) emit(ev *rafikiv1.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key == "" && len(g.held) == 0 {
		g.sink(ev)
		return
	}
	g.held = append(g.held, heldEvent{tag: g.key, ev: ev})
}

func (g *messageGate) await(id string) {
	if id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if id == g.key || slices.Contains(g.persisted, id) {
		return
	}
	g.key = id
	g.armStallLocked(id)
}

func (g *messageGate) markPersisted(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settle(id)
	if g.key == id {
		g.key = ""
		g.stopStallLocked()
	}
	g.flush()
}

// settle records id in the persisted ring. The caller holds mu.
func (g *messageGate) settle(id string) {
	g.persisted = append(g.persisted, id)
	if len(g.persisted) > persistedRingSize {
		g.persisted = g.persisted[len(g.persisted)-persistedRingSize:]
	}
}

func (g *messageGate) releaseAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopStallLocked()
	if g.key != "" {
		g.settle(g.key)
	}
	g.key = ""
	for _, h := range g.held {
		g.sink(h.ev)
	}
	g.held = nil
}

// armStallLocked (re)arms the bounded wait for id, replacing any wait on the id
// it supersedes. The caller holds mu.
func (g *messageGate) armStallLocked(id string) {
	g.stopStallLocked()
	after := g.stallAfter
	if after <= 0 {
		after = gateStallTimeout
	}
	g.stall = time.AfterFunc(after, func() { g.stalled(id, after) })
}

// stopStallLocked cancels the bounded wait. The caller holds mu.
func (g *messageGate) stopStallLocked() {
	if g.stall != nil {
		g.stall.Stop()
		g.stall = nil
	}
}

// stalled is the backstop release: the awaited response was never reported, so
// the wait expires rather than holding the child's event stream until the child
// exits. It settles the id exactly as releaseAll does (a late frame for it must
// not re-arm the gate) and names the child and the message in a warning — a
// silent release of now-stamped events would hide the capture that never
// reported, which is never what correct operation looks like.
func (g *messageGate) stalled(id string, waited time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key != id {
		return // settled or superseded between the timer firing and this lock
	}
	held := len(g.held)
	g.settle(id)
	g.key = ""
	g.stall = nil // the timer has already fired
	g.flush()
	slog.Warn("message gate: response was never reported; releasing held events",
		"child", g.childID, "message_id", id, "held", held, "waited", waited,
		"detail", "published without their message event; the row is in history")
}

// flush sends held events from the front while each is releasable. The caller
// holds mu.
func (g *messageGate) flush() {
	n := 0
	for n < len(g.held) {
		tag := g.held[n].tag
		if tag != "" && !slices.Contains(g.persisted, tag) {
			break
		}
		g.sink(g.held[n].ev)
		n++
	}
	g.held = g.held[n:]
	if len(g.held) == 0 {
		g.held = nil
	}
}
