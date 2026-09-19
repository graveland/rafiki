// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Stubs for the git-source pymodule RPCs, so the regenerated ControlHandler
// interface compiles before the daemon is wired. Task "daemon git-source
// wiring" replaces these stubs with real handlers backed by a git-source
// manager.

func (s *Server) AddPymoduleGitSource(ctx context.Context, req *connect.Request[rafikiv1.AddPymoduleGitSourceRequest]) (*connect.Response[rafikiv1.AddPymoduleGitSourceResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("pymodule git sources not yet wired"))
}

func (s *Server) ListPymoduleGitSources(ctx context.Context, req *connect.Request[rafikiv1.ListPymoduleGitSourcesRequest]) (*connect.Response[rafikiv1.ListPymoduleGitSourcesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("pymodule git sources not yet wired"))
}

func (s *Server) RefreshPymoduleGitSource(ctx context.Context, req *connect.Request[rafikiv1.RefreshPymoduleGitSourceRequest]) (*connect.Response[rafikiv1.RefreshPymoduleGitSourceResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("pymodule git sources not yet wired"))
}

func (s *Server) RemovePymoduleGitSource(ctx context.Context, req *connect.Request[rafikiv1.RemovePymoduleGitSourceRequest]) (*connect.Response[rafikiv1.RemovePymoduleGitSourceResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("pymodule git sources not yet wired"))
}
