// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Rename renames a live child and the conversations linked to it. See
// RenameRequest's comment in control.proto. It is userOnly in the policy
// table, so no child scope applies. An owner guard still applies: a non-admin
// user may rename only its own.
//
// Errors go through ConnectErr, as in SetBudget.
func (s *Server) Rename(
	ctx context.Context,
	req *connect.Request[rafikiv1.RenameRequest],
) (*connect.Response[rafikiv1.RenameResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	name := strings.TrimSpace(req.Msg.GetName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name is required"))
	}
	// ownerGuard: a non-admin user credential acts only on children it owns;
	// foreign, unknown and unowned ids refuse identically, before any side
	// effect.
	if g := s.ownerGuard(ctx); g != nil {
		if err := g.Authorize(childID); err != nil {
			return nil, err
		}
	}
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}
	if err := (*p).Rename(ctx, childID, name); err != nil {
		var ce *ControllerError
		if !errors.As(err, &ce) {
			slog.Error("connect: rename failed", "child_id", childID, "error", err)
		}
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.RenameResponse{ChildId: childID, Name: name}), nil
}
