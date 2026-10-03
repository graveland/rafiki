// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/childstore"
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
// The read-modify-write runs ENTIRELY inside childstore.Store.Update, which
// holds the session mutex, so a child steer cannot read a stored spec and then
// persist a merge that drops a concurrent operator write of a monotone flag
// (`nodata`/`zdr`) or an operator `only=`. That atomicity is what makes D4's
// monotone and child-only guarantees hold under concurrency.
//
// Steps, in order: parse the delta; resolve the target; canonicalise the
// delta's slugs (never under the session mutex — Resolve may fetch); then, in
// ONE locked closure: parse the stored spec, apply the child-only guard against
// the stored value and merge. Persist the record afterwards.
func (c *Controller) steer(ctx context.Context, callerChildID, childID, delta string, operator bool) (string, error) {
	deltaSpec, err := routing.ParseSpec(delta)
	if err != nil {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}

	// Existence is checked before slug resolution so an unknown child is
	// ErrNotFound regardless of the delta's slugs. The locked section below
	// re-checks atomically and is the authority.
	if _, ok := c.st.Get(childID); !ok {
		return "", notRegisteredErr(childID)
	}

	// A child setting only= is refused before its slug is resolved, so a
	// refusal is a PermissionDenied even for an unknown provider, matching the
	// guard order this path has always had. The stored-only half of the guard
	// needs the stored value and so lives in the locked section.
	if !operator && deltaSpec.Only != nil {
		return "", onlyDeniedErr()
	}

	// Canonicalise the delta's slugs OUTSIDE the mutex: Resolve may load the
	// ProviderDirectory, and a network fetch must never happen under the
	// session lock. Stored slugs were canonicalised when written, so only the
	// delta needs resolving here.
	deltaSpec, err = c.canonicalizeRoutingSlugs(ctx, deltaSpec)
	if err != nil {
		return "", err
	}

	var (
		merged routing.Spec
		denied bool
	)
	err = c.st.Update(childID, func(sess *childstore.Session) {
		storedSpec, perr := routing.ParseSpec(sess.Routing)
		if perr != nil {
			// A stored spec that no longer parses is only reachable through
			// corruption; treat it as unset rather than failing the steer, the
			// same posture RoutingFor takes.
			slog.Warn("routing: stored session spec no longer parses; treating as unset",
				"childId", childID, "routing", sess.Routing, "error", perr)
			storedSpec = routing.Spec{}
		}
		if !operator && (deltaSpec.Only != nil || storedSpec.Only != nil) {
			denied = true
			return
		}
		merged = deltaSpec.Merge(storedSpec)
		sess.Routing = merged.String()
	})
	if errors.Is(err, childstore.ErrNotFound) {
		return "", notRegisteredErr(childID)
	}
	if err != nil {
		return "", fmt.Errorf("set routing: %w", err)
	}
	if denied {
		return "", onlyDeniedErr()
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

func notRegisteredErr(childID string) *connectapi.ControllerError {
	return &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("agent %s is not registered", childID),
	}
}

func onlyDeniedErr() *connectapi.ControllerError {
	return &connectapi.ControllerError{
		Code:    protocol.ErrPermissionDenied,
		Message: "child credentials may not set or change only=: it bypasses provider bans; ask the operator",
	}
}

// canonicalizeRoutingSlugs resolves every provider slug spec names through the
// daemon's ProviderDirectory and rewrites it to the canonical slug Resolve
// returns, so a display name ("Fireworks AI") is stored as its slug rather
// than the raw string OpenRouter would silently ignore. A typo is refused
// here, before anything is persisted. A degraded directory (not loaded — an
// OpenRouter outage) ACCEPTS the slug as given: Resolve returns a guessed slug
// with degraded=true and no error, so an outage never blocks steering.
func (c *Controller) canonicalizeRoutingSlugs(ctx context.Context, spec routing.Spec) (routing.Spec, error) {
	if spec.Prefer != nil {
		prefer, err := c.resolveSlugs(ctx, spec.Prefer)
		if err != nil {
			return routing.Spec{}, err
		}
		spec.Prefer = prefer
	}
	if spec.Only != nil {
		only, err := c.resolveSlugs(ctx, spec.Only)
		if err != nil {
			return routing.Spec{}, err
		}
		spec.Only = only
	}
	return spec, nil
}

// resolveSlugs resolves each slug, keeping the input verbatim when the
// directory degraded (a guessed slug is not a canonical one, and storing it
// would silently change the operator's intent). Resolve's only error is
// ErrUnknownProvider, whose message is ours (the routing package authored it),
// so it rides the ControllerError verbatim.
func (c *Controller) resolveSlugs(ctx context.Context, slugs []string) ([]string, error) {
	out := make([]string, len(slugs))
	for i, slug := range slugs {
		canonical, degraded, err := c.providerGuard.Resolve(ctx, slug)
		if err != nil {
			return nil, &connectapi.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: err.Error(),
			}
		}
		if !degraded && canonical != "" {
			out[i] = canonical
		} else {
			out[i] = slug
		}
	}
	return out, nil
}
