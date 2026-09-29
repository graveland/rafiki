// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// fakeUserCLIStore drives one verb at a time: each test wires the function
// its verb calls and captures the arguments through it. Every other method
// panics via the nil embedded interface — loud on purpose, since a CLI path
// that reached an unwired store method is a bug the test should name.
type fakeUserCLIStore struct {
	users.Store
	createFn     func(users.NewUser) (users.User, string, error)
	setEmailFn   func(username, email string) (users.User, error)
	lookupFn     func(username string) (string, error)
	mintFn       func(userID string, t users.NewToken) (users.Token, string, error)
	listTokensFn func(userID string, includeRevoked bool) ([]users.Token, error)
	revokeFn     func(id string) (users.Token, error)
}

func (f fakeUserCLIStore) Create(_ context.Context, u users.NewUser) (users.User, string, error) {
	return f.createFn(u)
}

func (f fakeUserCLIStore) SetEmail(_ context.Context, username, email string) (users.User, error) {
	return f.setEmailFn(username, email)
}

func (f fakeUserCLIStore) LookupUsername(_ context.Context, username string) (string, error) {
	return f.lookupFn(username)
}

func (f fakeUserCLIStore) MintToken(_ context.Context, userID string, t users.NewToken) (users.Token, string, error) {
	return f.mintFn(userID, t)
}

func (f fakeUserCLIStore) ListTokens(_ context.Context, userID string, includeRevoked bool) ([]users.Token, error) {
	return f.listTokensFn(userID, includeRevoked)
}

func (f fakeUserCLIStore) RevokeToken(_ context.Context, id string) (users.Token, error) {
	return f.revokeFn(id)
}

// TestUserCreateCLIAdminFlag pins --admin: explicit only, never inferred from
// an empty database. The second create (no --admin, on the same now
// non-empty database) proves there is no zero-users inference either.
func TestUserCreateCLIAdminFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	store := usersdb.NewPostgresStore(pool)
	ctx := context.Background()

	var out bytes.Buffer
	c.Require().NoError(runUserCreateCLI(ctx, &out, store, users.NewUser{Username: "root", IsAdmin: true, MintToken: true}), "create --admin")
	if !strings.Contains(out.String(), "username: root") || !strings.Contains(out.String(), "token: ") {
		t.Fatalf("output = %q, want username and token lines", out.String())
	}

	rows, err := store.List(ctx, false, 0)
	c.Require().NoError(err, "list")
	c.Require().False(len(rows) != 1 || rows[0].Username != "root" || !rows[0].IsAdmin, "rows = %+v, want one admin user named root", rows)

	out.Reset()
	c.Require().NoError(runUserCreateCLI(ctx, &out, store, users.NewUser{Username: "alice"}), "create (no --admin, no --token)")
	rows, err = store.List(ctx, true, 0)
	c.Require().NoError(err, "list")
	var alice *bool
	for _, r := range rows {
		if r.Username == "alice" {
			v := r.IsAdmin
			alice = &v
		}
	}
	c.Require().NotNil(alice, "alice was not created")
	c.False(*alice, "alice was created admin without --admin; admin must never be inferred")
}

// TestUserCreateCLIDuplicate proves a repeated name surfaces as a clear
// "already exists" error rather than a raw constraint-violation message, and
// that the failed second attempt never touched the token printed by the
// first.
func TestUserCreateCLIDuplicate(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	store := usersdb.NewPostgresStore(pool)
	ctx := context.Background()

	var out bytes.Buffer
	c.Require().NoError(runUserCreateCLI(ctx, &out, store, users.NewUser{Username: "dup", MintToken: true}), "first create")

	out.Reset()
	err := runUserCreateCLI(ctx, &out, store, users.NewUser{Username: "dup", MintToken: true})
	c.Require().Error(err, "second create with the same name succeeded")
	c.StrContains(err.Error(), "already exists", "err = %v, want an \"already exists\" message", err)
	c.Eq(0, out.Len(), "output on failure = %q, want nothing written", out.String())
}

// TestUserCreateWithoutTokenPrintsHint pins the OIDC-recovery hint: a create
// without --token must not print a token line, and must tell the operator
// what <name> can do next — exactly one hint, naming the created user twice.
func TestUserCreateWithoutTokenPrintsHint(t *testing.T) {
	c := assert.NewCollecting(t)
	var got users.NewUser
	store := fakeUserCLIStore{createFn: func(u users.NewUser) (users.User, string, error) {
		got = u
		return users.User{ID: "u1", Username: u.Username, Email: u.Email, CreatedAt: time.Unix(100, 0).UTC()}, "", nil
	}}

	var out bytes.Buffer
	c.Require().NoError(runUserCreateCLI(context.Background(), &out, store, users.NewUser{Username: "root", Email: "root@example.com"}), "create without token")
	c.False(got.MintToken, "store was asked to mint without --token")
	c.False(got.IsAdmin, "store was asked to make an admin without --admin")
	c.Eq("root", got.Username, "username the store received")
	c.Eq("root@example.com", got.Email, "email the store received")
	c.StrContains(out.String(), "username: root", "created-user output:\n%s", out.String())
	c.StrContains(out.String(), "email: root@example.com", "created-user output:\n%s", out.String())
	c.NotStrContains(out.String(), "token: ", "a token line must not appear:\n%s", out.String())
	c.StrContains(out.String(),
		"no token minted; if the daemon has OIDC configured, root can 'rafiki login'; otherwise run 'rafikid user token mint root'",
		"hint line:\n%s", out.String())
}

// TestUserCreateWithTokenPrintsExactly pins the token output byte for byte:
// the with-token shape predates email support and must not have moved.
func TestUserCreateWithTokenPrintsExactly(t *testing.T) {
	c := assert.NewCollecting(t)
	var got users.NewUser
	store := fakeUserCLIStore{createFn: func(u users.NewUser) (users.User, string, error) {
		got = u
		return users.User{ID: "u1", Username: u.Username, CreatedAt: time.Unix(100, 0).UTC()}, "rfk_plain", nil
	}}

	var out bytes.Buffer
	c.Require().NoError(runUserCreateCLI(context.Background(), &out, store, users.NewUser{Username: "root", MintToken: true}))
	c.True(got.MintToken, "store was not asked to mint with --token")
	c.Eq("username: root\ntoken: rfk_plain\n", out.String(), "output")
}

// TestUserCreateEmailRefusal pins the email sentinels as CLI refusals naming
// the input, with nothing written to stdout.
func TestUserCreateEmailRefusal(t *testing.T) {
	c := assert.NewCollecting(t)
	store := fakeUserCLIStore{createFn: func(users.NewUser) (users.User, string, error) {
		return users.User{}, "", users.ErrEmailTaken
	}}

	var out bytes.Buffer
	err := runUserCreateCLI(context.Background(), &out, store, users.NewUser{Username: "root", Email: "root@example.com"})
	c.Require().Error(err, "email-taken create succeeded")
	c.ErrorContains(err, "already in use", "err = %v", err)
	c.Eq(0, out.Len(), "output on refusal = %q, want nothing written", out.String())

	store.createFn = func(users.NewUser) (users.User, string, error) {
		return users.User{}, "", errors.New("set email for root: connection refused")
	}
	err = runUserCreateCLI(context.Background(), &out, store, users.NewUser{Username: "root", Email: "root@example.com"})
	c.Require().Error(err, "store outage masqueraded as success")
	c.NotStrContains(err.Error(), "already in use", "an outage must not read as a refusal: %v", err)
}

// TestUserUpdateEmailSetsAndClears pins --email round-tripping through
// SetEmail, and the printed email always stating the new state ("(none)"
// after a clear).
func TestUserUpdateEmailSetsAndClears(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	var gotName, gotEmail string
	store := fakeUserCLIStore{setEmailFn: func(username, email string) (users.User, error) {
		gotName, gotEmail = username, email
		return users.User{ID: "u1", Username: username, Email: email, CreatedAt: time.Unix(100, 0).UTC()}, nil
	}}

	var out bytes.Buffer
	c.Require().NoError(runUserUpdateCLI(ctx, &out, store, "alice", "Alice@Example.com"), "set")
	c.Eq("alice", gotName, "username SetEmail received")
	c.Eq("Alice@Example.com", gotEmail, "email SetEmail received (normalization is the store's job)")
	c.Eq("username: alice\nemail: Alice@Example.com\n", out.String(), "set output")

	out.Reset()
	c.Require().NoError(runUserUpdateCLI(ctx, &out, store, "alice", ""), "clear")
	c.Eq("", gotEmail, "clearing email must reach the store as empty")
	c.StrContains(out.String(), "email: (none)", "clear output:\n%s", out.String())
}

// TestUserUpdateEmailRefusals pins unknown-user and email-taken as refusals
// naming the input, with the admin bit unreachable from update.
func TestUserUpdateEmailRefusals(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()

	store := fakeUserCLIStore{setEmailFn: func(string, string) (users.User, error) {
		return users.User{}, users.ErrNotFound
	}}
	var out bytes.Buffer
	err := runUserUpdateCLI(ctx, &out, store, "zed", "zed@example.com")
	c.Require().Error(err, "unknown user accepted")
	c.StrContains(err.Error(), "zed", "err = %v, want the username named", err)
	c.Eq(0, out.Len(), "output on refusal = %q, want nothing written", out.String())

	store.setEmailFn = func(string, string) (users.User, error) {
		return users.User{}, users.ErrEmailTaken
	}
	err = runUserUpdateCLI(ctx, &out, store, "alice", "taken@example.com")
	c.Require().Error(err, "email-taken update succeeded")
	c.StrContains(err.Error(), "already in use", "err = %v", err)
}

// TestUserTokenMintUnknownUser pins the mint path's unknown-user refusal: the
// error must name the user, and no token may be minted.
func TestUserTokenMintUnknownUser(t *testing.T) {
	c := assert.NewCollecting(t)
	minted := false
	store := fakeUserCLIStore{
		lookupFn: func(string) (string, error) { return "", users.ErrNotFound },
		mintFn: func(string, users.NewToken) (users.Token, string, error) {
			minted = true
			return users.Token{}, "", nil
		},
	}

	var out bytes.Buffer
	err := runUserTokenMintCLI(context.Background(), &out, store, "zed", "host somewhere", 0)
	c.Require().Error(err, "mint for an unknown user succeeded")
	c.StrContains(err.Error(), "zed", "err = %v, want the username named", err)
	c.False(minted, "MintToken was called for an unknown user")
	c.Eq(0, out.Len(), "output on refusal = %q, want nothing written", out.String())
}

// TestUserTokenMintPrintsPlaintext pins the happy path: LookupUsername
// resolves the id, MintToken gets OriginService (never OIDC — that origin is
// for federated sessions), and the plaintext is the only output.
func TestUserTokenMintPrintsPlaintext(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotID string
	var gotToken users.NewToken
	store := fakeUserCLIStore{
		lookupFn: func(string) (string, error) { return "uuid-1", nil },
		mintFn: func(userID string, t users.NewToken) (users.Token, string, error) {
			gotID, gotToken = userID, t
			return users.Token{ID: "t1", UserID: userID, Username: "root", Name: t.Name, Origin: t.Origin}, "rfk_plain", nil
		},
	}

	var out bytes.Buffer
	c.Require().NoError(runUserTokenMintCLI(context.Background(), &out, store, "root", "host somewhere", 72*time.Hour), "mint")
	c.Eq("uuid-1", gotID, "userID MintToken received")
	c.Eq("host somewhere", gotToken.Name, "token name")
	c.Eq(users.OriginService, gotToken.Origin, "origin MintToken received")
	c.Eq(72*time.Hour, gotToken.TTL, "TTL MintToken received")
	c.Eq("token: rfk_plain\n", out.String(), "output")
}

// TestUserTokenMintTTLParsing pins --ttl: empty means never expires (TTL 0 —
// the store's no-expiry value, not an unset marker), a real duration parses,
// and garbage is refused before the database is touched.
func TestUserTokenMintTTLParsing(t *testing.T) {
	c := assert.NewCollecting(t)
	d, err := tokenTTL("")
	c.Require().NoError(err, "empty --ttl")
	c.Eq(time.Duration(0), d, "empty --ttl must parse to TTL 0 (never expires)")
	d, err = tokenTTL("720h")
	c.Require().NoError(err, "real --ttl")
	c.Eq(720*time.Hour, d, "parsed --ttl")
	_, err = tokenTTL("bogus")
	c.Require().Error(err, "garbage --ttl accepted")
	c.StrContains(err.Error(), "bogus", "err = %v, want the input named", err)
}

// TestUserTokenListTable pins the ls table: pkg/table, the seven columns in
// the brief's order, never-expiring and unrevoked tokens rendered as
// "never"/"-", and the filters wired through.
func TestUserTokenListTable(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	created := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	expires := created.Add(24 * time.Hour)
	revoked := created.Add(time.Hour)
	var gotUserID string
	var gotRevoked bool
	store := fakeUserCLIStore{
		lookupFn: func(username string) (string, error) {
			if username != "root" {
				return "", users.ErrNotFound
			}
			return "uuid-1", nil
		},
		listTokensFn: func(userID string, includeRevoked bool) ([]users.Token, error) {
			gotUserID, gotRevoked = userID, includeRevoked
			return []users.Token{
				{ID: "t-1", UserID: "uuid-1", Username: "root", Name: "host h1", Origin: users.OriginService, CreatedAt: created},
				{ID: "t-2", UserID: "uuid-2", Username: "alice", Name: "ci", Origin: users.OriginOIDC, CreatedAt: created, ExpiresAt: &expires, RevokedAt: &revoked},
			}, nil
		},
	}

	var out bytes.Buffer
	c.Require().NoError(runUserTokenListCLI(ctx, &out, store, "", true), "ls all users, including revoked")
	c.Eq("", gotUserID, "no name must query every user (empty userID)")
	c.True(gotRevoked, "--revoked did not reach the store")
	for _, want := range []string{"ID", "USER", "NAME", "ORIGIN", "CREATED", "EXPIRES", "REVOKED",
		"t-1", "root", "host h1", "service", "never", "-",
		"t-2", "alice", "ci", "oidc", cliTime(expires), cliTime(revoked)} {
		c.StrContains(out.String(), want, "table missing %q:\n%s", want, out.String())
	}

	out.Reset()
	c.Require().NoError(runUserTokenListCLI(ctx, &out, store, "root", false), "ls one user")
	c.Eq("uuid-1", gotUserID, "the named user's id did not reach the store")
	c.False(gotRevoked, "revoked filter default")

	out.Reset()
	err := runUserTokenListCLI(ctx, &out, store, "zed", false)
	c.Require().Error(err, "ls for an unknown user succeeded")
	c.StrContains(err.Error(), "zed", "err = %v, want the username named", err)
}

// TestUserTokenRevoke pins revoke: the id reaches the store, the output names
// what was revoked, and an unknown id is a refusal naming the input.
func TestUserTokenRevoke(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	var gotID string
	store := fakeUserCLIStore{revokeFn: func(id string) (users.Token, error) {
		gotID = id
		return users.Token{ID: id, UserID: "uuid-1", Username: "root", Name: "host h1", Origin: users.OriginService, CreatedAt: time.Unix(100, 0).UTC()}, nil
	}}

	var out bytes.Buffer
	c.Require().NoError(runUserTokenRevokeCLI(ctx, &out, store, "t-9"), "revoke")
	c.Eq("t-9", gotID, "id the store received")
	c.StrContains(out.String(), "revoked: t-9", "output:\n%s", out.String())
	c.StrContains(out.String(), "root", "output must name the user:\n%s", out.String())

	store.revokeFn = func(string) (users.Token, error) { return users.Token{}, users.ErrNotFound }
	out.Reset()
	err := runUserTokenRevokeCLI(ctx, &out, store, "t-404")
	c.Require().Error(err, "unknown token accepted")
	c.StrContains(err.Error(), "t-404", "err = %v, want the id named", err)
	c.Eq(0, out.Len(), "output on refusal = %q, want nothing written", out.String())
}

// TestUserTokenRevokeHelpPinsDaemonScope pins the help text the brief
// requires: the host CLI revokes only what the daemon will honor on NEW
// requests, and the operator is pointed at 'rafiki token revoke' for
// interrupting live streams.
func TestUserTokenRevokeHelpPinsDaemonScope(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := ""
	cmd := newUserTokenRevokeCLICmd(&dsn)
	for _, want := range []string{"within 5s", "streams already open", "`rafiki token revoke`", "against the daemon"} {
		c.StrContains(cmd.Long, want, "revoke help missing %q:\n%s", want, cmd.Long)
	}
}
