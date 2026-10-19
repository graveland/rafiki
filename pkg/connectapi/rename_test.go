// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestRenameTrimsAndPassesFieldsThrough(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Rename(context.Background(), connect.NewRequest(&rafikiv1.RenameRequest{
		ChildId: "c_target",
		Name:    "  fix the flake  ",
	}))
	c := assert.NewAborting(t)
	c.NoError(err, "Rename")
	c.Eq("c_target", f.renameChildID, "lifecycle child id")
	c.Eq("fix the flake", f.renameName, "lifecycle name")
	c.Eq("fix the flake", resp.Msg.GetName(), "response name")
}

func TestRenameRejectsEmptyFields(t *testing.T) {
	for name, req := range map[string]*rafikiv1.RenameRequest{
		"no child":   {Name: "x"},
		"blank name": {ChildId: "c_target", Name: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			s := connectapi.NewServer(nil)
			s.SetChildLifecycle(&fakeLifecycle{})
			_, err := s.Rename(context.Background(), connect.NewRequest(req))
			assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
		})
	}
}

func TestRenameUnknownChildIsNotFound(t *testing.T) {
	f := &fakeLifecycle{renameErr: &connectapi.ControllerError{Code: protocol.ErrNotFound, Message: "gone"}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Rename(context.Background(), connect.NewRequest(&rafikiv1.RenameRequest{ChildId: "c_x", Name: "n"}))
	assert.NewAborting(t).Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
}
