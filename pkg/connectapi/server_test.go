package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"
)

type fakeLoader struct{ msgs []store.Message }

func (f fakeLoader) Load(context.Context, string) ([]store.Message, error) {
	return f.msgs, nil
}

func threeMessages() []store.Message {
	return []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("a"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("b"))},
		{Ordinal: 2, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("c"))},
	}
}

func TestGetHistoryReturnsAllByDefault(t *testing.T) {
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if got := len(resp.Msg.Events); got != 3 {
		t.Fatalf("got %d events, want 3", got)
	}
}

func TestGetHistoryFiltersByAfterOrdinal(t *testing.T) {
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId:      "c_1",
			AfterOrdinal: proto.Int32(0),
		}))
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if got := len(resp.Msg.Events); got != 2 {
		t.Fatalf("got %d events, want 2 (ordinals 1 and 2)", got)
	}
	if resp.Msg.Events[0].GetOrdinal() != 1 {
		t.Fatalf("first ordinal = %d, want 1", resp.Msg.Events[0].GetOrdinal())
	}
}

func TestGetHistoryRejectsEmptyChildID(t *testing.T) {
	s := connectapi.NewServer(fakeLoader{msgs: nil})

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: ""}))
	if err == nil {
		t.Fatal("want an error for an empty child id, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}
