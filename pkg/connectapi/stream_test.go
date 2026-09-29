// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"github.com/multigres/testkit/assert"
)

type fakeLineage struct {
	depth  map[string]map[string]int
	labels map[string]map[string]string
}

func (f *fakeLineage) DescendantDepth(a, c string) int {
	if f == nil || f.depth == nil {
		return -1
	}
	if m, ok := f.depth[a]; ok {
		if d, ok := m[c]; ok {
			return d
		}
	}
	return -1
}

func (f *fakeLineage) Labels(id string) (map[string]string, bool) {
	if f == nil || f.labels == nil {
		return nil, false
	}
	l, ok := f.labels[id]
	return l, ok
}

type fakeSource struct {
	mu     sync.Mutex
	ch     chan *rafikiv1.Event
	allCh  chan *rafikiv1.Event
	subbed []string
}

func (f *fakeSource) Subscribe(childID string) (<-chan *rafikiv1.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subbed = append(f.subbed, childID)
	return f.ch, func() {}
}

func (f *fakeSource) SubscribeAll() (<-chan *rafikiv1.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subbed = append(f.subbed, "*")
	return f.allCh, func() {}
}

func statusEvent(childID, state string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}},
	}
}

func setupStreamServer(t *testing.T, ln eventlog.Lineage, elog eventlog.Store, src connectapi.EventSource) rafikiv1connect.ControlClient {
	t.Helper()
	s := connectapi.NewServer(nil)
	s.SetChildResolver(fakeResolver{})
	if ln != nil {
		s.SetLineage(ln)
	}
	if elog != nil {
		s.SetEventLog(elog)
	}
	if src != nil {
		s.SetEventSource(src)
	}

	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
}

func TestStreamEventsNoCursorDoesNotReplay(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	elog := eventlog.NewMemory()
	_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "idle"))

	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	ln := &fakeLineage{depth: make(map[string]map[string]int), labels: make(map[string]map[string]string)}

	client := setupStreamServer(t, ln, elog, src)

	src.ch <- statusEvent("c_1", "idle")

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
	}))
	c.NoError(err, "StreamEvents")

	if !stream.Receive() {
		t.Fatalf("expected event, got err: %v", stream.Err())
	}
	c.Eq("idle", stream.Msg().GetAgentStatus().GetState(), "expected live event 'idle', got")
}

func TestStreamEventsSubtreeAdmitsAChildSpawnedAfterOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	ln := &fakeLineage{
		depth:  map[string]map[string]int{"c_root": {}},
		labels: make(map[string]map[string]string),
	}

	client := setupStreamServer(t, ln, eventlog.NewMemory(), src)

	// Event for unknown child c_new -> ignored
	src.allCh <- statusEvent("c_new", "idle")
	// Teach lineage that c_new is child of c_root
	ln.depth["c_root"]["c_new"] = 1
	// Event for c_new now admitted
	src.allCh <- statusEvent("c_new", "idle")

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Subtree{Subtree: "c_root"}},
	}))
	assert.NewAborting(t).NoError(err, "StreamEvents")

	if !stream.Receive() {
		t.Fatalf("expected event, got err: %v", stream.Err())
	}
	if stream.Msg().GetChildId() != "c_new" || stream.Msg().GetAgentStatus().GetState() != "idle" {
		t.Fatalf("got %+v", stream.Msg())
	}
}

func TestStreamEventsDurableTierExcludesDeltas(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	ln := &fakeLineage{depth: make(map[string]map[string]int), labels: make(map[string]map[string]string)}

	client := setupStreamServer(t, ln, eventlog.NewMemory(), src)

	// Send delta (ephemeral) then status (durable)
	src.ch <- &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{
			Delta: &rafikiv1.ContentBlockDelta_Text{Text: "hi"},
		}},
	}
	src.ch <- statusEvent("c_1", "idle")

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
		Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
	}))
	c.NoError(err, "StreamEvents")

	if !stream.Receive() {
		t.Fatalf("expected event, got err: %v", stream.Err())
	}
	c.Eq("idle", stream.Msg().GetAgentStatus().GetState(), "expected agent_status idle, got %+v", stream.Msg())
}

func TestStreamEventsCursorReplaysPerChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	elog := eventlog.NewMemory()
	for i := 0; i < 3; i++ {
		_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "idle"))
		_, _ = elog.Append(ctx, "c_2", statusEvent("c_2", "idle"))
	}

	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	ln := &fakeLineage{depth: make(map[string]map[string]int), labels: make(map[string]map[string]string)}

	client := setupStreamServer(t, ln, elog, src)

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}},
		Cursor: &rafikiv1.EventCursor{
			Ordinals: map[string]int32{
				"c_1": 1, // should replay ordinal 2
			},
		},
	}))
	assert.NewAborting(t).NoError(err, "StreamEvents")

	if !stream.Receive() {
		t.Fatalf("expected replay event for c_1, got err: %v", stream.Err())
	}
	if stream.Msg().GetChildId() != "c_1" || stream.Msg().GetOrdinal() != 2 {
		t.Fatalf("expected c_1:2, got %s:%d", stream.Msg().GetChildId(), stream.Msg().GetOrdinal())
	}
}

func TestStreamEventsRejectsAnEmptySubject(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	ln := &fakeLineage{}
	client := setupStreamServer(t, ln, eventlog.NewMemory(), &fakeSource{})

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{}))
	if err == nil {
		c.False(stream.Receive(), "expected error on empty subject")
		err = stream.Err()
	}
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestStreamEventsBlockedByHTTPHandlerWrap(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetLineage(&fakeLineage{})
	s.SetEventSource(&fakeSource{ch: make(chan *rafikiv1.Event, 1)})

	path, h := s.Routes()
	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}
	mux := http.NewServeMux()
	mux.Handle(path, denyAll(h))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
	stream, err := client.StreamEvents(context.Background(),
		connect.NewRequest(&rafikiv1.StreamEventsRequest{
			Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
		}))
	assert.NewAborting(t).False(err == nil && stream.Receive(), "deny-all http.Handler wrap did not block StreamEvents")
}

func TestStreamEventsEndsAfterReplayWithoutEventSource(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	elog := eventlog.NewMemory()
	_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "idle"))

	s := connectapi.NewServer(nil)
	s.SetLineage(&fakeLineage{})
	s.SetEventLog(elog)
	// No SetEventSource

	mux := http.NewServeMux()
	path, h := s.Routes()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
		Cursor:  &rafikiv1.EventCursor{Ordinals: map[string]int32{"c_1": -1}},
	}))
	c.NoError(err, "StreamEvents")
	if !stream.Receive() {
		t.Fatalf("expected replayed event: %v", stream.Err())
	}
	c.False(stream.Receive(), "stream kept going after replay; want closed when no event source wired")
	c.NoError(stream.Err(), "stream ended with error")
}

// Dummy use of proto package to avoid unused import if needed
var _ = proto.Marshal

// The replay must page until the log is drained, not stop at one Read's page
// size: an exited script with more than replayPageSize events replays
// ordinals 0..N and ENDS, where a capped replay switched to a live follow no
// one would ever satisfy — `rafiki logs` on such a child hung until
// interrupted.
func TestStreamEventsReplayPagesPastPageSize(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const total = 2500
	elog := eventlog.NewMemory()
	for i := 0; i < total; i++ {
		if _, err := elog.Append(ctx, "c_1", statusEvent("c_1", "s")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	client := setupStreamServer(t, &fakeLineage{}, elog, nil)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: "c_1"}},
		Cursor:  &rafikiv1.EventCursor{Ordinals: map[string]int32{"c_1": -1}},
	}))
	c.NoError(err, "StreamEvents")

	got := 0
	lastOrd := int32(-1)
	for stream.Receive() {
		ord := stream.Msg().GetOrdinal()
		if ord <= lastOrd {
			t.Fatalf("ordinal went backwards: %d after %d", ord, lastOrd)
		}
		lastOrd = ord
		got++
	}
	c.NoError(stream.Err(), "stream ended with error")
	if got != total {
		t.Fatalf("replayed %d events, want %d (last ordinal %d)", got, total, lastOrd)
	}
	if lastOrd != int32(total-1) {
		t.Fatalf("last ordinal %d, want %d", lastOrd, total-1)
	}
}

// A cockpit attached to a child subscribes to its subtree PLUS itself. Without
// include_self the attached child is the one row the rail never hears about,
// and the focus stream (ScopeChild) hides that until the user hops away.
func TestStreamEventsSubtreeIncludeSelfAdmitsTheRoot(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	ln := &fakeLineage{
		depth:  map[string]map[string]int{"c_root": {}},
		labels: make(map[string]map[string]string),
	}
	client := setupStreamServer(t, ln, eventlog.NewMemory(), src)

	src.allCh <- statusEvent("c_root", "idle")

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{
			Scope:       &rafikiv1.EventSubject_Subtree{Subtree: "c_root"},
			IncludeSelf: true,
		},
	}))
	c.NoError(err, "StreamEvents")
	if !stream.Receive() {
		t.Fatalf("expected the subtree root's own event, got err: %v", stream.Err())
	}
	c.Eq("c_root", stream.Msg().GetChildId(), "child id")
}
