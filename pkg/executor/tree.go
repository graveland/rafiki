// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// ReadTree streams a file or directory tree from this executor.
//
// Stub: the RPC exists so the service compiles and Describe can advertise
// tree_sync, but its behaviour lands separately.
func (s *Server) ReadTree(
	_ context.Context,
	_ *connect.Request[executorpb.ReadTreeRequest],
	_ *connect.ServerStream[executorpb.ReadTreeResponse],
) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("ReadTree: not implemented"))
}

// WriteTree receives a file or directory tree on this executor.
//
// Stub: the RPC exists so the service compiles and Describe can advertise
// tree_sync, but its behaviour lands separately.
func (s *Server) WriteTree(
	_ context.Context,
	_ *connect.ClientStream[executorpb.WriteTreeRequest],
) (*connect.Response[executorpb.WriteTreeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("WriteTree: not implemented"))
}
