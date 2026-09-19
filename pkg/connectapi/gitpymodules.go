// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
)

// GitSourceRow is one registered git source as the manager returns it: the
// human label used as the `repo` argument everywhere, plus the url and ref
// it is registered with.
type GitSourceRow struct {
	Name string
	URL  string
	Ref  string
}

// GitSourceScript and GitSourcePackage are one discovered entry of a git
// source's checkout, mirroring the executor protocol's messages of the same
// names — local structs rather than domain types, the same isolation
// PymoduleRow gives PymoduleManager from pkg/pymodules.
type GitSourceScript struct {
	Name        string
	Description string
}

type GitSourcePackage struct {
	Name        string
	Description string
}

// GitSourceManager is the narrow slice of the daemon needed to manage the
// caller's own git sources. The owner is resolved INSIDE the implementation
// from the request context — no method takes one, the same rule as
// PymoduleManager — and the anonymous unix-socket caller shares the daemon's
// unattributed bucket, the same owner its own children resolve to.
type GitSourceManager interface {
	AddGitSource(ctx context.Context, name, url, ref string) (GitSourceRow, error)
	ListGitSources(ctx context.Context) ([]GitSourceRow, error)
	RefreshGitSource(ctx context.Context, name string) (scripts []GitSourceScript, packages []GitSourcePackage, venvReady bool, venvError string, err error)
	RemoveGitSource(ctx context.Context, name string) error
}

// SetGitSourceManager attaches the git-source backend. Post-construction
// setter, same reason as SetPymoduleManager: the Controller is built after
// this Server. A nil manager is refused rather than stored: storing &m for
// a nil interface would defeat gitSourceManager's Unavailable path and
// nil-panic the first handler call instead.
func (s *Server) SetGitSourceManager(m GitSourceManager) {
	if m == nil {
		return
	}
	s.gitSources.Store(&m)
}

func (s *Server) gitSourceManager() (GitSourceManager, error) {
	p := s.gitSources.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("pymodule git sources backend not yet wired"))
	}
	return *p, nil
}

// gitSourceError maps the store's sentinel errors onto Connect codes the
// same way pymoduleError does: ErrNotFound (an unknown or already-removed
// name) is NotFound, the reserved "local" name is InvalidArgument, and
// everything else is Internal — a store failure must not surface shaped
// like something the caller said wrong.
func gitSourceError(err error) error {
	switch {
	case errors.Is(err, gitpymodules.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, gitpymodules.ErrReservedName):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// validateGitSourceName applies the git-source name rule client-side of the
// store: "local" is reserved for the blob store, everything else must be a
// bare Python identifier — the name becomes the `repo` argument's value
// across the whole pymodule tool surface, so the same rule that guards a
// pymodule name guards it.
func validateGitSourceName(name string) error {
	if name == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if err := gitpymodules.ValidateName(name); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return nil
}

func toProtoGitSourceRow(r GitSourceRow) *rafikiv1.GitSourceRow {
	return &rafikiv1.GitSourceRow{Name: r.Name, Url: r.URL, Ref: r.Ref}
}

func (s *Server) AddPymoduleGitSource(
	ctx context.Context, req *connect.Request[rafikiv1.AddPymoduleGitSourceRequest],
) (*connect.Response[rafikiv1.AddPymoduleGitSourceResponse], error) {
	m, err := s.gitSourceManager()
	if err != nil {
		return nil, err
	}
	if err := validateGitSourceName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	if req.Msg.GetUrl() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("url is required"))
	}
	row, err := m.AddGitSource(ctx, req.Msg.GetName(), req.Msg.GetUrl(), req.Msg.GetRef())
	if err != nil {
		return nil, gitSourceError(err)
	}
	return connect.NewResponse(&rafikiv1.AddPymoduleGitSourceResponse{Row: toProtoGitSourceRow(row)}), nil
}

func (s *Server) ListPymoduleGitSources(
	ctx context.Context, req *connect.Request[rafikiv1.ListPymoduleGitSourcesRequest],
) (*connect.Response[rafikiv1.ListPymoduleGitSourcesResponse], error) {
	m, err := s.gitSourceManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListGitSources(ctx)
	if err != nil {
		return nil, gitSourceError(err)
	}
	out := make([]*rafikiv1.GitSourceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoGitSourceRow(r))
	}
	return connect.NewResponse(&rafikiv1.ListPymoduleGitSourcesResponse{Rows: out}), nil
}

func (s *Server) RefreshPymoduleGitSource(
	ctx context.Context, req *connect.Request[rafikiv1.RefreshPymoduleGitSourceRequest],
) (*connect.Response[rafikiv1.RefreshPymoduleGitSourceResponse], error) {
	m, err := s.gitSourceManager()
	if err != nil {
		return nil, err
	}
	if err := validateGitSourceName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	scripts, packages, venvReady, venvError, err := m.RefreshGitSource(ctx, req.Msg.GetName())
	if err != nil {
		return nil, gitSourceError(err)
	}
	out := &rafikiv1.RefreshPymoduleGitSourceResponse{
		Scripts:   make([]*rafikiv1.GitSourceScript, 0, len(scripts)),
		Packages:  make([]*rafikiv1.GitSourcePackage, 0, len(packages)),
		VenvReady: venvReady,
		VenvError: venvError,
	}
	for _, sc := range scripts {
		out.Scripts = append(out.Scripts, &rafikiv1.GitSourceScript{Name: sc.Name, Description: sc.Description})
	}
	for _, pk := range packages {
		out.Packages = append(out.Packages, &rafikiv1.GitSourcePackage{Name: pk.Name, Description: pk.Description})
	}
	return connect.NewResponse(out), nil
}

func (s *Server) RemovePymoduleGitSource(
	ctx context.Context, req *connect.Request[rafikiv1.RemovePymoduleGitSourceRequest],
) (*connect.Response[rafikiv1.RemovePymoduleGitSourceResponse], error) {
	m, err := s.gitSourceManager()
	if err != nil {
		return nil, err
	}
	if err := validateGitSourceName(req.Msg.GetName()); err != nil {
		return nil, err
	}
	if err := m.RemoveGitSource(ctx, req.Msg.GetName()); err != nil {
		return nil, gitSourceError(err)
	}
	return connect.NewResponse(&rafikiv1.RemovePymoduleGitSourceResponse{}), nil
}
