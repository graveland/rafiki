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
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected event, got err: %v", stream.Err())
	}
	if stream.Msg().GetAgentStatus().GetState() != "idle" {
		t.Fatalf("expected live event 'idle', got %q", stream.Msg().GetAgentStatus().GetState())
	}
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
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected event, got err: %v", stream.Err())
	}
	if stream.Msg().GetChildId() != "c_new" || stream.Msg().GetAgentStatus().GetState() != "idle" {
		t.Fatalf("got %+v", stream.Msg())
	}
}

func TestStreamEventsDurableTierExcludesDeltas(t *testing.T) {
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
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected event, got err: %v", stream.Err())
	}
	if stream.Msg().GetAgentStatus().GetState() != "idle" {
		t.Fatalf("expected agent_status idle, got %+v", stream.Msg())
	}
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
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected replay event for c_1, got err: %v", stream.Err())
	}
	if stream.Msg().GetChildId() != "c_1" || stream.Msg().GetOrdinal() != 2 {
		t.Fatalf("expected c_1:2, got %s:%d", stream.Msg().GetChildId(), stream.Msg().GetOrdinal())
	}
}

func TestStreamEventsRejectsAnEmptySubject(t *testing.T) {
	ctx := context.Background()
	ln := &fakeLineage{}
	client := setupStreamServer(t, ln, eventlog.NewMemory(), &fakeSource{})

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{}))
	if err == nil {
		if stream.Receive() {
			t.Fatal("expected error on empty subject")
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
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
	if err == nil && stream.Receive() {
		t.Fatal("deny-all http.Handler wrap did not block StreamEvents")
	}
}

func TestStreamEventsEndsAfterReplayWithoutEventSource(t *testing.T) {
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
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("expected replayed event: %v", stream.Err())
	}
	if stream.Receive() {
		t.Fatal("stream kept going after replay; want closed when no event source wired")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream ended with error: %v", err)
	}
}

// Dummy use of proto package to avoid unused import if needed
var _ = proto.Marshal

// A cockpit attached to a child subscribes to its subtree PLUS itself. Without
// include_self the attached child is the one row the rail never hears about,
// and the focus stream (ScopeChild) hides that until the user hops away.
func TestStreamEventsSubtreeIncludeSelfAdmitsTheRoot(t *testing.T) {
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
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("expected the subtree root's own event, got err: %v", stream.Err())
	}
	if got := stream.Msg().GetChildId(); got != "c_root" {
		t.Fatalf("child id = %q, want c_root", got)
	}
}
