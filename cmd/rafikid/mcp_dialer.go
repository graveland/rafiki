// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// mcpDialer resolves every via_executor MCP server this child may use ONCE, at
// spawn, and closes the chosen executor IDs into the dialer (D10's "resolved
// once" rule, the same shape as preset resolution).
//
// Resolving once is what makes a config error a SPAWN error instead of a
// silently toolless child: a via_executor entry with no executor behind it is a
// deploy mistake, and degrading it into "no tools" is invisible. Per-server
// dial failures AFTER this point stay skip-and-redial, as every MCP server's
// do.
//
// A child with no via_executor entries gets a nil dialer and no executor lookup
// at all — every ordinary mcp.json behaves exactly as before.
func (c *Controller) mcpDialer(ro fundi.RuntimeOptions) (tools.MCPDialer, error) {
	if ro.NoMCP || ro.MCPConfig == "" {
		return nil, nil
	}
	return resolveMCPDialer(ro.MCPConfig, ro.MCPServers, c.execPoolConn)
}

// resolveMCPDialer does the resolution: it reads the child's mcp.json, keeps the
// servers this child's allowlist admits, and picks an executor for each
// via_executor entry.
//
// Failures are specific and fail closed:
//   - a malformed selector EXCLUDES rather than admits, so it is an error
//     naming the server;
//   - no live executor advertises the name at all says so, and how to declare
//     it (the laptop has not been updated);
//   - an executor advertises the name but none matching the selector says
//     THAT, which is a selector problem rather than a missing flag.
func resolveMCPDialer(cfgPath, allowlist string, pool *execpool.Pool) (tools.MCPDialer, error) {
	resolved, err := resolveMCPEntries(cfgPath, allowlist, pool)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, nil
	}
	return func(ctx context.Context, name string, sc tools.MCPServerConfig) (mcp.Transport, error) {
		r, ok := resolved[name]
		if !ok {
			// The dialer only ever serves entries resolved above, so this is a
			// daemon bug rather than a config problem — say so plainly rather
			// than dialling somewhere unvetted.
			return nil, fmt.Errorf("mcp server %q: not resolved for this child at spawn", name)
		}
		switch {
		case r.via.Proxy != "":
			// One flag covers "an HTTP endpoint this executor will forward
			// to"; the executor neither knows nor cares that this one speaks
			// MCP. The origin here is synthetic: the executor rebuilds the real
			// URL from the base its operator declared with --proxy, so no
			// endpoint address travels from the daemon.
			return &mcp.StreamableClientTransport{
				Endpoint: "http://" + r.via.Proxy,
				HTTPClient: &http.Client{Transport: &mcpHeaderRoundTripper{
					headers: sc.Headers,
					next:    execpool.NewProxyTransport(pool, r.executorID, r.via.Proxy),
				}},
			}, nil
		case r.via.Command != "":
			return execpool.NewMCPStdioTransport(pool, r.executorID, r.via.Command), nil
		}
		return nil, fmt.Errorf("mcp server %q: via_executor names neither a command nor a proxy", name)
	}, nil
}

// resolveMCPEntries is the resolution half, split out so a test can assert on
// WHICH executor each server was bound to rather than only that a dialer came
// back.
func resolveMCPEntries(cfgPath, allowlist string, pool *execpool.Pool) (map[string]resolvedMCP, error) {
	cfg, err := tools.LoadMCPConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	usable := tools.FilterMCPServers(cfg.MCPServers, allowlist)
	if len(cfg.ViaExecutorNames()) == 0 {
		return nil, nil
	}

	resolved := make(map[string]resolvedMCP, len(usable))
	for name, sc := range usable {
		ve := sc.ViaExecutor
		if ve == nil {
			continue
		}
		executorID, err := resolveMCPExecutor(name, ve, pool)
		if err != nil {
			return nil, err
		}
		resolved[name] = resolvedMCP{executorID: executorID, via: *ve}
		slog.Debug("mcp: resolved via_executor server", "server", name, "executor", executorID,
			"command", ve.Command, "proxy", ve.Proxy)
	}
	// An entry this child's allowlist excludes is not this spawn's business: it
	// is logged and skipped, never allowed to refuse the spawn, exactly as an
	// unreachable via_executor provider is.
	for _, name := range cfg.ViaExecutorNames() {
		if _, ok := usable[name]; !ok {
			slog.Warn("mcp: via_executor server excluded by this child's mcp-servers allowlist; not resolved",
				"server", name, "allowlist", allowlist)
		}
	}
	return resolved, nil
}

// resolvedMCP is one via_executor entry bound to the executor that hosts it.
type resolvedMCP struct {
	executorID string
	via        tools.MCPViaExecutor
}

// resolveMCPExecutor picks the executor for one via_executor entry: the lowest
// ID among live executors whose labels match the selector AND which advertises
// the declared name. Determinism matters — two live executors can satisfy one
// selector, and a child's server must land on the same machine twice in a row.
//
// A child's OWN executor gets no preference (D10): coupling "child is bound at
// A, server is advertised at B" would fail in a way that reads as a puzzle
// rather than a message.
func resolveMCPExecutor(name string, ve *tools.MCPViaExecutor, pool *execpool.Pool) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("mcp server %q: via_executor requires the executor plane (set RAFIKI_EXECUTORS_ENABLED=1 and RAFIKI_DB)", name)
	}
	sel, err := executors.ParseSelector(ve.Selector)
	if err != nil {
		return "", fmt.Errorf("mcp server %q: via_executor.selector %q: %w", name, ve.Selector, err)
	}

	var matching []string
	sawAdvertised := false
	for _, le := range pool.Live() {
		if !advertisesMCPName(le, ve) {
			continue
		}
		sawAdvertised = true
		if sel.Matches(le.Executor.Labels) {
			matching = append(matching, le.Executor.ID)
		}
	}
	if len(matching) > 0 {
		sort.Strings(matching)
		return matching[0], nil
	}
	if !sawAdvertised {
		if ve.Proxy != "" {
			return "", fmt.Errorf("mcp server %q: no live executor matching selector %q advertises proxy %q (start one with --proxy %s=…)",
				name, ve.Selector, ve.Proxy, ve.Proxy)
		}
		return "", fmt.Errorf("mcp server %q: no live executor matching selector %q declares MCP command %q (start it with --mcp-command %s=…)",
			name, ve.Selector, ve.Command, ve.Command)
	}
	if ve.Proxy != "" {
		return "", fmt.Errorf("mcp server %q: no executor matching selector %q advertises proxy %q", name, ve.Selector, ve.Proxy)
	}
	return "", fmt.Errorf("mcp server %q: no executor matching selector %q declares MCP command %q", name, ve.Selector, ve.Command)
}

// advertisesMCPName reports whether a live executor self-reported the name this
// entry needs — a command for a stdio server, a proxy for a URL one.
func advertisesMCPName(le execpool.LiveExecutor, ve *tools.MCPViaExecutor) bool {
	if ve.Proxy != "" {
		for _, p := range le.Proxies {
			if p == ve.Proxy {
				return true
			}
		}
		return false
	}
	for _, c := range le.MCPCommands {
		if c == ve.Command {
			return true
		}
	}
	return false
}

// mcpHeaderRoundTripper applies a via_executor entry's headers (an
// Authorization, say) and hands the request to the executor relay. The same
// disclosure a keyed provider's entry already documents: those headers travel
// over the relay, because they are the server's own credential and the daemon
// is the one that holds it.
type mcpHeaderRoundTripper struct {
	headers map[string]string
	next    http.RoundTripper
}

func (t *mcpHeaderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return t.next.RoundTrip(req)
}
