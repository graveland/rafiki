// SPDX-License-Identifier: Apache-2.0

package main

// Routing-spec resolution: the daemon parses a spawn's model string into a
// base id plus a routing spec exactly ONCE, in Controller.Spawn, and stores
// the merged spec on the childstore session. Resume never re-resolves — it
// reads the stored value back into the request it rebuilds, the same rule
// applyPreset's resolved fields follow.
//
// Resolution chain, most specific first (routing.Spec.Merge):
//
//	spawn spec (the brackets on the request's Model)
//	  > preset spec (the brackets on the preset's Model)
//	    > routing-policy row (routepolicy.Policy, by model line)
//
// Data flags (nodata/zdr) are monotone: set at any level they hold.
//
// What is deliberately NOT folded into the spec: a providers.toml alias's
// `only` pin and the static providerPins. pkg/llm applies them at request
// time as the pin — used only when the spec carries no `only` — and a pin
// does not bypass bans, while a spec `only` does. Folding them in here would
// silently make every alias pin a ban-bypassing decision.

import (
	"fmt"
	"log/slog"
	"strings"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
)

// resolveRouting parses spawnModel (the request's model BEFORE applyPreset
// filled it) and the applied preset's model with routing.ParseModel, merges
// the two specs over the routing policy's row for the resulting base id, and
// rewrites req: Model becomes the APPLIED model's base id (brackets stripped)
// and Routing the merged spec's canonical form. It runs UNCONDITIONALLY after
// applyPreset — also when both parsed specs are zero, since the policy row
// may still contribute and the base id still needs stripping. When the merged
// spec is zero, req.Routing is "" and nothing is recorded: the correct
// outcome, not a skipped step.
//
// spawnModel and the preset's model may each carry a spec; when the preset
// replaced the model, the two can name different ids entirely — both are
// parsed, spawn wins per key, and req.Model is set from whichever model
// applyPreset actually applied (the request's own when it named one, else the
// preset's). A parse error is ErrInvalidArgs naming the offending model
// string: unknown syntax is never silently dropped, because a misread spec
// silently changes where requests are served.
//
// req.Routing != "" marks an already-resolved request. Nothing on the daemon
// calls resolveRouting on such a request (resume rebuilds the request from
// the stored session and never comes back here), but if it is ever reached
// with req.Routing set AND a bracket-free req.Model, the stored spec is kept
// verbatim: resolution happened once, upstream, and re-resolving would let a
// policy edit after the spawn rewrite a running child's spec.
func (c *Controller) resolveRouting(spawnModel string, preset *presets.Record, req protocol.SpawnRequest) (protocol.SpawnRequest, error) {
	if req.Routing != "" && !strings.Contains(req.Model, "[") {
		// Already resolved (a rebuilt resume/respawn request): keep it.
		return req, nil
	}

	spawnBase, spawnSpec, err := parseRoutingModel(spawnModel)
	if err != nil {
		return req, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}
	var presetBase string
	var presetSpec routing.Spec
	if preset != nil && preset.Model != "" {
		presetBase, presetSpec, err = parseRoutingModel(preset.Model)
		if err != nil {
			return req, &connectapi.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("preset %q model: %v", preset.Name, err),
			}
		}
	}

	// The model applyPreset chose: the request's when it named one, else the
	// preset's (applyPreset fills Model only when the request left it empty).
	appliedBase := spawnBase
	if spawnModel == "" && presetBase != "" {
		appliedBase = presetBase
	}
	req.Model = appliedBase

	// The policy fills the gaps the spawn/preset specs leave. Skipped when
	// there is no base id to resolve against: an empty model has no line, and
	// pinning the global row's spec to a child whose model is not yet known
	// would freeze a spec the actual model may not deserve (the proxy face's
	// RoutingFor falls back to per-request policy resolution in that case).
	var policySpec routing.Spec
	if c.routePolicy != nil && appliedBase != "" {
		policySpec = c.routePolicy.Resolve(appliedBase)
	}

	req.Routing = spawnSpec.Merge(presetSpec).Merge(policySpec).String()
	return req, nil
}

// parseRoutingModel parses a spawn/preset model string that may be empty: ""
// is no model at all (empty base, zero spec), not a parse error — ParseModel
// refuses an empty string, and an empty model here is merely "nothing to
// resolve", a spawn that later steps either default or refuse.
func parseRoutingModel(s string) (string, routing.Spec, error) {
	if s == "" {
		return "", routing.Spec{}, nil
	}
	base, spec, err := routing.ParseModel(s)
	if err != nil {
		return "", routing.Spec{}, fmt.Errorf("routing spec in model %q: %v", s, err)
	}
	return base, spec, nil
}

// RoutingFor resolves the routing spec a proxied OpenRouter request must
// carry. Satisfies pkg/server's RoutingResolver; wired by main.go via
// proxyFace.SetController. sessionID is the request's X-Rafiki-Session — for
// a daemon-spawned child that child's id; an interactive or hand-configured
// client sends an id that resolves to no child, and an empty value is normal.
//
// A child whose session recorded a routing spec at spawn answers with that
// stored spec, parsed: it resolved ONCE, and a policy edit after the spawn
// must not rewrite a running child (the same rule the preset follows).
// Unknown session, empty id, or no stored spec falls through to the policy's
// per-request resolution — a client-driven session gets exactly what the
// routing policy says for the model it named. A stored spec that no longer
// parses (only reachable through corruption) logs and falls back the same
// way, rather than failing the request.
func (c *Controller) RoutingFor(sessionID, modelID string) routing.Spec {
	if sessionID != "" {
		if snap, ok := c.st.Get(sessionID); ok && snap.Routing != "" {
			spec, err := routing.ParseSpec(snap.Routing)
			if err != nil {
				slog.Warn("routing: stored session spec no longer parses; falling back to policy",
					"childId", sessionID, "routing", snap.Routing, "error", err)
			} else {
				return spec
			}
		}
	}
	if c.routePolicy == nil {
		return routing.Spec{}
	}
	return c.routePolicy.Resolve(modelID)
}
