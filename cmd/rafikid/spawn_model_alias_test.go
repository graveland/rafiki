// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/modelalias"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// ─── fixtures ───────────────────────────────────────────────────────────────

// failingAliasStore stands in for an alias-store outage: every method fails.
type failingAliasStore struct{ err error }

func (s failingAliasStore) Get(context.Context, string, string) (modelalias.Alias, error) {
	return modelalias.Alias{}, s.err
}

func (s failingAliasStore) List(context.Context, string) ([]modelalias.Alias, error) {
	return nil, s.err
}

func (s failingAliasStore) Put(context.Context, string, string, string) (modelalias.Alias, error) {
	return modelalias.Alias{}, s.err
}

func (s failingAliasStore) Delete(context.Context, string, string) error { return s.err }

// explodingAliasStore fails the test the moment it is consulted. It is how the
// "must not touch the store" rules are pinned: a model that is not an alias
// candidate (it has a slash) or a spawn that names no model at all must never
// cost a store round trip.
type explodingAliasStore struct{ t *testing.T }

func (s explodingAliasStore) Get(context.Context, string, string) (modelalias.Alias, error) {
	s.t.Error("the alias store was consulted; this model can never be an alias")
	return modelalias.Alias{}, modelalias.ErrNotFound
}

func (s explodingAliasStore) List(context.Context, string) ([]modelalias.Alias, error) {
	s.t.Error("the alias store was listed; this path must not read aliases")
	return nil, nil
}

func (s explodingAliasStore) Put(context.Context, string, string, string) (modelalias.Alias, error) {
	s.t.Error("the alias store was written; spawn never writes aliases")
	return modelalias.Alias{}, nil
}

func (s explodingAliasStore) Delete(context.Context, string, string) error {
	s.t.Error("the alias store was written; spawn never writes aliases")
	return nil
}

// aliasController is newTestController with an in-memory alias store — the
// shape a database-backed daemon has, without the database.
func aliasController(t *testing.T) *Controller {
	t.Helper()
	c := newTestController(t)
	c.modelAliasStore = modelalias.NewMemStore()
	return c
}

// putAlias stores one alias for owner, failing the test if the store refuses it.
func putAlias(t *testing.T, c *Controller, owner, name, target string) {
	t.Helper()
	_, err := c.modelAliasStore.Put(context.Background(), owner, name, target)
	assert.NewAborting(t).Require().NoError(err, "Put(%s -> %s)", name, target)
}

// spawnAliasReq is a real in-process fundi spawn: the cheapest shape that runs
// Controller.Spawn end to end (the same one the inbox tests use).
func spawnAliasReq(t *testing.T, c *Controller, owner users.Identity, req protocol.SpawnRequest) (protocol.SpawnResponseData, error) {
	t.Helper()
	req.Kind = protocol.KindFundi
	req.NoSession = true
	if req.Cwd == "" {
		req.Cwd = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return c.Spawn(ctx, req, owner)
}

// spawnAliasChild is spawnAliasReq for the success cases.
func spawnAliasChild(t *testing.T, c *Controller, owner users.Identity, req protocol.SpawnRequest) protocol.SpawnResponseData {
	t.Helper()
	res, err := spawnAliasReq(t, c, owner, req)
	assert.NewAborting(t).Require().NoError(err, "Spawn")
	return res
}

// aliasErrCode reports the ControllerError code err carries, failing when it is
// not a classified refusal at all.
func aliasErrCode(t *testing.T, err error) string {
	t.Helper()
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *connectapi.ControllerError", err, err)
	}
	return ce.Code
}

// aliasErrMessage reports the message of the ControllerError err carries.
func aliasErrMessage(t *testing.T, err error) string {
	t.Helper()
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *connectapi.ControllerError", err, err)
	}
	return ce.Message
}

// ─── Spawn ──────────────────────────────────────────────────────────────────

// The whole point of an alias: the name reaches the session as its TARGET, and
// the label records which name did it.
func TestSpawnModelAliasResolvesSpawnModel(t *testing.T) {
	ck := assert.NewAborting(t)
	c := aliasController(t)
	putAlias(t, c, "u1", "ds", "openrouter/~deepseek/deepseek-flash-latest")

	res := spawnAliasChild(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{Model: "ds"})
	snap := mustSnapshot(t, c, res.ChildID)
	ck.Eq("~deepseek/deepseek-flash-latest", snap.Model,
		"the session model must be the alias TARGET (stored provider-local)")
	ck.Eq("openrouter", snap.Provider, "the session's provider must be the target's")
	ck.Eq("ds", snap.Labels["rafiki/model-alias"], "rafiki/model-alias")
}

// The caller's spec merges OVER the target's, and the merged spec resolves
// exactly as a spelled-out model's brackets would.
func TestSpawnModelAliasMergesSpec(t *testing.T) {
	ck := assert.NewAborting(t)
	c := aliasController(t)
	putAlias(t, c, "u1", "ds", "openrouter/~deepseek/deepseek-flash-latest[sort=price,nodata]")

	res := spawnAliasChild(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{Model: "ds[sort=latency]"})
	snap := mustSnapshot(t, c, res.ChildID)
	ck.Eq("~deepseek/deepseek-flash-latest", snap.Model, "Model")
	ck.True(strings.Contains(snap.Routing, "sort=latency"),
		"the caller's sort must win over the alias target's; routing = %q", snap.Routing)
	ck.True(strings.Contains(snap.Routing, "nodata"),
		"the target's nodata must survive the merge; routing = %q", snap.Routing)
}

// A slash-free model that names no alias is a MISTAKE, not a model id: it is
// refused with the id shape that would have worked, and nothing is spawned.
func TestSpawnModelAliasMissRefused(t *testing.T) {
	t.Run("plain miss", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := aliasController(t)
		_, err := spawnAliasReq(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{Model: "nope"})
		ck.Require().Error(err, "Spawn")
		ck.Eq(protocol.ErrInvalidArgs, aliasErrCode(t, err), "code")
		ck.True(strings.Contains(aliasErrMessage(t, err), `no model alias "nope"`),
			"message = %q", aliasErrMessage(t, err))
		ck.Len(c.st.List(), 0, "a refused spawn must start nothing")
	})

	t.Run("anthropic hint", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := aliasController(t)
		_, err := spawnAliasReq(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{Model: "claude-sonnet-5-5"})
		ck.Require().Error(err, "Spawn")
		ck.Eq(protocol.ErrInvalidArgs, aliasErrCode(t, err), "code")
		ck.True(strings.Contains(aliasErrMessage(t, err), "did you mean anthropic/claude-sonnet-5-5"),
			"message = %q", aliasErrMessage(t, err))
		ck.Len(c.st.List(), 0, "a refused spawn must start nothing")
	})
}

// An alias-store outage is Unavailable, never a miss: reading an outage as
// "no such alias" would blame the caller's config for a server failure.
func TestSpawnModelAliasStoreErrorRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.modelAliasStore = failingAliasStore{err: errors.New("connection reset")}

	_, err := spawnAliasReq(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{Model: "ds"})
	ck.Require().Error(err, "Spawn")
	ck.Eq(protocol.ErrUnavailable, aliasErrCode(t, err), "code")
	ck.Len(c.st.List(), 0, "a refused spawn must start nothing")
}

// A preset's model resolves exactly once, per spawn. Repointing the alias
// reaches the NEXT spawn of that preset (the stored preset still says "ds"),
// and never rewrites a running child.
func TestSpawnModelAliasFromPreset(t *testing.T) {
	ck := assert.NewAborting(t)
	c := aliasController(t)
	c.presetStore = newFakePresetStore("u1", routingPreset("g", "ds"))
	putAlias(t, c, "u1", "ds", "openrouter/z-ai/glm-5.3-flash")

	owner := users.Identity{UserID: "u1"}
	first := spawnAliasChild(t, c, owner, protocol.SpawnRequest{Preset: "g"})
	ck.Eq("z-ai/glm-5.3-flash", mustSnapshot(t, c, first.ChildID).Model,
		"the preset's model must resolve to the alias target")
	ck.Eq("ds", mustSnapshot(t, c, first.ChildID).Labels["rafiki/model-alias"], "rafiki/model-alias")

	putAlias(t, c, "u1", "ds", "openrouter/z-ai/glm-5.3")

	second := spawnAliasChild(t, c, owner, protocol.SpawnRequest{Preset: "g"})
	ck.Eq("z-ai/glm-5.3", mustSnapshot(t, c, second.ChildID).Model,
		"a repointed alias must reach the preset's next spawn")
	ck.Eq("z-ai/glm-5.3-flash", mustSnapshot(t, c, first.ChildID).Model,
		"resolution is once, at spawn: the running child keeps its model")

	rec, err := c.presetStore.Get(context.Background(), "u1", "g")
	ck.Require().NoError(err, "preset Get")
	ck.Eq("ds", rec.Model, "the STORED preset must keep the alias name, never the resolved target")
}

// A spawn naming no MODEL has nothing to resolve, so the store is never
// consulted — the exploding store fails the test if it is. The model here comes
// from ExtraArgs, which is the only way a fundi spawn runs model-less through
// SpawnRequest.Model (the plan still requires a model from somewhere).
func TestSpawnEmptyModelSkipsAliasStore(t *testing.T) {
	c := newTestController(t)
	c.modelAliasStore = explodingAliasStore{t: t}

	_, err := spawnAliasReq(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{
		ExtraArgs: []string{"--model", "openrouter/z-ai/glm-5.3-flash"},
	})
	assert.NewAborting(t).NoError(err, "Spawn with no model")
}

// A model id — anything with a slash — never touches the alias store: the
// store only ever holds slash-free names.
func TestSpawnSlashedModelSkipsAliasStore(t *testing.T) {
	c := newTestController(t)
	c.modelAliasStore = explodingAliasStore{t: t}

	res, err := spawnAliasReq(t, c, users.Identity{UserID: "u1"},
		protocol.SpawnRequest{Model: "openrouter/z-ai/glm-5.3-flash"})
	assert.NewAborting(t).Require().NoError(err, "Spawn")
	snap := mustSnapshot(t, c, res.ChildID)
	assert.NewAborting(t).Eq("z-ai/glm-5.3-flash", snap.Model, "Model")
	if _, ok := snap.Labels["rafiki/model-alias"]; ok {
		t.Errorf("a model id must not be labelled rafiki/model-alias; labels = %v", snap.Labels)
	}
}

// Aliases are strictly per owner: another user's name is a miss, not a peek.
func TestSpawnModelAliasOwnerIsolation(t *testing.T) {
	ck := assert.NewAborting(t)
	c := aliasController(t)
	putAlias(t, c, "u1", "ds", "openrouter/z-ai/glm-5.3-flash")

	_, err := spawnAliasReq(t, c, users.Identity{UserID: "u2"}, protocol.SpawnRequest{Model: "ds"})
	ck.Require().Error(err, "owner B must not resolve owner A's alias")
	ck.Eq(protocol.ErrInvalidArgs, aliasErrCode(t, err), "code")
	ck.Len(c.st.List(), 0, "a refused spawn must start nothing")

	// The owner itself still resolves it, so the refusal above is about
	// ownership and not about the alias being unusable.
	spawnAliasChild(t, c, users.Identity{UserID: "u1"}, protocol.SpawnRequest{Model: "ds"})
}

// ─── review ─────────────────────────────────────────────────────────────────

// The review verb is a strict site: its request model is resolved like a
// spawn's, so a slash-free name that is not an alias is refused by name.
func TestConversationReviewModelAliasMissRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := reviewTestController(t, fakeReviewReads{ids: []string{"00000000-0000-0000-0000-0000000000aa"}})
	c.modelAliasStore = modelalias.NewMemStore()

	_, err := c.ConversationReview(context.Background(), insights.ScopeAll(),
		connectapi.ReviewRequest{Model: "nope"})
	ck.Require().Error(err, "ConversationReview")
	ck.Eq(protocol.ErrInvalidArgs, aliasErrCode(t, err), "code")
	ck.True(strings.Contains(aliasErrMessage(t, err), `no model alias "nope"`),
		"message = %q", aliasErrMessage(t, err))
}

// ─── lenient sites ──────────────────────────────────────────────────────────

// ModelRoutes resolves the alias before it trims the openrouter/ prefix, and an
// alias-store failure is Unavailable rather than "no endpoints".
func TestExplainModelRoutesAliasStoreError(t *testing.T) {
	ck := assert.NewAborting(t)
	exp := connectRouteExplainer{c: &Controller{
		modelAliasStore: failingAliasStore{err: errors.New("connection reset")},
	}}

	_, err := exp.ExplainModelRoutes(ownerCtx("u1"), "ds")
	ck.Require().Error(err, "ExplainModelRoutes")
	ck.Eq(protocol.ErrUnavailable, aliasErrCode(t, err), "code")
}

// ModelInfo is lenient: a hit reports the TARGET's facts, and a slash-free miss
// passes through unchanged (an unknown model, not an error).
func TestModelInfoResolvesAlias(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{
		modelAliasStore: modelalias.NewMemStore(),
		catalog:         routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler)),
	}
	c.catalog.SeedForTest([]routing.CatalogEntry{{
		ID: "openai/gpt-x", ContextLength: 400000, MaxCompletionTokens: 128000,
	}})
	putAlias(t, c, "u1", "ds", "openai/gpt-x")

	got, err := (connectChildOps{c: c}).ModelInfo(ownerCtx("u1"), "ds")
	ck.Require().NoError(err, "ModelInfo(ds)")
	ck.Eq("openai/gpt-x", got.Model, "Model must be the alias target")
	ck.True(got.Known, "the target's catalog facts must reach the answer")
	if got.ContextWindow != 400000 {
		t.Errorf("ContextWindow = %d, want the target's 400000", got.ContextWindow)
	}

	// Lenient: a miss is not an error, and the model comes back as asked for.
	miss, err := (connectChildOps{c: c}).ModelInfo(ownerCtx("u1"), "claude-sonnet-5-5")
	ck.Require().NoError(err, "ModelInfo(miss) must not error")
	ck.Eq("claude-sonnet-5-5", miss.Model, "a miss must pass through unchanged")
	ck.False(miss.Known, "the miss must not invent facts")
}

// ModelInfo's failures stay classified: the model string's own parse failure
// (a malformed bracket on an alias candidate) is InvalidArgument — the caller
// mistyped — while an alias-store outage is Unavailable. Wrapping both as
// Unavailable would blame the server for the caller's input.
func TestModelInfoModelAliasErrorCodes(t *testing.T) {
	t.Run("malformed bracket keeps InvalidArgument", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := &Controller{modelAliasStore: modelalias.NewMemStore()}

		_, err := (connectChildOps{c: c}).ModelInfo(ownerCtx("u1"), "ds[sort=")
		ck.Require().Error(err, "ModelInfo(ds[sort=) must refuse a malformed bracket")
		ck.Eq(protocol.ErrInvalidArgs, aliasErrCode(t, err), "code")
		ck.True(strings.Contains(aliasErrMessage(t, err), "ds[sort="),
			"the message must name the model as typed; got %q", aliasErrMessage(t, err))
	})

	t.Run("store outage is Unavailable", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := &Controller{modelAliasStore: failingAliasStore{err: errors.New("connection reset")}}

		_, err := (connectChildOps{c: c}).ModelInfo(ownerCtx("u1"), "ds")
		ck.Require().Error(err, "an alias-store outage must not read as \"unknown model\"")
		ck.Eq(protocol.ErrUnavailable, aliasErrCode(t, err), "code")
	})
}

// ─── listings ───────────────────────────────────────────────────────────────

// An alias is listable under its own name, carrying its TARGET's facts, and it
// obeys the same provider/kind/owner filters the target does.
func TestListModelRowsIncludesAliases(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{
		modelAliasStore: modelalias.NewMemStore(),
		catalog:         routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler)),
	}
	c.catalog.SeedForTest([]routing.CatalogEntry{{
		ID: "z-ai/glm-5.3", ContextLength: 200000, MaxCompletionTokens: 32000,
		Pricing: &routing.ModelPricing{PromptUSD: 0.0000005, CompletionUSD: 0.0000015},
	}})
	putAlias(t, c, "owner-a", "ds", "openrouter/z-ai/glm-5.3")

	rows, err := c.ListModelRows(context.Background(), "owner-a", "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows")
	ck.Require().True(len(rows) > 0, "no rows at all")
	ck.Eq("ds", rows[0].ID, "alias rows lead the listing")
	byID := rowsByID(rows)
	alias, ok := byID["ds"]
	ck.Require().True(ok, "the alias is missing from %d rows", len(rows))
	ck.Eq("alias", alias.Source, "Source")
	ck.Eq("openrouter", alias.Provider, "Provider")
	ck.Eq("z-ai/glm-5.3", alias.Model, "Model")
	target := byID["openrouter/z-ai/glm-5.3"]
	ck.EqDeep(target.ContextWindow, alias.ContextWindow, "the alias must carry the target's context window")
	ck.EqDeep(target.PromptUSD, alias.PromptUSD, "the alias must carry the target's price")
	ck.Eq(target.Name, alias.Name, "the alias must carry the target's display name")

	// A claude child cannot resolve an OpenRouter id, so an OpenRouter-target
	// alias must not be offered to it.
	claudeRows, err := c.ListModelRows(context.Background(), "owner-a", "", protocol.KindClaude)
	ck.Require().NoError(err, "ListModelRows(claude)")
	ck.NotHasKey(rowsByID(claudeRows), "ds", "an OpenRouter-target alias was offered to a claude child")

	// The provider filter applies to the TARGET: "anthropic" must not list an
	// alias whose target is an OpenRouter id.
	anthropicRows, err := c.ListModelRows(context.Background(), "owner-a", "anthropic", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows(provider=anthropic)")
	ck.NotHasKey(rowsByID(anthropicRows), "ds", "a provider filter that excludes the target listed the alias anyway")

	// Aliases are per owner.
	otherRows, err := c.ListModelRows(context.Background(), "owner-b", "", protocol.KindFundi)
	ck.Require().NoError(err, "ListModelRows(owner-b)")
	ck.NotHasKey(rowsByID(otherRows), "ds", "another owner's alias was listed")
}

// A claude-target alias IS offered to a claude child: the filter is on the
// target's provider, not on the alias existing.
func TestListModelRowsClaudeTargetAliasForClaudeKind(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{modelAliasStore: modelalias.NewMemStore()}
	putAlias(t, c, "owner-a", "son", "anthropic/claude-sonnet-5")

	claudeRows, err := c.ListModelRows(context.Background(), "owner-a", "", protocol.KindClaude)
	ck.Require().NoError(err, "ListModelRows(claude)")
	row, ok := rowsByID(claudeRows)["son"]
	ck.Require().True(ok, "an anthropic-target alias must be offered to a claude child")
	ck.Eq("anthropic", row.Provider, "Provider")
}
