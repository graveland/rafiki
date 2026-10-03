// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
)

// SetChildRouting merges a routing-spec delta over childID's stored spec with
// CHILD authority: callerChildID must be an ancestor of childID per stored
// lineage (childstore.IsDescendant). This is the single authority point for
// steering from a child credential — the Connect handler's ChildScope
// Authorize and the MCP spawner's authorize have already run, but this is the
// check that actually bounds the write.
//
// The subtree rule is deliberately the descendant rule, not SetChildBudget's
// direct-parentage rule: steering a grandchild's spec is the same authority
// the other subtree verbs (View/Send/Kill) grant through agent_list.
func (c *Controller) SetChildRouting(ctx context.Context, callerChildID, childID, delta string) (string, error) {
	if !c.st.IsDescendant(callerChildID, childID) {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrPermissionDenied,
			Message: fmt.Sprintf("agent %s is not in your subtree; use agent_list to see it", childID),
		}
	}
	return c.steer(ctx, callerChildID, childID, delta, false)
}

// SetChildRoutingAsOperator merges a routing-spec delta over childID's stored
// spec with OPERATOR authority: no lineage check (any child, at any depth may
// be targeted — this backs the Connect SetRouting RPC, a control-plane verb,
// not an agent-facing tool). Only the child-only guards are lifted; parsing,
// existence, slug validation and the store write are shared with the child path.
func (c *Controller) SetChildRoutingAsOperator(ctx context.Context, childID, delta string) (string, error) {
	return c.steer(ctx, "", childID, delta, true)
}

// steer is the one routing-spec mutation path, shared by SetChildRouting
// (child authority) and SetChildRoutingAsOperator (operator authority).
// operator is an EXPLICIT parameter, never sniffed from the context here, so a
// nil identity on the MCP path cannot silently become operator authority.
//
// Steps, in order: parse the delta; resolve the target; apply the child-only
// guard (no only= when not operator); merge delta over stored; validate every
// provider slug before persisting; then persist and record.
func (c *Controller) steer(ctx context.Context, callerChildID, childID, delta string, operator bool) (string, error) {
	deltaSpec, err := routing.ParseSpec(delta)
	if err != nil {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}

	snap, ok := c.st.Get(childID)
	if !ok {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("agent %s is not registered", childID),
		}
	}
	storedSpec, err := routing.ParseSpec(snap.Routing)
	if err != nil {
		// A stored spec that no longer parses is only reachable through
		// corruption; treat it as unset rather than failing the steer, the
		// same posture RoutingFor takes.
		slog.Warn("routing: stored session spec no longer parses; treating as unset",
			"childId", childID, "routing", snap.Routing, "error", err)
		storedSpec = routing.Spec{}
	}

	if !operator {
		if deltaSpec.Only != nil || storedSpec.Only != nil {
			return "", &connectapi.ControllerError{
				Code:    protocol.ErrPermissionDenied,
				Message: "child credentials may not set or change only=: it bypasses provider bans; ask the operator",
			}
		}
	}

	merged := deltaSpec.Merge(storedSpec)

	if err := c.validateRoutingSlugs(ctx, merged); err != nil {
		return "", err
	}

	if err := c.st.SetRouting(childID, merged.String()); err != nil {
		return "", fmt.Errorf("set routing: %w", err)
	}
	// Durability: the childstore write is in-memory only, exactly as MaxCost
	// is. writeRecord persists the whole snapshot — Routing included — the
	// same record write a status change performs, so a steered spec survives
	// a daemon restart rather than waiting for the child's next status
	// transition. Best-effort, matching every other record write.
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("routing: persist steered spec", "childId", childID, "error", err)
	}

	caller := "operator"
	if !operator {
		caller = callerChildID
	}
	slog.Info("routing: steered", "childId", childID, "caller", caller, "routing", merged.String())
	return merged.String(), nil
}

// validateRoutingSlugs resolves every provider slug the merged spec names
// through the daemon's ProviderDirectory, so a typo is refused before it is
// persisted rather than silently ignored by OpenRouter. A degraded directory
// (not loaded — an OpenRouter outage) ACCEPTS the slug as given: Resolve
// returns a guessed slug with no error, so an outage never blocks steering.
func (c *Controller) validateRoutingSlugs(ctx context.Context, spec routing.Spec) error {
	for _, slug := range append(append([]string{}, spec.Prefer...), spec.Only...) {
		_, _, err := c.providerGuard.Resolve(ctx, slug)
		if err == nil {
			continue
		}
		// Resolve's only error is ErrUnknownProvider, whose message is ours
		// (the routing package authored it), so it rides the ControllerError
		// verbatim.
		return &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}
	return nil
}
