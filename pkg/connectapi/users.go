// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// UserAdmin is the operator-side slice of the daemon behind the user
// create / update / list / remove and token RPCs. Create here NEVER
// mints an admin — admins come only from rafikid user create --admin's
// bootstrap path.
//
// Create's mintToken is tri-state: nil means the daemon decides (it mints
// iff OIDC login is NOT configured on it, so the user can authenticate at
// all); present, it is honoured as given.
type UserAdmin interface {
	Create(ctx context.Context, username, email string, mintToken *bool) (*rafikiv1.CreateUserResponse, error) // never admin; nil = daemon decides
	List(ctx context.Context, includeDeleted bool, limit int32) ([]*rafikiv1.UserRow, error)
	Remove(ctx context.Context, username string) error
	Update(ctx context.Context, username string, email *string) (*rafikiv1.UserRow, error)
	MintToken(ctx context.Context, username, name string, ttl time.Duration) (*rafikiv1.MintTokenResponse, error)
	ListTokens(ctx context.Context, username string, includeRevoked, allUsers bool) ([]*rafikiv1.TokenRow, error)
	RevokeToken(ctx context.Context, id string) (*rafikiv1.TokenRow, error)
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
	var cerr *ControllerError
	if !errors.As(err, &cerr) {
		slog.Error("connect: user-admin call failed", "error", err)
	}
	return ConnectErr(err)
}

// CreateUser serves the CreateUser RPC. Never mints an admin:
// admins come only from rafikid user create --admin.
func (s *Server) CreateUser(
	ctx context.Context,
	req *connect.Request[rafikiv1.CreateUserRequest],
) (*connect.Response[rafikiv1.CreateUserResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	// MintToken is read as the raw pointer, not GetMintToken(): nil and false
	// are different requests the backend resolves differently.
	resp, err := backend.Create(ctx, req.Msg.GetUsername(), req.Msg.GetEmail(), req.Msg.MintToken)
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(resp), nil
}

// UpdateUser serves the UpdateUser RPC: edit a user's email (the admin bit
// is not editable over Connect).
func (s *Server) UpdateUser(
	ctx context.Context,
	req *connect.Request[rafikiv1.UpdateUserRequest],
) (*connect.Response[rafikiv1.UpdateUserResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	row, err := backend.Update(ctx, req.Msg.GetUsername(), req.Msg.Email)
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(&rafikiv1.UpdateUserResponse{User: row}), nil
}

// MintToken serves the MintToken RPC. The plaintext token rides the response
// exactly once, like CreateUser's.
func (s *Server) MintToken(
	ctx context.Context,
	req *connect.Request[rafikiv1.MintTokenRequest],
) (*connect.Response[rafikiv1.MintTokenResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	if ttl := req.Msg.GetTtl(); ttl != nil {
		if err := ttl.CheckValid(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	if req.Msg.GetTtl().AsDuration() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ttl must not be negative"))
	}
	resp, err := backend.MintToken(ctx, req.Msg.GetUsername(), req.Msg.GetName(), req.Msg.GetTtl().AsDuration())
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(resp), nil
}

// ListTokens serves the ListTokens RPC: credentials only, never secrets.
func (s *Server) ListTokens(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListTokensRequest],
) (*connect.Response[rafikiv1.ListTokensResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	rows, err := backend.ListTokens(ctx, req.Msg.GetUsername(), req.Msg.GetIncludeRevoked(), req.Msg.GetAllUsers())
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(&rafikiv1.ListTokensResponse{Tokens: rows}), nil
}

// RevokeToken serves the RevokeToken RPC: tombstone one credential.
func (s *Server) RevokeToken(
	ctx context.Context,
	req *connect.Request[rafikiv1.RevokeTokenRequest],
) (*connect.Response[rafikiv1.RevokeTokenResponse], error) {
	backend, err := s.userAdminBackend()
	if err != nil {
		return nil, err
	}
	row, err := backend.RevokeToken(ctx, req.Msg.GetId())
	if err != nil {
		return nil, userAdminErr(err)
	}
	return connect.NewResponse(&rafikiv1.RevokeTokenResponse{Info: row}), nil
}

// ListUsers serves the ListUsers RPC. Tokens are never returned.
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

// RemoveUser serves the RemoveUser RPC: tombstone a user.
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
