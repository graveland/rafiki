// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// UserAdmin is the operator-side slice of the daemon behind the framed
// ctrl_user_create / ctrl_user_list / ctrl_user_rm verbs. Create here NEVER
// mints an admin — admins come only from rafikid user create --admin's
// bootstrap path.
type UserAdmin interface {
	Create(ctx context.Context, username string) (*rafikiv1.CreateUserResponse, error) // never admin
	List(ctx context.Context, includeDeleted bool, limit int32) ([]*rafikiv1.UserRow, error)
	Remove(ctx context.Context, username string) error
}

// SetUserAdmin attaches the user-admin backend. Post-construction setter for
// the same reason as SetSkillManager: the Controller is built after this
// Server. A nil backend is refused rather than stored, the same rule as
// SetSkillManager: storing &u for a nil interface would defeat the handler's
// Unavailable path and nil-panic the first handler call.
func (s *Server) SetUserAdmin(u UserAdmin) {
	if u == nil {
		return
	}
	s.userAdmin.Store(&u)
}

// userAdminBackend loads the wired backend, or Unavailable when Task 3.1's
// wiring (main.go, out of this task's scope) has not run yet.
func (s *Server) userAdminBackend() (UserAdmin, error) {
	p := s.userAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("user-admin backend not yet wired"))
	}
	return *p, nil
}

// userAdminErr passes an already-coded error through untouched — the admin
// gate's PermissionDenied refusal (cmd/rafikid connect_users.go, which runs
// before any of the three backend calls below) must reach the wire as
// permission_denied, not be redacted into internal, the same reasoning as
// conversations.go's queryError. Everything else goes through ConnectErr,
// whose redaction of a non-ControllerError is silent, so the cause is logged
// here first or it is lost — matching close.go and budget.go.
func userAdminErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	var cerr *control.ControllerError
	if !errors.As(err, &cerr) {
		slog.Error("connect: user-admin call failed", "error", err)
	}
	return ConnectErr(err)
}

// CreateUser serves the framed ctrl_user_create face. Never mints an admin:
// admins come only from rafikid user create --admin.
func (s *Server) CreateUser(
	ctx context.Context,
	req *connect.Request[rafikiv1.CreateUserRequest],
) (*connect.Response[rafikiv1.CreateUserResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	resp, err := backend.Create(ctx, req.Msg.GetUsername())
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(resp), nil
}

// ListUsers serves the framed ctrl_user_list face. Tokens are never returned.
func (s *Server) ListUsers(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListUsersRequest],
) (*connect.Response[rafikiv1.ListUsersResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	rows, err := backend.List(ctx, req.Msg.GetIncludeDeleted(), req.Msg.GetLimit())
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(&rafikiv1.ListUsersResponse{Users: rows}), nil
}

// RemoveUser serves the framed ctrl_user_rm face: tombstone a user.
func (s *Server) RemoveUser(
	ctx context.Context,
	req *connect.Request[rafikiv1.RemoveUserRequest],
) (*connect.Response[rafikiv1.RemoveUserResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	if err := backend.Remove(ctx, req.Msg.GetUsername()); err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(&rafikiv1.RemoveUserResponse{}), nil
}
