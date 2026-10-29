// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/modelalias"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
)

// lookupModelAlias resolves model through owner's aliases. It is LENIENT: a
// model that is not an alias candidate (modelalias.IsCandidate false), a nil
// store, or a miss returns model unchanged with alias "". On a hit it returns
// the target's base with the caller's [spec] merged over the target's
// (callerSpec.Merge(targetSpec)) re-rendered as base or base[spec], and the
// alias name. A store error is returned, never swallowed.
//
// This is one of the two sanctioned places outside resolveRouting that parses
// brackets: a slash-free base is an alias name, so it must be split from the
// caller's spec before the store is consulted. It never runs on resume.
func (c *Controller) lookupModelAlias(ctx context.Context, ownerUserID, model string) (resolved, alias string, err error) {
	if !modelalias.IsCandidate(model) || c.modelAliasStore == nil {
		return model, "", nil
	}
	callerBase, callerSpec, err := routing.ParseModel(model)
	if err != nil {
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("model %q: %v", model, err),
		}
	}
	a, err := c.modelAliasStore.Get(ctx, ownerUserID, callerBase)
	if errors.Is(err, modelalias.ErrNotFound) {
		return model, "", nil
	}
	if err != nil {
		return "", "", err
	}
	targetBase, targetSpec, err := routing.ParseModel(a.Target)
	if err != nil {
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("model alias %q has an unparsable target %q: %v", a.Name, a.Target, err),
		}
	}
	return renderModelAlias(targetBase, callerSpec.Merge(targetSpec)), a.Name, nil
}

// renderModelAlias renders a resolved base with its merged spec: the bare base
// when the spec is zero, else base[spec] in the canonical bracket form
// routing.Spec.String() produces — the same form routing.ParseModel reads back.
func renderModelAlias(base string, spec routing.Spec) string {
	if spec.IsZero() {
		return base
	}
	return base + "[" + spec.String() + "]"
}

// LookupModelAlias satisfies pkg/server's ModelAliasResolver for the proxy
// face: hit = (target, true, nil) where target is the merged string above;
// miss = ("", false, nil).
func (c *Controller) LookupModelAlias(ctx context.Context, ownerUserID, model string) (string, bool, error) {
	resolved, alias, err := c.lookupModelAlias(ctx, ownerUserID, model)
	if err != nil {
		return "", false, err
	}
	if alias == "" {
		return "", false, nil
	}
	return resolved, true, nil
}
