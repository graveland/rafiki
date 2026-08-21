// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
)

// relayTransport returns the HTTP transport for one provider: nil for a
// provider the daemon dials directly (no via_executor), and an executor-relay
// RoundTripper for one that has it.
//
// No matching executor is an ERROR, never a nil transport. A nil would fall
// through to a direct dial of base_url — which for a typical via_executor
// provider (a local server reachable only from the executor's own machine)
// means the DAEMON dialing its own localhost: it either refuses or reaches
// something unrelated, and either way the failure looks nothing like what
// actually went wrong. Failing the turn here, by name, is what
// docs/reference/executor-protocol.md's confinement postmortems all come back
// to: a guard that silently degrades into the wrong resource is worse than one
// that refuses.
func relayTransport(p providers.Provider, pool *execpool.Pool) (http.RoundTripper, error) {
	if p.ViaExecutor == nil {
		return nil, nil
	}
	if pool == nil {
		return nil, fmt.Errorf("provider %q: via_executor requires the executor plane (set RAFIKI_EXECUTORS_ENABLED=1 and RAFIKI_DB)", p.Name)
	}
	sel, err := executors.ParseSelector(p.ViaExecutor.Selector)
	if err != nil {
		// A malformed selector EXCLUDES rather than admits, matching every
		// other selector this daemon evaluates (effectiveExecutorSet,
		// Executor.Admits).
		return nil, fmt.Errorf("provider %q: via_executor.selector %q: %w", p.Name, p.ViaExecutor.Selector, err)
	}

	var sawProxy bool
	for _, le := range pool.Live() {
		if !hasProxy(le.Proxies, p.ViaExecutor.Proxy) {
			continue
		}
		sawProxy = true
		if sel.Matches(le.Executor.Labels) {
			return execpool.NewProxyTransport(pool, le.Executor.ID, p.ViaExecutor.Proxy), nil
		}
	}
	if !sawProxy {
		return nil, fmt.Errorf("provider %q: no live executor matching selector %q advertises proxy %q (start one with --proxy %s=%s)",
			p.Name, p.ViaExecutor.Selector, p.ViaExecutor.Proxy, p.ViaExecutor.Proxy, p.BaseURL)
	}
	return nil, fmt.Errorf("provider %q: no executor matching selector %q advertises proxy %q",
		p.Name, p.ViaExecutor.Selector, p.ViaExecutor.Proxy)
}

// hasProxy reports whether names contains proxyName.
func hasProxy(names []string, proxyName string) bool {
	for _, n := range names {
		if n == proxyName {
			return true
		}
	}
	return false
}

// providerSenders resolves relayTransport for every provider in set that
// declares via_executor and builds each one's sender. It iterates the whole
// registry rather than just the provider this child's Model names — see
// RuntimeOptions.ProviderSenders' doc comment — so a via_executor provider
// with no live executor fails every spawn while the registry carries it, not
// only spawns naming it. That is the accepted trade for resolving the relay
// once at client-construction time instead of on every send (recorded in the
// design doc's Scope section).
//
// A registry with no via_executor providers at all — providers.Default(), and
// every providers.toml until an operator opts in — returns a nil map: no
// executor lookup, no error, no behaviour change.
func providerSenders(set *providers.Set, pool *execpool.Pool) (map[string]llm.Sender, error) {
	if set == nil {
		return nil, nil
	}
	var out map[string]llm.Sender
	for _, name := range set.Names() {
		p, ok := set.Get(name)
		if !ok || p.ViaExecutor == nil {
			continue
		}
		rt, err := relayTransport(p, pool)
		if err != nil {
			return nil, err
		}
		sender, err := llm.SenderFor(p, rt)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		if out == nil {
			out = make(map[string]llm.Sender, 1)
		}
		out[name] = sender
	}
	return out, nil
}
