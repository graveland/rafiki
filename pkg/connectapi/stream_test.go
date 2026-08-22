package connectapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/store"
)

type fakeSource struct{ ch chan *rafikiv1.Event }

func (f *fakeSource) Subscribe(string) (<-chan *rafikiv1.Event, func()) {
	return f.ch, func() {}
}

func TestStreamEventsReplaysHistoryThenFollowsLive(t *testing.T) {
	src := &fakeSource{ch: make(chan *rafikiv1.Event, 4)}
	s := connectapi.NewServer(fakeLoader{msgs: []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("stored"))},
	}})
	s.SetChildResolver(fakeResolver{})
	s.SetEventSource(src)

	mux := http.NewServeMux()
	path, h := s.Routes()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamEvents(ctx,
		connect.NewRequest(&rafikiv1.StreamEventsRequest{ChildIds: []string{"c_1"}}))
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("no replayed event: %v", stream.Err())
	}
	if stream.Msg().GetOrdinal() != 0 {
		t.Fatalf("replayed ordinal = %d, want 0", stream.Msg().GetOrdinal())
	}

	src.ch <- &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: "busy"}},
	}

	if !stream.Receive() {
		t.Fatalf("no live event: %v", stream.Err())
	}
	if stream.Msg().GetAgentStatus().GetState() != "busy" {
		t.Fatalf("live event = %+v, want agent_status busy", stream.Msg())
	}
}

// With no live-event source wired, the stream must END after the durable
// replay rather than parking on ctx.Done(): a stream that will never deliver
// anything should not hold a goroutine per caller for the caller's lifetime.
func TestStreamEventsEndsAfterReplayWithoutEventSource(t *testing.T) {
	s := connectapi.NewServer(fakeLoader{msgs: []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("stored"))},
	}})
	s.SetChildResolver(fakeResolver{})
	// No SetEventSource call.

	mux := http.NewServeMux()
	path, h := s.Routes()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.StreamEvents(ctx,
		connect.NewRequest(&rafikiv1.StreamEventsRequest{ChildIds: []string{"c_1"}}))
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("no replayed event: %v", stream.Err())
	}
	if stream.Receive() {
		t.Fatal("stream kept going after replay; want it closed when no event source is wired")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream ended with an error: %v", err)
	}
}

func TestStreamEventsRejectsNoChildIDs(t *testing.T) {
	s := connectapi.NewServer(fakeLoader{})
	s.SetChildResolver(fakeResolver{})
	s.SetEventSource(&fakeSource{ch: make(chan *rafikiv1.Event)})

	err := s.StreamEvents(context.Background(),
		connect.NewRequest(&rafikiv1.StreamEventsRequest{}),
		&connect.ServerStream[rafikiv1.Event]{})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// Regression guard: a Connect UNARY interceptor does not wrap streaming
// handlers, so an auth mechanism built as a Connect interceptor would leave
// StreamEvents open even while every unary verb looked protected. This
// repo's actual auth mechanism (cmd/rafikid/proxy.go, via
// server.Handler.Mount) is plain http.Handler middleware wrapped around the
// WHOLE handler s.Routes() returns, not a Connect interceptor — this test
// proves that kind of wrap genuinely blocks a streaming RPC, by wrapping the
// handler with a deny-all stand-in and confirming the stream is blocked
// exactly like a unary call would be.
func TestStreamEventsBlockedByHTTPHandlerWrap(t *testing.T) {
	s := connectapi.NewServer(fakeLoader{msgs: []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("stored"))},
	}})
	// A resolver and history, so an unblocked handler would genuinely send
	// something — otherwise this test could pass because the stream was empty
	// rather than because the wrap denied it.
	s.SetChildResolver(fakeResolver{})
	s.SetEventSource(&fakeSource{ch: make(chan *rafikiv1.Event, 1)})

	path, h := s.Routes()
	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusUnauthorized)
		})
	}
	mux := http.NewServeMux()
	mux.Handle(path, denyAll(h))

	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
	stream, err := client.StreamEvents(context.Background(),
		connect.NewRequest(&rafikiv1.StreamEventsRequest{ChildIds: []string{"c_1"}}))
	if err == nil && stream.Receive() {
		t.Fatal("deny-all http.Handler wrap did not block StreamEvents; a plain http.Handler wrap must cover streaming RPCs exactly like unary ones")
	}
}
