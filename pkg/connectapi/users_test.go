// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

type fakeUserAdmin struct {
	createResp *rafikiv1.CreateUserResponse
	createErr  error
	listRows   []*rafikiv1.UserRow
	listErr    error
	removeErr  error

	gotIncludeDeleted bool
	gotLimit          int32
	gotRemoveName     string
}

func (f *fakeUserAdmin) Create(context.Context, string) (*rafikiv1.CreateUserResponse, error) {
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
	if _, err := s.RemoveUser(context.Background(), connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: "a"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("RemoveUser: got code %v, want Unavailable", connect.CodeOf(err))
	}
}

// TestUserRPCsRoundTrip proves the handlers pass requests and responses
// through the seam unmodified.
func TestUserRPCsRoundTrip(t *testing.T) {
	f := &fakeUserAdmin{
		createResp: &rafikiv1.CreateUserResponse{Id: "u1", Username: "alice", Token: "rfk_x", CreatedAtUnix: 100},
		listRows:   []*rafikiv1.UserRow{{Id: "u1", Username: "alice", CreatedAtUnix: 100}},
	}
	s := &Server{}
	s.SetUserAdmin(f)
	ctx := context.Background()

	createResp, err := s.CreateUser(ctx, connect.NewRequest(&rafikiv1.CreateUserRequest{Username: "alice"}))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if createResp.Msg.GetUsername() != "alice" || createResp.Msg.GetToken() != "rfk_x" || createResp.Msg.GetCreatedAtUnix() != 100 {
		t.Fatalf("CreateUser response = %+v", createResp.Msg)
	}

	listResp, err := s.ListUsers(ctx, connect.NewRequest(&rafikiv1.ListUsersRequest{IncludeDeleted: true, Limit: 5}))
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if !f.gotIncludeDeleted || f.gotLimit != 5 {
		t.Fatalf("List saw includeDeleted=%v limit=%d, want true 5", f.gotIncludeDeleted, f.gotLimit)
	}
	if len(listResp.Msg.GetUsers()) != 1 || listResp.Msg.GetUsers()[0].GetUsername() != "alice" {
		t.Fatalf("ListUsers response = %+v", listResp.Msg.GetUsers())
	}

	if _, err := s.RemoveUser(ctx, connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: "alice"})); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if f.gotRemoveName != "alice" {
		t.Fatalf("Remove saw username %q, want alice", f.gotRemoveName)
	}
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
				if _, err := s.CreateUser(ctx, connect.NewRequest(&rafikiv1.CreateUserRequest{Username: "a"})); err.Error() == tc.err.Error() {
					t.Error("uncoded error text leaked onto the wire; it must be redacted")
				}
			}
		})
	}
}
