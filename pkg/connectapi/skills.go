// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ErrSkillNotFound is what a SkillManager returns for a skill that does not
// exist. It is a package-local sentinel for the same reason ExecutorRow is a
// package-local type: cmd/rafiki links this package and must not be dragged
// into pgx's dependency graph through pkg/skills' store half.
var ErrSkillNotFound = errors.New("skill not found")

// ErrSkillSourceConflict is what a SkillManager returns when the
// (namespace, name) is held by an enabled row the caller is not replacing —
// an upsert that would change an enabled row's source, or an enable that
// would leave two enabled rows under one name. Package-local for the same
// reason as ErrSkillNotFound.
var ErrSkillSourceConflict = errors.New("skill name is held by an enabled row of another source")

// reservedCoreSource is owned by the daemon's startup sync of its embedded
// corpus. A client that could write it could plant a row the sync would then
// fight over on every restart.
const reservedCoreSource = "rafiki-core"

// SkillRow is one skill. Body is populated only on GetSkill and UpsertSkill.
type SkillRow struct {
	Namespace           string
	Name                string
	Description         string
	Body                string
	Source              string
	ShadowedCoreVersion string
	Enabled             bool
	UpdatedAt           string
}

// SkillManager is the narrow slice of the daemon needed to manage skills.
type SkillManager interface {
	ListSkills(ctx context.Context, includeDisabled bool) ([]SkillRow, error)
	GetSkill(ctx context.Context, namespace, name string) (SkillRow, error)
	UpsertSkill(ctx context.Context, r SkillRow) (SkillRow, error)
	DeleteSkill(ctx context.Context, namespace, name string) error
	SetSkillEnabled(ctx context.Context, namespace, name string, enabled bool) error
}

// SetSkillManager attaches the skills backend. Post-construction setter for the
// same reason as SetExecutorLister: the Controller is built after this Server.
func (s *Server) SetSkillManager(m SkillManager) { s.skills.Store(&m) }

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
	// AlreadyExists, not Internal: the caller is being told the name is taken
	// by a row it did not disable or delete first — an answer about the corpus,
	// the same class as a missing skill being NotFound.
	if errors.Is(err, ErrSkillSourceConflict) {
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// validSkillIdent guards one half of a qualified name. Both halves render into
// a model's skills inventory and come back through the skill tool's argument,
// where "ns:name" is the parse — a colon inside either half breaks the inverse
// — and a space, slash or control character would reach the paths that render
// or store them. "." and ".." are directory-lookup sentinels, not names.
func validSkillIdent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, " :/\t\r\n\x00")
}

func toProtoSkill(r SkillRow) *rafikiv1.SkillRow {
	return &rafikiv1.SkillRow{
		Namespace:           r.Namespace,
		Name:                r.Name,
		Description:         r.Description,
		Source:              r.Source,
		Enabled:             r.Enabled,
		ShadowedCoreVersion: r.ShadowedCoreVersion,
		UpdatedAt:           r.UpdatedAt,
		Body:                r.Body,
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

func (s *Server) UpsertSkill(
	ctx context.Context, req *connect.Request[rafikiv1.UpsertSkillRequest],
) (*connect.Response[rafikiv1.UpsertSkillResponse], error) {
	m, err := s.skillManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if req.Msg.GetBody() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("body is required"))
	}
	if req.Msg.GetSource() == reservedCoreSource {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("source \"rafiki-core\" is reserved for the daemon's own corpus"))
	}
	row := SkillRow{
		Namespace:           req.Msg.GetNamespace(),
		Name:                req.Msg.GetName(),
		Description:         req.Msg.GetDescription(),
		Body:                req.Msg.GetBody(),
		Source:              req.Msg.GetSource(),
		ShadowedCoreVersion: req.Msg.GetShadowedCoreVersion(),
		Enabled:             true,
	}
	if row.Namespace == "" {
		row.Namespace = "rafiki"
	}
	if row.Source == "" {
		row.Source = "manual"
	}
	for _, part := range []struct{ what, val string }{
		{"namespace", row.Namespace}, {"name", row.Name},
	} {
		if !validSkillIdent(part.val) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("%s %q must be a slug: no spaces, colons, slashes, control characters, or \".\"/\"..\"", part.what, part.val))
		}
	}
	out, err := m.UpsertSkill(ctx, row)
	if err != nil {
		return nil, skillError(err)
	}
	return connect.NewResponse(&rafikiv1.UpsertSkillResponse{Row: toProtoSkill(out)}), nil
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
