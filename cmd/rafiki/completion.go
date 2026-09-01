// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
)

// completionDeadline bounds a completion RPC. A shell is blocked while this
// runs, so the budget is small and a miss is an acceptable outcome — the old
// client-side path used the same 2s reasoning for its OpenRouter fetch.
const completionDeadline = 2 * time.Second

// completionChild is the slice of ChildSummary completion actually uses. A
// local struct rather than the proto type because it is what gets cached, and
// a cache file should not be a generated wire shape it must stay in sync with.
type completionChild struct {
	ChildID string            `json:"childId"`
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// completionEndpointKey names the endpoint a cached answer came from, so two
// daemons' answers never share a file. It reads the environment directly
// rather than resolving a full endpoint: this runs even when
// newConnectEndpoint would refuse (no token), and a refusal still needs a
// stable key for the drop path.
func completionEndpointKey() string {
	if u := remoteDialURL(); u != "" {
		return u
	}
	return "unix:" + paths.ConnectSocketPath()
}

// completionChildrenCached returns the cached rows, or nil on any miss.
func completionChildrenCached(ttl time.Duration) []completionChild {
	var out []completionChild
	if cacheRead("children", completionEndpointKey(), ttl, &out) {
		return out
	}
	return nil
}

// dropChildCompletionCache invalidates the child cache. Called by every verb
// that changes the child set — create, kill, close, label — which is what
// makes childCacheTTL safe at 15s: the staleness a user notices is the one
// they just caused.
func dropChildCompletionCache() { cacheDrop("children", completionEndpointKey()) }

// completionChildren returns every child the daemon knows, cached.
//
// It goes through newConnectEndpoint rather than dialing a socket directly.
// That is the whole fix: the previous implementation called
// client.Dial(socketFromCmd(cmd)) and never consulted RAFIKI_URL, so with a
// remote daemon it dialed a socket that was not there and returned nil —
// silently, because completion swallows every error by design.
//
// Every failure yields no candidates. A completion handler must never exit,
// never block long, and never print.
func completionChildren(cmd *cobra.Command) []completionChild {
	if rows := completionChildrenCached(childCacheTTL); rows != nil {
		return rows
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil // remote with no token: a 401 is the only possible outcome
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()

	resp, err := ep.control().ListChildren(ctx,
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		return nil
	}
	out := make([]completionChild, 0, len(resp.Msg.GetChildren()))
	for _, ch := range resp.Msg.GetChildren() {
		out = append(out, completionChild{
			ChildID: ch.GetChildId(),
			Name:    ch.GetName(),
			Status:  ch.GetStatus(),
			Labels:  ch.GetLabels(),
		})
	}
	cacheWrite("children", completionEndpointKey(), out)
	return out
}
