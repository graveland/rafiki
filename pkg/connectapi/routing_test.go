// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestSetRoutingHandlerChildOutsideSubtreeIsDenied proves the handler runs the
// subtree check BEFORE the lifecycle seam: a child scope that refuses every
// target must leave the lifecycle untouched, and must answer PermissionDenied.
func TestSetRoutingHandlerChildOutsideSubtreeIsDenied(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return emptyChildScope{} })

	_, err := s.SetRouting(context.Background(), connect.NewRequest(&rafikiv1.SetRoutingRequest{
		ChildId: "c_victim", Delta: "prefer=fireworks",
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "child outside subtree")
	c.Eq("", f.routingChildID, "lifecycle must not be reached for a refused target")
}

func TestSetRoutingHandlerEmptyDeltaIsInvalidArgument(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetRouting(context.Background(), connect.NewRequest(&rafikiv1.SetRoutingRequest{
		ChildId: "c_x",
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty delta")
	c.Eq("", f.routingChildID, "lifecycle must not be reached")
}

// TestSetRoutingHandlerPassesThrough proves the happy path forwards the delta
// and echoes the canonical stored spec back.
func TestSetRoutingHandlerPassesThrough(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{routingResult: "prefer=fireworks,nodata"}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.SetRouting(context.Background(), connect.NewRequest(&rafikiv1.SetRoutingRequest{
		ChildId: "c_target", Delta: "prefer=fireworks",
	}))
	c.NoError(err, "SetRouting")
	c.Eq("c_target", f.routingChildID, "forwarded child id")
	c.Eq("prefer=fireworks", f.routingDelta, "forwarded delta")
	c.Eq("c_target", resp.Msg.GetChildId(), "response child id")
	c.Eq("prefer=fireworks,nodata", resp.Msg.GetRouting(), "response routing")
}

// TestSetRoutingHandlerWithoutLifecycleFailsClosed pins the unwired seam.
func TestSetRoutingHandlerWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.SetRouting(context.Background(), connect.NewRequest(&rafikiv1.SetRoutingRequest{
		ChildId: "c_x", Delta: "prefer=a",
	}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "unwired lifecycle")
}
