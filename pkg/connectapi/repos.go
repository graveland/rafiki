// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/repos"
)

// RepoRow is one registered repo as the manager returns it: the human label
// used as the `repo` argument everywhere, the url and ref it is registered
// with, and the state its last refresh left behind.
type RepoRow struct {
	Name string
	URL  string
	Ref  string

	// Commit is the commit the last successful refresh resolved Ref to; empty
	// means never resolved.
	Commit string
	// RefreshInterval is the opt-in schedule; 0 means on demand.
	RefreshInterval time.Duration
	// ResolvedAt is when Commit was resolved; the zero time means never.
	ResolvedAt time.Time
	// Skills is how many skill rows the source currently owns for the caller's
	// owner (its repo:<name> set).
	Skills int
	// Scripts is how many scripts the source's cached discovery reported. It
	// is a cache read: zero until the source has been refreshed in this
	// daemon's lifetime.
	Scripts int
}

// RepoScript and RepoPackage are one discovered entry of a repo's
// checkout, mirroring the executor protocol's messages of the same
// names — local structs rather than domain types, the same isolation
// PymoduleRow gives PymoduleManager from pkg/pymodules.
type RepoScript struct {
	Name        string
	Description string
}

type RepoPackage struct {
	Name        string
	Description string
}

// RepoRefresh is one refresh's whole answer: what commit it resolved and
// fanned out, whether that commit was already the stored one, the discovered
// inventory and venv state, and what happened to the skills half.
//
// SkillErrors is the skills half's failure, never the refresh's: a bad skill
// is reported and the scripts still refreshed.
type RepoRefresh struct {
	Commit      string
	Unchanged   bool
	Scripts     []RepoScript
	Packages    []RepoPackage
	VenvReady   bool
	VenvError   string
	Skills      int
	SkillErrors []string
}

// RepoSetRequest changes a registration's ref and/or its refresh interval.
// Both halves are independent and presence-sensitive: a nil field leaves that
// half exactly as it was, while a PRESENT zero interval means "on demand" —
// which is why the interval is a pointer rather than a plain duration.
type RepoSetRequest struct {
	Name            string
	Ref             *string
	RefreshInterval *time.Duration
}

// RepoManager is the narrow slice of the daemon needed to manage the
// caller's own repos. The owner is resolved INSIDE the implementation
// from the request context — no method takes one, the same rule as
// PymoduleManager — and the anonymous unix-socket caller shares the daemon's
// unattributed bucket, the same owner its own children resolve to.
type RepoManager interface {
	// AddRepo registers (or repoints) the source and runs its first refresh.
	// refreshInterval 0 means on demand; a positive value below one minute is
	// refused with repos.ErrIntervalTooShort. The returned RepoRefresh is that
	// first refresh's outcome.
	AddRepo(ctx context.Context, name, url, ref string, refreshInterval time.Duration) (RepoRow, RepoRefresh, error)
	ListRepos(ctx context.Context) ([]RepoRow, error)
	RefreshRepo(ctx context.Context, name string) (RepoRefresh, error)
	SetRepo(ctx context.Context, req RepoSetRequest) (RepoRow, error)
	RemoveRepo(ctx context.Context, name string) error
}

// SetRepoManager attaches the repo backend. Post-construction
// setter, same reason as SetPymoduleManager: the Controller is built after
// this Server. A nil manager is refused rather than stored: storing &m for
// a nil interface would defeat repoManager's Unavailable path and
// nil-panic the first handler call instead.
func (s *Server) SetRepoManager(m RepoManager) {
	if m == nil {
		return
	}
	s.repos.Store(&m)
}

func (s *Server) repoManager() (RepoManager, error) {
	p := s.repos.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("repos backend not yet wired"))
	}
	return *p, nil
}

// repoError maps the store's sentinel errors onto Connect codes the
// same way pymoduleError does: ErrNotFound (an unknown or already-removed
// name) is NotFound, the reserved "local" name and an out-of-range refresh
// interval are InvalidArgument, and everything else is Internal — a store
// failure must not surface shaped like something the caller said wrong.
func repoError(err error) error {
	switch {
	case errors.Is(err, repos.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, repos.ErrReservedName):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, repos.ErrIntervalTooShort):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return unmappedErr(err, "mapper", "repoError")
}

// validateRepoName applies the repo name rule client-side of the
// store: "local" is reserved for the blob store, everything else must be a
// safe path segment (repos.ValidateName) — the name becomes the `repo`
// argument's value across the whole pymodule tool surface and a checkout
// directory on every executor.
func validateRepoName(name string) error {
	if name == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if err := repos.ValidateName(name); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return nil
}

// repoInterval converts an optional wire duration into the domain value and
// applies the floor: absent is on demand, and anything between zero and one
// minute is refused with the sentinel's own text rather than reaching the
// store's CHECK constraint.
func repoInterval(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("refresh_interval: %w", err))
	}
	interval := d.AsDuration()
	if interval < 0 || (interval > 0 && interval < repos.MinRefreshInterval) {
		return 0, connect.NewError(connect.CodeInvalidArgument, repos.ErrIntervalTooShort)
	}
	return interval, nil
}

func toProtoRepoRow(r RepoRow) *rafikiv1.RepoRow {
	row := &rafikiv1.RepoRow{
		Name:    r.Name,
		Url:     r.URL,
		Ref:     r.Ref,
		Commit:  r.Commit,
		Skills:  int32(r.Skills),
		Scripts: int32(r.Scripts),
	}
	if r.RefreshInterval > 0 {
		row.RefreshInterval = durationpb.New(r.RefreshInterval)
	}
	if !r.ResolvedAt.IsZero() {
		row.ResolvedAt = timestamppb.New(r.ResolvedAt)
	}
	return row
}

func (s *Server) AddRepo(
	ctx context.Context, req *connect.Request[rafikiv1.AddRepoRequest],
) (*connect.Response[rafikiv1.AddRepoResponse], error) {
	m, err := s.repoManager()
	if err != nil {
		return nil, err
	}
	if err := validateRepoName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	if req.Msg.GetUrl() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("url is required"))
	}
	// Resolved before the manager is touched: a refused interval must not
	// leave a registration behind.
	interval, err := repoInterval(req.Msg.RefreshInterval)
	if err != nil {
		return nil, err
	}
	row, refresh, err := m.AddRepo(ctx, req.Msg.GetName(), req.Msg.GetUrl(), req.Msg.GetRef(), interval)
	if err != nil {
		return nil, repoError(err)
	}
	return connect.NewResponse(&rafikiv1.AddRepoResponse{
		Row:         toProtoRepoRow(row),
		Commit:      refresh.Commit,
		Unchanged:   refresh.Unchanged,
		Skills:      int32(refresh.Skills),
		SkillErrors: refresh.SkillErrors,
	}), nil
}

func (s *Server) ListRepos(
	ctx context.Context, req *connect.Request[rafikiv1.ListReposRequest],
) (*connect.Response[rafikiv1.ListReposResponse], error) {
	m, err := s.repoManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListRepos(ctx)
	if err != nil {
		return nil, repoError(err)
	}
	out := make([]*rafikiv1.RepoRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoRepoRow(r))
	}
	return connect.NewResponse(&rafikiv1.ListReposResponse{Rows: out}), nil
}

func (s *Server) RefreshRepo(
	ctx context.Context, req *connect.Request[rafikiv1.RefreshRepoRequest],
) (*connect.Response[rafikiv1.RefreshRepoResponse], error) {
	m, err := s.repoManager()
	if err != nil {
		return nil, err
	}
	if err := validateRepoName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	refresh, err := m.RefreshRepo(ctx, req.Msg.GetName())
	if err != nil {
		return nil, repoError(err)
	}
	out := &rafikiv1.RefreshRepoResponse{
		Name:        req.Msg.GetName(),
		Scripts:     make([]*rafikiv1.RepoScript, 0, len(refresh.Scripts)),
		Packages:    make([]*rafikiv1.RepoPackage, 0, len(refresh.Packages)),
		VenvReady:   refresh.VenvReady,
		VenvError:   refresh.VenvError,
		Commit:      refresh.Commit,
		Unchanged:   refresh.Unchanged,
		Skills:      int32(refresh.Skills),
		SkillErrors: refresh.SkillErrors,
	}
	for _, sc := range refresh.Scripts {
		out.Scripts = append(out.Scripts, &rafikiv1.RepoScript{Name: sc.Name, Description: sc.Description})
	}
	for _, pk := range refresh.Packages {
		out.Packages = append(out.Packages, &rafikiv1.RepoPackage{Name: pk.Name, Description: pk.Description})
	}
	return connect.NewResponse(out), nil
}

// SetRepo changes a registration's ref and/or its refresh interval without
// touching the other half, and answers the updated row. An absent field means
// "leave it alone"; a present zero interval means "on demand".
func (s *Server) SetRepo(
	ctx context.Context, req *connect.Request[rafikiv1.SetRepoRequest],
) (*connect.Response[rafikiv1.SetRepoResponse], error) {
	m, err := s.repoManager()
	if err != nil {
		return nil, err
	}
	if err := validateRepoName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	set := RepoSetRequest{Name: req.Msg.GetName()}
	if req.Msg.Ref != nil {
		ref := req.Msg.GetRef()
		set.Ref = &ref
	}
	if req.Msg.RefreshInterval != nil {
		interval, err := repoInterval(req.Msg.RefreshInterval)
		if err != nil {
			return nil, err
		}
		set.RefreshInterval = &interval
	}
	row, err := m.SetRepo(ctx, set)
	if err != nil {
		return nil, repoError(err)
	}
	return connect.NewResponse(&rafikiv1.SetRepoResponse{Row: toProtoRepoRow(row)}), nil
}

func (s *Server) RemoveRepo(
	ctx context.Context, req *connect.Request[rafikiv1.RemoveRepoRequest],
) (*connect.Response[rafikiv1.RemoveRepoResponse], error) {
	m, err := s.repoManager()
	if err != nil {
		return nil, err
	}
	if err := validateRepoName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	if err := m.RemoveRepo(ctx, req.Msg.GetName()); err != nil {
		return nil, repoError(err)
	}
	return connect.NewResponse(&rafikiv1.RemoveRepoResponse{}), nil
}
