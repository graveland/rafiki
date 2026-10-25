package child

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

type recordingSink struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingSink) send(ev *rafikiv1.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ev.GetChildId())
}

func (r *recordingSink) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func newTestGate() (*messageGate, *recordingSink) {
	rec := &recordingSink{}
	return &messageGate{sink: rec.send}, rec
}

func ev(id string) *rafikiv1.Event { return &rafikiv1.Event{ChildId: id} }

func TestGateOpenPassesThrough(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.emit(ev("A"))
	g.emit(ev("B"))
	ck.EqDeep([]string{"A", "B"}, rec.got())
}

func TestGateHoldsUntilPersisted(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.await("X")
	g.emit(ev("A"))
	g.emit(ev("B"))
	ck.Eq(0, len(rec.got()))
	g.markPersisted("X")
	ck.EqDeep([]string{"A", "B"}, rec.got())
}

func TestGatePersistedBeforeAwaitLeavesGateOpen(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.markPersisted("X")
	g.await("X")
	g.emit(ev("A"))
	ck.EqDeep([]string{"A"}, rec.got())
}

func TestGateSecondAwaitHoldsBehindFirst(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.await("X")
	g.emit(ev("A"))
	g.await("Y")
	g.emit(ev("B"))
	g.markPersisted("X")
	ck.EqDeep([]string{"A"}, rec.got())
	g.markPersisted("Y")
	ck.EqDeep([]string{"A", "B"}, rec.got())
}

func TestGateEventAfterReleaseQueuesBehindHeld(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.await("X")
	g.emit(ev("A"))
	g.await("Y")
	g.emit(ev("B"))
	g.markPersisted("Y")
	ck.Eq(0, len(rec.got()))
	g.markPersisted("X")
	ck.EqDeep([]string{"A", "B"}, rec.got())
}

func TestGateEmitQueuesWhileHeldEvenAfterKeyClears(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.await("X")
	g.emit(ev("A"))
	g.await("Y")
	g.markPersisted("Y")
	g.emit(ev("B"))
	ck.Eq(0, len(rec.got()))
	g.markPersisted("X")
	ck.EqDeep([]string{"A", "B"}, rec.got())
}

func TestGateReleaseAllFlushesInOrder(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.await("X")
	g.emit(ev("A"))
	g.await("Y")
	g.emit(ev("B"))
	g.releaseAll()
	ck.EqDeep([]string{"A", "B"}, rec.got())
	g.emit(ev("C"))
	ck.EqDeep([]string{"A", "B", "C"}, rec.got())
}

func TestGateReleaseAllSettlesKeyForLateAwait(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.await("X")
	g.emit(ev("A"))
	g.releaseAll()
	ck.EqDeep([]string{"A"}, rec.got())
	g.await("X")
	g.emit(ev("B"))
	ck.EqDeep([]string{"A", "B"}, rec.got())
}

func TestGatePersistedRingIsBounded(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	for i := 0; i <= persistedRingSize; i++ {
		g.markPersisted(fmt.Sprintf("m%d", i))
	}
	g.await("m0")
	g.emit(ev("A"))
	ck.Eq(0, len(rec.got()))
	g.markPersisted("m0")
	ck.EqDeep([]string{"A"}, rec.got())
}

func TestGateConcurrentEmitAndPersist(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			g.emit(ev(fmt.Sprintf("e%d", i)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			g.await(fmt.Sprintf("m%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			g.markPersisted(fmt.Sprintf("m%d", i))
		}
	}()
	wg.Wait()
	g.releaseAll()
	seen := map[string]int{}
	for _, id := range rec.got() {
		seen[id]++
	}
	ck.Eq(n, len(seen))
	for id, c := range seen {
		ck.Eq(1, c, id)
	}
}

// gatedTestProvider awaits "msg-1" for every line and reports each
// GatedMessageID call on seen, so a test knows the previous line's events have
// already been emitted (or held) before the next line is awaited.
type gatedTestProvider struct {
	IdentityProvider
	seen chan struct{}
}

func (p *gatedTestProvider) Fresh() ProtocolProvider { return p }

func (p *gatedTestProvider) GatedMessageID(line []byte) string {
	p.seen <- struct{}{}
	return "msg-1"
}

func (p *gatedTestProvider) BusFramesNative(line []byte, _ int64) []*rafikiv1.Event {
	return []*rafikiv1.Event{{}}
}

func TestChildExitReleasesHeldEvents(t *testing.T) {
	ck := assert.NewAborting(t)
	stub := newResettableStubRunner()
	prov := &gatedTestProvider{seen: make(chan struct{}, 2)}
	got := make(chan *rafikiv1.Event, 4)
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:    "c_gate_exit",
		Cwd:        t.TempDir(),
		Runner:     stub,
		Provider:   prov,
		NativeSink: func(ev *rafikiv1.Event) { got <- ev },
	})
	ck.NoError(err, "spawn")
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })

	for range 2 {
		_, err = stub.stdoutW.Write([]byte(`{"type":"assistant"}` + "\n"))
		ck.NoError(err, "write stdout")
	}
	for range 2 {
		select {
		case <-prov.seen:
		case <-time.After(3 * time.Second):
			t.Fatal("stdout line never reached the provider")
		}
	}
	ck.Eq(0, len(got), "events delivered before their response was persisted")

	ck.NoError(stub.stdoutW.Close(), "close stdout")

	for range 2 {
		select {
		case ev := <-got:
			ck.Eq("c_gate_exit", ev.GetChildId())
		case <-time.After(3 * time.Second):
			t.Fatal("held event not released on child exit")
		}
	}
}
