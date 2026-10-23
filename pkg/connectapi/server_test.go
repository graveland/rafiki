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

// historyT0 is the fixture's base timestamp. Rows 0 and 1 share it, the way
// every row one transaction wrote shares its transaction-start created_at;
// row 2 is one second later.
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

// limit keeps the newest N events, so a caller paging backwards gets the most
// recent page first.
func TestGetHistoryLimitReturnsNewest(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	s.SetChildResolver(fakeResolver{})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId: "c_1",
			Limit:   proto.Int32(1),
		}))
	c.NoError(err, "GetHistory")
	c.Eq(1, len(resp.Msg.Events), "got %d events, want 1", len(resp.Msg.Events))
	c.Eq(2, resp.Msg.Events[0].GetOrdinal(), "the one event is the newest")
}

// Rows 0 and 1 share a timestamp because one transaction wrote them. A page of
// 2 must therefore widen back to the whole group and return all three events:
// cutting between 0 and 1 would let a client paging with `before` skip ordinal
// 0 entirely.
func TestGetHistoryLimitKeepsSameTimestampGroup(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	s.SetChildResolver(fakeResolver{})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId: "c_1",
			Limit:   proto.Int32(2),
		}))
	c.NoError(err, "GetHistory")
	c.Eq(3, len(resp.Msg.Events), "got %d events, want 3 (the whole t0 group)", len(resp.Msg.Events))
	c.Eq(0, resp.Msg.Events[0].GetOrdinal(), "first ordinal")
	c.Eq(1, resp.Msg.Events[1].GetOrdinal(), "second ordinal")
	c.Eq(2, resp.Msg.Events[2].GetOrdinal(), "third ordinal")
}

// before is STRICTLY earlier: an event whose ts equals the cursor is excluded,
// which is what makes paging with the oldest kept ts not repeat a row.
func TestGetHistoryBeforeFiltersStrictly(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(fakeLoader{msgs: threeMessages()})
	s.SetChildResolver(fakeResolver{})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId: "c_1",
			Before:  timestamppb.New(historyT0.Add(time.Second)),
		}))
	c.NoError(err, "GetHistory")
	c.Eq(2, len(resp.Msg.Events), "got %d events, want 2", len(resp.Msg.Events))
	c.Eq(0, resp.Msg.Events[0].GetOrdinal(), "first ordinal")
	c.Eq(1, resp.Msg.Events[1].GetOrdinal(), "second ordinal")
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
