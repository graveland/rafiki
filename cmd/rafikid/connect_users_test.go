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

// userAdminFakeStore is a users.Store whose Create/List/Delete and token
// behaviour the test drives directly — bootstrapStore (user_bootstrap_test.go)
// panics on List/Delete, which this adapter's List/Remove need to exercise,
// and fakeUserStore (proxy_observability_test.go) answers Authenticate by
// token rather than tracking Create/List/Delete calls.
type userAdminFakeStore struct {
	rows      []users.User
	createErr error
	removeErr error
	removed   []string
	listLimit int // the limit the store actually received

	setEmailUser string // last SetEmail (username, email)
	setEmailMail string
	setEmailErr  error

	mintedUser   string // last MintToken (userID, request)
	mintedReq    users.NewToken
	mintErr      error
	mintedTokens []users.Token // rows the fake hands out for minted tokens

	tokens         []users.Token // ListTokens' answer
	listTokUserID  string        // the userID ListTokens received
	listTokRevoked bool
	getTok         users.Token
	getTokErr      error
	revokeErr      error
	revokedIDs     []string

	lookupErr error
}

func (f *userAdminFakeStore) Create(_ context.Context, u users.NewUser) (users.User, string, error) {
	if f.createErr != nil {
		return users.User{}, "", f.createErr
	}
	created := users.User{ID: "u_" + u.Username, Username: u.Username, Email: u.Email, IsAdmin: u.IsAdmin, CreatedAt: time.Unix(100, 0).UTC()}
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

func (f *userAdminFakeStore) LookupUsername(_ context.Context, username string) (string, error) {
	if f.lookupErr != nil {
		return "", f.lookupErr
	}
	return "id_" + username, nil
}

func (f *userAdminFakeStore) SetEmail(_ context.Context, username, email string) (users.User, error) {
	f.setEmailUser, f.setEmailMail = username, email
	if f.setEmailErr != nil {
		return users.User{}, f.setEmailErr
	}
	return users.User{ID: "id_" + username, Username: username, Email: email, CreatedAt: time.Unix(100, 0).UTC()}, nil
}

func (f *userAdminFakeStore) MintToken(_ context.Context, userID string, t users.NewToken) (users.Token, string, error) {
	f.mintedUser, f.mintedReq = userID, t
	if f.mintErr != nil {
		return users.Token{}, "", f.mintErr
	}
	tok := users.Token{ID: "tok_1", UserID: userID, Username: "owner-" + userID, Name: t.Name, Origin: t.Origin, CreatedAt: time.Unix(200, 0).UTC()}
	if t.TTL > 0 {
		exp := time.Unix(200, 0).UTC().Add(t.TTL)
		tok.ExpiresAt = &exp
	}
	f.mintedTokens = append(f.mintedTokens, tok)
	return tok, "rfk_minted", nil
}

func (f *userAdminFakeStore) ListTokens(_ context.Context, userID string, includeRevoked bool) ([]users.Token, error) {
	f.listTokUserID, f.listTokRevoked = userID, includeRevoked
	return f.tokens, nil
}

func (f *userAdminFakeStore) GetToken(_ context.Context, id string) (users.Token, error) {
	if f.getTokErr != nil {
		return users.Token{}, f.getTokErr
	}
	return f.getTok, nil
}

func (f *userAdminFakeStore) RevokeToken(_ context.Context, id string) (users.Token, error) {
	if f.revokeErr != nil {
		return users.Token{}, f.revokeErr
	}
	f.revokedIDs = append(f.revokedIDs, id)
	revoked := time.Unix(300, 0).UTC()
	f.getTok.RevokedAt = &revoked
	return f.getTok, nil
}

// adminCtx is an admin user credential's context; userCtx a non-admin's.
func adminCtx() context.Context {
	return server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "root", Via: server.ProvenanceUser, IsAdmin: true})
}

func plainUserCtx(userID, username string) context.Context {
	return server.WithIdentity(context.Background(),
		&server.Identity{UserID: userID, Username: username, Via: server.ProvenanceUser})
}

// TestUserRPCsRefuseNonAdmin pins who may NOT administer users: a non-admin
// user credential, a per-child token, and a child-attributed identity (even
// one whose owner is admin) — none may reach the store.
func TestUserRPCsRefuseNonAdmin(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   *server.Identity
	}{
		{"non-admin user", &server.Identity{UserID: "u2", Username: "alice", Via: server.ProvenanceUser}},
		{"child token", &server.Identity{UserID: "u1", Via: server.ProvenanceChildToken, ChildID: "c1", IsAdmin: true}},
		{"child attributed to admin", &server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed, IsAdmin: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := server.WithIdentity(context.Background(), tc.id)
			st := &userAdminFakeStore{}
			a := connectUserAdmin{c: &Controller{users: st}}

			if _, err := a.Create(ctx, "alice", "", nil); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("Create code %v, want PermissionDenied", connect.CodeOf(err))
			}
			if _, err := a.List(ctx, false, 0); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("List code %v, want PermissionDenied", connect.CodeOf(err))
			}
			err := a.Remove(ctx, "alice")
			assert.NewCollecting(t).Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Remove code")
			// Update is admin authority like Create; the token verbs have their
			// own finer rules tested below.
			email := "a@x.dev"
			if _, err := a.Update(ctx, "alice", &email); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("Update code %v, want PermissionDenied", connect.CodeOf(err))
			}
			if len(st.rows) != 0 || len(st.removed) != 0 || st.setEmailUser != "" {
				t.Errorf("a refused caller reached the store: rows=%v removed=%v email=%q",
					st.rows, st.removed, st.setEmailUser)
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

			resp, err := a.Create(ctx, "alice", "", nil)
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
// even when the caller administering it IS an admin: the adapter passes no
// admin bit of its own.
func TestCreateUserNeverMintsAdmin(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true})
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}

	_, err := a.Create(ctx, "bob", "", nil)
	ck.NoError(err, "Create")
	ck.Len(st.rows, 1, "rows")
	if st.rows[0].IsAdmin {
		t.Errorf("Create minted an admin: %+v", st.rows[0])
	}
}

// TestCreateMintDecision pins the tri-state mint decision on every branch:
// mint_token absent → mint iff OIDC login is NOT configured (reason "oidc
// not configured"); present → honoured as given, reason "requested" when it
// mints. login_configured rides the response either way, so the client can
// word its hint.
func TestCreateMintDecision(t *testing.T) {
	configured := func() bool { return true }
	unconfigured := func() bool { return false }
	mintTrue, mintFalse := true, false

	for _, tc := range []struct {
		name            string
		mintToken       *bool
		loginConfigured func() bool
		wantMint        bool
		wantReason      string
		wantConfigured  bool
	}{
		{"absent + unconfigured mints", nil, unconfigured, true, "oidc not configured", false},
		{"absent + configured does not mint", nil, configured, false, "", true},
		{"explicit true mints even when configured", &mintTrue, configured, true, "requested", true},
		{"explicit false never mints even when unconfigured", &mintFalse, unconfigured, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			st := &userAdminFakeStore{}
			a := connectUserAdmin{c: &Controller{users: st}, loginConfigured: tc.loginConfigured}
			resp, err := a.Create(adminCtx(), "alice", "", tc.mintToken)
			ck.Require().NoError(err, "Create")
			ck.Eq(tc.wantReason, resp.GetTokenReason(), "token_reason")
			ck.Eq(tc.wantConfigured, resp.GetLoginConfigured(), "login_configured")
			if resp.GetToken() != "" != tc.wantMint {
				t.Fatalf("minted = %v, want %v (token %q)", resp.GetToken() == "", tc.wantMint, resp.GetToken())
			}
		})
	}

	// A nil loginConfigured func counts as "not configured".
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}
	resp, err := a.Create(adminCtx(), "alice", "", nil)
	assert.NewAborting(t).NoError(err, "Create with nil loginConfigured")
	if resp.GetToken() == "" || resp.GetTokenReason() != "oidc not configured" {
		t.Fatalf("nil loginConfigured = %+v, want minted with the oidc reason", resp)
	}
}

// TestCreateForwardsEmail pins that Create forwards the email to the store
// verbatim — normalization (trim, lowercase) is the store's job, tested in
// pkg/users.
func TestCreateForwardsEmail(t *testing.T) {
	ck := assert.NewAborting(t)
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}
	_, err := a.Create(adminCtx(), "alice", "  Alice@X.dev ", nil)
	ck.Require().NoError(err, "Create with email")
	ck.Eq("  Alice@X.dev ", st.rows[0].Email, "store saw the email verbatim")
}

// TestCreateUserMapsSentinels pins the adapter's mapping of the user store's
// caller-error sentinels onto the framed texts (pkg/control/dispatch.go
// userCreate): a taken name and a malformed name are answers on the wire, not
// redacted internal errors. Each mapped error is already coded, so
// userAdminErr (pkg/connectapi/users.go) passes it through untouched — no
// slog, no redaction. An unmapped store error keeps the raw pass-through that
// userAdminErr logs and redacts.
func TestCreateUserMapsSentinels(t *testing.T) {
	for _, tc := range []struct {
		name      string
		email     string
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
		{
			name:      "taken email",
			email:     "alice@x.dev",
			createErr: fmt.Errorf("usersdb: %w", users.ErrEmailTaken),
			wantCode:  connect.CodeInvalidArgument,
			wantMsg:   "email alice@x.dev is already taken",
		},
		{
			name:      "invalid email",
			email:     "alice@x.dev",
			createErr: fmt.Errorf("%w: must contain an @", users.ErrInvalidEmail),
			wantCode:  connect.CodeInvalidArgument,
			wantMsg:   "invalid email: must contain an @",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			st := &userAdminFakeStore{createErr: tc.createErr}
			a := connectUserAdmin{c: &Controller{users: st}}
			_, err := a.Create(adminCtx(), "alice", tc.email, nil)
			ck.Require().Eq(tc.wantCode, connect.CodeOf(err), "code")
			ck.Eq(tc.wantMsg, connectErrMsg(t, err), "message")
		})
	}
	// Everything else keeps the current path: the error passes through UNCoded
	// (CodeOf reports Unknown) so userAdminErr is the one that logs the cause
	// and redacts the wire text.
	st := &userAdminFakeStore{createErr: errors.New("pgx: dial tcp db.internal:5432")}
	a := connectUserAdmin{c: &Controller{users: st}}
	if _, err := a.Create(adminCtx(), "alice", "", nil); err == nil || connect.CodeOf(err) != connect.CodeUnknown {
		t.Errorf("unmapped store error = %v, want it to pass through uncoded", err)
	}
}

// TestUpdateRefusesNilEmail pins the pre-store guard: a request with no
// optional email set is a client mistake, refused before the store is asked.
func TestUpdateRefusesNilEmail(t *testing.T) {
	ck := assert.NewAborting(t)
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}
	_, err := a.Update(adminCtx(), "alice", nil)
	ck.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	ck.Eq("nothing to update", connectErrMsg(t, err), "message")
	ck.Empty(st.setEmailUser, "nil email reached the store")
}

// TestUpdateResolvesEmail pins the email→store forwarding, the wire row, and
// the sentinel mapping of SetEmail.
func TestUpdateResolvesEmail(t *testing.T) {
	ck := assert.NewAborting(t)
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}
	email := "Alice@X.dev"
	row, err := a.Update(adminCtx(), "alice", &email)
	ck.Require().NoError(err, "Update")
	ck.Eq("alice", st.setEmailUser, "store saw username")
	ck.Eq("Alice@X.dev", st.setEmailMail, "store saw email verbatim (the store normalizes)")
	ck.Eq("alice", row.GetUsername(), "row username")
	ck.Eq("Alice@X.dev", row.GetEmail(), "row email")

	st2 := &userAdminFakeStore{setEmailErr: fmt.Errorf("usersdb: %w", users.ErrEmailTaken)}
	a2 := connectUserAdmin{c: &Controller{users: st2}}
	_, err = a2.Update(adminCtx(), "alice", &email)
	ck.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ErrEmailTaken code")
	ck.Eq("email is already taken", connectErrMsg(t, err), "ErrEmailTaken message")

	st3 := &userAdminFakeStore{setEmailErr: fmt.Errorf("usersdb: %w", users.ErrNotFound)}
	a3 := connectUserAdmin{c: &Controller{users: st3}}
	_, err = a3.Update(adminCtx(), "ghost", &email)
	ck.Require().Eq(connect.CodeNotFound, connect.CodeOf(err), "ErrNotFound code")
	ck.Eq("no active user named ghost", connectErrMsg(t, err), "ErrNotFound message")
}

// TestMintTokenAuthority pins the token verbs' authority rules: a user mints
// for itself (empty username = the caller), cannot mint for another user
// without admin, an admin can, and a nil identity must name a user.
func TestMintTokenAuthority(t *testing.T) {
	t.Run("user mints for self", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		resp, err := a.MintToken(plainUserCtx("u1", "alice"), "", "ci", 3600)
		ck.Require().NoError(err, "MintToken with empty username")
		ck.Eq("u1", st.mintedUser, "resolved target = the caller")
		ck.Eq(users.OriginService, st.mintedReq.Origin, "origin")
		ck.Eq(time.Hour, st.mintedReq.TTL, "ttl")
		ck.Eq("rfk_minted", resp.GetToken(), "plaintext")
		ck.Eq("tok_1", resp.GetInfo().GetId(), "info row")

		// Naming itself resolves to itself too, without a store lookup.
		if _, err := a.MintToken(plainUserCtx("u1", "alice"), "alice", "ci", 0); err != nil {
			t.Fatalf("MintToken with own name: %v", err)
		}
		ck.Eq("u1", st.mintedUser, "own name resolved to own id")
	})
	t.Run("user cannot mint for another", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.MintToken(plainUserCtx("u1", "alice"), "bob", "ci", 0)
		ck.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		ck.Empty(st.mintedUser, "a refused caller reached MintToken")
	})
	t.Run("admin mints for another", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.MintToken(adminCtx(), "bob", "ci", 0)
		ck.Require().NoError(err, "admin MintToken for another")
		ck.Eq("id_bob", st.mintedUser, "admin's target resolved through the store")
	})
	t.Run("nil identity without username refused", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.MintToken(context.Background(), "", "ci", 0)
		ck.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
		ck.StrContains(connectErrMsg(t, err), "--user is required", "message")
		ck.Empty(st.mintedUser, "the store was reached without a target")
	})
	t.Run("nil identity with username resolves through the store", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.MintToken(context.Background(), "bob", "ci", 0)
		ck.Require().NoError(err, "UDS MintToken for a named user")
		ck.Eq("id_bob", st.mintedUser, "target")
	})
	t.Run("unknown username is not found", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{lookupErr: fmt.Errorf("usersdb: %w", users.ErrNotFound)}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.MintToken(adminCtx(), "ghost", "ci", 0)
		ck.Require().Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
		ck.Eq("no active user named ghost", connectErrMsg(t, err), "message")
	})
	t.Run("negative ttl refused", func(t *testing.T) {
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.MintToken(adminCtx(), "", "ci", -1)
		assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "negative ttl code")
	})
	t.Run("child credential refused", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{}
		a := connectUserAdmin{c: &Controller{users: st}}
		ctx := server.WithIdentity(context.Background(),
			&server.Identity{UserID: "u1", Via: server.ProvenanceChildToken, ChildID: "c1"})
		_, err := a.MintToken(ctx, "", "ci", 0)
		ck.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		_, err = a.MintToken(ctx, "bob", "ci", 0)
		ck.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code with username")
		ck.Empty(st.mintedUser, "a child credential reached MintToken")
	})
}

// TestListTokensAuthority pins the list rules: self by default, all_users is
// admin (or nil identity) only, and rows carry the origin/revocation shape.
func TestListTokensAuthority(t *testing.T) {
	revoked := time.Unix(300, 0).UTC()
	expires := time.Unix(400, 0).UTC()
	rows := []users.Token{
		{ID: "tok_1", UserID: "u1", Username: "alice", Name: "ci", Origin: users.OriginService, CreatedAt: time.Unix(200, 0).UTC()},
		{ID: "tok_2", UserID: "u1", Username: "alice", Name: "oidc", Origin: users.OriginOIDC, CreatedAt: time.Unix(200, 0).UTC(), ExpiresAt: &expires, RevokedAt: &revoked},
	}

	t.Run("self by default", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{tokens: rows}
		a := connectUserAdmin{c: &Controller{users: st}}
		out, err := a.ListTokens(plainUserCtx("u1", "alice"), "", false, false)
		ck.Require().NoError(err, "ListTokens")
		ck.Eq("u1", st.listTokUserID, "target")
		ck.Eq(2, len(out), "rows")
		ck.Eq("service", out[0].GetOrigin(), "origin")
		ck.Nil(out[0].RevokedAtUnix, "an active token has no revoked_at")
		ck.NotNil(out[1].RevokedAtUnix, "a revoked token carries its timestamp")
		ck.NotNil(out[1].ExpiresAtUnix, "an expiring token carries its expiry")
		ck.Eq("oidc", out[1].GetOrigin(), "origin")
	})
	t.Run("all_users refused to non-admin", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{tokens: rows}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.ListTokens(plainUserCtx("u1", "alice"), "", false, true)
		ck.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		ck.Eq("", st.listTokUserID, "a refused caller reached ListTokens with a target")
	})
	t.Run("all_users admitted to admin and nil identity", func(t *testing.T) {
		for name, ctx := range map[string]context.Context{
			"admin":         adminCtx(),
			"anonymous UDS": context.Background(),
		} {
			t.Run(name, func(t *testing.T) {
				st := &userAdminFakeStore{tokens: rows}
				a := connectUserAdmin{c: &Controller{users: st}}
				if _, err := a.ListTokens(ctx, "", false, true); err != nil {
					t.Fatalf("ListTokens all_users refused: %v", err)
				}
				if st.listTokUserID != "" {
					t.Fatalf("all_users passed userID %q, want empty", st.listTokUserID)
				}
			})
		}
	})
}

// TestRevokeTokenAuthority pins the revoke rules: unknown id is NotFound, a
// non-admin cannot revoke another user's token, a user CAN revoke its own,
// and admin/nil identity can revoke anything.
func TestRevokeTokenAuthority(t *testing.T) {
	t.Run("unknown token not found", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{getTokErr: fmt.Errorf("usersdb: %w", users.ErrNotFound)}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.RevokeToken(adminCtx(), "tok_x")
		ck.Require().Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
		ck.Eq("no such token", connectErrMsg(t, err), "message")
		ck.Empty(st.revokedIDs, "an unknown token reached RevokeToken")
	})
	t.Run("non-admin cannot revoke another's token", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{getTok: users.Token{ID: "tok_1", UserID: "u_other"}}
		a := connectUserAdmin{c: &Controller{users: st}}
		_, err := a.RevokeToken(plainUserCtx("u1", "alice"), "tok_1")
		ck.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		ck.Empty(st.revokedIDs, "a foreign token reached RevokeToken")
	})
	t.Run("user revokes own token", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{getTok: users.Token{ID: "tok_1", UserID: "u1"}}
		a := connectUserAdmin{c: &Controller{users: st}}
		row, err := a.RevokeToken(plainUserCtx("u1", "alice"), "tok_1")
		ck.Require().NoError(err, "RevokeToken own")
		if len(st.revokedIDs) != 1 || st.revokedIDs[0] != "tok_1" {
			t.Fatalf("revoked = %v, want [tok_1]", st.revokedIDs)
		}
		ck.NotNil(row.RevokedAtUnix, "revoked_at on the row")
	})
	t.Run("admin and nil identity revoke anything", func(t *testing.T) {
		for name, ctx := range map[string]context.Context{
			"admin":         adminCtx(),
			"anonymous UDS": context.Background(),
		} {
			t.Run(name, func(t *testing.T) {
				st := &userAdminFakeStore{getTok: users.Token{ID: "tok_1", UserID: "u_other"}}
				a := connectUserAdmin{c: &Controller{users: st}}
				if _, err := a.RevokeToken(ctx, "tok_1"); err != nil {
					t.Fatalf("RevokeToken foreign refused: %v", err)
				}
			})
		}
	})
	t.Run("child credential refused", func(t *testing.T) {
		ck := assert.NewAborting(t)
		st := &userAdminFakeStore{getTok: users.Token{ID: "tok_1", UserID: "u1"}}
		a := connectUserAdmin{c: &Controller{users: st}}
		ctx := server.WithIdentity(context.Background(),
			&server.Identity{UserID: "u1", Via: server.ProvenanceChildToken, ChildID: "c1"})
		_, err := a.RevokeToken(ctx, "tok_1")
		ck.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		ck.Empty(st.revokedIDs, "a child credential reached RevokeToken")
	})
}

// connectErrMsg returns the wire message of an error the adapter coded, or
// fails the test when the error is not a coded *connect.Error.
func connectErrMsg(t *testing.T, err error) string {
	t.Helper()
	var ce *connect.Error
	assert.NewAborting(t).True(errors.As(err, &ce), "not a coded connect error: %v", err)
	return ce.Message()
}

// TestUserListClampsLimit pins the framed clamp (dispatch.go userList): a
// limit ≤0 or above maxUserListLimit is clamped to the cap before the store
// sees it — never rejected, never passed through raw.
func TestUserListClampsLimit(t *testing.T) {
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
			_, err := a.List(adminCtx(), false, tc.limit)
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
	st := &userAdminFakeStore{}
	a := connectUserAdmin{c: &Controller{users: st}}
	err := a.Remove(adminCtx(), "")
	ck.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	ck.Eq("username is required", connectErrMsg(t, err), "message")
	ck.Empty(st.removed, "empty username reached the store: removed=")
}

// TestUserRemoveMapsNotFound pins the framed not-found text (dispatch.go
// userRm): a miss is an answer naming the requested user, not a redacted
// internal error, and composed from the requested name like framed does.
func TestUserRemoveMapsNotFound(t *testing.T) {
	ck := assert.NewCollecting(t)
	st := &userAdminFakeStore{removeErr: fmt.Errorf("usersdb: %w", users.ErrNotFound)}
	a := connectUserAdmin{c: &Controller{users: st}}
	err := a.Remove(adminCtx(), "alice")
	ck.Require().Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
	ck.Eq("no active user named alice", connectErrMsg(t, err), "message")
	ck.Empty(st.removed, "failed removal recorded: removed=")
}

// TestCreateUserNoStore proves a daemon with no database surfaces as an
// error rather than a silent success, for an admin-admitted caller.
func TestCreateUserNoStore(t *testing.T) {
	a := connectUserAdmin{c: &Controller{}}
	_, err := a.Create(context.Background(), "alice", "", nil)
	assert.NewAborting(t).Error(err, "Create succeeded with no user store configured")
}
