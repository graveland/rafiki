// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestSpawnWorkspaceToolsChildMaySetIt pins field 36 as child-allowed: a child
// caller setting only workspace_tools is not refused. It only narrows the
// child's own tool set, so it grants no authority.
func TestSpawnWorkspaceToolsChildMaySetIt(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := childScopedServer(f)

	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/tmp", WorkspaceTools: "rafiki",
	}))
	c.Require().NoError(err, "child Spawn with only workspace_tools refused")
}

// TestSpawnWorkspaceToolsUnknownValueIsInvalidArgument pins the handler-side
// validation: a value outside "", "native" and "rafiki" never reaches the
// lifecycle, and the refusal names both accepted values so a caller can fix it
// without reading the docs.
func TestSpawnWorkspaceToolsUnknownValueIsInvalidArgument(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeLifecycle{}
	s := childScopedServer(f)

	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/tmp", WorkspaceTools: "none",
	}))
	c.Require().Error(err, "Spawn with workspace_tools=none succeeded")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	var ce *connect.Error
	c.True(errors.As(err, &ce), "refusal is not a *connect.Error: %v", err)
	c.Eq(`workspace_tools must be "native" or "rafiki"`, ce.Message(), "message")
	c.Eq("", f.got.Cwd, "Spawn reached the lifecycle despite the refusal: %+v", f.got)
}

// TestSpawnWorkspaceToolsForwardedToLifecycle pins the proto→SpawnParams
// mapping: every accepted value reaches the lifecycle verbatim, so the
// controller can normalize and resolve the mode once at spawn.
func TestSpawnWorkspaceToolsForwardedToLifecycle(t *testing.T) {
	for _, v := range []string{"", "native", "rafiki"} {
		c := assert.NewAborting(t)
		f := &fakeLifecycle{}
		s := childScopedServer(f)
		_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
			Cwd: "/tmp", WorkspaceTools: v,
		}))
		c.Require().NoError(err, "Spawn workspace_tools=%q", v)
		c.Eq(v, f.got.WorkspaceTools, "SpawnParams.WorkspaceTools for %q", v)
	}
}
