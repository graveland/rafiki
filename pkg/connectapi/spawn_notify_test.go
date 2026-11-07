// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestSpawnNotifyChildMaySetIt pins field 34 as child-allowed: a child caller
// setting only notify is not refused.
func TestSpawnNotifyChildMaySetIt(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := childScopedServer(f)

	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", Notify: "result"}))
	c.Require().NoError(err, "child Spawn with only notify refused")
}

// TestSpawnNotifyUnknownValueIsInvalidArgument pins the handler-side
// validation: a value outside "", "all" and "result" never reaches the
// lifecycle.
func TestSpawnNotifyUnknownValueIsInvalidArgument(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := childScopedServer(f)

	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", Notify: "none"}))
	c.Require().Error(err, "Spawn with notify=none succeeded")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq("", f.got.Cwd, "Spawn reached the lifecycle despite the refusal: %+v", f.got)
}

// TestSpawnNotifyForwardedToLifecycle pins the proto->SpawnParams mapping: the
// value reaches the lifecycle verbatim.
func TestSpawnNotifyForwardedToLifecycle(t *testing.T) {
	for _, v := range []string{"", "all", "result"} {
		c := assert.NewAborting(t)
		f := &fakeLifecycle{}
		s := childScopedServer(f)
		_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/tmp", Notify: v}))
		c.Require().NoError(err, "Spawn notify=%q", v)
		c.Eq(v, f.got.Notify, "SpawnParams.Notify for %q", v)
	}
}
