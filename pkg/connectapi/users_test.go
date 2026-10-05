// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

type fakeUserAdmin struct {
	createResp *rafikiv1.CreateUserResponse
	createErr  error
	listRows   []*rafikiv1.UserRow
	listErr    error
	removeErr  error
	updateRow  *rafikiv1.UserRow
	updateErr  error
	mintResp   *rafikiv1.MintTokenResponse
	mintErr    error
	tokenRows  []*rafikiv1.TokenRow
	listTokErr error
	revokeRow  *rafikiv1.TokenRow
	revokeErr  error

	gotIncludeDeleted bool
	gotLimit          int32
	gotRemoveName     string
	gotCreate         [3]any // username, email, mintToken
	gotUpdate         [2]any // username, email
	gotMint           [3]any // username, name, ttlSeconds
	gotListTokens     [3]any // username, includeRevoked, allUsers
	gotRevokeID       string
}

func (f *fakeUserAdmin) Create(_ context.Context, username, email string, mintToken *bool) (*rafikiv1.CreateUserResponse, error) {
	f.gotCreate = [3]any{username, email, mintToken}
	return f.createResp, f.createErr
}

func (f *fakeUserAdmin) List(_ context.Context, includeDeleted bool, limit int32) ([]*rafikiv1.UserRow, error) {
	f.gotIncludeDeleted = includeDeleted
	f.gotLimit = limit
	return f.listRows, f.listErr
}

func (f *fakeUserAdmin) Remove(_ context.Context, username string) error {
	f.gotRemoveName = username
	return f.removeErr
}

func (f *fakeUserAdmin) Update(_ context.Context, username string, email *string) (*rafikiv1.UserRow, error) {
	f.gotUpdate = [2]any{username, email}
	return f.updateRow, f.updateErr
}

func (f *fakeUserAdmin) MintToken(_ context.Context, username, name string, ttlSeconds int64) (*rafikiv1.MintTokenResponse, error) {
	f.gotMint = [3]any{username, name, ttlSeconds}
	return f.mintResp, f.mintErr
}

func (f *fakeUserAdmin) ListTokens(_ context.Context, username string, includeRevoked, allUsers bool) ([]*rafikiv1.TokenRow, error) {
	f.gotListTokens = [3]any{username, includeRevoked, allUsers}
	return f.tokenRows, f.listTokErr
}

func (f *fakeUserAdmin) RevokeToken(_ context.Context, id string) (*rafikiv1.TokenRow, error) {
	f.gotRevokeID = id
	return f.revokeRow, f.revokeErr
}

// TestUserAdminHandlersUnwiredAreUnavailable pins the same Unavailable path
// SetSkillManager/SetProviderBanManager's nil refusal pins: a stored pointer
// to a nil interface would defeat it. Complements (does not duplicate)
// TestSetUserAdminNilIsRefused in newseams_test.go, which pins the storage
// state directly rather than through a handler.
func TestUserAdminHandlersUnwiredAreUnavailable(t *testing.T) {
	s := &Server{}
	s.SetUserAdmin(nil)
	if _, err := s.CreateUser(context.Background(), connect.NewRequest(&rafikiv1.CreateUserRequest{Username: "a"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("CreateUser: got code %v, want Unavailable", connect.CodeOf(err))
	}
	if _, err := s.ListUsers(context.Background(), connect.NewRequest(&rafikiv1.ListUsersRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ListUsers: got code %v, want Unavailable", connect.CodeOf(err))
	}
	_, err := s.RemoveUser(context.Background(), connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: "a"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "RemoveUser: got code")
	for name, call := range map[string]func() error{
		"UpdateUser": func() error {
			_, e := s.UpdateUser(context.Background(), connect.NewRequest(&rafikiv1.UpdateUserRequest{Username: "a"}))
			return e
		},
		"MintToken": func() error {
			_, e := s.MintToken(context.Background(), connect.NewRequest(&rafikiv1.MintTokenRequest{Username: "a"}))
			return e
		},
		"ListTokens": func() error {
			_, e := s.ListTokens(context.Background(), connect.NewRequest(&rafikiv1.ListTokensRequest{}))
			return e
		},
		"RevokeToken": func() error {
			_, e := s.RevokeToken(context.Background(), connect.NewRequest(&rafikiv1.RevokeTokenRequest{Id: "t"}))
			return e
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Errorf("%s: got code %v, want Unavailable", name, connect.CodeOf(err))
		}
	}
}

// TestUserRPCsRoundTrip proves the handlers pass requests and responses
// through the seam unmodified.
func TestUserRPCsRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	created := int64(1800000100)
	f := &fakeUserAdmin{
		createResp: &rafikiv1.CreateUserResponse{Id: "u1", Username: "alice", Token: "rfk_x", CreatedAt: timestamppb.New(time.Unix(100, 0))},
		listRows:   []*rafikiv1.UserRow{{Id: "u1", Username: "alice", CreatedAt: timestamppb.New(time.Unix(100, 0))}},
		updateRow:  &rafikiv1.UserRow{Id: "u1", Username: "alice", Email: "alice@x.dev", CreatedAt: timestamppb.New(time.Unix(100, 0))},
		mintResp:   &rafikiv1.MintTokenResponse{Info: &rafikiv1.TokenRow{Id: "t1", Username: "alice", Name: "n"}, Token: "rfk_y"},
		tokenRows:  []*rafikiv1.TokenRow{{Id: "t1", Username: "alice"}, {Id: "t2", Username: "bob"}},
		revokeRow:  &rafikiv1.TokenRow{Id: "t1", RevokedAt: timestamppb.New(time.Unix(created, 0))},
	}
	s := &Server{}
	s.SetUserAdmin(f)
	ctx := context.Background()

	mintTrue := true
	createResp, err := s.CreateUser(ctx, connect.NewRequest(&rafikiv1.CreateUserRequest{Username: "alice", Email: "a@x.dev", MintToken: &mintTrue}))
	c.NoError(err, "CreateUser")
	if createResp.Msg.GetUsername() != "alice" || createResp.Msg.GetToken() != "rfk_x" || createResp.Msg.GetCreatedAt().AsTime().Unix() != 100 {
		t.Fatalf("CreateUser response = %+v", createResp.Msg)
	}
	if f.gotCreate[0] != "alice" || f.gotCreate[1] != "a@x.dev" {
		t.Fatalf("Create saw = %v", f.gotCreate)
	}
	if p, ok := f.gotCreate[2].(*bool); !ok || p == nil || !*p {
		t.Fatalf("Create saw mintToken = %v, want &true", f.gotCreate[2])
	}

	listResp, err := s.ListUsers(ctx, connect.NewRequest(&rafikiv1.ListUsersRequest{IncludeDeleted: true, Limit: 5}))
	c.NoError(err, "ListUsers")
	if !f.gotIncludeDeleted || f.gotLimit != 5 {
		t.Fatalf("List saw includeDeleted=%v limit=%d, want true 5", f.gotIncludeDeleted, f.gotLimit)
	}
	if len(listResp.Msg.GetUsers()) != 1 || listResp.Msg.GetUsers()[0].GetUsername() != "alice" {
		t.Fatalf("ListUsers response = %+v", listResp.Msg.GetUsers())
	}

	// UpdateUser passes the optional email as the raw pointer the request
	// carries; a nil pointer would be a different request.
	email := "alice@x.dev"
	updateResp, err := s.UpdateUser(ctx, connect.NewRequest(&rafikiv1.UpdateUserRequest{Username: "alice", Email: &email}))
	c.NoError(err, "UpdateUser")
	if updateResp.Msg.GetUser().GetEmail() != "alice@x.dev" {
		t.Fatalf("UpdateUser response = %+v", updateResp.Msg)
	}
	if f.gotUpdate[0] != "alice" || f.gotUpdate[1] == nil || *(f.gotUpdate[1].(*string)) != "alice@x.dev" {
		t.Fatalf("Update saw = %v", f.gotUpdate)
	}

	mintResp, err := s.MintToken(ctx, connect.NewRequest(&rafikiv1.MintTokenRequest{Name: "n", TtlSeconds: 3600, Username: "alice"}))
	c.NoError(err, "MintToken")
	if mintResp.Msg.GetToken() != "rfk_y" || mintResp.Msg.GetInfo().GetId() != "t1" {
		t.Fatalf("MintToken response = %+v", mintResp.Msg)
	}
	if f.gotMint[0] != "alice" || f.gotMint[1] != "n" || f.gotMint[2] != int64(3600) {
		t.Fatalf("MintToken saw = %v", f.gotMint)
	}

	listTokResp, err := s.ListTokens(ctx, connect.NewRequest(&rafikiv1.ListTokensRequest{Username: "bob", IncludeRevoked: true, AllUsers: true}))
	c.NoError(err, "ListTokens")
	if len(listTokResp.Msg.GetTokens()) != 2 || listTokResp.Msg.GetTokens()[1].GetUsername() != "bob" {
		t.Fatalf("ListTokens response = %+v", listTokResp.Msg.GetTokens())
	}
	if f.gotListTokens != [3]any{"bob", true, true} {
		t.Fatalf("ListTokens saw = %v", f.gotListTokens)
	}

	revokeResp, err := s.RevokeToken(ctx, connect.NewRequest(&rafikiv1.RevokeTokenRequest{Id: "t1"}))
	c.NoError(err, "RevokeToken")
	if revokeResp.Msg.GetInfo().GetId() != "t1" || revokeResp.Msg.GetInfo().RevokedAt == nil {
		t.Fatalf("RevokeToken response = %+v", revokeResp.Msg)
	}
	c.Eq("t1", f.gotRevokeID, "RevokeToken saw id")

	if _, err := s.RemoveUser(ctx, connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: "alice"})); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	c.Eq("alice", f.gotRemoveName, "Remove saw username")
}

// TestUserAdminErrMapping proves userAdminErr passes an already-coded error
// through untouched (the admin gate's PermissionDenied must reach the wire as
// permission_denied, not be redacted), maps a ControllerError through
// ConnectErr's code table, and redacts any other error to CodeInternal.
func TestUserAdminErrMapping(t *testing.T) {
	ctx := context.Background()
	denied := connect.NewError(connect.CodePermissionDenied, errors.New("admins only"))
	notFound := &ControllerError{Code: protocol.ErrNotFound, Message: "no such user"}

	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		{"permission passes through", denied, connect.CodePermissionDenied},
		{"controller error maps by code", notFound, connect.CodeNotFound},
		{"uncoded error redacted", errors.New("pgx: dial tcp db.internal:5432"), connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.SetUserAdmin(&fakeUserAdmin{createErr: tc.err, listErr: tc.err, removeErr: tc.err})

			if _, err := s.CreateUser(ctx, connect.NewRequest(&rafikiv1.CreateUserRequest{Username: "a"})); connect.CodeOf(err) != tc.want {
				t.Errorf("CreateUser code %v, want %v", connect.CodeOf(err), tc.want)
			}
			if _, err := s.ListUsers(ctx, connect.NewRequest(&rafikiv1.ListUsersRequest{})); connect.CodeOf(err) != tc.want {
				t.Errorf("ListUsers code %v, want %v", connect.CodeOf(err), tc.want)
			}
			if _, err := s.RemoveUser(ctx, connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: "a"})); connect.CodeOf(err) != tc.want {
				t.Errorf("RemoveUser code %v, want %v", connect.CodeOf(err), tc.want)
			}
			if tc.name == "uncoded error redacted" {
				_, err := s.CreateUser(ctx, connect.NewRequest(&rafikiv1.CreateUserRequest{Username: "a"}))
				assert.NewCollecting(t).NotEq(tc.err.Error(), err.Error(), "uncoded error text leaked onto the wire; it must be redacted")
			}
		})
	}
}
