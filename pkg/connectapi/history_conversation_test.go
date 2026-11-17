// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/conv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// guardRecordingScope is a ChildScope that records every AuthorizeRead call,
// so a test can prove the conversation-keyed GetHistory path never consults
// the child relationship. Its refusals refuse EVERYTHING: a path that leaks
// through it fails loudly rather than passing by accident.
type guardRecordingScope struct {
	id      string
	reads   []string
	acts    []string
	authzOK bool
}

func (g *guardRecordingScope) ChildID() string { return g.id }

func (g *guardRecordingScope) Authorize(target string) error {
	g.acts = append(g.acts, target)
	if !g.authzOK {
		return connect.NewError(connect.CodePermissionDenied, errors.New("no act authority"))
	}
	return nil
}

func (g *guardRecordingScope) AuthorizeRead(target string) error {
	g.reads = append(g.reads, target)
	if !g.authzOK {
		return connect.NewError(connect.CodePermissionDenied, errors.New("no read authority"))
	}
	return nil
}

func (g *guardRecordingScope) OwnedChildren([]string) []protocol.ChildSummary { return nil }

func (g *guardRecordingScope) Subtree([]string) []protocol.ChildSummary { return nil }

func (g *guardRecordingScope) ConversationInScope(string) bool { return false }

// guardRecordingGuard is an OwnerGuard that records every Authorize call and
// refuses everything, for the same reason.
type guardRecordingGuard struct {
	calls []string
}

func (g *guardRecordingGuard) Authorize(childID string) error {
	g.calls = append(g.calls, childID)
	return connect.NewError(connect.CodePermissionDenied, errors.New("not owned"))
}

func (g *guardRecordingGuard) Owns(string) bool { return false }

func (g *guardRecordingGuard) OwnsConversation(string) bool { return false }

// conversationKeyedFixture wires a server whose conversation seam answers
// visible for every id, with recording guards installed that refuse
// everything: the conversation path must never reach either.
func conversationKeyedFixture(t *testing.T) (*connectapi.Server, *fakeConversationInsights, *countingLoader, *guardRecordingScope, *guardRecordingGuard) {
	t.Helper()
	loader := &countingLoader{}
	f := &fakeConversationInsights{visible: true}
	s := connectapi.NewServer(loader)
	s.SetConversationInsights(f)
	scope := &guardRecordingScope{id: "c_caller"}
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return scope })
	guard := &guardRecordingGuard{}
	s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return guard })
	return s, f, loader, scope, guard
}

// countingLoader is a HistoryLoader that counts Page calls, so a test can
// prove a refused request never reached the store.
type countingLoader struct {
	calls int
}

func (l *countingLoader) Page(context.Context, string, conv.PageQuery) ([]*conv.Node, error) {
	l.calls++
	return nil, nil
}

// TestGetHistoryByConversationRequiresExactlyOneID pins the addressing rule:
// both ids and neither are refused before anything is resolved.
func TestGetHistoryByConversationRequiresExactlyOneID(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, loader, _, _ := conversationKeyedFixture(t)
	conversationID := "0198c0de-0000-7000-8000-000000000001"

	for _, tc := range []struct {
		name string
		req  *rafikiv1.GetHistoryRequest
	}{
		{"both", &rafikiv1.GetHistoryRequest{ChildId: "c_1", ConversationId: conversationID}},
		{"neither", &rafikiv1.GetHistoryRequest{}},
	} {
		_, err := s.GetHistory(context.Background(), connect.NewRequest(tc.req))
		c.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: err = %v, want code", tc.name, err)
		var ce *connect.Error
		c.Require().True(errors.As(err, &ce), "%s: want a *connect.Error, got %T", tc.name, err)
		c.Eq("exactly one of child_id and conversation_id is required", ce.Message(), "%s: message", tc.name)
		c.False(f.visibleSeen, "%s: Visible must not be consulted", tc.name)
		c.Eq(0, loader.calls, "%s: the store must not be read", tc.name)
	}
}

// TestGetHistoryByConversationRefusesInvisible pins the one refusal a
// conversation-keyed read can produce: outside the caller's scope and unknown
// are the SAME not-found answer, and neither reaches the transcript store.
func TestGetHistoryByConversationRefusesInvisible(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, loader, _, _ := conversationKeyedFixture(t)
	f.visible = false
	conversationID := "0198c0de-0000-7000-8000-000000000002"

	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ConversationId: conversationID}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "invisible conversation err = %v, want", err)
	var ce *connect.Error
	c.Require().True(errors.As(err, &ce), "want a *connect.Error, got %T", err)
	c.Eq("conversation not found", ce.Message(), "message")
	c.Eq(conversationID, f.gotVisible, "the requested id must reach Visible")
	c.Eq(0, loader.calls, "an invisible conversation must not be paged")
}

// TestGetHistoryByConversationSkipsTheChildGuards pins the authorization
// shape of a conversation-keyed read: the child relationship checks are not
// consulted at all, because there is no child to authorize.
func TestGetHistoryByConversationSkipsTheChildGuards(t *testing.T) {
	c := assert.NewCollecting(t)
	s, _, loader, scope, guard := conversationKeyedFixture(t)
	conversationID := "0198c0de-0000-7000-8000-000000000003"

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ConversationId: conversationID}))
	c.NoError(err, "GetHistory by conversation = %v, want served", err)
	c.EqDeep([]string(nil), scope.reads, "AuthorizeRead must not be consulted")
	c.EqDeep([]string(nil), scope.acts, "Authorize must not be consulted")
	c.EqDeep([]string(nil), guard.calls, "the owner guard must not be consulted")
	c.Eq(1, loader.calls, "the admitted read must reach the store")
	c.Empty(resp.Msg.GetEvents(), "the counting loader serves no nodes")
}

// TestGetHistoryByConversationServesEvents pins the happy path end to end on
// a real tree: the page comes from the NAMED conversation, and its events
// carry no child identity.
func TestGetHistoryByConversationServesEvents(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTranscript(t)
	tr.user("a")
	tr.assistant("b")

	f := &fakeConversationInsights{visible: true}
	s := connectapi.NewServer(tr.store)
	s.SetConversationInsights(f)

	resp, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ConversationId: tr.convID}))
	c.Require().NoError(err, "GetHistory by conversation")
	c.Eq(tr.convID, f.gotVisible, "the named conversation must be resolved")
	events := resp.Msg.GetEvents()
	c.Require().Len(events, 2, "events = %d, want 2", len(events))
	c.Eq("a", firstText(t, events[0]), "event 0 text")
	c.Eq("b", firstText(t, events[1]), "event 1 text")
	c.Eq("", events[0].GetChildId(), "a conversation-keyed event carries no child id")
}

// TestGetHistoryByConversationUnwiredInsightsFailsUnavailable: without the
// conversation seam there is no scope to answer Visible from, so the read
// fails closed rather than serving unscoped.
func TestGetHistoryByConversationUnwiredInsightsFailsUnavailable(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(&countingLoader{})
	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ConversationId: "0198c0de-0000-7000-8000-000000000004"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "unwired insights err = %v, want", err)
}

// TestGetHistoryByConversationKeepsTheLimitCheckFirst: a malformed limit is
// refused before the scope is probed, on the conversation path as on the
// child path.
func TestGetHistoryByConversationKeepsTheLimitCheckFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, loader, _, _ := conversationKeyedFixture(t)
	limit := int32(0)
	_, err := s.GetHistory(context.Background(),
		connect.NewRequest(&rafikiv1.GetHistoryRequest{ConversationId: "0198c0de-0000-7000-8000-000000000005", Limit: &limit}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "zero limit err = %v, want", err)
	c.False(f.visibleSeen, "Visible must not be consulted")
	c.Eq(0, loader.calls, "the store must not be read")
}
