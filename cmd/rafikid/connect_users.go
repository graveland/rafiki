// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
)

var _ connectapi.UserAdmin = connectUserAdmin{}

// connectUserAdmin adapts *Controller's identity verbs to
// connectapi.UserAdmin. Create/List/Remove here serve an operator managing
// OTHER users' accounts over Connect — a different surface from the framed
// ctrl_user_create bootstrap path and from `rafikid user create`'s direct DSN
// access, neither of which this type touches. Every method requires admin
// authority BEFORE calling into the Controller, the same ordering
// requireBanAuthority enforces for provider bans (connect_providerbans.go).
type connectUserAdmin struct{ c *Controller }

// requireUserAdmin admits the anonymous local socket (no identity — the
// socket itself is the credential, same trust boundary requireBanAuthority
// honours) or an admin user credential; refuses everything else, including a
// non-admin user and any child-attributed identity.
func requireUserAdmin(ctx context.Context) error {
	id := server.IdentityFromContext(ctx)
	if id == nil || (id.IsUserCredential() && id.IsAdmin) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied,
		errors.New("user administration requires an admin user credential"))
}

// Create mints a non-admin user. Controller.UserCreate (unlike
// UserCreateBootstrap/UserCreateLocal) never infers admin from an empty user
// table, so this can never mint an admin regardless of caller.
func (a connectUserAdmin) Create(ctx context.Context, username string) (*rafikiv1.CreateUserResponse, error) {
	if err := requireUserAdmin(ctx); err != nil {
		return nil, err
	}
	data, err := a.c.UserCreate(ctx, username)
	if err != nil {
		return nil, err
	}
	createdAt, err := time.Parse(time.RFC3339, data.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rafikiv1.CreateUserResponse{
		Id:            data.ID,
		Username:      data.Username,
		Token:         data.Token,
		CreatedAtUnix: createdAt.Unix(),
	}, nil
}

// List returns every user row, tokens never included.
func (a connectUserAdmin) List(ctx context.Context, includeDeleted bool, limit int32) ([]*rafikiv1.UserRow, error) {
	if err := requireUserAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := a.c.UserList(ctx, includeDeleted, int(limit))
	if err != nil {
		return nil, err
	}
	out := make([]*rafikiv1.UserRow, 0, len(rows))
	for _, u := range rows {
		row := &rafikiv1.UserRow{
			Id:            u.ID,
			Username:      u.Username,
			IsAdmin:       u.IsAdmin,
			CreatedAtUnix: u.CreatedAt.Unix(),
		}
		if u.DeletedAt != nil {
			deletedAt := u.DeletedAt.Unix()
			row.DeletedAtUnix = &deletedAt
		}
		out = append(out, row)
	}
	return out, nil
}

// Remove tombstones a user.
func (a connectUserAdmin) Remove(ctx context.Context, username string) error {
	if err := requireUserAdmin(ctx); err != nil {
		return err
	}
	return a.c.UserRm(ctx, username)
}
