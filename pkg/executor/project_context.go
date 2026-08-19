package executor

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/projectctx"
)

// ProjectContext returns the instruction files belonging to a workspace.
//
// The git-root walk and the @include expansion both happen here, not on the
// daemon: both resolve paths on THIS filesystem, and the daemon has neither the
// checkout nor any way to know where it begins.
func (s *Server) ProjectContext(
	_ context.Context,
	req *connect.Request[executorpb.ProjectContextRequest],
) (*connect.Response[executorpb.ProjectContextResponse], error) {
	// Resolve through the workspace handle rather than accepting a path: an
	// unknown handle must be refused, or the grant is a formality and anything
	// reaching this executor could read instruction files anywhere on it.
	ws, ok := s.wsReg.get(req.Msg.GetWorkspaceId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			errors.New("unknown workspace"))
	}

	content, err := projectctx.LoadProjectContext(ws.workdir)
	if err != nil {
		// Best-effort by design, matching LoadProjectContext's own contract:
		// one unreadable file must not cost the agent every instruction file it
		// does have, and a hard failure here would fail the whole spawn.
		slog.Warn("executor: project context partially unreadable", "workspace", req.Msg.GetWorkspaceId(), "error", err)
	}
	return connect.NewResponse(&executorpb.ProjectContextResponse{ContextFiles: content}), nil
}
