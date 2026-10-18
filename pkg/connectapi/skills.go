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

// SkillFile is one supporting file of a skill, as the manager reports it: the
// path relative to the skill's directory, its 0644/0755 mode, and its size in
// bytes. The content stays on the daemon — the agent surfaces fetch it by the
// skill's id.
type SkillFile struct {
	Path string
	Mode uint32
	Size int64
}

// SkillRow is one skill. Body and Files are populated only on GetSkill.
//
// ID and OwnerUserID are the row's identity: a name is unique only within a
// layer, so the same namespace:name can be three rows. Layer is "core",
// "global" or "user"; Shadowed marks an enabled row a higher layer overrides,
// so an inventory can say "listed, but not in effect".
type SkillRow struct {
	ID          string
	Namespace   string
	Name        string
	Description string
	Body        string
	Source      string
	OwnerUserID string
	Layer       string
	Enabled     bool
	Shadowed    bool
	Files       []SkillFile
	UpdatedAt   time.Time
}

// SkillManager is the narrow slice of the daemon needed to manage skills.
//
// There is no write-a-skill verb: skills come from repos and from the daemon's
// own embedded corpus, each owning its own rows, and a client-side upsert could
// only ever overwrite one of those owners' content.
//
// The two writes enforce the permission boundary, because only the
// implementation can see the caller's identity and the target row's layer: a
// shared row (core or global) may be changed only by an admin or over the local
// socket, and DeleteSkill refuses a source that owns its row's lifecycle (the
// core corpus, a repo). Those refusals must be returned as an already-coded
// *connect.Error — PermissionDenied and FailedPrecondition respectively — so
// skillError passes the code through instead of reporting internal.
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

// skillError maps a manager failure onto a Connect code. An unknown skill is
// NotFound — a missing skill is an ANSWER, not a failure. An already-coded
// *connect.Error passes through untouched: the daemon's adapter chooses the
// code for a refusal deliberately (PermissionDenied for a shared row a
// non-admin may not toggle, FailedPrecondition for a row whose source owns its
// lifecycle), and re-wrapping it would report every refusal as internal.
func skillError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, ErrSkillNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func toProtoSkill(r SkillRow) *rafikiv1.SkillRow {
	out := &rafikiv1.SkillRow{
		Namespace:   r.Namespace,
		Name:        r.Name,
		Description: r.Description,
		Source:      r.Source,
		Enabled:     r.Enabled,
		UpdatedAt:   timestamppb.New(r.UpdatedAt),
		Body:        r.Body,
		Id:          r.ID,
		OwnerUserId: r.OwnerUserID,
		Layer:       r.Layer,
		Shadowed:    r.Shadowed,
	}
	for _, f := range r.Files {
		out.Files = append(out.Files, &rafikiv1.SkillFileInfo{
			Path: f.Path,
			Mode: f.Mode,
			Size: f.Size,
		})
	}
	return out
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
