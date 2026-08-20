package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/skills"
)

// ProjectSkills is part of the ExecutorService Connect RPC surface.

// ProjectSkills returns the skills discovered in a workspace — the project
// tier only. User and system skills belong to whoever runs the agent loop and
// are never reported here.
//
// Metadata only. Bodies are fetched by SkillBody on the turn a model actually
// asks for one: an inventory is a handful of lines per skill, while a body is
// a document, and returning every body at spawn would put the whole project's
// skill text into every child.
func (s *Server) ProjectSkills(
	_ context.Context,
	req *connect.Request[executorpb.ProjectSkillsRequest],
) (*connect.Response[executorpb.ProjectSkillsResponse], error) {
	ws, ok := s.wsReg.get(req.Msg.GetWorkspaceId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			errors.New("unknown workspace"))
	}

	dirs := []string{
		filepath.Join(ws.workdir, ".claude", "skills"),
		filepath.Join(ws.workdir, ".rafiki", "skills"),
	}
	discovered, err := skills.DiscoverSkills(dirs, nil)
	if err != nil {
		return nil, fmt.Errorf("discover project skills: %w", err)
	}

	out := make([]*executorpb.ProjectSkill, 0, len(discovered))
	for _, m := range discovered {
		out = append(out, &executorpb.ProjectSkill{
			Name:        m.Name,
			Description: m.Description,
			Dir:         m.Dir,
		})
	}
	return connect.NewResponse(&executorpb.ProjectSkillsResponse{Skills: out}), nil
}

// SkillBody returns one project skill's body, resolved by NAME against the
// executor's own discovery.
//
// By name and not by path, for the same reason ProjectContext takes a
// workspace id: a path parameter would let anything that reaches this
// executor read an arbitrary file on it.
func (s *Server) SkillBody(
	_ context.Context,
	req *connect.Request[executorpb.SkillBodyRequest],
) (*connect.Response[executorpb.SkillBodyResponse], error) {
	ws, ok := s.wsReg.get(req.Msg.GetWorkspaceId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			errors.New("unknown workspace"))
	}

	dirs := []string{
		filepath.Join(ws.workdir, ".claude", "skills"),
		filepath.Join(ws.workdir, ".rafiki", "skills"),
	}
	discovered, err := skills.DiscoverSkills(dirs, nil)
	if err != nil {
		return nil, fmt.Errorf("discover project skills: %w", err)
	}

	name := req.Msg.GetName()
	for _, m := range discovered {
		if m.Name == name {
			body, err := skills.SkillBody(m.Path)
			if err != nil {
				return nil, fmt.Errorf("read skill body: %w", err)
			}
			return connect.NewResponse(&executorpb.SkillBodyResponse{
				Body: body,
				Dir:  m.Dir,
			}), nil
		}
	}

	return nil, connect.NewError(connect.CodeNotFound,
		fmt.Errorf("skill %q not found in this workspace", name))
}
