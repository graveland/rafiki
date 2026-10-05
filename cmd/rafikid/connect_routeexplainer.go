// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
)

// connectRouteExplainer adapts the OpenRouter endpoint catalog, the model-page
// stats scraper and the daemon's ONE ProviderGuard to connectapi.RouteExplainer.
// It is pure read-and-join: it never writes state and never runs on a request
// path. The guard is the same instance the request paths consult, so a ban
// shown here is the ban that would actually apply.
//
// A nil stats source answers with no stats at all (stats_note empty); this is
// how a daemon with no measured-speed source — or a test that doesn't care —
// still gets a full routing explanation.
type connectRouteExplainer struct {
	cat   *routing.EndpointCatalog
	stats routing.StatsSource
	guard *routing.ProviderGuard

	// now is the clock IgnoredFor reads; nil means time.Now. Injectable so a
	// test with a TTL-less ban doesn't depend on wall time.
	now func() time.Time
}

// ExplainModelRoutes implements connectapi.RouteExplainer. Errors it returns
// are already classified: a malformed model line is a ControllerError
// (ErrInvalidArgs), and a catalog fetch failure is a connect.Error
// (Unavailable); connectapi.modelRoutesError passes both through.
func (c connectRouteExplainer) ExplainModelRoutes(ctx context.Context, model string) (*rafikiv1.ModelRoutesResponse, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "openrouter/")
	base, spec, err := routing.ParseModel(model)
	if err != nil {
		// The model line is the caller's input, and the parser's message names
		// the bad item: author it and classify it InvalidArgument.
		return nil, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}

	eps, stale, err := c.cat.Endpoints(ctx, base)
	if err != nil {
		// The catalog is an upstream dependency; Unavailable is the honest
		// code. connect.NewError forwards err.Error() in the wire message, so
		// the caller sees the upstream fetch failure; a retry may help.
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}

	var stats []routing.EndpointStats
	var note string
	if c.stats != nil {
		stats, err = c.stats.Stats(ctx, base)
		if err != nil {
			// Stats are advisory: a scrape failure renders as a dash in the
			// note, never as a failed routing answer.
			note, stats = err.Error(), nil
		}
	}

	now := time.Now
	if c.now != nil {
		now = c.now
	}
	rows := routing.ExplainRoutes(eps, stats, spec, c.guard.IgnoredFor(now(), base))

	resp := &rafikiv1.ModelRoutesResponse{
		Model:     base,
		Routing:   spec.String(),
		Stale:     stale,
		StatsNote: note,
		Endpoints: make([]*rafikiv1.RouteEndpoint, 0, len(rows)),
	}
	for _, row := range rows {
		resp.Endpoints = append(resp.Endpoints, toProtoRouteEndpoint(row))
	}
	return resp, nil
}

// toProtoRouteEndpoint maps one ExplainRow onto the wire type, preserving
// absence on every optional field: a price or a measured stat the
// catalog/scrape could not report is omitted, never written as zero, so a free
// price stays distinguishable from an unknown one.
func toProtoRouteEndpoint(r routing.ExplainRow) *rafikiv1.RouteEndpoint {
	out := &rafikiv1.RouteEndpoint{
		Provider:             r.Endpoint.Provider,
		Slug:                 r.Endpoint.Slug,
		Tag:                  r.Endpoint.Tag,
		Quantization:         r.Endpoint.Quantization,
		PromptUsdPerMtok:     usdPerMTok(r.Endpoint.PromptPrice),
		CompletionUsdPerMtok: usdPerMTok(r.Endpoint.CompletionPrice),
		ContextLength:        int32(r.Endpoint.ContextLength),
		Uptime_30M:           r.Endpoint.Uptime30m,
		Tools:                r.Endpoint.Tools,
		Eligible:             r.Eligible,
		ExcludedReason:       string(r.Reason),
		Rank:                 int32(r.Rank),
		Preferred:            r.Preferred,
	}
	if r.Stats != nil {
		out.P50TokensPerSec = statOrNil(r.Stats.P50Throughput)
		out.P90TokensPerSec = statOrNil(r.Stats.P90Throughput)
		if r.Stats.P50LatencyMs >= 0 {
			out.P50Latency = durationpb.New(time.Duration(r.Stats.P50LatencyMs * float64(time.Millisecond)))
		}
		if r.Stats.Requests >= 0 {
			v := int32(r.Stats.Requests)
			out.StatsRequests = &v
		}
	}
	return out
}

// usdPerMTok converts a USD-per-token price, as OpenRouter reports it, into the
// USD-per-million-tokens unit the wire uses. A -1 (unknown) price is absent.
func usdPerMTok(perToken float64) *float64 {
	if perToken < 0 {
		return nil
	}
	v := perToken * 1e6
	return &v
}

// statOrNil converts a stats field's -1 "absent" sentinel to an absent wire
// field. Every other value, zero included, is reported.
func statOrNil(v float64) *float64 {
	if v < 0 {
		return nil
	}
	out := v
	return &out
}
