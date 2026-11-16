// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ExecutorInstaller puts an executor on an ssh target through a launcher
// executor, and removes one it installed.
//
// The implementation is CALLER-BOUND: it resolves who is asking from the
// request context (the same rule PymoduleManager follows), so neither method
// takes an owner and no request field carries one. A nil identity (the unix
// socket) means the shared unattributed caller.
type ExecutorInstaller interface {
	Install(ctx context.Context, req *rafikiv1.InstallExecutorRequest) (*rafikiv1.ExecutorRow, error)
	Uninstall(ctx context.Context, ref string) error
}

// SetExecutorInstaller attaches the install orchestrator. Post-construction
// setter, same reason as SetSkillManager: the Controller is built after this
// Server. A nil installer is refused rather than stored: storing &m for a nil
// interface would defeat executorInstaller's Unavailable path and nil-panic
// the first handler call instead.
func (s *Server) SetExecutorInstaller(m ExecutorInstaller) {
	if m == nil {
		return
	}
	s.execInstaller.Store(&m)
}

func (s *Server) executorInstaller() (ExecutorInstaller, error) {
	p := s.execInstaller.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor installer not wired"))
	}
	return *p, nil
}

// InstallExecutor installs an executor on an ssh target through a launcher
// and answers the new row once it has connected.
//
// childScoped at the policy gate: a child may install through a launcher it
// can reach, and the installer bounds it — the launcher must be in its
// reachable set and the owner comes from the credential.
func (s *Server) InstallExecutor(
	ctx context.Context, req *connect.Request[rafikiv1.InstallExecutorRequest],
) (*connect.Response[rafikiv1.InstallExecutorResponse], error) {
	m, err := s.executorInstaller()
	if err != nil {
		return nil, err
	}
	row, err := m.Install(ctx, req.Msg)
	if err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.InstallExecutorResponse{Executor: row}), nil
}

// UninstallExecutor removes an executor this lineage installed. childScoped
// at the policy gate, and the installer refuses a child that is not the
// installer or one of its descendants.
func (s *Server) UninstallExecutor(
	ctx context.Context, req *connect.Request[rafikiv1.UninstallExecutorRequest],
) (*connect.Response[rafikiv1.UninstallExecutorResponse], error) {
	m, err := s.executorInstaller()
	if err != nil {
		return nil, err
	}
	if err := m.Uninstall(ctx, req.Msg.GetExecutor()); err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.UninstallExecutorResponse{}), nil
}
