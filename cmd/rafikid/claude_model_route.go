package main

import (
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/server"
)

// checkClaudeModelRoutable refuses, at spawn, a proxied claude child whose
// model the proxy is certain to refuse or fail on. Without it the child starts,
// attaches, and then dies on its first turn behind a 400 or a retried 502: the
// operator sees an agent that is merely busy, and finds the cause in the proxy
// log.
//
// It takes the proxy's own two steps -- routing.ResolveModel, then
// server.RouteModel -- with the passthrough decision the child is launched with
// and the OpenRouter key the proxy is built with, and only for children the
// proxy serves: an unproxied claude child talks to whatever base URL is ambient
// on its executor, which this daemon cannot see. A model ResolveModel cannot
// resolve yet (an alias against a cold catalog) is admitted: the catalog may be
// warm by the first turn, and the proxy remains the authority.
func (c *Controller) checkClaudeModelRoutable(req protocol.SpawnRequest) error {
	if req.Kind != protocol.KindClaude || req.Model == "" {
		return nil
	}
	if url, _ := c.proxyEndpoint(); url == "" || !proxyRoutesKind(req.Kind) {
		return nil
	}
	resolved, err := routing.ResolveModel(c.catalog, "", req.Model)
	if err != nil {
		return nil
	}
	mode, perr := proxyenv.ParsePassthroughMode(req.PassthroughAuth)
	if perr != nil {
		mode = proxyenv.PassthroughAuto // agent_runtime.go degrades the same way
	}
	passthrough := proxyenv.PassthroughAuthFor(mode, req.Model)
	_, err = server.RouteModel(resolved, passthrough, paths.Get("OPENROUTER_API_KEY") != "")
	switch {
	case err == nil:
		return nil
	case errors.Is(err, server.ErrPassthroughNonAnthropic):
		return claudeModelRefusal("model %q is not an Anthropic model, and passthrough auth bills your Claude subscription, which the proxy will not route to OpenRouter; set passthrough_auth=off or use an Anthropic model", req.Model)
	case errors.Is(err, server.ErrOpenRouterNotConfigured):
		return claudeModelRefusal("model %q routes through OpenRouter, but the daemon has no OPENROUTER_API_KEY; set it or use an Anthropic model", req.Model)
	default:
		return claudeModelRefusal("model %q cannot be routed: %v", req.Model, err)
	}
}

func claudeModelRefusal(format string, args ...any) error {
	return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: fmt.Sprintf(format, args...)}
}
