package child

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

// holding reports whether the gate is still awaiting id with n events queued
// behind it: the state a test waits for before asserting nothing was delivered.
func (g *messageGate) holding(id string, n int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.key == id && len(g.held) == n
}

// syncBuf collects slog output written from the stall timer's goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A response nothing ever reports must not hold the child's event stream until
// the child exits: the bounded wait releases what it held, and says so at WARN
// with the child and the message id. Silence here is the defect — sentinel sees
// a chat turn that never ends, and nothing anywhere says why.
func TestGateStallReleasesHeldEventsLoudly(t *testing.T) {
	ck := assert.NewAborting(t)
	logs := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	g, rec := newTestGate()
	g.childID = "c_stall"
	g.stallAfter = 20 * time.Millisecond
	g.await("msg_stall")
	g.emit(ev("A"))
	g.emit(ev("B"))
	ck.Eq(0, len(rec.got()), "events delivered before their response was reported")

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.got()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ck.EqDeep([]string{"A", "B"}, rec.got(), "held events released when the wait expired")

	for !strings.Contains(logs.String(), "msg_stall") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	logged := logs.String()
	ck.StrContains(logged, "c_stall", "the warning names the child")
	ck.StrContains(logged, "msg_stall", "the warning names the message id")
	ck.StrContains(logged, "level=WARN", "the release is a warning, never a silent release")

	// The released id is settled, exactly as releaseAll settles it: a late
	// report neither re-publishes nor re-arms the gate.
	g.markPersisted("msg_stall")
	g.await("msg_stall")
	g.emit(ev("C"))
	ck.EqDeep([]string{"A", "B", "C"}, rec.got(), "no re-arm after the stall release")
}

// A report settles the wait it names: the bounded wait is gone (so nothing can
// fire a spurious release or a warning for a response that WAS reported) and the
// gate is open for the next turn's events.
func TestGateReportSettlesTheStallWait(t *testing.T) {
	ck := assert.NewAborting(t)
	g, rec := newTestGate()
	g.stallAfter = time.Minute
	g.await("X")
	g.emit(ev("A"))
	g.markPersisted("X")
	ck.EqDeep([]string{"A"}, rec.got(), "released by the report")

	g.mu.Lock()
	armed := g.stall != nil
	g.mu.Unlock()
	ck.False(armed, "the bounded wait is still armed after the response was reported")

	g.emit(ev("B"))
	ck.EqDeep([]string{"A", "B"}, rec.got(), "the gate is open after the report")
}

// A claude child whose response IS reported publishes its held events then: the
// report is the release path. This is the call a release-only capture report (a
// request declaring no client tools) makes, and the frame that armed the gate is
// a real main-thread assistant frame from the real provider.
func TestClaudeChildTurnEndIsReleasedByTheResponseReport(t *testing.T) {
	ck := assert.NewAborting(t)
	stub := newResettableStubRunner()
	got := make(chan *rafikiv1.Event, 8)
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:    "c_gate_report",
		Cwd:        t.TempDir(),
		Runner:     stub,
		Provider:   ClaudeProvider{},
		NativeSink: func(ev *rafikiv1.Event) { got <- ev },
	})
	ck.NoError(err, "spawn")
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })
	ck.Require().NotNil(ch.gate, "claude child has no message gate")
	ch.gate.mu.Lock()
	ch.gate.stallAfter = time.Minute // the report, not the backstop, must release this
	ch.gate.mu.Unlock()

	for _, line := range []string{
		`{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","session_id":"sess-1"}`,
	} {
		_, werr := stub.stdoutW.Write([]byte(line + "\n"))
		ck.NoError(werr, "write stdout")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !ch.gate.holding("msg_1", 2) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ck.True(ch.gate.holding("msg_1", 2), "the assistant frame did not arm the gate behind msg_1")
	select {
	case ev := <-got:
		t.Fatalf("event %T delivered before its response was reported", ev.GetPayload())
	default:
	}

	ch.MessagePersisted("msg_1") // the capture report for that response

	seen := make([]string, 0, 2)
	for time.Now().Before(deadline) {
		select {
		case ev := <-got:
			if ev.GetTurnEnd() != nil {
				seen = append(seen, "turn_end")
				ck.EqDeep([]string{"turn_start", "turn_end"}, seen, "released in order")
				return
			}
			seen = append(seen, "turn_start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("TurnEnd never reached the sink after the report; got %v", seen)
}
