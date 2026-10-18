// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ErrSkillNotFound is what a SkillManager returns for a skill that does not
// exist. It is a package-local sentinel for the same reason ExecutorRow is a
// package-local type: cmd/rafiki links this package and must not be dragged
// into pgx's dependency graph through pkg/skills' store half.
var ErrSkillNotFound = errors.New("skill not found")

// SkillRow is one skill. Body is populated only on GetSkill.
type SkillRow struct {
	Namespace   string
	Name        string
	Description string
	Body        string
	Source      string
	Enabled     bool
	UpdatedAt   time.Time
}

// SkillManager is the narrow slice of the daemon needed to manage skills.
//
// There is no write-a-skill verb: skills come from repos and from the daemon's
// own embedded corpus, each owning its own rows, and a client-side upsert could
// only ever overwrite one of those owners' content.
type SkillManager interface {
	ListSkills(ctx context.Context, includeDisabled bool) ([]SkillRow, error)
	GetSkill(ctx context.Context, namespace, name string) (SkillRow, error)
	DeleteSkill(ctx context.Context, namespace, name string) error
	SetSkillEnabled(ctx context.Context, namespace, name string, enabled bool) error
}

// SetSkillManager attaches the skills backend. Post-construction setter for the
// same reason as SetExecutorLister: the Controller is built after this Server.
// A nil manager is refused rather than stored, the same rule as
// SetPymoduleManager: storing &m for a nil interface would defeat
// skillManager's Unavailable path and nil-panic the first handler call.
func (s *Server) SetSkillManager(m SkillManager) {
	if m == nil {
		return
	}
	s.skills.Store(&m)
}

func (s *Server) skillManager() (SkillManager, error) {
	p := s.skills.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("skills backend not yet wired"))
	}
	return *p, nil
}

func skillError(err error) error {
	if errors.Is(err, ErrSkillNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func toProtoSkill(r SkillRow) *rafikiv1.SkillRow {
	return &rafikiv1.SkillRow{
		Namespace:   r.Namespace,
		Name:        r.Name,
		Description: r.Description,
		Source:      r.Source,
		Enabled:     r.Enabled,
		UpdatedAt:   timestamppb.New(r.UpdatedAt),
		Body:        r.Body,
	}
}

func (s *Server) ListSkills(
	ctx context.Context, req *connect.Request[rafikiv1.ListSkillsRequest],
) (*connect.Response[rafikiv1.ListSkillsResponse], error) {
	m, err := s.skillManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListSkills(ctx, req.Msg.GetIncludeDisabled())
	if err != nil {
		return nil, skillError(err)
	}
	out := make([]*rafikiv1.SkillRow, 0, len(rows))
	for _, r := range rows {
		r.Body = "" // an inventory carries no documents
		out = append(out, toProtoSkill(r))
	}
	return connect.NewResponse(&rafikiv1.ListSkillsResponse{Rows: out}), nil
}

func (s *Server) GetSkill(
	ctx context.Context, req *connect.Request[rafikiv1.GetSkillRequest],
) (*connect.Response[rafikiv1.GetSkillResponse], error) {
	m, err := s.skillManager()
	if err != nil {
		return nil, err
	}
	row, err := m.GetSkill(ctx, req.Msg.GetNamespace(), req.Msg.GetName())
	if err != nil {
		return nil, skillError(err)
	}
	return connect.NewResponse(&rafikiv1.GetSkillResponse{Row: toProtoSkill(row)}), nil
}

func (s *Server) DeleteSkill(
	ctx context.Context, req *connect.Request[rafikiv1.DeleteSkillRequest],
) (*connect.Response[rafikiv1.DeleteSkillResponse], error) {
	m, err := s.skillManager()
	if err != nil {
		return nil, err
	}
	if err := m.DeleteSkill(ctx, req.Msg.GetNamespace(), req.Msg.GetName()); err != nil {
		return nil, skillError(err)
	}
	return connect.NewResponse(&rafikiv1.DeleteSkillResponse{}), nil
}

func (s *Server) SetSkillEnabled(
	ctx context.Context, req *connect.Request[rafikiv1.SetSkillEnabledRequest],
) (*connect.Response[rafikiv1.SetSkillEnabledResponse], error) {
	m, err := s.skillManager()
	if err != nil {
		return nil, err
	}
	if err := m.SetSkillEnabled(ctx, req.Msg.GetNamespace(), req.Msg.GetName(), req.Msg.GetEnabled()); err != nil {
		return nil, skillError(err)
	}
	return connect.NewResponse(&rafikiv1.SetSkillEnabledResponse{}), nil
}
