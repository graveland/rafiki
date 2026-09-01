// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// closeCode maps a Controller.Close error onto its Connect code, mirroring the
// framed face's mapErr: the code the daemon attached at the source IS the
// classification, so a client double-closing a child learns NotFound rather
// than a code that means "server bug". Authored ControllerError messages are
// forwarded with the code — Controller.Close returns only values whose text
// the daemon wrote (children.Delete failures are logged, never returned), so
// forwarding them is the same decision the framed forget handler makes.
func closeCode(err error) connect.Code {
	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		return connect.CodeInternal
	}
	switch ce.Code {
	case protocol.ErrNotFound:
		return connect.CodeNotFound
	case protocol.ErrNotExited:
		return connect.CodeFailedPrecondition
	default:
		return connect.CodeInternal
	}
}

// Close finalizes an exited child. See CloseRequest's comment for what does and
// does not survive.
func (s *Server) Close(
	ctx context.Context,
	req *connect.Request[rafikiv1.CloseRequest],
) (*connect.Response[rafikiv1.CloseResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}
	if err := (*p).Close(ctx, childID); err != nil {
		return nil, connect.NewError(closeCode(err), err)
	}
	return connect.NewResponse(&rafikiv1.CloseResponse{ChildId: childID}), nil
}
