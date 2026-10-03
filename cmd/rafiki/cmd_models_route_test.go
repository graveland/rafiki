// SPDX-License-Identifier: Apache-2.0

package main

// The `rafiki models route` verb rides the Connect control plane's ModelRoutes
// RPC through newConnectEndpoint, with the profile's token as usual. These
// tests drive the real command against an in-process Connect handler served on
// a socket profile's own socket (serveRouteScratch's shape, for ModelRoutes),
// so the argument's pass-through and the rendered table/JSON are pinned on the
// wire.

import (
	"context"
	"encoding/json"
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

// modelsRouteStub serves ModelRoutes and records the request's model. resp is
// the canned response the daemon would compute.
type modelsRouteStub struct {
	rafikiv1connect.UnimplementedControlHandler
	mu   sync.Mutex
	last *connect.Request[rafikiv1.ModelRoutesRequest]
	resp *rafikiv1.ModelRoutesResponse
	err  error
}

func (s *modelsRouteStub) ModelRoutes(
	_ context.Context, req *connect.Request[rafikiv1.ModelRoutesRequest],
) (*connect.Response[rafikiv1.ModelRoutesResponse], error) {
	s.mu.Lock()
	s.last = req
	resp, err := s.resp, s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = &rafikiv1.ModelRoutesResponse{}
	}
	return connect.NewResponse(resp), nil
}

func (s *modelsRouteStub) lastRequest() *connect.Request[rafikiv1.ModelRoutesRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *modelsRouteStub) setResponse(resp *rafikiv1.ModelRoutesResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resp = resp
}

// serveModelsRouteScratch seeds one socket profile "it" at a short temp dir and
// serves the ModelRoutes stub on the profile's own socket — the path
// newConnectEndpoint dials. The dir must be SHORT: unix socket paths cap at
// ~104 bytes.
func serveModelsRouteScratch(t *testing.T, stub *modelsRouteStub) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-mr")
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)
	writeTokenedProfile(t, sock, "")
}

// tpsEndpoint builds one eligible endpoint carrying only the fields a test
// cares about; absent optionals stay nil, so the display's absence handling is
// exercised by construction rather than by convention.
func tpsEndpoint(provider string, rank int32, p50 *float64) *rafikiv1.RouteEndpoint {
	return &rafikiv1.RouteEndpoint{
		Provider:         provider,
		Slug:             strings.ToLower(provider),
		Quantization:     "fp8",
		Eligible:         true,
		Rank:             rank,
		P50TokensPerSec:  p50,
		PromptUsdPerMtok: f64Ptr(0.15),
	}
}

// TestModelsRouteRendersEligibleSortedByTPS drives the default table: eligible
// rows ordered tps:desc, the header naming the model and spec, and the excluded
// rows hidden behind a footer count.
func TestModelsRouteRendersEligibleSortedByTPS(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	stub.setResponse(&rafikiv1.ModelRoutesResponse{
		Model:   "deepseek/deepseek-v4.1-flash",
		Routing: "sort=price",
		Endpoints: []*rafikiv1.RouteEndpoint{
			tpsEndpoint("FastHost", 1, f64Ptr(120)),
			tpsEndpoint("SlowHost", 2, f64Ptr(40)),
			{
				Provider:        "ExcludedHost",
				Eligible:        false,
				ExcludedReason:  "quant",
				P50TokensPerSec: f64Ptr(999),
			},
		},
	})

	root, out := userTestRoot(t, newModelsRouteCmd(), "route", "deepseek/deepseek-v4.1-flash[sort=price]")
	c.NoError(root.Execute(), "models route failed: %v\n%s", out.String(), out.String())
	table := out.String()

	c.StrContains(table, "deepseek/deepseek-v4.1-flash  spec: sort=price", "header line:\n%s", table)
	for _, h := range []string{"PROVIDER", "QUANT", "TPS", "P90", "LAT ms", "IN $/M", "OUT $/M", "UPTIME %", "#", "NOTE"} {
		c.StrContains(table, h, "table missing %q:\n%s", h, table)
	}
	// Default is tps:desc: the 120 tok/s host precedes the 40.
	fast := strings.Index(table, "FastHost")
	slow := strings.Index(table, "SlowHost")
	c.True(fast >= 0 && slow >= 0 && fast < slow, "tps:desc order wrong (FastHost=%d SlowHost=%d):\n%s", fast, slow, table)

	c.NotStrContains(table, "ExcludedHost", "an excluded row rendered without --all:\n%s", table)
	c.StrContains(table, "1 endpoint(s) excluded by this spec (--all to show)", "excluded footer:\n%s", table)
}

// TestModelsRouteSortAbsentLastBothDirections pins the modelquery absence rule:
// an endpoint whose throughput was never measured sorts LAST whether the order
// is descending or ascending — absence is not the smallest value.
func TestModelsRouteSortAbsentLastBothDirections(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	stub.setResponse(&rafikiv1.ModelRoutesResponse{
		Model: "m/x",
		Endpoints: []*rafikiv1.RouteEndpoint{
			tpsEndpoint("Ten", 1, f64Ptr(10)),
			tpsEndpoint("Unmeasured", 3, nil),
			tpsEndpoint("Thirty", 2, f64Ptr(30)),
		},
	})

	root, out := userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "--sort", "tps:desc")
	c.NoError(root.Execute(), "tps:desc failed: %v\n%s", out.String(), out.String())
	table := out.String()
	c.True(strings.Index(table, "Thirty") < strings.Index(table, "Ten") &&
		strings.Index(table, "Ten") < strings.Index(table, "Unmeasured"),
		"tps:desc order wrong, want Thirty<Ten<Unmeasured:\n%s", table)

	root, out = userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "--sort", "tps:asc")
	c.NoError(root.Execute(), "tps:asc failed: %v\n%s", out.String(), out.String())
	table = out.String()
	c.True(strings.Index(table, "Ten") < strings.Index(table, "Thirty") &&
		strings.Index(table, "Thirty") < strings.Index(table, "Unmeasured"),
		"tps:asc order wrong, want Ten<Thirty<Unmeasured (absent still last):\n%s", table)
}

// TestModelsRouteHidesExcludedUnlessAll pins --all: by default only eligible
// rows render and the footer counts the hidden ones; --all adds them with an
// empty rank and their exclusion reason, and drops the footer.
func TestModelsRouteHidesExcludedUnlessAll(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	stub.setResponse(&rafikiv1.ModelRoutesResponse{
		Model: "m/x",
		Endpoints: []*rafikiv1.RouteEndpoint{
			tpsEndpoint("Eligible", 1, f64Ptr(50)),
			{Provider: "NotInOnly", Eligible: false, ExcludedReason: "not-in-only"},
			{Provider: "Banned", Eligible: false, ExcludedReason: "banned"},
		},
	})

	root, out := userTestRoot(t, newModelsRouteCmd(), "route", "m/x")
	c.NoError(root.Execute(), "default failed: %v\n%s", out.String(), out.String())
	table := out.String()
	c.StrContains(table, "Eligible", "eligible row missing:\n%s", table)
	c.NotStrContains(table, "NotInOnly", "excluded row shown without --all:\n%s", table)
	c.StrContains(table, "2 endpoint(s) excluded by this spec (--all to show)", "footer:\n%s", table)

	root, out = userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "--all")
	c.NoError(root.Execute(), "--all failed: %v\n%s", out.String(), out.String())
	table = out.String()
	for _, want := range []string{"NotInOnly", "Banned", "not-in-only", "banned"} {
		c.StrContains(table, want, "--all table missing %q:\n%s", want, table)
	}
	c.NotStrContains(table, "excluded by this spec", "--all printed a hidden-rows footer:\n%s", table)
}

// TestModelsRouteJSONIsProtojsonOfResponse pins the -j/-J contract: -j is the
// response's canonical protojson (int64-as-string, optionals omitted when
// absent), -J one endpoint per line with no envelope.
func TestModelsRouteJSONIsProtojsonOfResponse(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	stub.setResponse(&rafikiv1.ModelRoutesResponse{
		Model:   "m/x",
		Routing: "sort=price",
		Endpoints: []*rafikiv1.RouteEndpoint{
			{
				Provider:             "HasStats",
				Slug:                 "hasstats",
				Quantization:         "fp8",
				Eligible:             true,
				Rank:                 1,
				PromptUsdPerMtok:     f64Ptr(0.15),
				CompletionUsdPerMtok: f64Ptr(0.30),
				Uptime_30M:           f64Ptr(0.98),
				P50TokensPerSec:      f64Ptr(120),
				StatsRequests:        i32Ptr(42),
			},
			{Provider: "NoStats", Eligible: true, Rank: 2},
		},
	})

	// -j: the whole response, protojson field names (camelCase).
	root, out := userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "-j")
	c.NoError(root.Execute(), "-j failed: %v\n%s", out.String(), out.String())
	var resp map[string]any
	c.NoError(json.Unmarshal(out.Bytes(), &resp), "-j printed non-JSON:\n%s", out.String())
	c.Eq("m/x", resp["model"], "model")
	c.Eq("sort=price", resp["routing"], "routing")
	eps, ok := resp["endpoints"].([]any)
	c.True(ok && len(eps) == 2, "endpoints = %v, want two", resp["endpoints"])
	first, _ := eps[0].(map[string]any)
	c.Eq("HasStats", first["provider"], "first provider")
	price, ok := first["promptUsdPerMtok"].(float64)
	c.True(ok, "price is a protojson number, got %T %v", first["promptUsdPerMtok"], first["promptUsdPerMtok"])
	c.True(price > 0.1499 && price < 0.1501, "price = %v, want 0.15 (USD per million, not per token)", price)
	// The absence rule is what matters: NoStats carries no measured keys at all.
	second, _ := eps[1].(map[string]any)
	c.NotHasKey(second, "p50TokensPerSec", "an absent measurement was serialised:\n%v", second)
	c.NotHasKey(second, "uptime30m", "an absent uptime was serialised:\n%v", second)

	// -J: one compact endpoint per line, unwrapped.
	root, out = userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "-J")
	c.NoError(root.Execute(), "-J failed: %v\n%s", out.String(), out.String())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.Require().Len(lines, 2, "-J printed %d lines, want one per endpoint:\n%s", len(lines), out.String())
	var line map[string]any
	c.NoError(json.Unmarshal([]byte(lines[0]), &line), "JSONL line %q", lines[0])
	c.Eq("HasStats", line["provider"], "JSONL provider")
}

// TestModelsRouteUnknownSortKeyIsAnError pins the fast-fail: an unknown --sort
// field (or direction) is a user error naming the valid set, rejected before
// any daemon round trip.
func TestModelsRouteUnknownSortKeyIsAnError(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	root, _ := userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "--sort", "bogus")
	err := root.Execute()
	c.Error(err, "an unknown --sort field was accepted")
	c.StrContains(err.Error(), "unknown sort field", "error text: %v", err)
	c.StrContains(err.Error(), "tps", "error must name the valid fields: %v", err)
	c.Nil(stub.lastRequest(), "the CLI dialled the daemon for a bad --sort")

	root, _ = userTestRoot(t, newModelsRouteCmd(), "route", "m/x", "--sort", "tps:sideways")
	err = root.Execute()
	c.Error(err, "an unknown --sort direction was accepted")
	c.StrContains(err.Error(), "asc", "error text: %v", err)
}

// TestModelsRouteBracketArgIsPassedThrough pins the argument contract: the
// model string (bracket included) reaches the daemon VERBATIM — the server
// parses the spec — and it does so through the models command's own dispatch
// (cobra resolves the `route` subcommand ahead of the parent's NoArgs), while a
// stray argument to `models` itself is still refused.
func TestModelsRouteBracketArgIsPassedThrough(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	arg := "deepseek/deepseek-v4.1-flash[sort=price,quant=fp8+]"
	root, out := userTestRoot(t, newModelsCmd(), "models", "route", arg)
	c.NoError(root.Execute(), "models route failed: %v\n%s", out.String(), out.String())
	req := stub.lastRequest()
	c.NotNil(req, "the stub never saw a ModelRoutes request")
	c.Eq(arg, req.Msg.GetModel(), "the client did not pass the bracket argument through verbatim")

	// The parent still resolves the subcommand and still validates its own args.
	modelsCmd := newModelsCmd()
	sub, _, err := modelsCmd.Find([]string{"route"})
	c.NoError(err, "models.Find(route)")
	c.Eq("route", sub.Name(), "`route` did not resolve to the subcommand")
	c.Error(modelsCmd.Args(modelsCmd, []string{"bogus"}), "`models bogus` must still be refused")
	c.NoError(modelsCmd.Args(modelsCmd, nil), "bare `models` must still be accepted")
}

// TestModelsRouteStaleAndStatsNote pins the two advisory notes: a stale
// endpoint list is named in the header, and an unavailable stats scrape is a
// stderr line (never an error), leaving the table itself intact.
func TestModelsRouteStaleAndStatsNote(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &modelsRouteStub{}
	serveModelsRouteScratch(t, stub)

	stub.setResponse(&rafikiv1.ModelRoutesResponse{
		Model:     "m/x",
		Stale:     true,
		StatsNote: "model page for m/x: unavailable (last fetch failed)",
		Endpoints: []*rafikiv1.RouteEndpoint{tpsEndpoint("Host", 1, f64Ptr(10))},
	})

	root, out := userTestRoot(t, newModelsRouteCmd(), "route", "m/x")
	c.NoError(root.Execute(), "failed: %v\n%s", out.String(), out.String())
	text := out.String()
	c.StrContains(text, "(cached list is stale)", "stale marker missing:\n%s", text)
	c.StrContains(text, "stats unavailable: model page for m/x: unavailable (last fetch failed)", "stats note missing:\n%s", text)
	c.StrContains(text, "Host", "the table was not rendered alongside the notes:\n%s", text)
}
