// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Close finalizes an exited child. See CloseRequest's comment for what does and
// does not survive.
//
// Errors go through ConnectErr: the code the daemon attached at the source IS
// the classification, so a double-close reads as NotFound and closing a
// still-running child as FailedPrecondition — not Internal, which tells the
// client the daemon is broken. Controller.Close returns only ControllerError
// values whose text the daemon wrote (children.Delete failures are logged,
// never returned), so the authored message is forwarded with it — the same
// decision the daemon's error mapping makes. An error that is NOT a
// ControllerError is infrastructure text and is redacted by ConnectErr; its
// cause is logged here so it is not lost.
func (s *Server) Close(
	ctx context.Context,
	req *connect.Request[rafikiv1.CloseRequest],
) (*connect.Response[rafikiv1.CloseResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	// childScoped: the subtree boundary runs before the close is attempted.
	if sc := s.childScope(ctx); sc != nil {
		if err := sc.Authorize(childID); err != nil {
			return nil, err
		}
	}
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}
	var descendants []string
	if req.Msg.GetIncludeDescendants() {
		var err error
		descendants, err = cascadeClose(ctx, *p, childID)
		if err != nil {
			return nil, cascadeErr(err)
		}
	}
	if err := (*p).Close(ctx, childID); err != nil {
		var ce *ControllerError
		if !errors.As(err, &ce) {
			// ConnectErr redacts this below; log the cause here or lose it.
			slog.Error("connect: close failed", "child_id", childID, "error", err)
		}
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.CloseResponse{ChildId: childID, DescendantIds: descendants}), nil
}
