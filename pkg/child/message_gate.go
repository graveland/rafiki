package child

import (
	"slices"
	"sync"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// messageGated is the optional provider capability that turns the gate on.
// GatedMessageID returns the message id of a main-thread assistant frame,
// "" for every other line.
type messageGated interface {
	GatedMessageID(line []byte) string
}

const persistedRingSize = 64

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
	key       string // the latest awaited id not yet persisted; "" = open
	held      []heldEvent
	persisted []string // last persistedRingSize persisted ids, oldest first
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
}

func (g *messageGate) markPersisted(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settle(id)
	if g.key == id {
		g.key = ""
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
	if g.key != "" {
		g.settle(g.key)
	}
	g.key = ""
	for _, h := range g.held {
		g.sink(h.ev)
	}
	g.held = nil
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
