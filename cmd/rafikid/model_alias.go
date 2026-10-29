// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// resolveModelAlias is lookupModelAlias for the STRICT sites (Spawn, the
// review verb): a slash-free model that names no alias is an error rather than
// passed on. A slash-free string on the control surface is a user alias or an
// error; anything with a slash is a model id and never touches the store.
//
// It resolves the same way lookupModelAlias does (the caller's spec merged over
// the target's, re-rendered), and classifies its failures:
//
//   - a store outage is ErrUnavailable — never a fall-through, because reading
//     an outage as "no such alias" would be a lie about the caller's config;
//   - a miss on a candidate is ErrInvalidArgs, naming the alias and pointing at
//     the provider-qualified id the caller probably meant.
//
// A nil store (a DB-less daemon, which cannot hold an alias) passes the model
// through and errors on nothing: there is no alias corpus to miss in. An empty
// model is "nothing named" and maps to "".
//
// This never runs on resume: resolution happens once, at spawn, and the stored
// session keeps the target.
func (c *Controller) resolveModelAlias(ctx context.Context, ownerUserID, model string) (resolved, alias string, err error) {
	if model == "" {
		return "", "", nil
	}
	if !modelalias.IsCandidate(model) || c.modelAliasStore == nil {
		return model, "", nil
	}
	base := model
	if i := strings.IndexByte(base, '['); i >= 0 {
		base = base[:i]
	}
	resolved, alias, err = c.lookupModelAlias(ctx, ownerUserID, model)
	if err != nil {
		// A parse refusal from the alias layer is already classified
		// InvalidArgument and names the offending string; pass it through
		// rather than relabelling a caller's bad input as an outage.
		var ce *connectapi.ControllerError
		if errors.As(err, &ce) {
			return "", "", err
		}
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrUnavailable,
			Message: fmt.Sprintf("model alias lookup for %q: %v", base, err),
		}
	}
	if alias != "" {
		return resolved, alias, nil
	}
	return "", "", &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: fmt.Sprintf("no model alias %q (model ids are provider/id%s)", base, aliasMissHint(base)),
	}
}

// aliasMissHint names the provider prefix a bare id most likely left off, so a
// refused spawn says how to write the model instead. Only the Anthropic family
// is guessed: those ids are the ones people type bare ("claude-sonnet-5-5")
// because the native sender accepts them everywhere else.
//
// A "did you mean openrouter/<id>" guess is deliberately NOT made: the catalog
// has no exact-id lookup (entryFor resolves through ResolveModel, which applies
// latest-family and prefix rewriting), so claiming a match would occasionally
// point at an id the catalog never listed.
func aliasMissHint(base string) string {
	if strings.HasPrefix(base, "claude-") {
		return "; did you mean anthropic/" + base + "?"
	}
	return ""
}

// presetModelAliasError names the preset in a model-alias refusal, keeping the
// refused code: the message must say WHICH model string failed, since a preset
// spawn names none itself.
func presetModelAliasError(presetName string, err error) error {
	var ce *connectapi.ControllerError
	if errors.As(err, &ce) {
		return &connectapi.ControllerError{
			Code:    ce.Code,
			Message: fmt.Sprintf("preset %q model: %s", presetName, ce.Message),
		}
	}
	return fmt.Errorf("preset %q model: %w", presetName, err)
}
