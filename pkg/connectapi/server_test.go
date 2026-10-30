package connectapi_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/conv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// historyChild is the child id these tests ask about; the fixture's resolver
// maps it to a conversation id, so the two identifiers stay distinct.
const historyChild = "c_1"

type fakeLoader struct{ nodes []*conv.Node }

func (f fakeLoader) Page(context.Context, string, conv.PageQuery) ([]*conv.Node, error) {
	return f.nodes, nil
}

// recordingLoader remembers the id and page it was queried with, so a test
// can prove the handler looked up the RESOLVED conversation id rather than the
// child id, and handed the request's bounds to the store. A nil inner serves no
// nodes, which is all a guard test needs from the seam.
type recordingLoader struct {
	inner    *conv.Store
	gotID    string
	gotQuery conv.PageQuery
}

func (r *recordingLoader) Page(ctx context.Context, conversationID string, q conv.PageQuery) ([]*conv.Node, error) {
	r.gotID = conversationID
	r.gotQuery = q
	if r.inner == nil {
		return nil, nil
	}
	return r.inner.Page(ctx, conversationID, q)
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

// transcript is the production history seam without a database: a real
// conv.Store over an in-memory tree. Tests append through it, so the pages the
// handler serves are the ones a daemon would serve.
type transcript struct {
	t      *testing.T
	store  *conv.Store
	convID string
	handle *conv.Handle
}

func newTranscript(t *testing.T) *transcript {
	t.Helper()
	backend := conv.NewMemBackend()
	convID := "conv-history"
	backend.Ensure(convID)
	st := conv.NewStore(backend, conv.Options{})
	return &transcript{
		t:      t,
		store:  st,
		convID: convID,
		handle: st.Open(convID, conv.AsOwner()),
	}
}

func text(role msg.Role, body string) conv.NewNode {
	return conv.NewNode{Msg: msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: body}}}}
}

// append appends a chain from the conversation's head.
func (f *transcript) append(nodes ...conv.NewNode) conv.Result {
	f.t.Helper()
	res, err := f.handle.Append(context.Background(), conv.Append{FromHead: true, Nodes: nodes})
	assert.NewAborting(f.t).NoError(err, "append")
	return res
}

func (f *transcript) user(body string) conv.Result {
	f.t.Helper()
	return f.append(text(msg.User, body))
}

func (f *transcript) assistant(body string) conv.Result {
	f.t.Helper()
	return f.append(text(msg.Assistant, body))
}

// server wires the store into the Control service the way the daemon does,
// resolving historyChild to the fixture's conversation.
func (f *transcript) server(loader connectapi.HistoryLoader) *connectapi.Server {
	s := connectapi.NewServer(loader)
	s.SetChildResolver(fakeResolver{known: map[string]string{historyChild: f.convID}})
	return s
}

// history serves one GetHistory request against the fixture's tree.
func (f *transcript) history(req *rafikiv1.GetHistoryRequest) []*rafikiv1.Event {
	f.t.Helper()
	c := assert.NewAborting(f.t)
	if req.ChildId == "" {
		req.ChildId = historyChild
	}
	resp, err := f.server(f.store).GetHistory(context.Background(), connect.NewRequest(req))
	c.NoError(err, "GetHistory")
	return resp.Msg.Events
}

// firstText is the text of a message event's first content block.
func firstText(t *testing.T, ev *rafikiv1.Event) string {
	t.Helper()
	blocks := ev.GetUserMessage().GetContent()
	if blocks == nil {
		blocks = ev.GetAssistantMessage().GetContent()
	}
	if len(blocks) == 0 || blocks[0].GetText() == nil {
		t.Fatalf("event %d carries no text block: %T", ev.GetOrdinal(), ev.Payload)
	}
	return blocks[0].GetText().GetText()
}

func TestGetHistoryReturnsAllByDefault(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("a")
	f.assistant("b")
	f.user("c")

	events := f.history(&rafikiv1.GetHistoryRequest{})

	c.Eq(3, len(events), "got")
	c.Eq(0, events[0].GetOrdinal(), "event 0 ordinal")
	c.Eq(2, events[2].GetOrdinal(), "event 2 ordinal")
}

// The page semantics are conv.Store.Page's (TestPageBounds in pkg/conv); the
// handler's job is to hand every bound over intact, and to leave an unset one
// unset rather than collapsing it to a zero value.
func TestGetHistoryPassesBoundsToTheStore(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("a")
	f.assistant("b")
	f.user("c")
	before := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	loader := &recordingLoader{inner: f.store}
	s := f.server(loader)

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{
			ChildId:      historyChild,
			AfterOrdinal: proto.Int32(0),
			Before:       timestamppb.New(before),
			Limit:        proto.Int32(2),
		}))
	c.NoError(err, "GetHistory")
	c.NotNil(loader.gotQuery.AfterOrdinal, "after_ordinal")
	c.Eq(0, *loader.gotQuery.AfterOrdinal, "after_ordinal")
	c.NotNil(loader.gotQuery.Before, "before")
	c.True(before.Equal(*loader.gotQuery.Before), "before = %v", *loader.gotQuery.Before)
	c.NotNil(loader.gotQuery.Limit, "limit")
	c.Eq(2, *loader.gotQuery.Limit, "limit")

	_, err = s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: historyChild}))
	c.NoError(err, "GetHistory")
	c.Nil(loader.gotQuery.AfterOrdinal, "unset after_ordinal")
	c.Nil(loader.gotQuery.Before, "unset before")
	c.Nil(loader.gotQuery.Limit, "unset limit")
}

// History is the transcript along the head's history, so a user row retracted
// before it was answered — RetractHead moves the head back and deletes nothing —
// is not part of it. Only the retry comes back.
func TestGetHistoryOmitsDeadBranch(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("first")
	retracted := f.user("retracted")
	c.NoError(f.handle.RetractHead(context.Background(), retracted.Nodes[0].ID), "retract")
	f.user("retry")

	events := f.history(&rafikiv1.GetHistoryRequest{})

	c.Len(events, 2, "events")
	c.Eq("first", firstText(t, events[0]), "event 0 text")
	c.Eq("retry", firstText(t, events[1]), "event 1 text")
	c.Eq(2, events[1].GetOrdinal(), "the retry keeps the ordinal it was appended at")
}

// A compaction boundary is a derived root, so the history walks across it and
// keeps the rows before it. The tail copies written after the boundary render
// no event and never count toward a page.
func TestGetHistoryCrossesCompaction(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("a")
	last := f.assistant("b")
	preTokens := int64(182000)
	summary := text(msg.User, "summary")
	summary.InputTokens = &preTokens
	_, err := f.handle.Append(context.Background(), conv.Append{
		Root:          conv.KindCompactionSummary,
		DerivedFromID: last.Nodes[0].ID,
		Nodes:         []conv.NewNode{summary},
	})
	c.NoError(err, "compaction boundary")
	tailA := text(msg.User, "a")
	tailA.Kind = conv.KindCompactionTail
	tailB := text(msg.Assistant, "b")
	tailB.Kind = conv.KindCompactionTail
	f.append(tailA, tailB)
	f.assistant("d")

	events := f.history(&rafikiv1.GetHistoryRequest{})

	c.Len(events, 4, "events")
	c.EqDeep([]int32{0, 1, 2, 5}, eventOrdinals(events), "pre-boundary rows, the boundary, and the row after the tails")
	c.Eq("a", firstText(t, events[0]), "event 0 text")
	c.Eq("b", firstText(t, events[1]), "event 1 text")
	cb := events[2].GetCompactionBoundary()
	c.NotNil(cb, "event 2 is %T, want Event_CompactionBoundary", events[2].Payload)
	c.Eq(int32(preTokens), cb.GetPreTokens(), "pre_tokens")
	c.Eq("d", firstText(t, events[3]), "event 3 text")
}

// A page never splits rows that share a ts, so a limit that would cut between
// them widens backwards to the start of the group: both rows of the later
// transaction come back.
func TestGetHistoryPageWidensSharedCreatedAt(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("old")
	// One commit stamps every row it writes with the same created_at; the sleep
	// keeps the two transactions' stamps apart on the wall clock.
	time.Sleep(2 * time.Millisecond)
	f.append(text(msg.Assistant, "new 1"), text(msg.User, "new 2"))

	events := f.history(&rafikiv1.GetHistoryRequest{Limit: proto.Int32(1)})

	c.Len(events, 2, "the same-ts group widened past the limit")
	c.EqDeep([]int32{1, 2}, eventOrdinals(events), "ordinals")
	c.Eq("new 1", firstText(t, events[0]), "event 0 text")
	c.Eq("new 2", firstText(t, events[1]), "event 1 text")
}

func eventOrdinals(events []*rafikiv1.Event) []int32 {
	out := make([]int32, len(events))
	for i, ev := range events {
		out[i] = ev.GetOrdinal()
	}
	return out
}

// A limit <= 0 is refused before the conversation resolves, so no resolver is
// needed to reach the check.
func TestGetHistoryRejectsNonPositiveLimit(t *testing.T) {
	for _, n := range []int32{0, -1} {
		c := assert.NewAborting(t)
		s := connectapi.NewServer(fakeLoader{})

		_, err := s.GetHistory(context.Background(),
			connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: historyChild, Limit: proto.Int32(n)}))
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
	s := connectapi.NewServer(fakeLoader{})

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: ""}))
	c.Error(err, "want an error for an empty child id, got nil")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// A conversation the store does not hold is NotFound, the answer every other
// verb gives for a missing object. An empty page would hide a wrong id and
// Internal would claim the daemon is broken, so neither is acceptable here.
func TestGetHistoryUnknownConversationIsNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	// No conversation is ever created on this backend.
	s := connectapi.NewServer(conv.NewStore(conv.NewMemBackend(), conv.Options{}))
	s.SetChildResolver(fakeResolver{known: map[string]string{
		historyChild: "0b8f7c1e-0000-4000-8000-000000000000",
	}})

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: historyChild}))

	c.Error(err, "want an error for a conversation the store does not hold, got nil")
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
	c.Nil(resp, "response")
}

// The child id and the conversation id are different identifiers: child ids
// are ULIDs the daemon mints per spawn, while the transcript is keyed by the
// conversation's UUID. Without a resolver the handler must fail rather than
// pass the child id through as if it were a conversation id.
func TestGetHistoryFailsClosedWithoutResolver(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("a")
	s := connectapi.NewServer(f.store)
	// No SetChildResolver call.

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: historyChild}))
	c.Error(err, "want an error when no resolver is wired, got nil")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestGetHistoryRejectsUnknownChild(t *testing.T) {
	c := assert.NewAborting(t)
	f := newTranscript(t)
	f.user("a")
	s := connectapi.NewServer(f.store)
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
	f := newTranscript(t)
	f.user("a")
	loader := &recordingLoader{inner: f.store}
	s := f.server(loader)

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: historyChild}))
	c.NoError(err, "GetHistory")
	c.Eq(f.convID, loader.gotID, "loaded with")
	c.Eq(historyChild, resp.Msg.Events[0].GetChildId(), "event child id")
}
