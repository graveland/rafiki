// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

type fakeRoutes struct {
	rows      []RouteRow
	listErr   error
	setErr    error
	deleteErr error

	setLine, setSpec string
	setCalls         int
	deleted          []string
}

func (f *fakeRoutes) ListRoutes(context.Context) ([]RouteRow, error) {
	return f.rows, f.listErr
}

func (f *fakeRoutes) SetRoute(_ context.Context, line, spec string) (RouteRow, error) {
	f.setCalls++
	f.setLine, f.setSpec = line, spec
	if f.setErr != nil {
		return RouteRow{}, f.setErr
	}
	return RouteRow{ModelLine: line, Spec: spec, CreatedAt: time.Unix(100, 0).UTC()}, nil
}

func (f *fakeRoutes) DeleteRoute(_ context.Context, line string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, line)
	return nil
}

func TestSetRouteManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetRouteManager(nil)
	_, err := s.ListRoutes(context.Background(), connect.NewRequest(&rafikiv1.ListRoutesRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetRouteManager(nil): got code")
}

func TestRouteHandlersUnwiredAreUnavailable(t *testing.T) {
	ctx := context.Background()
	s := &Server{}
	c := assert.NewCollecting(t)

	_, err := s.ListRoutes(ctx, connect.NewRequest(&rafikiv1.ListRoutesRequest{}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "ListRoutes unwired: code")

	_, err = s.SetRoute(ctx, connect.NewRequest(&rafikiv1.SetRouteRequest{ModelLine: "z-ai/glm-5.3", Spec: "sort=price"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "SetRoute unwired: code")
	c.True(strings.Contains(err.Error(), "routes backend not yet wired"), "SetRoute unwired: message %q", err)

	_, err = s.DeleteRoute(ctx, connect.NewRequest(&rafikiv1.DeleteRouteRequest{ModelLine: "z-ai/glm-5.3"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "DeleteRoute unwired: code")
}

func TestSetRouteRejectsBadSpec(t *testing.T) {
	ctx := context.Background()
	f := &fakeRoutes{}
	s := &Server{}
	s.SetRouteManager(f)
	c := assert.NewCollecting(t)

	for _, line := range []string{"", "   "} {
		_, err := s.SetRoute(ctx, connect.NewRequest(&rafikiv1.SetRouteRequest{ModelLine: line, Spec: "sort=price"}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty model_line %q: code", line)
	}
	c.Eq(0, f.setCalls, "empty model_line must not reach the manager")

	_, err := s.SetRoute(ctx, connect.NewRequest(&rafikiv1.SetRouteRequest{ModelLine: "z-ai/glm-5.3", Spec: "sort=nonsense"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "bad spec: code")
	c.True(strings.Contains(err.Error(), `unknown sort "nonsense"`),
		"refusal must carry the parser's message, got %q", err)
	c.Eq(0, f.setCalls, "a spec ParseSpec rejects must not reach the manager")

	// The empty spec is the zero spec, not an argument error: it parses and
	// the manager sees it.
	if _, err := s.SetRoute(ctx, connect.NewRequest(&rafikiv1.SetRouteRequest{ModelLine: "z-ai/glm-5.3"})); err != nil {
		t.Fatal(err)
	}
	c.Eq(1, f.setCalls, "empty spec reaches the manager")
	c.Eq("", f.setSpec, "manager saw the empty spec")
}

func TestDeleteRouteMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	c := assert.NewCollecting(t)

	s := &Server{}
	s.SetRouteManager(&fakeRoutes{deleteErr: fmt.Errorf("store: %w", ErrRouteNotFound)})
	_, err := s.DeleteRoute(ctx, connect.NewRequest(&rafikiv1.DeleteRouteRequest{ModelLine: "z-ai/glm-5.3"}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "absent line: code")

	s2 := &Server{}
	s2.SetRouteManager(&fakeRoutes{deleteErr: errors.New("db down")})
	_, err = s2.DeleteRoute(ctx, connect.NewRequest(&rafikiv1.DeleteRouteRequest{ModelLine: "z-ai/glm-5.3"}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "unrelated failure: code")
}

// TestRouteRowCreatedAtIsUTC pins the read path's rendering and the write
// path's pass-through: created_at is a Timestamp in UTC regardless of the
// source timezone, and the trimmed line and spec reach the manager.
func TestRouteRowCreatedAtIsUTC(t *testing.T) {
	ctx := context.Background()
	f := &fakeRoutes{rows: []RouteRow{{
		ModelLine: "z-ai/glm-5.3", Spec: "sort=price",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("UTC+2", 2*3600)),
	}}}
	s := &Server{}
	s.SetRouteManager(f)
	c := assert.NewCollecting(t)

	resp, err := s.ListRoutes(ctx, connect.NewRequest(&rafikiv1.ListRoutesRequest{}))
	c.Require().NoError(err)
	c.Eq(time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC), resp.Msg.GetRows()[0].GetCreatedAt().AsTime(), "created_at rendering")

	if _, err := s.SetRoute(ctx, connect.NewRequest(&rafikiv1.SetRouteRequest{
		ModelLine: "  z-ai/glm-5.3  ", Spec: "  sort=price  ",
	})); err != nil {
		t.Fatal(err)
	}
	c.Eq("z-ai/glm-5.3", f.setLine, "line reaches the manager trimmed")
	c.Eq("sort=price", f.setSpec, "spec reaches the manager trimmed")
}

// The wave's verify command selects tests with an unanchored substring -run
// pattern whose first alternative is "TestRoute"; TestSetRouteRejectsBadSpec
// and TestDeleteRouteMissingIsNotFound contain "Route" but not "TestRoute",
// so without these pattern-prefixed shims the command would silently skip
// them (each calls the pinned body verbatim).
func TestRouteSetRejectsBadSpec(t *testing.T)       { TestSetRouteRejectsBadSpec(t) }
func TestRouteDeleteMissingIsNotFound(t *testing.T) { TestDeleteRouteMissingIsNotFound(t) }

// fakeRouteExplainer stands in for the daemon's ModelRoutes backend. It
// records the model string it was handed so a test can pin that the handler
// passes the caller's value through (trimmed) rather than parsing it itself.
type fakeRouteExplainer struct {
	resp *rafikiv1.ModelRoutesResponse
	err  error
	got  string
}

func (f *fakeRouteExplainer) ExplainModelRoutes(_ context.Context, model string) (*rafikiv1.ModelRoutesResponse, error) {
	f.got = model
	return f.resp, f.err
}

func TestModelRoutesHandlerUnwiredIsUnavailable(t *testing.T) {
	s := &Server{}
	_, err := s.ModelRoutes(context.Background(),
		connect.NewRequest(&rafikiv1.ModelRoutesRequest{Model: "deepseek/deepseek-v4.1-flash"}))
	c := assert.NewAborting(t)
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "unwired ModelRoutes: code")
	c.True(strings.Contains(err.Error(), "route explainer not wired"), "unwired message = %q", err)
}

func TestModelRoutesHandlerEmptyModelIsInvalidArgument(t *testing.T) {
	c := assert.NewCollecting(t)
	// An unwired server is deliberate: the empty-model check must run first,
	// so a blank request is an argument error rather than Unavailable.
	s := &Server{}
	for _, model := range []string{"", "   "} {
		_, err := s.ModelRoutes(context.Background(),
			connect.NewRequest(&rafikiv1.ModelRoutesRequest{Model: model}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "blank model %q: code", model)
	}
}

func TestModelRoutesHandlerBadBracketIsInvalidArgument(t *testing.T) {
	// Reproduce what the daemon adapter (cmd/rafikid/connect_routeexplainer.go)
	// returns for a malformed line: a ControllerError carrying the parser's
	// message with the ErrInvalidArgs code. The handler must render it
	// InvalidArgument, not the Internal an unclassified error gets.
	_, _, perr := routing.ParseModel("z-ai/glm-5.3[sort=nonsense]")
	if perr == nil {
		t.Fatal("routing.ParseModel accepted a bad sort")
	}
	f := &fakeRouteExplainer{err: &ControllerError{Code: protocol.ErrInvalidArgs, Message: perr.Error()}}
	s := &Server{}
	s.SetRouteExplainer(f)
	_, err := s.ModelRoutes(context.Background(),
		connect.NewRequest(&rafikiv1.ModelRoutesRequest{Model: " z-ai/glm-5.3[sort=nonsense] "}))
	c := assert.NewAborting(t)
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "bad bracket: code")
	c.True(strings.Contains(err.Error(), "unknown sort"), "refusal carries the parser's message, got %q", err)
	c.Eq("z-ai/glm-5.3[sort=nonsense]", f.got, "the handler trims and forwards the model unchanged")
}

func TestSetRouteExplainerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetRouteExplainer(nil)
	assert.NewAborting(t).Nil(s.routeExplainer.Load(), "SetRouteExplainer(nil) stored a pointer")
	_, err := s.ModelRoutes(context.Background(),
		connect.NewRequest(&rafikiv1.ModelRoutesRequest{Model: "deepseek/deepseek-v4.1-flash"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetRouteExplainer(nil): code")
}
