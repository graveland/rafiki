// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
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
}

func (f *userAdminFakeStore) Create(_ context.Context, username string, isAdmin bool) (users.User, string, error) {
	if f.createErr != nil {
		return users.User{}, "", f.createErr
	}
	u := users.User{ID: "u_" + username, Username: username, IsAdmin: isAdmin, CreatedAt: time.Unix(100, 0).UTC()}
	f.rows = append(f.rows, u)
	return u, "rfk_tok", nil
}

func (f *userAdminFakeStore) List(context.Context, bool, int) ([]users.User, error) {
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
			if err := a.Remove(ctx, "alice"); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("Remove code %v, want PermissionDenied", connect.CodeOf(err))
			}
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
			ctx := context.Background()
			if tc.id != nil {
				ctx = server.WithIdentity(ctx, tc.id)
			}
			st := &userAdminFakeStore{}
			a := connectUserAdmin{c: &Controller{users: st}}

			resp, err := a.Create(ctx, "alice")
			if err != nil {
				t.Fatalf("Create refused: %v", err)
			}
			if resp.GetUsername() != "alice" || resp.GetToken() != "rfk_tok" {
				t.Errorf("Create response = %+v", resp)
			}

			if _, err := a.List(ctx, false, 0); err != nil {
				t.Errorf("List refused: %v", err)
			}
			if err := a.Remove(ctx, "alice"); err != nil {
				t.Errorf("Remove refused: %v", err)
			}
			if len(st.rows) != 1 || len(st.removed) != 1 {
				t.Fatalf("rows=%v removed=%v, want one of each", st.rows, st.removed)
			}
		})
	}
}

// TestCreateUserNeverMintsAdmin proves Create can never produce an admin,
// even when the caller administering it IS an admin: Controller.UserCreate
// (unlike UserCreateBootstrap/UserCreateLocal) never infers admin from an
// empty user table, and this adapter passes no admin bit of its own.
func TestCreateUserNeverMintsAdmin(t *testing.T) {
	ctx := server.WithIdentity(context.Background(), &server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}

	if _, err := a.Create(ctx, "bob"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one", st.rows)
	}
	if st.rows[0].IsAdmin {
		t.Errorf("Create minted an admin: %+v", st.rows[0])
	}
}

// TestCreateUserNoStore proves a daemon with no database surfaces as an
// error rather than a silent success, for an admin-admitted caller.
func TestCreateUserNoStore(t *testing.T) {
	a := connectUserAdmin{c: &Controller{}}
	if _, err := a.Create(context.Background(), "alice"); err == nil {
		t.Fatal("Create succeeded with no user store configured")
	}
}
