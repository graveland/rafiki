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

// Server implements the Control service.
type Server struct {
	history HistoryLoader
}

func NewServer(h HistoryLoader) *Server { return &Server{history: h} }

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

	msgs, err := s.history.Load(ctx, childID)
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
