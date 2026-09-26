// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ChildOps is the operator-side slice of the daemon behind the framed
// ctrl_resume / ctrl_forget_all_exited / ctrl_set_labels / ctrl_status /
// ctrl_search / ctrl_daemon_shutdown / ctrl_model_info /
// ctrl_conversation_stats verbs. It is a seam because the daemon's Controller
// — the only thing that can answer any of it — is built after the proxy face
// that owns this Server, so it attaches post-construction, and because
// depending on the interface keeps the handler testable without a database.
type ChildOps interface {
	Resume(ctx context.Context, childID, apiKey string) (string, error)
	CloseAllExited(ctx context.Context, olderThanMs int64) ([]string, error)
	SetLabels(ctx context.Context, childID string, set map[string]string, remove []string) (map[string]string, error)
	Status(ctx context.Context) (*rafikiv1.StatusResponse, error)
	Search(ctx context.Context, req *rafikiv1.SearchRequest) (*rafikiv1.SearchResponse, error)
	ShutdownDaemon(ctx context.Context) error
	ModelInfo(ctx context.Context, model string) (*rafikiv1.ModelInfoResponse, error)
	ConversationStats(ctx context.Context, req *rafikiv1.ConversationStatsRequest) (string, error) // JSON
}

// SetChildOps attaches the child-operator backend. Post-construction setter
// for the same reason as SetSkillManager: the Controller is built after this
// Server. A nil backend is refused rather than stored, the same rule as
// SetSkillManager: storing &o for a nil interface would defeat the handler's
// Unavailable path and nil-panic the first handler call.
func (s *Server) SetChildOps(o ChildOps) {
	if o == nil {
		return
	}
	s.childOps.Store(&o)
}

// Resume serves the framed ctrl_resume face: re-spawn an exited child.
func (s *Server) Resume(
	ctx context.Context,
	req *connect.Request[rafikiv1.ResumeRequest],
) (*connect.Response[rafikiv1.ResumeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Resume: not yet implemented"))
}

// CloseAllExited serves the framed ctrl_forget_all_exited face.
func (s *Server) CloseAllExited(
	ctx context.Context,
	req *connect.Request[rafikiv1.CloseAllExitedRequest],
) (*connect.Response[rafikiv1.CloseAllExitedResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("CloseAllExited: not yet implemented"))
}

// SetLabels serves the framed ctrl_set_labels face.
func (s *Server) SetLabels(
	ctx context.Context,
	req *connect.Request[rafikiv1.SetLabelsRequest],
) (*connect.Response[rafikiv1.SetLabelsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("SetLabels: not yet implemented"))
}

// Status serves the framed ctrl_status face.
func (s *Server) Status(
	ctx context.Context,
	req *connect.Request[rafikiv1.StatusRequest],
) (*connect.Response[rafikiv1.StatusResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Status: not yet implemented"))
}

// Search serves the framed ctrl_search face.
func (s *Server) Search(
	ctx context.Context,
	req *connect.Request[rafikiv1.SearchRequest],
) (*connect.Response[rafikiv1.SearchResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Search: not yet implemented"))
}

// ShutdownDaemon serves the framed ctrl_daemon_shutdown face.
func (s *Server) ShutdownDaemon(
	ctx context.Context,
	req *connect.Request[rafikiv1.ShutdownDaemonRequest],
) (*connect.Response[rafikiv1.ShutdownDaemonResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ShutdownDaemon: not yet implemented"))
}

// ModelInfo serves the framed ctrl_model_info face: the daemon's own catalog
// answer for one model, so the client never reads OpenRouter itself.
func (s *Server) ModelInfo(
	ctx context.Context,
	req *connect.Request[rafikiv1.ModelInfoRequest],
) (*connect.Response[rafikiv1.ModelInfoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ModelInfo: not yet implemented"))
}

// ConversationStats serves the framed ctrl_conversation_stats face.
func (s *Server) ConversationStats(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationStatsRequest],
) (*connect.Response[rafikiv1.ConversationStatsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ConversationStats: not yet implemented"))
}
