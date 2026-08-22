package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

type stubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	events []*rafikiv1.Event
}

func (s *stubControl) GetHistory(
	_ context.Context,
	_ *connect.Request[rafikiv1.GetHistoryRequest],
) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{Events: s.events}), nil
}

func TestRenderHistoryPrintsTextBlocks(t *testing.T) {
	evs := []*rafikiv1.Event{{
		ChildId: "c_1",
		Ordinal: proto.Int32(0),
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hello world"}},
			}},
		}},
	}}

	var sb strings.Builder
	renderHistory(&sb, evs)

	out := sb.String()
	if !strings.Contains(out, "hello world") {
		t.Fatalf("output missing message text:\n%s", out)
	}
	if !strings.Contains(out, "user") {
		t.Fatalf("output missing role label:\n%s", out)
	}
}

func TestHistoryClientRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	path, h := rafikiv1connect.NewControlHandler(&stubControl{events: []*rafikiv1.Event{{
		ChildId: "c_1",
		Ordinal: proto.Int32(7),
	}}})
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := rafikiv1connect.NewControlClient(http.DefaultClient, srv.URL)
	resp, err := client.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(resp.Msg.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(resp.Msg.Events))
	}
	if resp.Msg.Events[0].GetOrdinal() != 7 {
		t.Fatalf("ordinal = %d, want 7", resp.Msg.Events[0].GetOrdinal())
	}
}
