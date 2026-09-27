// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestClosePassesChildIDThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	c.Require().NoError(err, "Close")
	c.Eq("c_1", f.closedID, "closedID")
	c.Eq("c_1", resp.Msg.GetChildId(), "resp child_id")
}

func TestCloseRequiresChildID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{})
	_, err := s.Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestCloseWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

// Controller.Close returns authored connectapi.ControllerError values, and the code the
// daemon attached at the source is the classification. A double-close must
// read as NotFound and closing a still-running child as FailedPrecondition —
// not Internal, which tells the client the daemon is broken. The authored
// message text is forwarded with it (the daemon writes both strings).
func TestCloseNotFoundBecomesNotFound(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{closeErr: &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: "child not found: c_1",
	}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
	c.False(err == nil || !strings.Contains(err.Error(), "child not found: c_1"), "message = %v, want the daemon's authored text", err)
}

func TestCloseNotExitedBecomesFailedPrecondition(t *testing.T) {
	f := &fakeLifecycle{closeErr: &connectapi.ControllerError{
		Code:    protocol.ErrNotExited,
		Message: "child is still running",
	}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	assert.NewCollecting(t).Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
}

// A generic error — something that is not a connectapi.ControllerError — keeps the
// blanket Internal the rest of this file uses, with the raw cause redacted:
// the peer sees only the fixed internal text, never infrastructure text like
// a pgx failure naming the database.
func TestCloseErrorBecomesInternal(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{closeErr: errors.New("still running")}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "code")
	c.False(err == nil || strings.Contains(err.Error(), "still running"), "err.Error() = %v, want the raw cause redacted", err)
}
