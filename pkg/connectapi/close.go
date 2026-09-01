// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

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
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&rafikiv1.CloseResponse{ChildId: childID}), nil
}
