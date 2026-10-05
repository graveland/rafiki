// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// connectUserAdmin adapts the users store's identity verbs to
// connectapi.UserAdmin. Create/List/Remove here serve an operator managing
// OTHER users' accounts over Connect — a different surface from `rafikid user
// create`'s direct DSN access, which this type does not touch.
//
// The token RPCs (MintToken/ListTokens/RevokeToken) are deliberately NOT
// admin-gated: a user manages its OWN credentials, which is why their methods
// resolve the target through resolveTokenTarget instead of
// requireUserAdmin. The adapter calls the store (a.c.users) directly rather
// than through Controller methods: Controller.UserCreate always mints a
// token, which contradicts the mint decision Create resolves here.
//
// loginConfigured is the wired LoginConfigured — whether this daemon serves
// OIDC login — consulted only by Create's mint decision. A nil func counts as
// "not configured", so an adapter built without the field behaves like a
// daemon without oidc.toml.
type connectUserAdmin struct {
	c               *Controller
	loginConfigured func() bool
}

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

// loginConfiguredNow reports whether OIDC login is configured on this daemon;
// a nil func counts as not configured.
func (a connectUserAdmin) loginConfiguredNow() bool {
	return a.loginConfigured != nil && a.loginConfigured()
}

// Create mints a non-admin user, deciding the token mint:
//
//	mintToken present     honoured as given (reason "requested" when true)
//	mintToken nil         mint iff OIDC login is NOT configured — a user on a
//	                      daemon with no IdP would otherwise be unable to
//	                      authenticate at all
//
// Controller.UserCreate is deliberately NOT called here: it always mints.
// token_reason and login_configured ride the response so the client can word
// its hint without guessing.
func (a connectUserAdmin) Create(ctx context.Context, username, email string, mintToken *bool) (*rafikiv1.CreateUserResponse, error) {
	if err := requireUserAdmin(ctx); err != nil {
		return nil, err
	}
	if a.c.users == nil {
		return nil, errNoUserStore
	}
	loginConfigured := a.loginConfiguredNow()
	mint, reason := false, ""
	switch {
	case mintToken != nil:
		mint = *mintToken
		if mint {
			reason = "requested"
		}
	case !loginConfigured:
		mint = true
		reason = "oidc not configured"
	}
	u, token, err := a.c.users.Create(ctx, users.NewUser{Username: username, Email: email, IsAdmin: false, MintToken: mint})
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
		case errors.Is(err, users.ErrEmailTaken):
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("email %s is already taken", email))
		case errors.Is(err, users.ErrInvalidEmail):
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid email: %s",
					strings.TrimPrefix(err.Error(), users.ErrInvalidEmail.Error()+": ")))
		}
		return nil, err
	}
	slog.Info("user created", "username", u.Username, "id", u.ID, "is_admin", false, "token_minted", mint)
	return &rafikiv1.CreateUserResponse{
		Id:              u.ID,
		Username:        u.Username,
		Token:           token,
		CreatedAt:       timestamppb.New(u.CreatedAt),
		TokenReason:     reason,
		LoginConfigured: loginConfigured,
	}, nil
}

// Update edits a user's email. The admin bit is deliberately absent from the
// request type: it comes only from `rafikid user create --admin` on the
// daemon host.
func (a connectUserAdmin) Update(ctx context.Context, username string, email *string) (*rafikiv1.UserRow, error) {
	if err := requireUserAdmin(ctx); err != nil {
		return nil, err
	}
	if email == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("nothing to update"))
	}
	if a.c.users == nil {
		return nil, errNoUserStore
	}
	u, err := a.c.users.SetEmail(ctx, username, *email)
	if err != nil {
		return nil, userSentinelErr(err, username)
	}
	return newUserRow(u), nil
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
		out = append(out, newUserRow(u))
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

// MintToken mints a service token for the resolved target user. The plaintext
// rides the response exactly once, like CreateUser's.
func (a connectUserAdmin) MintToken(ctx context.Context, username, name string, ttlSeconds int64) (*rafikiv1.MintTokenResponse, error) {
	if ttlSeconds < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ttl must not be negative"))
	}
	userID, err := a.resolveTokenTarget(ctx, username)
	if err != nil {
		return nil, err
	}
	if a.c.users == nil {
		return nil, errNoUserStore
	}
	t, token, err := a.c.users.MintToken(ctx, userID, users.NewToken{
		Name:   name,
		Origin: users.OriginService,
		TTL:    time.Duration(ttlSeconds) * time.Second,
	})
	if err != nil {
		// Unreachable through the ordinary paths (resolveTokenTarget already
		// verified an explicitly named user; a caller-resolved id was valid at
		// authentication) — the store can still answer ErrNotFound if the row
		// was tombstoned in between, and a miss is an answer, not an outage.
		if errors.Is(err, users.ErrNotFound) {
			if username != "" {
				return nil, connect.NewError(connect.CodeNotFound,
					fmt.Errorf("no active user named %s", username))
			}
			return nil, connect.NewError(connect.CodeNotFound, errors.New("user no longer active"))
		}
		return nil, err
	}
	return &rafikiv1.MintTokenResponse{Info: newTokenRow(t), Token: token}, nil
}

// ListTokens lists credentials, never secrets. all_users is admin (or the
// anonymous local socket) authority; otherwise the target resolves through
// resolveTokenTarget — self by default. Naming both a username and all_users
// is refused before any authority check: silently widening to all users would
// answer a scoped question with the whole fleet's rows.
func (a connectUserAdmin) ListTokens(ctx context.Context, username string, includeRevoked, allUsers bool) ([]*rafikiv1.TokenRow, error) {
	if allUsers && username != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("all_users and username are mutually exclusive"))
	}
	var userID string
	if allUsers {
		id := server.IdentityFromContext(ctx)
		if id != nil && (!id.IsUserCredential() || !id.IsAdmin) {
			return nil, connect.NewError(connect.CodePermissionDenied,
				errors.New("listing every user's tokens requires an admin user credential"))
		}
	} else {
		var err error
		if userID, err = a.resolveTokenTarget(ctx, username); err != nil {
			return nil, err
		}
	}
	if a.c.users == nil {
		return nil, errNoUserStore
	}
	rows, err := a.c.users.ListTokens(ctx, userID, includeRevoked)
	if err != nil {
		return nil, err
	}
	out := make([]*rafikiv1.TokenRow, 0, len(rows))
	for _, t := range rows {
		out = append(out, newTokenRow(t))
	}
	return out, nil
}

// RevokeToken tombstones one credential. GetToken runs FIRST because the
// ownership check needs the row; a token id is a random UUID, so reporting
// NotFound for an unknown id and PermissionDenied for a foreign one leaks
// nothing.
func (a connectUserAdmin) RevokeToken(ctx context.Context, id string) (*rafikiv1.TokenRow, error) {
	ident := server.IdentityFromContext(ctx)
	if ident != nil && !ident.IsUserCredential() {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("token administration requires a user credential"))
	}
	if a.c.users == nil {
		return nil, errNoUserStore
	}
	t, err := a.c.users.GetToken(ctx, id)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no such token"))
		}
		return nil, err
	}
	if ident != nil && !ident.IsAdmin && t.UserID != ident.UserID {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("revoking another user's token requires an admin user credential"))
	}
	t, err = a.c.users.RevokeToken(ctx, id)
	if err != nil {
		return nil, err
	}
	// The cut, not just the tombstone: any stream this token still holds open
	// ends now. Already-revoked tokens reach here too — the store answers nil
	// for them and the registry has nothing registered, so the call is
	// idempotent and harmless. The count (when nonzero) is logged by the
	// registry itself.
	a.c.streamRevoke.revokeToken(id)
	return newTokenRow(t), nil
}

// resolveTokenTarget resolves a token RPC's target user:
//
//	identity nil (UDS)      username required — there is no caller to default
//	                        to — and resolved through the store
//	user credential         "" or its own name → the caller itself; another
//	                        name → admin authority
//	anything else           refused (the policy table already refuses a child
//	                        credential on these userOnly verbs; defend anyway)
func (a connectUserAdmin) resolveTokenTarget(ctx context.Context, username string) (string, error) {
	id := server.IdentityFromContext(ctx)
	switch {
	case id == nil:
		if username == "" {
			return "", connect.NewError(connect.CodeInvalidArgument,
				errors.New("--user is required on an unauthenticated socket"))
		}
		return a.lookupUserID(ctx, username)
	case id.IsUserCredential():
		if username == "" || username == id.Username {
			return id.UserID, nil
		}
		if !id.IsAdmin {
			return "", connect.NewError(connect.CodePermissionDenied,
				errors.New("another user's tokens require an admin user credential"))
		}
		return a.lookupUserID(ctx, username)
	default:
		return "", connect.NewError(connect.CodePermissionDenied,
			errors.New("token administration requires a user credential"))
	}
}

// lookupUserID resolves an active username, mapping a miss to a NotFound
// composed from the REQUESTED name — an answer, not a redacted internal.
func (a connectUserAdmin) lookupUserID(ctx context.Context, username string) (string, error) {
	if a.c.users == nil {
		return "", errNoUserStore
	}
	userID, err := a.c.users.LookupUsername(ctx, username)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return "", connect.NewError(connect.CodeNotFound,
				fmt.Errorf("no active user named %s", username))
		}
		return "", err
	}
	return userID, nil
}

// userSentinelErr maps the user store's caller-error sentinels onto coded
// Connect errors, so userAdminErr passes them through untouched (no slog, no
// redaction) and the caller gets the authored reason, not invalid_argument.
func userSentinelErr(err error, username string) error {
	switch {
	case errors.Is(err, users.ErrNotFound):
		return connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no active user named %s", username))
	case errors.Is(err, users.ErrEmailTaken):
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("email is already taken"))
	case errors.Is(err, users.ErrInvalidEmail):
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("invalid email: %s",
				strings.TrimPrefix(err.Error(), users.ErrInvalidEmail.Error()+": ")))
	}
	return err
}

// newUserRow converts a store user to its wire row.
func newUserRow(u users.User) *rafikiv1.UserRow {
	row := &rafikiv1.UserRow{
		Id:        u.ID,
		Username:  u.Username,
		IsAdmin:   u.IsAdmin,
		Email:     u.Email,
		CreatedAt: timestamppb.New(u.CreatedAt),
	}
	if u.DeletedAt != nil {
		row.DeletedAt = timestamppb.New(*u.DeletedAt)
	}
	return row
}

// newTokenRow converts a store token to its wire row. The plaintext is never
// a field: TokenRow is metadata only.
func newTokenRow(t users.Token) *rafikiv1.TokenRow {
	row := &rafikiv1.TokenRow{
		Id:        t.ID,
		Username:  t.Username,
		Name:      t.Name,
		Origin:    string(t.Origin),
		CreatedAt: timestamppb.New(t.CreatedAt),
	}
	if t.ExpiresAt != nil {
		row.ExpiresAt = timestamppb.New(*t.ExpiresAt)
	}
	if t.RevokedAt != nil {
		row.RevokedAt = timestamppb.New(*t.RevokedAt)
	}
	return row
}
