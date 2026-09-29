// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// userAdminFakeStore is a users.Store whose Create/List/Delete behaviour the
// test drives directly — bootstrapStore (user_bootstrap_test.go) panics on
// List/Delete, which this adapter's List/Remove need to exercise, and
// fakeUserStore (proxy_observability_test.go) answers Authenticate by token
// rather than tracking Create/List/Delete calls.
type userAdminFakeStore struct {
	rows      []users.User
	createErr error
	removeErr error
	removed   []string
	listLimit int // the limit the store actually received
}

func (f *userAdminFakeStore) Create(_ context.Context, u users.NewUser) (users.User, string, error) {
	if f.createErr != nil {
		return users.User{}, "", f.createErr
	}
	created := users.User{ID: "u_" + u.Username, Username: u.Username, IsAdmin: u.IsAdmin, CreatedAt: time.Unix(100, 0).UTC()}
	f.rows = append(f.rows, created)
	if !u.MintToken {
		return created, "", nil
	}
	return created, "rfk_tok", nil
}

func (f *userAdminFakeStore) List(_ context.Context, _ bool, limit int) ([]users.User, error) {
	f.listLimit = limit
	return f.rows, nil
}

func (f *userAdminFakeStore) Delete(_ context.Context, username string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, username)
	return nil
}

func (f *userAdminFakeStore) Authenticate(context.Context, string) (users.Identity, error) {
	panic("unused")
}
func (f *userAdminFakeStore) CountActive(context.Context) (int, error) { panic("unused") }
func (f *userAdminFakeStore) LookupUsername(context.Context, string) (string, error) {
	panic("unused")
}
func (f *userAdminFakeStore) SetEmail(context.Context, string, string) (users.User, error) {
	panic("unused")
}
func (f *userAdminFakeStore) MintToken(context.Context, string, users.NewToken) (users.Token, string, error) {
	panic("unused")
}
func (f *userAdminFakeStore) ListTokens(context.Context, string, bool) ([]users.Token, error) {
	panic("unused")
}
func (f *userAdminFakeStore) GetToken(context.Context, string) (users.Token, error) {
	panic("unused")
}
func (f *userAdminFakeStore) RevokeToken(context.Context, string) (users.Token, error) {
	panic("unused")
}

// TestUserRPCsRefuseNonAdmin pins who may NOT administer users: a non-admin
// user credential, a per-child token, and a child-attributed identity (even
// one whose owner is admin) — none may reach the store.
func TestUserRPCsRefuseNonAdmin(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   *server.Identity
	}{
		{"non-admin user", &server.Identity{UserID: "u2", Via: server.ProvenanceUser}},
		{"child token", &server.Identity{UserID: "u1", Via: server.ProvenanceChildToken, ChildID: "c1", IsAdmin: true}},
		{"child attributed to admin", &server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed, IsAdmin: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := server.WithIdentity(context.Background(), tc.id)
			st := &userAdminFakeStore{}
			a := connectUserAdmin{c: &Controller{users: st}}

			if _, err := a.Create(ctx, "alice"); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("Create code %v, want PermissionDenied", connect.CodeOf(err))
			}
			if _, err := a.List(ctx, false, 0); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("List code %v, want PermissionDenied", connect.CodeOf(err))
			}
			err := a.Remove(ctx, "alice")
			assert.NewCollecting(t).Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Remove code")
			if len(st.rows) != 0 || len(st.removed) != 0 {
				t.Errorf("a refused caller reached the store: rows=%v removed=%v", st.rows, st.removed)
			}
		})
	}
}

// TestUserRPCsAdmitNilAndAdmin pins who MAY administer users: the anonymous
// local socket (no identity presented at all) and an admin user credential.
func TestUserRPCsAdmitNilAndAdmin(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   *server.Identity
	}{
		{"anonymous UDS", nil},
		{"admin user", &server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			ctx := context.Background()
			if tc.id != nil {
				ctx = server.WithIdentity(ctx, tc.id)
			}
			st := &userAdminFakeStore{}
			a := connectUserAdmin{c: &Controller{users: st}}

			resp, err := a.Create(ctx, "alice")
			ck.Require().NoError(err, "Create refused")
			ck.False(resp.GetUsername() != "alice" || resp.GetToken() != "rfk_tok", "Create response = %+v", resp)

			if _, err := a.List(ctx, false, 0); err != nil {
				t.Errorf("List refused: %v", err)
			}
			ck.NoError(a.Remove(ctx, "alice"), "Remove refused")
			if len(st.rows) != 1 || len(st.removed) != 1 {
				t.Fatalf("rows=%v removed=%v, want one of each", st.rows, st.removed)
			}
		})
	}
}

// TestCreateUserNeverMintsAdmin proves Create can never produce an admin,
// even when the caller administering it IS an admin: Controller.UserCreate
// never infers admin from an
// empty user table, and this adapter passes no admin bit of its own.
func TestCreateUserNeverMintsAdmin(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := server.WithIdentity(context.Background(), &server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}

	_, err := a.Create(ctx, "bob")
	ck.NoError(err, "Create")
	ck.Len(st.rows, 1, "rows")
	if st.rows[0].IsAdmin {
		t.Errorf("Create minted an admin: %+v", st.rows[0])
	}
}

// connectErrMsg returns the wire message of an error the adapter coded, or
// fails the test when the error is not a coded *connect.Error.
func connectErrMsg(t *testing.T, err error) string {
	t.Helper()
	var ce *connect.Error
	assert.NewAborting(t).True(errors.As(err, &ce), "not a coded connect error: %v", err)
	return ce.Message()
}

// TestCreateUserMapsSentinels pins the adapter's mapping of the user store's
// caller-error sentinels onto the framed texts (pkg/control/dispatch.go
// userCreate): a taken name and a malformed name are answers on the wire, not
// redacted internal errors. Each mapped error is already coded, so
// userAdminErr (pkg/connectapi/users.go) passes it through untouched — no
// slog, no redaction. An unmapped store error keeps the raw pass-through that
// userAdminErr logs and redacts.
func TestCreateUserMapsSentinels(t *testing.T) {
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	for _, tc := range []struct {
		name      string
		createErr error
		wantCode  connect.Code
		wantMsg   string
	}{
		{
			name:      "taken name",
			createErr: fmt.Errorf("usersdb: %w", users.ErrUsernameTaken),
			wantCode:  connect.CodeInvalidArgument,
			wantMsg:   "username alice is already taken",
		},
		{
			name:      "invalid username",
			createErr: fmt.Errorf("%w: must not be empty or whitespace", users.ErrInvalidUsername),
			wantCode:  connect.CodeInvalidArgument,
			wantMsg:   "invalid username: must not be empty or whitespace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			st := &userAdminFakeStore{createErr: tc.createErr}
			a := connectUserAdmin{c: &Controller{users: st}}
			_, err := a.Create(ctx, "alice")
			ck.Require().Eq(tc.wantCode, connect.CodeOf(err), "code")
			ck.Eq(tc.wantMsg, connectErrMsg(t, err), "message")
		})
	}
	// Everything else keeps the current path: the error passes through UNCoded
	// (CodeOf reports Unknown) so userAdminErr is the one that logs the cause
	// and redacts the wire text.
	st := &userAdminFakeStore{createErr: errors.New("pgx: dial tcp db.internal:5432")}
	a := connectUserAdmin{c: &Controller{users: st}}
	if _, err := a.Create(ctx, "alice"); err == nil || connect.CodeOf(err) != connect.CodeUnknown {
		t.Errorf("unmapped store error = %v, want it to pass through uncoded", err)
	}
}

// TestUserListClampsLimit pins the framed clamp (dispatch.go userList): a
// limit ≤0 or above maxUserListLimit is clamped to the cap before the store
// sees it — never rejected, never passed through raw.
func TestUserListClampsLimit(t *testing.T) {
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	for _, tc := range []struct {
		name  string
		limit int32
		want  int
	}{
		{"zero becomes the framed cap", 0, maxUserListLimit},
		{"negative becomes the framed cap", -5, maxUserListLimit},
		{"huge request clamped", 1 << 30, maxUserListLimit},
		{"just over the cap clamped", maxUserListLimit + 1, maxUserListLimit},
		{"exact cap passes through", maxUserListLimit, maxUserListLimit},
		{"ordinary value passes through", 25, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			st := &userAdminFakeStore{}
			a := connectUserAdmin{c: &Controller{users: st}}
			_, err := a.List(ctx, false, tc.limit)
			ck.Require().NoError(err, "List")
			ck.Eq(tc.want, st.listLimit, "store saw limit")
		})
	}
}

// TestUserRemoveRefusesEmptyUsername pins framed userRm's pre-store check
// (dispatch.go): an empty username is an invalid argument with framed's text,
// and the store is never asked.
func TestUserRemoveRefusesEmptyUsername(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}
	err := a.Remove(ctx, "")
	ck.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	ck.Eq("username is required", connectErrMsg(t, err), "message")
	ck.Empty(st.removed, "empty username reached the store: removed=")
}

// TestUserRemoveMapsNotFound pins the framed not-found text (dispatch.go
// userRm): a miss is an answer naming the requested user, not a redacted
// internal error, and composed from the requested name like framed does.
func TestUserRemoveMapsNotFound(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	st := &userAdminFakeStore{removeErr: fmt.Errorf("usersdb: %w", users.ErrNotFound)}
	a := connectUserAdmin{c: &Controller{users: st}}
	err := a.Remove(ctx, "alice")
	ck.Require().Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
	ck.Eq("no active user named alice", connectErrMsg(t, err), "message")
	ck.Empty(st.removed, "failed removal recorded: removed=")
}

// TestCreateUserNoStore proves a daemon with no database surfaces as an
// error rather than a silent success, for an admin-admitted caller.
func TestCreateUserNoStore(t *testing.T) {
	a := connectUserAdmin{c: &Controller{}}
	_, err := a.Create(context.Background(), "alice")
	assert.NewAborting(t).Error(err, "Create succeeded with no user store configured")
}
