package connectapi_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

type fakeLoader struct{ msgs []store.Message }

func (f fakeLoader) LoadPage(context.Context, string, store.HistoryPage) ([]store.Message, error) {
	return f.msgs, nil
}

// recordingLoader remembers the id and page it was queried with, so a test
// can prove the handler looked up the RESOLVED conversation id rather than the
// child id, and handed the request's bounds to the store.
type recordingLoader struct {
	msgs    []store.Message
	gotID   string
	gotPage store.HistoryPage
}

func (r *recordingLoader) LoadPage(_ context.Context, conversationID string, page store.HistoryPage) ([]store.Message, error) {
	r.gotID = conversationID
	r.gotPage = page
	return r.msgs, nil
}

// fakeResolver maps every input to itself unless told otherwise — these tests
// have no real conversation ids, so identity resolution preserves their
// behaviour while still exercising the resolver code path (a test that never
// sets a resolver at all now fails with CodeUnavailable, which is the point).
type fakeResolver struct{ known map[string]string } // childID -> conversationID; nil map = identity

func (f fakeResolver) ConversationID(childID string) (string, bool) {
	if f.known == nil {
		return childID, true
	}
	cid, ok := f.known[childID]
	return cid, ok
}

// historyT0 is the fixture's base timestamp.
var historyT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func threeMessages() []store.Message {
	return []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("a")), CreatedAt: historyT0},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("b")), CreatedAt: historyT0},
		{Ordinal: 2, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("c")), CreatedAt: historyT0.Add(time.Second)},
	}
}

func TestGetHistoryReturnsAllByDefault(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	s.SetChildResolver(fakeResolver{})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	c.NoError(err, "GetHistory")
	c.Eq(3, len(resp.Msg.Events), "got")
}

// The page semantics are store.Messages.LoadPage's (TestLoadPage* in
// pkg/store); the handler's job is to hand every bound over intact, and to
// leave an unset one unset rather than collapsing it to a zero value.
func TestGetHistoryPassesBoundsToTheStore(t *testing.T) {
	c := assert.NewAborting(t)
	loader := &recordingLoader{msgs: threeMessages()}
	s := connectapi.NewServer(loader)
	s.SetChildResolver(fakeResolver{})

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId:      "c_1",
			AfterOrdinal: proto.Int32(0),
			Before:       timestamppb.New(historyT0.Add(time.Second)),
			Limit:        proto.Int32(2),
		}))
	c.NoError(err, "GetHistory")
	c.NotNil(loader.gotPage.AfterOrdinal, "after_ordinal")
	c.Eq(0, *loader.gotPage.AfterOrdinal, "after_ordinal")
	c.NotNil(loader.gotPage.Before, "before")
	c.True(historyT0.Add(time.Second).Equal(*loader.gotPage.Before), "before = %v", *loader.gotPage.Before)
	c.NotNil(loader.gotPage.Limit, "limit")
	c.Eq(2, *loader.gotPage.Limit, "limit")

	_, err = s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	c.NoError(err, "GetHistory")
	c.Nil(loader.gotPage.AfterOrdinal, "unset after_ordinal")
	c.Nil(loader.gotPage.Before, "unset before")
	c.Nil(loader.gotPage.Limit, "unset limit")
}

// A limit <= 0 is refused before the conversation resolves, so no resolver is
// needed to reach the check.
func TestGetHistoryRejectsNonPositiveLimit(t *testing.T) {
	for _, n := range []int32{0, -1} {
		c := assert.NewAborting(t)
		s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})

		_, err := s.GetHistory(context.Background(),
			connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1", Limit: proto.Int32(n)}))
		c.Error(err, "want an error for limit=%d, got nil", n)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "limit=%d code", n)
	}
}

func TestRoutesReturnsControlHandler(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{})

	path, h := s.Routes()
	c.NotEq("", path, "empty path")
	c.NotNil(h, "nil handler")
}

// An empty child id is rejected before resolution is ever attempted, so this
// case deliberately wires no resolver: it must stay InvalidArgument rather
// than becoming the Unavailable a missing resolver produces.
func TestGetHistoryRejectsEmptyChildID(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: nil})

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: ""}))
	c.Error(err, "want an error for an empty child id, got nil")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// The child id and the conversation id are different identifiers: child ids
// are ULIDs the daemon mints per spawn, while store.Messages.Load queries
// WHERE conversation_id = $1::uuid. Without a resolver the handler must fail
// rather than pass the child id through as if it were a conversation id.
func TestGetHistoryFailsClosedWithoutResolver(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	// No SetChildResolver call.

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	c.Error(err, "want an error when no resolver is wired, got nil")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestGetHistoryRejectsUnknownChild(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	s.SetChildResolver(fakeResolver{known: map[string]string{}}) // resolves nothing

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_unknown"}))
	c.Error(err, "want an error for an unresolvable child id, got nil")
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
}

// The resolver must be consulted for the STORAGE lookup only: events still
// carry the child id the caller asked about, not the conversation UUID.
func TestGetHistoryLoadsByConversationIDButLabelsByChildID(t *testing.T) {
	c := assert.NewAborting(t)
	loader := &recordingLoader{msgs: threeMessages()}
	s := connectapi.NewServer(loader)
	s.SetChildResolver(fakeResolver{known: map[string]string{
		"c_1": "1e3f4a9c-0000-4000-8000-000000000001",
	}})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: "c_1"}))
	c.NoError(err, "GetHistory")
	c.Eq("1e3f4a9c-0000-4000-8000-000000000001", loader.gotID, "loaded with")
	c.Eq("c_1", resp.Msg.Events[0].GetChildId(), "event child id")
}
