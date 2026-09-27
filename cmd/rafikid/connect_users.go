// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
)

// maxUserListLimit bounds List: a limit ≤0 or above the cap is CLAMPED to the
// cap, never rejected and never passed through raw — so a 0 (which the store
// would otherwise default to 100) and a 2^30 get the same 500-row window.
const maxUserListLimit = 500

var _ connectapi.UserAdmin = connectUserAdmin{}

// connectUserAdmin adapts *Controller's identity verbs to
// connectapi.UserAdmin. Create/List/Remove here serve an operator managing
// OTHER users' accounts over Connect — a different surface from `rafikid user
// create`'s direct DSN access, which this type does not touch. Every method requires admin
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

// Create mints a non-admin user. Controller.UserCreate never infers admin
// from an empty user table, so this can never mint an admin regardless of
// caller.
func (a connectUserAdmin) Create(ctx context.Context, username string) (*rafikiv1.CreateUserResponse, error) {
	if err := requireUserAdmin(ctx); err != nil {
		return nil, err
	}
	data, err := a.c.UserCreate(ctx, username)
	if err != nil {
		// The store's caller-error sentinels are answers, not infrastructure
		// failures: they are already-coded Connect errors, so userAdminErr
		// (pkg/connectapi/users.go) passes
		// them through untouched — no slog, no redaction. Everything else keeps
		// the raw pass-through that userAdminErr logs and redacts.
		switch {
		case errors.Is(err, users.ErrUsernameTaken):
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("username %s is already taken", username))
		case errors.Is(err, users.ErrInvalidUsername):
			// pkg/users composes the reason after
			// the sentinel text ("must not be empty or whitespace", "longer than
			// 64 bytes"); forward that detail, which is authored text and leaks
			// nothing.
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid username: %s",
					strings.TrimPrefix(err.Error(), users.ErrInvalidUsername.Error()+": ")))
		}
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
	// Framed parity: dispatch.go's userList clamps ≤0 and above the cap to
	// maxUserListLimit before the store sees the request.
	if limit <= 0 || limit > maxUserListLimit {
		limit = maxUserListLimit
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
	// Framed userRm refuses an empty name before the store, with the same
	// text userCreate uses for it (dispatch.go).
	if username == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("username is required"))
	}
	if err := a.c.UserRm(ctx, username); err != nil {
		// A miss is an answer: the text composes from the REQUESTED name.
		if errors.Is(err, users.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound,
				fmt.Errorf("no active user named %s", username))
		}
		return err
	}
	return nil
}
