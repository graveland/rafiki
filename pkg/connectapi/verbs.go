// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// The five verbs below are stubs so *Server keeps satisfying the generated
// ControlHandler interface between plan tasks. Tasks 5-8 replace each one.

func (s *Server) Send(context.Context, *connect.Request[rafikiv1.SendRequest]) (*connect.Response[rafikiv1.SendResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Send not implemented"))
}

func (s *Server) ListChildren(context.Context, *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListChildren not implemented"))
}

func (s *Server) GetChild(context.Context, *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GetChild not implemented"))
}

func (s *Server) Spawn(context.Context, *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Spawn not implemented"))
}

func (s *Server) Kill(context.Context, *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Kill not implemented"))
}
