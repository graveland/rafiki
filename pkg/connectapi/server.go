// SPDX-License-Identifier: Apache-2.0

// Package connectapi serves rafikid's Connect control plane. Handlers mount on
// the same http.ServeMux as the proxy faces, so there is one listener, one TLS
// config and one route to operate.
package connectapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventconv"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/store"
)

// HistoryLoader is the narrow slice of store.Messages this package needs.
// Depending on the interface rather than the concrete type keeps the handler
// testable without a database.
type HistoryLoader interface {
	Load(ctx context.Context, conversationID string) ([]store.Message, error)
}

// ConversationResolver maps a daemon-assigned child id (a ULID, "c_...") to
// the persisted conversation's UUID — the id store.Messages.Load actually
// expects. These are different identifiers: the child id is runtime-only,
// minted per spawn, while the conversation id is what conversation_message
// rows are keyed by. false means the child is unknown, has exited and been
// forgotten, or is not a fundi child (only fundi children have a conversation
// UUID as their session id — see childstore.Snapshot.SessionID).
type ConversationResolver interface {
	ConversationID(childID string) (string, bool)
}

// Server implements the Control service.
//
// Every dependency below is attached AFTER construction, because the daemon's
// Controller — the only thing that can answer any of them — is built after the
// proxy face that owns this Server. They are atomic.Pointer rather than plain
// fields because the HTTP listener is already serving by the time main.go
// wires them: a plain field write there is a data race by the Go memory model,
// even though the lost-race outcome is a benign fail-closed.
type Server struct {
	history HistoryLoader

	events    atomic.Pointer[EventSource]
	lineageLn atomic.Pointer[eventlog.Lineage]
	evlog     atomic.Pointer[eventlog.Store]
	resolver  atomic.Pointer[ConversationResolver]
	inbox     atomic.Pointer[inbox.Inbox]
	children  atomic.Pointer[ChildLister]
	lifecycle atomic.Pointer[ChildLifecycle]
}

func NewServer(h HistoryLoader) *Server { return &Server{history: h} }

// SetEventSource attaches the live-event source. Without one, StreamEvents
// serves the durable replay and then ends the stream.
func (s *Server) SetEventSource(src EventSource) { s.events.Store(&src) }

func (s *Server) eventSource() EventSource {
	if p := s.events.Load(); p != nil {
		return *p
	}
	return nil
}

// SetLineage attaches the lineage provider.
func (s *Server) SetLineage(ln eventlog.Lineage) { s.lineageLn.Store(&ln) }

func (s *Server) lineage() eventlog.Lineage {
	if p := s.lineageLn.Load(); p != nil {
		return *p
	}
	return nil
}

// SetEventLog attaches the durable event log store.
func (s *Server) SetEventLog(l eventlog.Store) { s.evlog.Store(&l) }

func (s *Server) eventLog() eventlog.Store {
	if p := s.evlog.Load(); p != nil {
		return *p
	}
	return nil
}

// SetChildResolver attaches the child-id-to-conversation-id resolver. It is a
// post-construction setter, not a constructor argument, because the daemon's
// Controller (the only thing that knows this mapping) is constructed AFTER
// the proxy face that owns this Server — see cmd/rafikid/main.go, which wires
// it the same way it wires ctrl.SetProxy(face.URL, face.Token). Until it is
// set, every call that needs it fails closed (CodeUnavailable) rather than
// passing the child id through as if it were already a conversation id — the
// exact bug this resolver exists to close.
func (s *Server) SetChildResolver(r ConversationResolver) { s.resolver.Store(&r) }

// SetInbox attaches the inbound-message sink. Until it is set, Send fails
// closed rather than reporting success for a message that reached nothing.
func (s *Server) SetInbox(in inbox.Inbox) { s.inbox.Store(&in) }

// resolveConversation turns a request's child_id into the conversation id
// store.Messages.Load needs. Centralized here because GetHistory and
// StreamEvents both need it with identical semantics.
func (s *Server) resolveConversation(childID string) (string, error) {
	p := s.resolver.Load()
	if p == nil {
		return "", connect.NewError(connect.CodeUnavailable,
			errors.New("child resolver not yet wired"))
	}
	conversationID, ok := (*p).ConversationID(childID)
	if !ok {
		return "", connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no conversation for child %q", childID))
	}
	return conversationID, nil
}

// Routes returns the mux path prefix and handler for the Control service,
// with the given interceptors applied. An empty-token interceptor admits
// everything — see NewAuthInterceptor.
func (s *Server) Routes(interceptors ...connect.Interceptor) (string, http.Handler) {
	return rafikiv1connect.NewControlHandler(s, connect.WithInterceptors(interceptors...))
}

// GetHistory serves the durable tier: persisted messages converted to native
// events, filtered to those after the caller's cursor. The cursor is the
// stored ordinal, which is exact — unlike a timestamp, which collides because
// postgres now() is transaction-start time and is identical for every row
// written in one transaction.
func (s *Server) GetHistory(
	ctx context.Context,
	req *connect.Request[rafikiv1.GetHistoryRequest],
) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}

	conversationID, err := s.resolveConversation(childID)
	if err != nil {
		return nil, err
	}

	msgs, err := s.history.Load(ctx, conversationID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("load history: %w", err))
	}

	if req.Msg.AfterOrdinal != nil {
		after := int(req.Msg.GetAfterOrdinal())
		kept := msgs[:0]
		for _, m := range msgs {
			if m.Ordinal > after {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}

	return connect.NewResponse(&rafikiv1.GetHistoryResponse{
		Events: eventconv.EventsFromMessages(childID, msgs),
	}), nil
}

// Compile-time proof that the production type satisfies the test seam.
var _ HistoryLoader = (*store.Messages)(nil)
