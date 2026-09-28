// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// completeModel returns tab-completion candidates for the --model flag.
//
// It asks the DAEMON, which is the only thing that knows what it can run. The
// previous implementation called models.ListSources locally: that fetched the
// OpenRouter catalog with the client's own credentials and probed ollama and
// LM Studio on the CLIENT's localhost, so against a remote daemon it offered
// the models on your laptop — which that daemon cannot run.
//
// Kind scoping now lives in the daemon too (sourcesForKind in cmd/rafikid): a
// "claude" child resolves only Anthropic ids, and offering it an OpenRouter id
// produces a child that spawns, attaches and then never answers.
func completeModel(cmd *cobra.Command, kind, toComplete string) []string {
	// Inside a routing spec's brackets ("...[sort=p<TAB>") the candidates are
	// grammar, not data — nothing here dials the daemon, so an unreachable
	// daemon still completes a spec, and the base model's id list is
	// irrelevant inside the brackets anyway.
	if open := strings.IndexByte(toComplete, '['); open >= 0 {
		return completeRoutingSpecTail(toComplete[open:])
	}
	return filterByPrefix(modelIDs(cmd, kind), toComplete)
}

// specKeys are the items a routing spec can carry, in ParseSpec's accepted
// spelling and Spec.String's key order; offered each once, since ParseSpec
// refuses a repeated key. specValues are the two value sets that are grammar
// too: sort= takes one of ParseSpec's four spellings (the parse error's
// price|throughput|latency|balanced order), quant= names OpenRouter's
// quantizations (pkg/routing/spec.go's quantTiers plus "unknown"; a caller
// appends "+" for a floor). only= takes provider slugs, which only the
// daemon's catalog knows, so nothing is offered for it — these tables mirror
// pkg/routing's grammar, and the daemon is the validator either way.
var (
	specKeys   = []string{"sort=", "quant=", "only=", "nodata", "zdr"}
	specValues = map[string][]string{
		"sort":  {"price", "throughput", "latency", "balanced"},
		"quant": {"int4", "fp4", "mxfp4", "nvfp4", "fp6", "int8", "fp8", "mxfp8", "fp16", "bf16", "fp32", "unknown"},
	}
)

// completeRoutingSpecTail returns completion candidates for the inside of a
// model string's routing brackets. tail begins at the "[" (everything before
// it is base id). Client-side by design — a completion must never dial the
// daemon, never exit, never block past the completion deadline, never print.
func completeRoutingSpecTail(tail string) []string {
	inner := tail[1:]
	if strings.Contains(inner, "]") {
		return nil // the spec is closed; nothing left to offer
	}
	items := strings.Split(inner, ",")
	last := items[len(items)-1]
	if key, val, hasVal := strings.Cut(last, "="); hasVal {
		return prefixFilter(specValues[key], val)
	}
	used := make(map[string]bool)
	for _, item := range items[:len(items)-1] {
		usedKey, _, _ := strings.Cut(item, "=")
		used[usedKey] = true
	}
	out := make([]string, 0, len(specKeys))
	for _, k := range specKeys {
		if used[strings.TrimSuffix(k, "=")] || !strings.HasPrefix(k, last) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// prefixFilter returns the subset of candidates starting with prefix, in the
// candidates' own order (grammar order here, unlike the sorted id lists
// filterByPrefix serves).
func prefixFilter(candidates []string, prefix string) []string {
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// filterByPrefix returns the sorted subset of ids starting with prefix.
// Shared by every completion function that offers ids fetched from the
// daemon (models, executors) so they can never drift in how they filter.
func filterByPrefix(ids []string, prefix string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// modelIDs returns every model id the daemon offers for kind, cached.
//
// The cache is keyed by KIND as well as endpoint: the two kinds have different
// model universes, and serving a claude completion from the fundi cache offers
// ids Claude Code cannot resolve.
func modelIDs(cmd *cobra.Command, kind string) []string {
	if kind == "" {
		kind = "fundi"
	}
	cacheKind := "models-" + kind

	var ids []string
	if cacheRead(cacheKind, completionEndpointKey(cmd), modelCacheTTL, &ids) {
		return ids
	}
	rows, err := fetchModelRows(cmd, "", kind)
	if err != nil {
		return nil
	}
	ids = make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GetId())
	}
	cacheWrite(cacheKind, completionEndpointKey(cmd), ids)
	return ids
}

// fetchModelRows asks the daemon for its model rows. Shared by completion and
// by `rafiki models`, which passes its own filters and ignores the cache.
func fetchModelRows(cmd *cobra.Command, provider, kind string) ([]*rafikiv1.ModelRow, error) {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()

	resp, err := ep.control().ListModels(ctx,
		connect.NewRequest(&rafikiv1.ListModelsRequest{Provider: provider, Kind: kind}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetModels(), nil
}
