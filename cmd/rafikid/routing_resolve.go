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
// One spec is refused on the :batch leg: any key the OpenRouter Batch wire
// cannot accept (sort/quant/nodata/zdr — the envelope carries only
// provider.only). See the guard at the end of resolveRouting.
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
	"go.graveland.dev/rafiki/pkg/llm"
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
// preset's). A preset whose model names a spec applies that spec EVEN when
// the caller overrides the model string: the preset's spec is part of its
// declared routing intent for the seat — its data flags and preferences
// travel with the preset, not with one model id (wave-3 review finding 1).
// A parse error is ErrInvalidArgs naming the offending model
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
	//
	// translatedBase is the applied base id resolved through the provider set:
	// appliedBase may name a providers.toml ALIAS ("openrouter/glm-flash@together"),
	// and the policy's lines are keyed by the real OpenRouter id. One
	// translation serves both consumers below (the policy lookup and the :batch
	// refusal) — two copies would be exactly the drift finding-10 fixed.
	// Only the lookup translates; the alias's `only` pin still never folds into
	// the stored spec, and req.Model stays the applied base id as spawned.
	translatedBase := appliedBase
	if _, modelID, err := providersOrDefault(c.providers).Split(appliedBase); err == nil {
		translatedBase = modelID
	}
	var policySpec routing.Spec
	if c.routePolicy != nil && appliedBase != "" {
		policySpec = c.routePolicy.Resolve(translatedBase)
	}

	merged := spawnSpec.Merge(presetSpec).Merge(policySpec)
	req.Routing = merged.String()

	// OpenRouter's Batch API accepts only provider.only on the batch envelope
	// (Task 0.1's probe: sort, data_collection, zdr and ignore are each a
	// submit-time 400 "Unrecognized key"), so a :batch model whose merged spec
	// carries any key the batch wire cannot serve is refused at spawn — never
	// a silent fallback to unrouted. A spec with ONLY only= (or a zero spec)
	// passes: only rides the wire as {"only": [...]} (parkSend's batchOnly).
	// The refusal tests the TRANSLATED id: an alias whose id ends in :batch is
	// a supported shape (see the :batch/API-key guard in controller.go), and
	// the refusal must fire on it as on a literal :batch request.
	if llm.IsBatchModel(translatedBase) {
		if merged.Sort != "" || merged.Quant != nil || merged.NoData || merged.ZDR {
			return req, &connectapi.ControllerError{
				Code: protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("a routing spec cannot apply to %s: OpenRouter's Batch API accepts only provider.only; drop the spec or the :batch suffix",
					appliedBase),
			}
		}
	}
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
// proxyFace.SetController. sessionID is the request's X-Rafiki-Session (or,
// for a child-credential caller, that credential's bound child id) — for a
// daemon-spawned child that child's id; an interactive or hand-configured
// client sends an id that resolves to no child, and an empty value is normal.
//
// A child whose session recorded a routing spec at spawn answers with that
// stored spec merged over the policy's per-request row: the stored spec
// resolved ONCE and a policy edit never rewrites its STORED value (the same
// rule the preset follows), but the policy's data flags are monotone and
// reach running children too — a nodata/zdr row set after the spawn holds on
// every request it serves, filling the keys the stored spec leaves unset
// (stored wins per key). Unknown session, empty id, or no stored spec falls
// through to the policy's per-request resolution — a client-driven session
// gets exactly what the routing policy says for the model it named. A stored
// spec that no longer parses (only reachable through corruption) logs and
// falls back the same way, rather than failing the request.
func (c *Controller) RoutingFor(sessionID, modelID string) routing.Spec {
	var stored routing.Spec
	hasStored := false
	if sessionID != "" {
		if snap, ok := c.st.Get(sessionID); ok && snap.Routing != "" {
			spec, err := routing.ParseSpec(snap.Routing)
			if err != nil {
				slog.Warn("routing: stored session spec no longer parses; falling back to policy",
					"childId", sessionID, "routing", snap.Routing, "error", err)
			} else {
				stored, hasStored = spec, true
			}
		}
	}
	if c.routePolicy == nil {
		if hasStored {
			return stored
		}
		return routing.Spec{}
	}
	// stored wins per key; the policy fills gaps and its data flags OR in
	// (monotone). With no stored spec this is the plain per-request resolve.
	return stored.Merge(c.routePolicy.Resolve(modelID))
}
