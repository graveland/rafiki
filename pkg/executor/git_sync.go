// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// GitRefs reports whether a repo exists on this executor and, if so, its
// branches plus the scratch directory bundle files may be written to.
//
// Stub: the RPC exists so the service compiles and Describe can advertise
// tree_sync, but its behaviour lands separately.
func (s *Server) GitRefs(
	_ context.Context,
	_ *connect.Request[executorpb.GitRefsRequest],
) (*connect.Response[executorpb.GitRefsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GitRefs: not implemented"))
}

// GitBundle writes one branch to a bundle file in this executor's scratch
// directory, skipping the objects the caller already has.
//
// Stub: the RPC exists so the service compiles and Describe can advertise
// tree_sync, but its behaviour lands separately.
func (s *Server) GitBundle(
	_ context.Context,
	_ *connect.Request[executorpb.GitBundleRequest],
) (*connect.Response[executorpb.GitBundleResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GitBundle: not implemented"))
}

// GitFetchBundle fetches a bundle file into a repo on this executor, refusing
// a checked-out branch and a non-fast-forward update unless force.
//
// Stub: the RPC exists so the service compiles and Describe can advertise
// tree_sync, but its behaviour lands separately.
func (s *Server) GitFetchBundle(
	_ context.Context,
	_ *connect.Request[executorpb.GitFetchBundleRequest],
) (*connect.Response[executorpb.GitFetchBundleResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GitFetchBundle: not implemented"))
}
