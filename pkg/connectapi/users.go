// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

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

// CreateUser serves the framed ctrl_user_create face. Never mints an admin:
// admins come only from rafikid user create --admin.
func (s *Server) CreateUser(
	ctx context.Context,
	req *connect.Request[rafikiv1.CreateUserRequest],
) (*connect.Response[rafikiv1.CreateUserResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("CreateUser: not yet implemented"))
}

// ListUsers serves the framed ctrl_user_list face. Tokens are never returned.
func (s *Server) ListUsers(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListUsersRequest],
) (*connect.Response[rafikiv1.ListUsersResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListUsers: not yet implemented"))
}

// RemoveUser serves the framed ctrl_user_rm face: tombstone a user.
func (s *Server) RemoveUser(
	ctx context.Context,
	req *connect.Request[rafikiv1.RemoveUserRequest],
) (*connect.Response[rafikiv1.RemoveUserResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("RemoveUser: not yet implemented"))
}
