// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"

	"github.com/multigres/testkit/assert"
)

// childstoreWithSession returns a store holding one exited session row under
// childID, so Controller.GetStreams/Send's known-child paths run without a
// database. Status is the zero value (spawning): GetStreams sees a session
// but no live process (alive=false); Send's validateSendTarget lets the zero
// status through and Send falls to sendFrame, which finds no live child.
func childstoreWithSession(t *testing.T, childID string) *childstore.Store {
	t.Helper()
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: childID, Cwd: t.TempDir()})
	return st
}

// TestRawChildIOGetStreamsMatchesFramed pins the answer a debugging client
// consumes: the Connect adapter's GetStreams must equal, field by field, the
// GetStreamsResponseData the GetStreams RPC served from the
// same Controller — alive flag, stdin frames (order preserved, one []byte per
// frame) and the err field (always nil by design; live stderr races the
// reader goroutine).
func TestRawChildIOGetStreamsMatchesFramed(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := &Controller{st: childstoreWithSession(t, "child-1"), cm: newChildManager()}
	a := connectRawChildIO{c: ctrl}

	// Unknown child: the framed handler's ControllerError, unchanged.
	if _, err := a.GetStreams(context.Background(), "c_missing", "all"); err == nil {
		t.Fatal("unknown child: err = nil, want child_not_found ControllerError")
	} else if err.Error() != "child not found: c_missing" {
		t.Errorf("unknown child: err = %v, want the Controller's authored text", err)
	}

	// Known-but-exited child: alive=false with nothing populated — the caller
	// falls back to the on-disk dump. In must be an EMPTY slice, not nil, so
	// the proto repeated field never serializes as absent.
	res, err := a.GetStreams(context.Background(), "child-1", "all")
	ck.Require().NoError(err, "GetStreams(exited)")
	want, werr := ctrl.GetStreams("child-1", "all") // the framed payload, verbatim
	ck.Require().NoError(werr, "framed GetStreams")
	ck.Eq(want.Alive, res.Alive, "alive")
	ck.NotNil(res.In, "In = nil, want an empty slice")
	ck.Len(res.In, len(want.In), "len(In) = %d, want", len(res.In))
	ck.Empty(res.Err, "Err")
}

// TestRawChildIOGetStreamsPassesWhichThrough pins that the which selector
// reaches Controller.GetStreams untouched — the framed handler passed the
// field through and validated nothing beyond the enumerated set (which the
// connectapi handler owns here).
func TestRawChildIOGetStreamsPassesWhichThrough(t *testing.T) {
	ctrl := &Controller{st: childstoreWithSession(t, "child-1"), cm: newChildManager()}
	a := connectRawChildIO{c: ctrl}
	for _, which := range []string{"", "in", "err", "all"} {
		_, err := a.GetStreams(context.Background(), "child-1", which)
		assert.NewCollecting(t).NoError(err, "GetStreams(which=%q)", which)
	}
}

// TestRawChildIOSendFrameDelegatesToSend pins that the adapter is pure
// delegation: the frame reaches Controller.Send verbatim, and Send's own
// classification (child_not_found on an unknown target) is what the caller
// sees.
func TestRawChildIOSendFrameDelegatesToSend(t *testing.T) {
	ctrl := &Controller{st: childstoreWithSession(t, "child-1"), cm: newChildManager()}
	a := connectRawChildIO{c: ctrl}

	if err := a.SendFrame(context.Background(), "c_missing", json.RawMessage(`{"type":"get_state"}`)); err == nil {
		t.Fatal("unknown child: err = nil, want child_not_found ControllerError")
	} else if err.Error() != "child not found: c_missing" {
		t.Errorf("err = %v, want the Controller's authored text", err)
	}
}

// TestRawChildIOImplementsSeam pins the adapter satisfies the Wave-1 seam, so
// the wiring task can attach it to SetRawChildIO unchanged.
func TestRawChildIOImplementsSeam(t *testing.T) {
	var _ connectapi.RawChildIO = connectRawChildIO{c: nil}
}
