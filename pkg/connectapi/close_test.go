// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestClosePassesChildIDThrough(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.closedID != "c_1" {
		t.Errorf("closedID = %q, want c_1", f.closedID)
	}
	if resp.Msg.GetChildId() != "c_1" {
		t.Errorf("resp child_id = %q, want c_1", resp.Msg.GetChildId())
	}
}

func TestCloseRequiresChildID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{})
	_, err := s.Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestCloseWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}

// Controller.Close returns authored ControllerError values, and the code the
// daemon attached at the source is the classification. A double-close must
// read as NotFound and closing a still-running child as FailedPrecondition —
// not Internal, which tells the client the daemon is broken. The authored
// message text is forwarded with it (the daemon writes both strings).
func TestCloseNotFoundBecomesNotFound(t *testing.T) {
	f := &fakeLifecycle{closeErr: &control.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: "child not found: c_1",
	}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
	if err == nil || !strings.Contains(err.Error(), "child not found: c_1") {
		t.Errorf("message = %v, want the daemon's authored text", err)
	}
}

func TestCloseNotExitedBecomesFailedPrecondition(t *testing.T) {
	f := &fakeLifecycle{closeErr: &control.ControllerError{
		Code:    protocol.ErrNotExited,
		Message: "child is still running",
	}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// A generic error — something that is not a ControllerError — keeps the
// blanket Internal the rest of this file uses.
func TestCloseErrorBecomesInternal(t *testing.T) {
	f := &fakeLifecycle{closeErr: errors.New("still running")}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	_, err := s.Close(context.Background(),
		connect.NewRequest(&rafikiv1.CloseRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}
