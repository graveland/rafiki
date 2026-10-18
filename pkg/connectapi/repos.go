// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/repos"
)

// RepoRow is one registered repo as the manager returns it: the
// human label used as the `repo` argument everywhere, plus the url and ref
// it is registered with.
type RepoRow struct {
	Name string
	URL  string
	Ref  string
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

// RepoManager is the narrow slice of the daemon needed to manage the
// caller's own repos. The owner is resolved INSIDE the implementation
// from the request context — no method takes one, the same rule as
// PymoduleManager — and the anonymous unix-socket caller shares the daemon's
// unattributed bucket, the same owner its own children resolve to.
type RepoManager interface {
	AddRepo(ctx context.Context, name, url, ref string) (RepoRow, error)
	ListRepos(ctx context.Context) ([]RepoRow, error)
	RefreshRepo(ctx context.Context, name string) (scripts []RepoScript, packages []RepoPackage, venvReady bool, venvError string, err error)
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
			errors.New("pymodule repos backend not yet wired"))
	}
	return *p, nil
}

// repoError maps the store's sentinel errors onto Connect codes the
// same way pymoduleError does: ErrNotFound (an unknown or already-removed
// name) is NotFound, the reserved "local" name is InvalidArgument, and
// everything else is Internal — a store failure must not surface shaped
// like something the caller said wrong.
func repoError(err error) error {
	switch {
	case errors.Is(err, repos.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, repos.ErrReservedName):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, err)
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

func toProtoRepoRow(r RepoRow) *rafikiv1.RepoRow {
	return &rafikiv1.RepoRow{Name: r.Name, Url: r.URL, Ref: r.Ref}
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
	row, err := m.AddRepo(ctx, req.Msg.GetName(), req.Msg.GetUrl(), req.Msg.GetRef())
	if err != nil {
		return nil, repoError(err)
	}
	return connect.NewResponse(&rafikiv1.AddRepoResponse{Row: toProtoRepoRow(row)}), nil
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
	scripts, packages, venvReady, venvError, err := m.RefreshRepo(ctx, req.Msg.GetName())
	if err != nil {
		return nil, repoError(err)
	}
	out := &rafikiv1.RefreshRepoResponse{
		Name:      req.Msg.GetName(),
		Scripts:   make([]*rafikiv1.RepoScript, 0, len(scripts)),
		Packages:  make([]*rafikiv1.RepoPackage, 0, len(packages)),
		VenvReady: venvReady,
		VenvError: venvError,
	}
	for _, sc := range scripts {
		out.Scripts = append(out.Scripts, &rafikiv1.RepoScript{Name: sc.Name, Description: sc.Description})
	}
	for _, pk := range packages {
		out.Packages = append(out.Packages, &rafikiv1.RepoPackage{Name: pk.Name, Description: pk.Description})
	}
	return connect.NewResponse(out), nil
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
