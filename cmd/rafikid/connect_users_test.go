// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/store"
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
	// Naming a user AND all_users is a contradictory request: refused before
	// any authority or store work, so it can never silently widen to the
	// all-users listing (rafiki-py gets the same answer over the same wire).
	t.Run("username with all_users is invalid", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		st := &userAdminFakeStore{tokens: rows}
		a := connectUserAdmin{c: &Controller{users: st}}
		for name, ctx := range map[string]context.Context{
			"admin":         adminCtx(),
			"anonymous UDS": context.Background(),
		} {
			_, err := a.ListTokens(ctx, "alice", false, true)
			ck.Require().Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s: code", name)
			ck.Eq("all_users and username are mutually exclusive", connectErrMsg(t, err), "%s: message", name)
		}
		ck.Eq("", st.listTokUserID, "the contradictory request reached ListTokens with a target")
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

// cutUserStore answers Authenticate, GetToken and RevokeToken for the
// revoke-cuts-stream tests: the SAME token authenticates the stream and is
// then revoked through the Connect surface, so one identity and one row cover
// both halves. Authenticate is faithful about the tombstones: a revoked token
// or a removed user is ErrNotFound, exactly like the real store's
// revoked_at/deleted_at join — the cache-purge tests lean on that to prove a
// re-opened request re-checks the store after revocation.
type cutUserStore struct {
	users.Store
	token string
	id    users.Identity
	rows  map[string]users.Token

	revoked   bool // RevokeToken was called
	deleted   bool // Delete (UserRm) was called
	authCalls int  // Authenticate invocations — a warm cache keeps this flat
}

func (s *cutUserStore) Authenticate(_ context.Context, token string) (users.Identity, error) {
	s.authCalls++
	if token != s.token || s.revoked || s.deleted {
		return users.Identity{}, users.ErrNotFound
	}
	return s.id, nil
}

// LookupUsername and Delete back Controller.UserRm's remove path.
func (s *cutUserStore) LookupUsername(_ context.Context, username string) (string, error) {
	if s.deleted || username != s.id.Username {
		return "", users.ErrNotFound
	}
	return s.id.UserID, nil
}

func (s *cutUserStore) Delete(_ context.Context, username string) error {
	if s.deleted || username != s.id.Username {
		return users.ErrNotFound
	}
	s.deleted = true
	return nil
}

func (s *cutUserStore) GetToken(_ context.Context, id string) (users.Token, error) {
	t, ok := s.rows[id]
	if !ok {
		return users.Token{}, users.ErrNotFound
	}
	return t, nil
}

func (s *cutUserStore) RevokeToken(_ context.Context, id string) (users.Token, error) {
	t, ok := s.rows[id]
	if !ok {
		return users.Token{}, users.ErrNotFound
	}
	now := time.Now()
	t.RevokedAt = &now
	s.rows[id] = t
	s.revoked = true
	return t, nil
}

// stubSessions is the ExecutorSessions backend for the stream tests: it opens
// instantly and the handler then blocks until its context ends.
type stubSessions struct{}

func (stubSessions) Open(_ context.Context, _ *rafikiv1.ExecutorSessionRequest) (*rafikiv1.ExecutorSessionReady, error) {
	return &rafikiv1.ExecutorSessionReady{ExecutorId: "sess-test"}, nil
}

// TestRevokeTokenCancelsOpenStream pins the whole cut over a real Connect
// mount: a user opens ExecutorSession — a real server-streaming RPC through
// connectControlRoute's composition — then revokes the very token the stream
// rides. connectUserAdmin.RevokeToken must tombstone the row AND cut the open
// stream, which the still-connected client observes as Canceled: connect maps
// a handler that returns nil on a cancelled context to a clean end of stream,
// so the interceptor reports the deliberate cut as Canceled rather than
// leaving the client to guess at a bare EOF.
func TestRevokeTokenCancelsOpenStream(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	ustore := &cutUserStore{
		token: proxyUserToken,
		id:    users.Identity{UserID: "u1", Username: "brent", TokenID: "tok-1"},
		rows: map[string]users.Token{
			"tok-1": {ID: "tok-1", UserID: "u1", Username: "brent", Name: "primary", Origin: users.OriginService},
		},
	}
	ctrl := &Controller{users: ustore, streamRevoke: reg}
	srv := connectapi.NewServer(store.NewMessages(nil))
	srv.SetExecutorSessions(stubSessions{})
	srv.SetUserAdmin(connectUserAdmin{c: ctrl})
	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv, reg)
	mux := http.NewServeMux()
	h.Mount(mux, server.NewUserTokenAuth(ustore, proxyBootToken, server.DefaultAuthCacheTTL).Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	client := rafikiv1connect.NewControlClient(ts.Client(), ts.URL)

	// The ready message is sent by the handler, which runs strictly after the
	// interceptor registered the stream — receiving it proves the stream is
	// open AND registered, with no timing window to poll.
	req := connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "laptop"})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)
	stream, err := client.ExecutorSession(context.Background(), req)
	if err != nil {
		t.Fatalf("open executor session: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("ready: %v", stream.Err())
	}
	c.Eq(1, registryCount(reg), "the open stream must be registered under the credential that opened it")

	rev := connect.NewRequest(&rafikiv1.RevokeTokenRequest{Id: "tok-1"})
	rev.Header().Set("Authorization", "Bearer "+proxyUserToken)
	if _, err := client.RevokeToken(context.Background(), rev); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		got <- stream.Err()
	}()
	select {
	case err := <-got:
		c.Eq(connect.CodeCanceled, connect.CodeOf(err), "the revoked stream must end Canceled, got %v", err)
	case <-time.After(5 * time.Second):
		t.Error("the revoked stream never ended")
	}
	c.Eq(0, registryCount(reg), "the cut stream deregisters itself")
}

// cutMount is the TestRevokeTokenCancelsOpenStream mount, parameterised: the
// auth resolver is returned so a test can wire it as the registry's forget
// hook (main.go's wiring) while the mount keeps using the same instance — the
// cache-purge contract is about THAT instance, not an interchangeable one.
func cutMount(t *testing.T, ustore *cutUserStore, reg *streamRegistry, auth *server.UserTokenAuth) rafikiv1connect.ControlClient {
	t.Helper()
	ctrl := &Controller{users: ustore, streamRevoke: reg}
	srv := connectapi.NewServer(store.NewMessages(nil))
	srv.SetExecutorSessions(stubSessions{})
	srv.SetUserAdmin(connectUserAdmin{c: ctrl})
	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv, reg)
	mux := http.NewServeMux()
	h.Mount(mux, auth.Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return rafikiv1connect.NewControlClient(ts.Client(), ts.URL)
}

// openExecutorSession opens the stub's executor-session stream under the cut
// store's token and returns it after the ready message — receiving it proves
// the stream is open AND registered, with no timing window to poll.
func openExecutorSession(t *testing.T, client rafikiv1connect.ControlClient) *connect.ServerStreamForClient[rafikiv1.ExecutorSessionEvent] {
	t.Helper()
	req := connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "laptop"})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)
	stream, err := client.ExecutorSession(context.Background(), req)
	if err != nil {
		t.Fatalf("open executor session: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("ready: %v", stream.Err())
	}
	return stream
}

// revokedStreamEndsCanceled drains a server stream cut by revocation and
// pins the deliberate-cut code (connect maps a nil handler return on a
// cancelled context to a bare EOF unless the interceptor reports Canceled).
func revokedStreamEndsCanceled(t *testing.T, stream *connect.ServerStreamForClient[rafikiv1.ExecutorSessionEvent]) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		got <- stream.Err()
	}()
	select {
	case err := <-got:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Errorf("the revoked stream must end Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the revoked stream never ended")
	}
}

// refusedReopen re-opens the executor-session stream and returns the error
// the refusal carries: a connect streaming call returns the stream object
// before the server has answered, so the HTTP-level refusal surfaces on the
// first Receive. Receiving anything means the stream was ADMITTED, which is
// the bug this suite exists to catch.
func refusedReopen(t *testing.T, client rafikiv1connect.ControlClient) error {
	t.Helper()
	req := connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "laptop"})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)
	stream, err := client.ExecutorSession(context.Background(), req)
	if err != nil {
		return err
	}
	if stream.Receive() {
		t.Fatal("the re-opened stream was admitted; revocation did not refuse it")
	}
	return stream.Err()
}

// TestRevokeTokenPurgesAuthCacheForNewRequests pins the OTHER half of
// daemon-side revocation: the cut cancels open streams, and the auth-cache
// purge makes the NEXT request refuse — without the purge, a stream
// re-opened inside the cache's TTL re-authenticates from a warm entry and
// re-registers under the already-revoked token id, which nothing will ever
// fire for again. The store's authCalls counter is the proof of re-checking:
// warm (1) through the whole revoke, then 2 on the re-open.
func TestRevokeTokenPurgesAuthCacheForNewRequests(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	ustore := &cutUserStore{
		token: proxyUserToken,
		id:    users.Identity{UserID: "u1", Username: "brent", TokenID: "tok-1"},
		rows: map[string]users.Token{
			"tok-1": {ID: "tok-1", UserID: "u1", Username: "brent", Name: "primary", Origin: users.OriginService},
		},
	}
	// A TTL far beyond the test's runtime, so a passing refusal can only be
	// the purge — never the entry expiring underneath it.
	auth := server.NewUserTokenAuth(ustore, proxyBootToken, time.Minute)
	reg.forget = auth
	client := cutMount(t, ustore, reg, auth)

	stream := openExecutorSession(t, client)
	c.Eq(1, registryCount(reg), "the open stream must be registered under the credential that opened it")
	c.Eq(1, ustore.authCalls, "the stream's middleware call must have warmed the cache")

	rev := connect.NewRequest(&rafikiv1.RevokeTokenRequest{Id: "tok-1"})
	rev.Header().Set("Authorization", "Bearer "+proxyUserToken)
	if _, err := client.RevokeToken(context.Background(), rev); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	revokedStreamEndsCanceled(t, stream)

	// The re-open: refused BEFORE registering, because the middleware had to
	// re-check the store (where the token is now revoked) instead of
	// answering from the still-warm cache. connect reports a streaming call's
	// HTTP-level refusal on the first Receive, not on the call.
	refused := refusedReopen(t, client)
	c.Require().Error(refused, "a revoked token must not open a stream")
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(refused), "code")
	c.Eq(2, ustore.authCalls, "the re-open must have re-checked the store, not the warm cache")
	c.Eq(0, registryCount(reg), "the refused re-open must not have registered")
}

// TestUserRmPurgesAuthCacheForNewRequests pins the user-removal half: UserRm
// cuts the user's open stream AND forgets the user's cached identity, so the
// same token is refused on its next request even though the middleware saw
// it moments before.
func TestUserRmPurgesAuthCacheForNewRequests(t *testing.T) {
	c := assert.NewAborting(t)
	reg := newStreamRegistry()
	ustore := &cutUserStore{
		token: proxyUserToken,
		id:    users.Identity{UserID: "u1", Username: "brent", TokenID: "tok-1", IsAdmin: true},
		rows: map[string]users.Token{
			"tok-1": {ID: "tok-1", UserID: "u1", Username: "brent", Name: "primary", Origin: users.OriginService},
		},
	}
	auth := server.NewUserTokenAuth(ustore, proxyBootToken, time.Minute)
	reg.forget = auth
	client := cutMount(t, ustore, reg, auth)

	stream := openExecutorSession(t, client)
	c.Eq(1, ustore.authCalls, "the stream's middleware call must have warmed the cache")

	rm := connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: "brent"})
	rm.Header().Set("Authorization", "Bearer "+proxyUserToken)
	if _, err := client.RemoveUser(context.Background(), rm); err != nil {
		t.Fatalf("user rm: %v", err)
	}

	revokedStreamEndsCanceled(t, stream)

	// Same refusal shape as the token revoke, with the purge keyed by the
	// USER: the store's Authenticate now tombstones the whole user.
	refused := refusedReopen(t, client)
	c.Require().Error(refused, "a removed user's token must not open a stream")
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(refused), "code")
	c.Eq(2, ustore.authCalls, "the re-open must have re-checked the store, not the warm cache")
	c.Eq(0, registryCount(reg), "the refused re-open must not have registered")
}
