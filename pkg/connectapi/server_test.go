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

	"github.com/multigres/testkit/assert"
)

type fakeLoader struct{ msgs []store.Message }

func (f fakeLoader) Load(context.Context, string) ([]store.Message, error) {
	return f.msgs, nil
}

// recordingLoader remembers the id it was queried with, so a test can prove
// the handler looked up the RESOLVED conversation id rather than the child id.
type recordingLoader struct {
	msgs  []store.Message
	gotID string
}

func (r *recordingLoader) Load(_ context.Context, conversationID string) ([]store.Message, error) {
	r.gotID = conversationID
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

func threeMessages() []store.Message {
	return []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("a"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("b"))},
		{Ordinal: 2, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("c"))},
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

func TestGetHistoryFiltersByAfterOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	s.SetChildResolver(fakeResolver{})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId:      "c_1",
			AfterOrdinal: proto.Int32(0),
		}))
	c.NoError(err, "GetHistory")
	c.Eq(2, len(resp.Msg.Events), "got")
	c.Eq(1, resp.Msg.Events[0].GetOrdinal(), "first ordinal")
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
