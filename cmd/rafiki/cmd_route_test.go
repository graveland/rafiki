// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"github.com/multigres/testkit/assert"
)

// routeSetStubControl serves the one SetRouting RPC `rafiki route set` calls
// and records what the CLI sent. It mirrors the daemon's own refusals where
// the tests exercise them: the CLI re-validates nothing, so an empty child or
// delta lands here, on the wire.
type routeSetStubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	mu       sync.Mutex
	requests []*connect.Request[rafikiv1.SetRoutingRequest]
	// routing is the canonical stored spec the stub reports back after the
	// merge. Empty echoes the delta, which is the trivial fixed point.
	routing string
	setErr  error
}

func (s *routeSetStubControl) SetRouting(
	_ context.Context, req *connect.Request[rafikiv1.SetRoutingRequest],
) (*connect.Response[rafikiv1.SetRoutingResponse], error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	routing, setErr := s.routing, s.setErr
	s.mu.Unlock()
	if setErr != nil {
		return nil, setErr
	}
	if routing == "" {
		routing = req.Msg.GetDelta()
	}
	return connect.NewResponse(&rafikiv1.SetRoutingResponse{
		ChildId: req.Msg.GetChildId(),
		Routing: routing,
	}), nil
}

func (s *routeSetStubControl) lastSet() *connect.Request[rafikiv1.SetRoutingRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	return s.requests[len(s.requests)-1]
}

// serveRouteSetScratch seeds one socket profile "it" at a short temp dir and
// serves the stub on the profile's own socket — the path newConnectEndpoint
// dials (serveRouteScratch's shape, for the SetRouting stub). dir must be a
// SHORT path: unix socket paths are capped at ~104 bytes.
func serveRouteSetScratch(t *testing.T, stub *routeSetStubControl) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-rs")
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)
	writeTokenedProfile(t, sock, "")
}

// TestRouteSetSendsChildAndDelta pins that the verb forwards its two
// positionals verbatim — the child as child_id, the spec as delta — and
// confirms with the child and the returned canonical spec.
func TestRouteSetSendsChildAndDelta(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &routeSetStubControl{}
	serveRouteSetScratch(t, stub)

	root, out := userTestRoot(t, newRouteSetCmd(), "set", "c_01HXABC", "prefer=fireworks|deepinfra")
	c.NoError(root.Execute(), "route set failed: %v\n%s", out.String())

	req := stub.lastSet()
	c.NotNil(req, "the stub never saw a SetRouting request")
	c.Eq("c_01HXABC", req.Msg.GetChildId(), "child_id")
	c.Eq("prefer=fireworks|deepinfra", req.Msg.GetDelta(), "delta")
	c.StrContains(out.String(), "c_01HXABC  routing: prefer=fireworks|deepinfra", "set output:\n%s", out.String())
}

// TestRouteSetRendersCanonicalSpec pins that the confirmation prints the
// daemon's post-merge canonical spec, not the delta the operator typed: the
// merge is the daemon's (delta-over-stored), and the CLI must show the result
// so a coordinator sees the whole spec it steered the child to.
func TestRouteSetRendersCanonicalSpec(t *testing.T) {
	c := assert.NewAborting(t)
	// The delta names only prefer=; the stored spec carried sort=price, so the
	// canonical result has BOTH — the value that must reach the operator.
	stub := &routeSetStubControl{routing: "sort=price,prefer=fireworks"}
	serveRouteSetScratch(t, stub)

	root, out := userTestRoot(t, newRouteSetCmd(), "set", "c_01HXABC", "prefer=fireworks")
	c.NoError(root.Execute(), "route set failed: %v\n%s", out.String())
	c.StrContains(out.String(), "c_01HXABC  routing: sort=price,prefer=fireworks", "canonical output:\n%s", out.String())
	c.NotStrContains(out.String(), "routing: prefer=fireworks\n", "the delta was echoed instead of the canonical merge:\n%s", out.String())
}

// TestRouteSetJSONIsProtojson pins the -j/-J contract: the canonical protojson
// of SetRoutingResponse — childId and routing, camelCase — with no client-side
// output struct in between.
func TestRouteSetJSONIsProtojson(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &routeSetStubControl{routing: "sort=price,prefer=fireworks"}
	serveRouteSetScratch(t, stub)

	root, out := userTestRoot(t, newRouteSetCmd(), "set", "c_01HXABC", "prefer=fireworks", "-j")
	c.NoError(root.Execute(), "route set -j failed: %v\n%s", out.String())
	var pretty map[string]any
	c.NoError(json.Unmarshal(out.Bytes(), &pretty), "route set -j printed non-JSON:\n%s", out.String())
	c.Eq("c_01HXABC", pretty["childId"], "protojson childId")
	c.Eq("sort=price,prefer=fireworks", pretty["routing"], "protojson routing")
	c.NotStrContains(out.String(), "\"child_id\"", "protojson used the snake_case field name:\n%s", out.String())

	root, out = userTestRoot(t, newRouteSetCmd(), "set", "c_01HXABC", "prefer=fireworks", "-J")
	c.NoError(root.Execute(), "route set -J failed: %v\n%s", out.String())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.Require().Len(lines, 1, "route set -J printed %d lines, want one:\n%s", len(lines), out.String())
	var got map[string]any
	c.NoError(json.Unmarshal([]byte(lines[0]), &got), "JSONL line %q", lines[0])
	c.Eq("c_01HXABC", got["childId"], "JSONL childId")
	c.Eq("sort=price,prefer=fireworks", got["routing"], "JSONL routing")
}

// TestRouteSetSurfacesPermissionDenied pins that a server refusal is an
// ordinary command error, never swallowed into a success line: a child
// credential attempting an operator-only key reaches the operator as
// permission_denied on stderr and a non-zero exit.
func TestRouteSetSurfacesPermissionDenied(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &routeSetStubControl{setErr: connect.NewError(connect.CodePermissionDenied,
		errors.New("only= is operator authority; a child credential may not set it"))}
	serveRouteSetScratch(t, stub)

	root, out := userTestRoot(t, newRouteSetCmd(), "set", "c_01HXABC", "only=fireworks")
	err := root.Execute()
	c.Error(err, "route set accepted a refused steer")
	c.StrContains(err.Error(), "only= is operator authority", "error text: %v", err)
	c.NotNil(stub.lastSet(), "the CLI never sent the steer to the daemon")
	c.NotStrContains(out.String(), "routing:", "a success line printed for a refused steer:\n%s", out.String())
}
