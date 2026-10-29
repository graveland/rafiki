// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/modelalias"
	"go.graveland.dev/rafiki/pkg/server"
)

// errModelAliasUnwired is what the adapter answers before main wires the
// store; the Connect manager turns it into CodeInternal, and the server never
// registers the manager without a store, so this is a bug fence rather than a
// reachable path.
var errModelAliasUnwired = errors.New("model aliases backend not yet wired")

// connectModelAliases adapts *Controller to connectapi.ModelAliasManager,
// modelled on connectPresets: the owner comes from the request CONTEXT via
// spawnOwner — never a request field — so a child lists its own owner's
// aliases and can never name someone else's.
//
// Listing is policyAnyCaller (connect_policy.go): a child reads its owner's
// aliases, which is what lets an agent use one in a prompt. Writing is
// policyUserOnly, and the adapter refuses a non-user credential a second time
// as defence in depth — a policy row is one line away from an operator
// mistake, and "no child credential and no MCP tool may write an alias" is a
// design rule, not a convention. A nil identity (the local socket) is a user
// and is allowed.
type connectModelAliases struct{ c *Controller }

// ListModelAliases lists the caller's owner's aliases, ordered by name.
func (m connectModelAliases) ListModelAliases(ctx context.Context) ([]modelalias.Alias, error) {
	if m.c.modelAliasStore == nil {
		return nil, errModelAliasUnwired
	}
	return m.c.modelAliasStore.List(ctx, spawnOwner(ctx).UserID)
}

// PutModelAlias validates and stores name → target for the caller's owner.
// A validation failure surfaces as modelalias.ErrInvalid (modelAliasError maps
// that to CodeInvalidArgument); every other failure is the store's own.
func (m connectModelAliases) PutModelAlias(ctx context.Context, name, target string) (modelalias.Alias, error) {
	if err := refuseChildAliasWrite(ctx); err != nil {
		return modelalias.Alias{}, err
	}
	if m.c.modelAliasStore == nil {
		return modelalias.Alias{}, errModelAliasUnwired
	}
	return m.c.modelAliasStore.Put(ctx, spawnOwner(ctx).UserID, name, target)
}

// DeleteModelAlias removes name's alias for the caller's owner.
func (m connectModelAliases) DeleteModelAlias(ctx context.Context, name string) error {
	if err := refuseChildAliasWrite(ctx); err != nil {
		return err
	}
	if m.c.modelAliasStore == nil {
		return errModelAliasUnwired
	}
	return m.c.modelAliasStore.Delete(ctx, spawnOwner(ctx).UserID, name)
}

// refuseChildAliasWrite is the adapter's own authoring gate: any non-nil
// identity that is not a user credential — a per-child secret, a per-boot
// token, or the empty Identity a bare per-boot token yields — may read aliases
// but never write one.
func refuseChildAliasWrite(ctx context.Context) error {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.IsUserCredential() {
		return nil
	}
	return fmt.Errorf("%w: model aliases are written by a user credential only", connectapi.ErrModelAliasAuthoring)
}
