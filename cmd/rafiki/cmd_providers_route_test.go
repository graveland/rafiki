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
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// routeStubControl serves the three route RPCs and records what the CLI sent.
// It mirrors the daemon's own refusals where the tests exercise them: an empty
// model line is invalid_argument (pkg/connectapi's SetRoute/DeleteRoute), the
// CLI never re-validates — it passes the string through, so the refusal lands
// here, on the wire.
type routeStubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	mu         sync.Mutex
	setReqs    []*connect.Request[rafikiv1.SetRouteRequest]
	deleteReqs []*connect.Request[rafikiv1.DeleteRouteRequest]
	rows       []*rafikiv1.RouteRow
	deleteErr  error
}

func (s *routeStubControl) ListRoutes(
	_ context.Context, _ *connect.Request[rafikiv1.ListRoutesRequest],
) (*connect.Response[rafikiv1.ListRoutesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&rafikiv1.ListRoutesResponse{Rows: s.rows}), nil
}

func (s *routeStubControl) SetRoute(
	_ context.Context, req *connect.Request[rafikiv1.SetRouteRequest],
) (*connect.Response[rafikiv1.SetRouteResponse], error) {
	s.mu.Lock()
	s.setReqs = append(s.setReqs, req)
	s.mu.Unlock()
	if req.Msg.GetModelLine() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model_line is required"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&rafikiv1.SetRouteResponse{Row: &rafikiv1.RouteRow{
		ModelLine: req.Msg.GetModelLine(),
		Spec:      req.Msg.GetSpec(),
		CreatedAt: timestamppb.New(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)),
	}}), nil
}

func (s *routeStubControl) DeleteRoute(
	_ context.Context, req *connect.Request[rafikiv1.DeleteRouteRequest],
) (*connect.Response[rafikiv1.DeleteRouteResponse], error) {
	s.mu.Lock()
	s.deleteReqs = append(s.deleteReqs, req)
	delErr := s.deleteErr
	s.mu.Unlock()
	if req.Msg.GetModelLine() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model_line is required"))
	}
	if delErr != nil {
		return nil, delErr
	}
	return connect.NewResponse(&rafikiv1.DeleteRouteResponse{}), nil
}

func (s *routeStubControl) lastSet() *connect.Request[rafikiv1.SetRouteRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.setReqs) == 0 {
		return nil
	}
	return s.setReqs[len(s.setReqs)-1]
}

func (s *routeStubControl) lastDelete() *connect.Request[rafikiv1.DeleteRouteRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deleteReqs) == 0 {
		return nil
	}
	return s.deleteReqs[len(s.deleteReqs)-1]
}

func (s *routeStubControl) setRows(rows []*rafikiv1.RouteRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = rows
}

// serveRouteScratch seeds one socket profile "it" at a short temp dir and
// serves the route stub on the profile's own socket — the path
// newConnectEndpoint dials (serveUserScratch's shape, for the route stub).
// dir must be a SHORT path: unix socket paths are capped at ~104 bytes.
func serveRouteScratch(t *testing.T, stub *routeStubControl) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-rt")
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)
	writeTokenedProfile(t, sock, "")
}

// TestProvidersRouteSetListDelete drives set → list → delete against the
// in-process Connect harness: the line and spec reach the daemon verbatim,
// the table is MODEL LINE / SPEC / SINCE with the RFC3339 stamp rendered the
// bans table's way, and -j/-J are the response's canonical protojson.
func TestProvidersRouteSetListDelete(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &routeStubControl{}
	serveRouteScratch(t, stub)

	// set: the line and spec pass through untouched; the daemon's stored row
	// comes back and the confirmation names both.
	root, out := userTestRoot(t, newProvidersRouteSetCmd(), "set", "z-ai/glm-5.3", "sort=price,quant=fp8+")
	c.NoError(root.Execute(), "route set failed: %v\n%s", out.String())
	req := stub.lastSet()
	c.NotNil(req, "the stub never saw a SetRoute request")
	c.Eq("z-ai/glm-5.3", req.Msg.GetModelLine(), "model line")
	c.Eq("sort=price,quant=fp8+", req.Msg.GetSpec(), "spec")
	c.StrContains(out.String(), "set z-ai/glm-5.3 to sort=price,quant=fp8+", "set output:\n")

	// list: the table carries the stub's row with the stamp rendered local
	// date+time (the RFC3339 wire string parsed, never echoed raw).
	row := &rafikiv1.RouteRow{ModelLine: "z-ai/glm-5.3", Spec: "sort=price,quant=fp8+", CreatedAt: timestamppb.New(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))}
	stub.setRows([]*rafikiv1.RouteRow{row})
	root, out = userTestRoot(t, newProvidersRouteListCmd(), "list")
	c.NoError(root.Execute(), "route list failed: %v\n%s", out.String())
	table := out.String()
	for _, want := range []string{"MODEL LINE", "SPEC", "SINCE", "z-ai/glm-5.3", "sort=price,quant=fp8+"} {
		c.StrContains(table, want, "route list table missing")
	}
	stamp := row.GetCreatedAt().AsTime()
	c.StrContains(table, stamp.Local().Format(time.DateTime), "SINCE column missing the local stamp:\n%s", table)

	// -j: the canonical protojson of the whole response — the rows envelope.
	root, out = userTestRoot(t, newProvidersRouteListCmd(), "list", "-j")
	c.NoError(root.Execute(), "route list -j failed: %v\n%s", out.String())
	var pretty map[string]any
	c.NoError(json.Unmarshal(out.Bytes(), &pretty), "route list -j printed non-JSON:\n%s", out.String())
	rows, ok := pretty["rows"].([]any)
	c.True(ok && len(rows) == 1, "route list -j rows = %v, want the one live row", pretty["rows"])

	// -J: one compact record per row, no envelope.
	root, out = userTestRoot(t, newProvidersRouteListCmd(), "list", "-J")
	c.NoError(root.Execute(), "route list -J failed: %v\n%s", out.String())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.Require().Len(lines, 1, "route list -J printed %d lines, want one per row:\n%s", len(lines), out.String())
	var got map[string]any
	c.NoError(json.Unmarshal([]byte(lines[0]), &got), "JSONL line %q", lines[0])
	c.Eq("z-ai/glm-5.3", got["modelLine"], "JSONL row's model line")
	c.Eq("sort=price,quant=fp8+", got["spec"], "JSONL row's spec")

	// delete: the line reaches the daemon; the confirmation names it.
	root, out = userTestRoot(t, newProvidersRouteDeleteCmd(), "delete", "z-ai/glm-5.3")
	c.NoError(root.Execute(), "route delete failed: %v\n%s", out.String())
	del := stub.lastDelete()
	c.NotNil(del, "the stub never saw a DeleteRoute request")
	c.Eq("z-ai/glm-5.3", del.Msg.GetModelLine(), "delete line")
	c.StrContains(out.String(), "deleted the route for z-ai/glm-5.3", "delete output:\n")
}

// TestProvidersRouteRejectsEmptyLine pins the division of labor the brief
// states: the CLI does not re-validate — it passes the line through and the
// daemon refuses an empty one with invalid_argument, which surfaces as the
// command's error.
func TestProvidersRouteRejectsEmptyLine(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &routeStubControl{}
	serveRouteScratch(t, stub)

	root, out := userTestRoot(t, newProvidersRouteSetCmd(), "set", "", "sort=price")
	err := root.Execute()
	c.Error(err, "route set accepted an empty model line")
	c.StrContains(err.Error(), "model_line is required", "error text: %v", err)
	c.NotNil(stub.lastSet(), "the CLI never sent the empty line to the daemon")
	c.NotStrContains(out.String(), "set  to", "a set confirmation printed for a refused line:\n%s", out.String())

	root, _ = userTestRoot(t, newProvidersRouteDeleteCmd(), "delete", "")
	err = root.Execute()
	c.Error(err, "route delete accepted an empty model line")
	c.StrContains(err.Error(), "model_line is required", "error text: %v", err)
	c.NotNil(stub.lastDelete(), "the CLI never sent the empty line to the daemon")
}

// TestProvidersRouteDeleteOfAnAbsentLineSurfacesNotFound drives the NotFound
// case end to end: the stub refuses the way the daemon does for a line with
// no live row, and the CLI surfaces that rather than printing a confirmation.
func TestProvidersRouteDeleteOfAnAbsentLineSurfacesNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &routeStubControl{deleteErr: connect.NewError(connect.CodeNotFound,
		errors.New("route has no live row for model line nobody"))}
	serveRouteScratch(t, stub)

	root, out := userTestRoot(t, newProvidersRouteDeleteCmd(), "delete", "nobody")
	err := root.Execute()
	c.Error(err, "route delete of an absent line succeeded")
	c.StrContains(err.Error(), "no live row", "error text: %v", err)
	c.NotStrContains(out.String(), "deleted", "a delete confirmation printed for a refused line:\n%s", out.String())
}

// TestProvidersRouteCompletionOffersTheSpecTail pins the client-side half of
// the routing spec's completion: after a "[", the grammar's keys, each unused
// one once; after "sort=" its values (routing.Sort*'s spellings — the test
// reads the constants so the two cannot drift); after "quant=" the
// quantization names (pkg/routing/spec.go's quantTiers plus "unknown"). The
// profile is remote and unreachable: only the client-side branch can answer,
// so a candidate here proves no daemon call happened on this path.
func TestProvidersRouteCompletionOffersTheSpecTail(t *testing.T) {
	c := assert.NewCollecting(t)
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")

	got := completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[")
	c.EqDiff([]string{"sort=", "quant=", "only=", "nodata", "zdr"}, got, "keys after [")

	got = completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[sort=")
	c.EqDiff([]string{
		string(routing.SortPrice), string(routing.SortThroughput),
		string(routing.SortLatency), string(routing.SortBalanced),
	}, got, "values after sort=")

	got = completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[sort=p")
	c.EqDiff([]string{"price"}, got, "values after sort=p")

	got = completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[sort=price,")
	c.EqDiff([]string{"quant=", "only=", "nodata", "zdr"}, got, "keys after a used sort= must drop it")

	got = completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[nodata,zdr,q")
	c.EqDiff([]string{"quant="}, got, "keys after two used flags, filtered")

	got = completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[quant=")
	c.EqDiff([]string{
		"int4", "fp4", "mxfp4", "nvfp4", "fp6", "int8", "fp8", "mxfp8", "fp16", "bf16", "fp32", "unknown",
	}, got, "quantization names after quant=")

	got = completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[quant=fp8")
	c.EqDiff([]string{"fp8"}, got, "quantization names filtered by fp8")

	// Nothing to offer for the two open-ended cases: a closed bracket (the
	// spec is complete) and only= (provider slugs are the daemon's catalog's
	// knowledge, and completion must not dial for them).
	c.Empty(completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[sort=price]"), "a closed spec offered candidates")
	c.Empty(completeModel(nil, "fundi", "openrouter/z-ai/glm-5.3[only="), "only= offered values")
}
