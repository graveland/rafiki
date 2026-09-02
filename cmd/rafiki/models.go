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
	ids := modelIDs(cmd, kind)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(id, toComplete) {
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
