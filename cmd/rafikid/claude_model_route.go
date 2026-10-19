package main

import (
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/proxyenv"
)

// checkClaudeModelRoutable refuses, at spawn, a proxied claude child whose
// model the proxy is certain to refuse or fail on. Without it the child starts,
// attaches, and then dies on its first turn behind a 400 or a retried 502: the
// operator sees an agent that is merely busy, and finds the cause in the proxy
// log.
//
// It applies the proxy's own rules (MessagesProxy.selectUpstream), resolved
// through the same provider set, and only to children the proxy serves: an
// unproxied claude child talks to whatever base URL is ambient on its executor,
// which this daemon cannot see. A bare name such as "sonnet" resolves to the
// default Anthropic provider and is admitted -- unknown is not "not Anthropic".
func (c *Controller) checkClaudeModelRoutable(req protocol.SpawnRequest) error {
	if req.Kind != protocol.KindClaude || req.Model == "" {
		return nil
	}
	if url, _ := c.proxyEndpoint(); url == "" || !proxyRoutesKind(req.Kind) {
		return nil
	}
	prov, _, err := providersOrDefault(c.providers).Split(req.Model)
	if err != nil {
		return claudeModelRefusal("model %q cannot be routed: %v", req.Model, err)
	}
	switch prov.Kind {
	case providers.KindAnthropic:
		return nil
	case providers.KindAnthropicOpenRouter:
		mode, perr := proxyenv.ParsePassthroughMode(req.PassthroughAuth)
		if perr != nil {
			mode = proxyenv.PassthroughAuto
		}
		if proxyenv.PassthroughAuthFor(mode, req.Model) {
			return claudeModelRefusal("model %q is not an Anthropic model, and passthrough auth bills your Claude subscription, which the proxy will not route to OpenRouter; set passthrough_auth=off or use an Anthropic model", req.Model)
		}
		if paths.Get("OPENROUTER_API_KEY") == "" {
			return claudeModelRefusal("model %q routes through OpenRouter, but the daemon has no OPENROUTER_API_KEY; set it or use an Anthropic model", req.Model)
		}
		return nil
	default:
		return claudeModelRefusal("model %q resolves to provider %q (kind %s), which the proxy cannot serve a claude child from", req.Model, prov.Name, prov.Kind)
	}
}

func claudeModelRefusal(format string, args ...any) error {
	return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: fmt.Sprintf(format, args...)}
}
