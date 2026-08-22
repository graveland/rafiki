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

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
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
type Server struct {
	history  HistoryLoader
	events   EventSource
	resolver ConversationResolver
}

func NewServer(h HistoryLoader) *Server { return &Server{history: h} }

// SetChildResolver attaches the child-id-to-conversation-id resolver. It is a
// post-construction setter, not a constructor argument, because the daemon's
// Controller (the only thing that knows this mapping) is constructed AFTER
// the proxy face that owns this Server — see cmd/rafikid/main.go, which wires
// it the same way it wires ctrl.SetProxy(face.URL, face.Token). Until it is
// set, every call that needs it fails closed (CodeUnavailable) rather than
// passing the child id through as if it were already a conversation id — the
// exact bug this resolver exists to close.
func (s *Server) SetChildResolver(r ConversationResolver) { s.resolver = r }

// resolveConversation turns a request's child_id into the conversation id
// store.Messages.Load needs. Centralized here because GetHistory and
// StreamEvents both need it with identical semantics.
func (s *Server) resolveConversation(childID string) (string, error) {
	if s.resolver == nil {
		return "", connect.NewError(connect.CodeUnavailable,
			errors.New("child resolver not yet wired"))
	}
	conversationID, ok := s.resolver.ConversationID(childID)
	if !ok {
		return "", connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no conversation for child %q", childID))
	}
	return conversationID, nil
}

// Routes returns the mux path prefix and handler for the Control service.
// Auth is the caller's responsibility — mount it under the same middleware
// that already protects the other HTTP faces (see cmd/rafikid/proxy.go).
func (s *Server) Routes() (string, http.Handler) {
	return rafikiv1connect.NewControlHandler(s)
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
