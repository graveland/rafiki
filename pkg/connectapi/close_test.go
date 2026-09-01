// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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
