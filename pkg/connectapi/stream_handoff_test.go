// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// liveBus is pkg/bus's delivery rule in miniature: an event reaches only the
// subscribers present when it is published. fakeSource's pre-filled channels
// cannot show a subscribe-after-replay gap; this can.
type liveBus struct {
	mu   sync.Mutex
	subs []chan *rafikiv1.Event
}

func (b *liveBus) subscribe() (<-chan *rafikiv1.Event, func()) {
	ch := make(chan *rafikiv1.Event, 64)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch, func() {}
}

func (b *liveBus) Subscribe(string) (<-chan *rafikiv1.Event, func()) { return b.subscribe() }
func (b *liveBus) SubscribeAll() (<-chan *rafikiv1.Event, func())    { return b.subscribe() }

func (b *liveBus) publish(ev *rafikiv1.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		ch <- ev
	}
}

// hookedLog wraps a Store, running onFirstRead around the first Read (before
// it reads when beforeRead is set, after otherwise) and counting every Read.
type hookedLog struct {
	eventlog.Store
	beforeRead  bool
	onFirstRead func()
	once        sync.Once
	reads       atomic.Int32
}

func (h *hookedLog) Read(ctx context.Context, childID string, after int32, limit int) ([]eventlog.Record, error) {
	h.reads.Add(1)
	if h.beforeRead && h.onFirstRead != nil {
		h.once.Do(h.onFirstRead)
		return h.Store.Read(ctx, childID, after, limit)
	}
	recs, err := h.Store.Read(ctx, childID, after, limit)
	if h.onFirstRead != nil {
		h.once.Do(h.onFirstRead)
	}
	return recs, err
}

func turnStart(childID string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: childID, Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}}
}

// appendAndPublish is publishEvent's order: log first, then the bus copy
// carrying the assigned ordinal.
func appendAndPublish(t *testing.T, elog eventlog.Store, bus *liveBus, ev *rafikiv1.Event) {
	t.Helper()
	ord, err := elog.Append(context.Background(), ev.GetChildId(), ev)
	assert.NewAborting(t).NoError(err, "Append")
	live := proto.Clone(ev).(*rafikiv1.Event)
	live.Ordinal = &ord
	bus.publish(live)
}

func appendN(t *testing.T, elog eventlog.Store, childID string, n int) {
	t.Helper()
	for range n {
		_, err := elog.Append(context.Background(), childID, statusEvent(childID, "idle"))
		assert.NewAborting(t).NoError(err, "Append")
	}
}

// receiveOrdinals reads n events and returns their child:ordinal pairs.
func receiveOrdinals(t *testing.T, stream *connect.ServerStreamForClient[rafikiv1.Event], n int) []string {
	t.Helper()
	var got []string
	for range n {
		if !stream.Receive() {
			t.Fatalf("after %v: stream ended: %v", got, stream.Err())
		}
		got = append(got, stream.Msg().GetChildId()+":"+strconv.Itoa(int(stream.Msg().GetOrdinal())))
	}
	return got
}

func newTLSServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func childStream(child string, after int32) *rafikiv1.StreamEventsRequest {
	return &rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: child}},
		Cursor:  &rafikiv1.EventCursor{Ordinals: map[string]int32{child: after}},
	}
}

func emptyLineage() *fakeLineage {
	return &fakeLineage{depth: map[string]map[string]int{}, labels: map[string]map[string]string{}}
}

// A quiet stream's response headers arrive at once. Without the flush,
// connect-go sends them with the first message, and a client cannot tell a
// stream that opened from one that never did.
func TestStreamEventsFlushesHeadersOnOpen(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(nil)
	s.SetLineage(emptyLineage())
	s.SetEventLog(eventlog.NewMemory())
	s.SetEventSource(&liveBus{})
	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := newTLSServer(t, mux)

	msg, err := proto.Marshal(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
	})
	c.NoError(err, "marshal")
	var body bytes.Buffer
	body.WriteByte(0)
	c.NoError(binary.Write(&body, binary.BigEndian, uint32(len(msg))), "length")
	body.Write(msg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/rafiki.v1.Control/StreamEvents", &body)
	c.NoError(err, "request")
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")

	started := time.Now()
	resp, err := srv.Client().Do(req)
	c.NoError(err, "a quiet stream's headers must arrive before any event")
	defer func() { _ = resp.Body.Close() }()
	c.Eq(http.StatusOK, resp.StatusCode, "status")
	c.True(time.Since(started) < time.Second, "headers took %v", time.Since(started))
}

// An event published while replay is reading the log must not fall between
// replay and the live subscription.
func TestStreamEventsKeepsAnEventPublishedDuringReplay(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mem := eventlog.NewMemory()
	appendN(t, mem, "c_1", 2)
	bus := &liveBus{}
	elog := &hookedLog{Store: mem}
	// After replay's read returns, ordinal 2 is logged and published: the
	// read missed it, so only the live subscription can deliver it.
	elog.onFirstRead = func() { appendAndPublish(t, mem, bus, statusEvent("c_1", "working")) }

	client := setupStreamServer(t, emptyLineage(), elog, bus)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(childStream("c_1", 0)))
	c.NoError(err, "StreamEvents")
	c.EqDeep([]string{"c_1:1", "c_1:2"}, receiveOrdinals(t, stream, 2), "replay then the event published during it")
}

// A live copy of an event replay already delivered is dropped.
func TestStreamEventsDropsALiveCopyReplayDelivered(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mem := eventlog.NewMemory()
	appendN(t, mem, "c_1", 2)
	bus := &liveBus{}
	// Ordinal 2 is logged AND published before replay reads, so replay
	// delivers it and the live copy is a duplicate.
	elog := &hookedLog{Store: mem, beforeRead: true}
	elog.onFirstRead = func() { appendAndPublish(t, mem, bus, statusEvent("c_1", "working")) }

	client := setupStreamServer(t, emptyLineage(), elog, bus)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(childStream("c_1", 0)))
	c.NoError(err, "StreamEvents")
	c.EqDeep([]string{"c_1:1", "c_1:2"}, receiveOrdinals(t, stream, 2), "replay")

	appendAndPublish(t, mem, bus, statusEvent("c_1", "idle"))
	c.EqDeep([]string{"c_1:3"}, receiveOrdinals(t, stream, 1), "the next live event, not the duplicate")
}

// The bus drops a slow subscriber's copies rather than block a turn. A live
// event that skips ordinals is preceded by the missing range from the log.
func TestStreamEventsBackfillsADroppedRangeFromTheLog(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mem := eventlog.NewMemory()
	appendN(t, mem, "c_1", 2)
	bus := &liveBus{}
	client := setupStreamServer(t, emptyLineage(), mem, bus)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(childStream("c_1", 0)))
	c.NoError(err, "StreamEvents")
	// Receiving the replayed ordinal 1 proves replay's read is done, so the
	// appends below can only reach this stream through the live loop.
	c.EqDeep([]string{"c_1:1"}, receiveOrdinals(t, stream, 1), "replay")

	// Ordinals 2 and 3 reach the log but their bus copies are lost.
	appendN(t, mem, "c_1", 2)
	appendAndPublish(t, mem, bus, statusEvent("c_1", "idle"))
	c.EqDeep([]string{"c_1:2", "c_1:3", "c_1:4"}, receiveOrdinals(t, stream, 3), "the dropped range, then the live event")
}

// A type-filtered stream delivers non-contiguous ordinals by design; events
// it filtered out still count as seen, so it never mistakes them for a gap.
func TestStreamEventsTypeFilterIsNotReadAsAGap(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mem := eventlog.NewMemory()
	bus := &liveBus{}
	elog := &hookedLog{Store: mem}
	client := setupStreamServer(t, emptyLineage(), elog, bus)
	req := childStream("c_1", -1)
	req.Types = []string{"turn_start"}
	stream, err := client.StreamEvents(ctx, connect.NewRequest(req))
	c.NoError(err, "StreamEvents")

	for range 3 {
		appendAndPublish(t, mem, bus, statusEvent("c_1", "idle"))
		appendAndPublish(t, mem, bus, turnStart("c_1"))
	}
	c.EqDeep([]string{"c_1:1", "c_1:3", "c_1:5"}, receiveOrdinals(t, stream, 3), "only the turn_starts")
	c.Eq(int32(1), elog.reads.Load(), "the one replay read; filtered events must not trigger a backfill")
}

// With a floor, a child the cursor does not name replays from its first
// record at or after the floor. A child whose records all predate it, a child
// the subject does not admit, and a closed child (in the log, unknown to the
// lineage) replay nothing.
func TestStreamEventsFloorReplaysChildrenTheCursorDoesNotName(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mem := eventlog.NewMemory()
	appendN(t, mem, "c_old", 1)
	appendN(t, mem, "c_new", 1)
	recs, err := mem.Read(ctx, "c_new", -1, 0)
	c.NoError(err, "Read")
	floor := recs[0].CreatedAt
	appendN(t, mem, "c_new", 1)
	appendN(t, mem, "c_outside", 1)
	appendN(t, mem, "c_closed", 1)

	ln := emptyLineage()
	ln.depth["c_root"] = map[string]int{"c_old": 1, "c_new": 1, "c_closed": 1}
	ln.labels["c_old"] = map[string]string{}
	ln.labels["c_new"] = map[string]string{}
	ln.labels["c_outside"] = map[string]string{}
	bus := &liveBus{}
	client := setupStreamServer(t, ln, mem, bus)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Subtree{Subtree: "c_root"}},
		Cursor:  &rafikiv1.EventCursor{Floor: timestamppb.New(floor)},
	}))
	c.NoError(err, "StreamEvents")
	c.EqDeep([]string{"c_new:0", "c_new:1"}, receiveOrdinals(t, stream, 2), "c_new from the floor")

	// Nothing else replayed: the next thing on the stream is this live event,
	// not c_old (all before the floor), c_outside (not in the subtree) or
	// c_closed (the lineage no longer knows it).
	appendAndPublish(t, mem, bus, statusEvent("c_new", "idle"))
	c.EqDeep([]string{"c_new:2"}, receiveOrdinals(t, stream, 1), "the live event")
}

// Without a floor, only the children a cursor names replay: an unbounded
// floor would replay every child the subject admits from its first event.
func TestStreamEventsNoFloorReplaysOnlyNamedChildren(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mem := eventlog.NewMemory()
	appendN(t, mem, "c_named", 2)
	appendN(t, mem, "c_other", 2)
	bus := &liveBus{}
	client := setupStreamServer(t, emptyLineage(), mem, bus)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}},
		Cursor:  &rafikiv1.EventCursor{Ordinals: map[string]int32{"c_named": 0}},
	}))
	c.NoError(err, "StreamEvents")
	c.EqDeep([]string{"c_named:1"}, receiveOrdinals(t, stream, 1), "the named child")

	appendAndPublish(t, mem, bus, statusEvent("c_named", "idle"))
	c.EqDeep([]string{"c_named:2"}, receiveOrdinals(t, stream, 1), "live, with nothing of c_other's before it")
}
