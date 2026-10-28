// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
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

// providerSenderCreds is the slice of *providerCreds providerSenders reads: the
// availability gate, one owner's token source, and that owner's per-model facts.
//
// It is an interface rather than the concrete pointer so a test can hand it a
// fake that records which owner each sender was built for: a built sender is a
// pkg/llm value whose token source cannot be read back out, and the property
// that matters here (owner A's sender spends A's bearer, never B's) is
// otherwise unobservable.
type providerSenderCreds interface {
	Available() (bool, string)
	TokenSourceFor(owner, provider string) llm.TokenSource
	ModelFactsFor(owner, provider string) llm.ModelFacts
}

var _ providerSenderCreds = (*providerCreds)(nil)

// providerSenders resolves a sender for every provider in set that needs one
// built here rather than from the daemon's environment.
//
// Two kinds qualify:
//
//   - via_executor providers (below), whose relay it resolves once, at
//     client-construction time, rather than per-send. An executor arriving
//     after the child's client is built stays invisible to that child until it
//     is rebuilt (recorded in the design doc's Scope section). It is scoped to
//     the providers THIS spawn's model can actually reach: the model's own
//     provider plus that provider's configured Fallback chain
//     (reachableProviders). A via_executor provider outside that set with no
//     live executor is unrelated to this spawn: it is logged and skipped, not
//     treated as a spawn-ending error. A provider inside the reachable set
//     behaves as before, so no live executor fails the spawn.
//   - openai-responses-chatgpt providers, which authenticate as the OWNING USER
//     and so can only be built when this spawn has an owner and the daemon's
//     credential store is usable. The sender it builds is bound to that one
//     owner's token source and that owner's model facts; nothing is shared
//     between two owners' senders, not even a cookie jar. An ownerless spawn
//     gets no sender at all for such a provider, and Spawn refuses it up front
//     (checkLoginProviderUsable) rather than letting the first turn discover it.
//
// A registry that needs neither (providers.Default(), and every providers.toml
// until an operator opts in) returns a nil map: no lookup, no error, no
// behaviour change.
func providerSenders(set *providers.Set, pool *execpool.Pool, model, owner string, creds providerSenderCreds) (map[string]llm.Sender, error) {
	if set == nil {
		return nil, nil
	}
	reachable, err := reachableProviders(set, model)
	if err != nil {
		return nil, err
	}
	var out map[string]llm.Sender
	for _, name := range set.Names() {
		p, ok := set.Get(name)
		if !ok {
			continue
		}
		if p.Kind == providers.KindOpenAIResponsesChatGPT {
			sender, why, err := chatGPTSenderFor(p, owner, creds)
			if err != nil {
				return nil, err
			}
			if sender == nil {
				if reachable[name] {
					slog.Warn("ChatGPT-login provider has no usable sender for this spawn; it will be unavailable to the child",
						"provider", name, "model", model, "owner", owner, "reason", why)
				}
				continue
			}
			if out == nil {
				out = make(map[string]llm.Sender, 1)
			}
			out[name] = sender
			continue
		}
		if p.ViaExecutor == nil {
			continue
		}
		rt, err := relayTransport(p, pool)
		if err != nil {
			if !reachable[name] {
				slog.Warn("via_executor provider has no live executor; skipping it (not reachable by this spawn's model)",
					"provider", name, "model", model, "error", err)
				continue
			}
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

// chatGPTSenderFor builds one owner's sender for a ChatGPT-login provider. A
// nil sender with a nil error means there is nothing to build for this spawn
// (no owner, no credential machinery, or unavailable credential machinery), and
// why says which. p.ViaExecutor and p.BaseURL cannot be set on this kind
// (providers.Set.Validate refuses both), which is why the transport is nil: the
// endpoint is fixed and the credential never leaves the daemon.
func chatGPTSenderFor(p providers.Provider, owner string, creds providerSenderCreds) (llm.Sender, string, error) {
	if owner == "" {
		return nil, "no owner", nil
	}
	if creds == nil {
		return nil, "provider credentials are not configured", nil
	}
	if ok, why := creds.Available(); !ok {
		return nil, why, nil
	}
	sender, err := llm.NewChatGPTResponsesSender(p, creds.TokenSourceFor(owner, p.Name), creds.ModelFactsFor(owner, p.Name), nil)
	if err != nil {
		return nil, "", fmt.Errorf("provider %q: %w", p.Name, err)
	}
	return sender, "", nil
}

// reachableProviders returns the names of every provider model can send to:
// its own provider (resolved via set.Split, which also validates the model
// string) plus that provider's configured Fallback chain. Single level only —
// llm.Client's callModel sends to p.Fallback directly and never recurses into
// a fallback's own Fallback list, so neither does this.
func reachableProviders(set *providers.Set, model string) (map[string]bool, error) {
	p, _, err := set.Split(model)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", model, err)
	}
	out := map[string]bool{p.Name: true}
	for _, fb := range p.Fallback {
		out[fb] = true
	}
	return out, nil
}
