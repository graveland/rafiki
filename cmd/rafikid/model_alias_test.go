// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/modelalias"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// spyStore wraps a modelalias.Store, recording every Get and optionally
// failing it. Embeds the interface so only the calls a test cares about need
// overriding.
type spyStore struct {
	modelalias.Store
	gets   []string
	getErr error
}

func (s *spyStore) Get(ctx context.Context, ownerUserID, name string) (modelalias.Alias, error) {
	s.gets = append(s.gets, name)
	if s.getErr != nil {
		return modelalias.Alias{}, s.getErr
	}
	return s.Store.Get(ctx, ownerUserID, name)
}

// newAliasController returns a Controller whose alias store is st.
func newAliasController(st modelalias.Store) *Controller {
	return &Controller{modelAliasStore: st}
}

// A miss is not an error: the model comes back exactly as it went in, spec and
// all, with no alias name.
func TestLookupModelAliasMissIsUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newAliasController(modelalias.NewMemStore())

	for _, model := range []string{"ds", "ds[sort=price]", "glm-5.3"} {
		resolved, alias, err := ctrl.lookupModelAlias(context.Background(), "", model)
		c.NoError(err, "lookup %q", model)
		c.Eq(model, resolved, "resolved %q must be unchanged", model)
		c.Eq("", alias, "miss must report no alias for %q", model)
	}

	// A DB-less daemon has no store at all; the same lenient answer.
	dbless := newAliasController(nil)
	resolved, alias, err := dbless.lookupModelAlias(context.Background(), "", "ds")
	c.NoError(err, "nil store")
	c.Eq("ds", resolved, "nil store must leave the model unchanged")
	c.Eq("", alias, "nil store must report no alias")
}

// A model id (anything with a slash) is never an alias candidate, so the store
// must not even be consulted — a broken store would otherwise turn every
// ordinary model id into an error.
func TestLookupModelAliasSlashedSkipsStore(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newAliasController(&spyStore{Store: modelalias.NewMemStore(), getErr: errors.New("store must not be consulted")})

	for _, model := range []string{
		"openrouter/z-ai/glm-5.3-flash",
		"openrouter/z-ai/glm-5.3-flash[sort=price]",
		"anthropic/claude-sonnet-5-5",
	} {
		resolved, alias, err := ctrl.lookupModelAlias(context.Background(), "", model)
		c.NoError(err, "lookup %q must not reach the store", model)
		c.Eq(model, resolved, "resolved %q", model)
		c.Eq("", alias, "no alias for %q", model)
	}
}

// The caller's spec is merged OVER the target's: its keys win, while nodata
// (like zdr) is monotone and survives from the target.
func TestLookupModelAliasMergesCallerSpecOverTarget(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := modelalias.NewMemStore()
	_, err := st.Put(ctx, "", "ds", "openrouter/~deepseek/deepseek-flash-latest[sort=price,nodata]")
	c.NoError(err, "seed")
	ctrl := newAliasController(st)

	resolved, alias, err := ctrl.lookupModelAlias(ctx, "", "ds[sort=latency]")
	c.NoError(err, "lookup")
	c.Eq("ds", alias, "alias name")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest[sort=latency,nodata]", resolved,
		"caller sort must win while the target's nodata holds")

	// No caller spec: the target renders as stored.
	resolved, _, err = ctrl.lookupModelAlias(ctx, "", "ds")
	c.NoError(err, "lookup bare")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest[sort=price,nodata]", resolved,
		"a bare caller inherits the target's whole spec")
}

// A store failure is the caller's to see — never a silent fall-through to the
// unresolved model, which would route to a model nobody asked for.
func TestLookupModelAliasStoreErrorSurfaces(t *testing.T) {
	c := assert.NewAborting(t)
	boom := errors.New("dial tcp 10.0.0.5:5432: connection refused")
	ctrl := newAliasController(&spyStore{Store: modelalias.NewMemStore(), getErr: boom})

	resolved, alias, err := ctrl.lookupModelAlias(context.Background(), "", "ds")
	c.ErrorIs(err, boom, "the store error must surface")
	c.Eq("", resolved, "no resolution on error")
	c.Eq("", alias, "no alias on error")

	_, ok, err := ctrl.LookupModelAlias(context.Background(), "", "ds")
	c.ErrorIs(err, boom, "LookupModelAlias must surface the store error too")
	c.False(ok, "no hit on error")
}

// Aliases are strictly per owner: one owner's name never resolves for another.
func TestLookupModelAliasOwnerScoped(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := modelalias.NewMemStore()
	_, err := st.Put(ctx, "u1", "ds", "openrouter/~deepseek/deepseek-flash-latest")
	c.NoError(err, "seed")
	ctrl := newAliasController(st)

	resolved, alias, err := ctrl.lookupModelAlias(ctx, "u1", "ds")
	c.NoError(err, "owner hit")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest", resolved, "owner's own alias resolves")
	c.Eq("ds", alias, "alias name")

	resolved, alias, err = ctrl.lookupModelAlias(ctx, "u2", "ds")
	c.NoError(err, "other owner's miss")
	c.Eq("ds", resolved, "another owner's alias must not resolve")
	c.Eq("", alias, "no alias for u2")
}

// LookupModelAlias is the proxy face's shape: a hit reports the resolved
// target, a miss the empty string.
func TestLookupModelAliasReportsHit(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := modelalias.NewMemStore()
	_, err := st.Put(ctx, "u1", "ds", "openrouter/~deepseek/deepseek-flash-latest[nodata]")
	c.NoError(err, "seed")
	ctrl := newAliasController(st)

	target, ok, err := ctrl.LookupModelAlias(ctx, "u1", "ds")
	c.NoError(err, "hit")
	c.True(ok, "must report a hit")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest[nodata]", target, "merged target")

	target, ok, err = ctrl.LookupModelAlias(ctx, "u1", "nope")
	c.NoError(err, "miss")
	c.False(ok, "must report a miss")
	c.Eq("", target, "a miss reports no target")
}

// Deleting an owner's alias makes the next lookup a miss.
func TestLookupModelAliasDeleteMakesItMiss(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := modelalias.NewMemStore()
	_, err := st.Put(ctx, "u1", "ds", "openrouter/x")
	c.NoError(err, "seed")
	c.NoError(st.Delete(ctx, "u1", "ds"), "delete")

	ctrl := newAliasController(st)
	resolved, alias, err := ctrl.lookupModelAlias(ctx, "u1", "ds")
	c.NoError(err, "lookup after delete")
	c.Eq("ds", resolved, "a deleted alias is a miss")
	c.Eq("", alias, "no alias after delete")
}

// A stored target that no longer parses is reported as invalid arguments (it
// names the alias), never passed through as if it were a model.
func TestLookupModelAliasUnparsableTargetIsInvalid(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	ctrl := newAliasController(seedStore{target: "openrouter/x[bogus=1]"})

	_, _, err := ctrl.lookupModelAlias(ctx, "", "ds")
	var ce *connectapi.ControllerError
	c.True(errors.As(err, &ce), "want a ControllerError, got %v", err)
	c.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	c.True(strings.Contains(ce.Message, "ds"), "the message must name the alias: %q", ce.Message)
}

// seedStore returns one alias whose target the test controls, so a target that
// validation would refuse can be planted.
type seedStore struct {
	modelalias.Store
	target string
}

func (s seedStore) Get(context.Context, string, string) (modelalias.Alias, error) {
	return modelalias.Alias{Name: "ds", Target: s.target}, nil
}

// A child credential reads its owner's aliases but may never write one: the
// adapter refuses Put and Delete even if the policy table ever let them
// through. A nil identity — the local socket — is a user and is allowed.
func TestConnectModelAliasesRefusesChildWrites(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newAliasController(modelalias.NewMemStore())
	mgr := connectModelAliases{c: ctrl}
	childCtx := server.WithIdentity(context.Background(), &server.Identity{
		Via:     server.ProvenanceChildToken,
		ChildID: "c1",
		UserID:  "u1",
	})

	rows, err := mgr.ListModelAliases(childCtx)
	c.NoError(err, "a child may list")
	c.Len(rows, 0, "fresh owner has no aliases")

	_, err = mgr.PutModelAlias(childCtx, "ds", "openrouter/x")
	c.Require().ErrorIs(err, connectapi.ErrModelAliasAuthoring, "PutModelAlias from a child")
	c.ErrorIs(mgr.DeleteModelAlias(childCtx, "ds"), connectapi.ErrModelAliasAuthoring,
		"DeleteModelAlias from a child")

	// A bare per-boot token yields an EMPTY identity, which is not a user
	// credential either: it must be refused on the same path.
	emptyCtx := server.WithIdentity(context.Background(), &server.Identity{})
	_, err = mgr.PutModelAlias(emptyCtx, "ds", "openrouter/x")
	c.ErrorIs(err, connectapi.ErrModelAliasAuthoring, "PutModelAlias from an empty identity")

	// The local socket (no identity at all) is a user.
	_, err = mgr.PutModelAlias(context.Background(), "ds", "openrouter/x")
	c.NoError(err, "PutModelAlias on the anonymous socket")
	c.NoError(mgr.DeleteModelAlias(context.Background(), "ds"), "DeleteModelAlias on the anonymous socket")

	// So is a real user credential.
	userCtx := server.WithIdentity(context.Background(), &server.Identity{Via: server.ProvenanceUser, UserID: "u1"})
	_, err = mgr.PutModelAlias(userCtx, "ds", "openrouter/x")
	c.NoError(err, "PutModelAlias from a user credential")
}
