package main

import (
	"errors"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/server"
	"git.graveland.dev/brent/pi-controller/internal/store"
	"git.graveland.dev/brent/pi-controller/protocol"
)

// TestController_GetStreams_StoreMiss verifies that GetStreams returns a
// ControllerError with ErrChildNotFound when the child is absent from the store.
func TestController_GetStreams_StoreMiss(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	_, err := ctrl.GetStreams("does-not-exist", "all")
	if err == nil {
		t.Fatalf("expected error for missing child, got nil")
	}
	var ce *server.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *server.ControllerError, got %T: %v", err, err)
	}
	if ce.Code != protocol.ErrChildNotFound {
		t.Fatalf("expected code %s, got %s", protocol.ErrChildNotFound, ce.Code)
	}
}

// TestController_GetStreams_StoreOnlyChild verifies the Alive:false path: a
// child present in the store but not registered in the child manager (e.g. one
// that has exited) returns Alive:false with no error and no stream data.
func TestController_GetStreams_StoreOnlyChild(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	ctrl.st.Insert(&store.Session{
		ChildID: "exited-child",
		Status:  protocol.StatusExited,
	})

	res, err := ctrl.GetStreams("exited-child", "all")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Alive {
		t.Fatalf("expected alive=false for store-only child, got %+v", res)
	}
	if res.In != nil || res.Err != nil {
		t.Fatalf("expected no stream data, got %+v", res)
	}
}
