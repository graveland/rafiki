// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routepolicy"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// routingPolicy builds a Policy preloaded with rows, the shape main.go serves
// after a startup Load.
func routingPolicy(t *testing.T, rows ...routepolicy.Row) *routepolicy.Policy {
	t.Helper()
	p := routepolicy.NewPolicy()
	assert.NewAborting(t).NoError(p.Load(rows), "policy Load")
	return p
}

// routingPreset is a fundi preset whose model carries (or not) a bracketed
// routing spec.
func routingPreset(name, model string) presets.Record {
	rec := presetFixture(name)
	rec.Model = model
	return rec
}

// resolveVia runs a request through the exact two-step resolution
// Controller.Spawn performs — capture the pre-preset model, applyPreset,
// resolveRouting — without forking a child. Every stored-spec assertion below
// is against the request this returns, which is what Spawn writes to the
// session verbatim.
func resolveVia(t *testing.T, c *Controller, owner string, req protocol.SpawnRequest) (protocol.SpawnRequest, error) {
	t.Helper()
	spawnModel := req.Model
	req, rec, err := c.applyPreset(context.Background(), req, owner)
	if err != nil {
		return req, err
	}
	return c.resolveRouting(spawnModel, rec, req)
}

// TestRoutingSpawnSpecBeatsPreset pins the chain's first rule: the spawn's
// bracketed spec wins per key over the preset's, the preset's remaining keys
// still apply, and the policy row fills only what both leave unset. Model is
// rewritten to the APPLIED model's base id.
func TestRoutingSpawnSpecBeatsPreset(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "z-ai/glm-5.3", Spec: "quant=fp8+"}))
	c.presetStore = newFakePresetStore("owner-1", routingPreset("g", "openrouter/z-ai/glm-5.3[sort=price]"))

	req, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
		Kind:   protocol.KindFundi,
		Cwd:    "/tmp/w",
		Preset: "g",
		Model:  "openrouter/z-ai/glm-5.3[sort=throughput]",
	})
	ck.Require().NoError(err, "resolveRouting")
	ck.Eq("sort=throughput,quant=fp8+", req.Routing, "spawn sort must beat preset sort; policy quant fills the gap")
	ck.Eq("openrouter/z-ai/glm-5.3", req.Model, "Model must be the applied model's base id")
}

// TestRoutingPresetSpecApplies pins the preset leg alone: a preset model
// carrying a spec, with the spawn naming NO model, resolves to the preset's
// spec — and the applied model is the preset's base.
func TestRoutingPresetSpecApplies(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.presetStore = newFakePresetStore("owner-1", routingPreset("g", "openrouter/x[nodata]"))

	req, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
		Kind:   protocol.KindFundi,
		Cwd:    "/tmp/w",
		Preset: "g",
	})
	ck.Require().NoError(err, "resolveRouting")
	ck.Eq("nodata", req.Routing, "the preset's spec must resolve when the spawn names no model")
	ck.Eq("openrouter/x", req.Model, "Model must be the preset model's base id")
}

// TestRoutingPolicyFillsGaps pins the policy leg: with no spawn/preset spec
// the policy row's spec is the whole stored value, and with a spawn spec the
// policy contributes only the keys the spawn left unset.
func TestRoutingPolicyFillsGaps(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "z-ai/glm-5.3-flash", Spec: "quant=fp8+"}))

	req, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   "/tmp/w",
		Model: "openrouter/z-ai/glm-5.3-flash",
	})
	ck.Require().NoError(err, "resolveRouting (plain model)")
	ck.Eq("quant=fp8+", req.Routing, "policy-only spec")

	req, err = resolveVia(t, c, "owner-1", protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   "/tmp/w",
		Model: "openrouter/z-ai/glm-5.3-flash[only=together]",
	})
	ck.Require().NoError(err, "resolveRouting (spec on model)")
	ck.Eq("quant=fp8+,only=together", req.Routing, "policy fills the keys the spawn spec leaves unset")
}

// TestRoutingDataFlagsSurviveSpawnSpec pins the monotone leg: nodata/zdr are
// confidentiality, not preference — set at ANY level they hold. A policy "*"
// row carrying zdr must survive a spawn spec that says nothing about data
// policy, and vice versa the merged spec keeps both.
func TestRoutingDataFlagsSurviveSpawnSpec(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "*", Spec: "zdr"}))

	req, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   "/tmp/w",
		Model: "m[only=a]",
	})
	ck.Require().NoError(err, "resolveRouting")
	ck.Eq("only=a,zdr", req.Routing, "policy zdr must survive the spawn's only")
}

// TestRoutingModelIsBaseId proves the stored session carries the BASE id: the
// bracketed spec is parsed out of Model and recorded on Routing, never left
// in Model where providers.Split would choke on it. Drives the REAL spawn
// path (fake claude child) so the session insert is the one Spawn writes.
func TestRoutingModelIsBaseId(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)

	id := spawnRoutingChild(t, c, "openrouter/z-ai/glm-5.3-flash[sort=price]")
	snap, ok := c.st.Get(id)
	ck.Require().True(ok, "child has no store row")
	ck.False(strings.Contains(snap.Model, "["), "Session.Model = %q must be the base id, not the bracketed model", snap.Model)
	ck.Eq("sort=price", snap.Routing, "Session.Routing")

	// And the same value reaches the wire summary.
	sum, ok := c.GetChild(id)
	ck.Require().True(ok, "GetChild")
	ck.Eq("sort=price", sum.Routing, "ChildSummary.Routing")
}

// TestRoutingBadSpecRejected pins the parse contract: a malformed spec is
// ErrInvalidArgs naming the model string — never silently ignored, because a
// misread spec silently changes where requests are served. Both legs can
// carry the brackets: the request's model and the preset's.
func TestRoutingBadSpecRejected(t *testing.T) {
	t.Run("spawn model", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newTestController(t)
		_, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
			Kind:  protocol.KindFundi,
			Cwd:   "/tmp/w",
			Model: "openrouter/z-ai/glm-5.3-flash[sort=bogus]",
		})
		msg := wantInvalidArgs(t, err)
		ck.StrContains(msg, "sort=bogus", "the error must name the model string")
	})

	t.Run("preset model", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newTestController(t)
		c.presetStore = newFakePresetStore("owner-1", routingPreset("g", "x[nodata,quant=none]"))
		_, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
			Kind:   protocol.KindFundi,
			Cwd:    "/tmp/w",
			Preset: "g",
		})
		msg := wantInvalidArgs(t, err)
		ck.StrContains(msg, "quant=none", "the error must name the offending item")
	})
}

// TestRoutingResumeKeepsStoredRouting pins the resolve-ONCE rule end to end:
// a spawn records the spec; the policy then CHANGES; the resumed child still
// runs the stored spec. Resume never re-resolves — the value comes from the
// stored session, both into the rebuilt request and back onto the resumed row.
func TestRoutingResumeKeepsStoredRouting(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.SetRoutePolicy(routingPolicy(t)) // empty at spawn: the spec is the spawn's own

	id := spawnRoutingChild(t, c, "openrouter/z-ai/glm-5.3-flash[sort=price]")
	before, ok := c.st.Get(id)
	ck.Require().True(ok, "child has no store row")
	ck.Require().Eq("sort=price", before.Routing, "spawn must store the spec")

	// Kill and wait for the exited row the resume reads.
	kctx, kcancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, err := c.Kill(kctx, id, 1000, 1000)
	kcancel()
	ck.Require().NoError(err, "Kill")
	waitForExited(t, c.st, id, 10*time.Second)

	// The policy changes AFTER the spawn — a line row that would resolve to a
	// different sort entirely.
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "z-ai/glm-5.3", Spec: "sort=latency"}))

	rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, err = c.Resume(rctx, id, "")
	rcancel()
	ck.Require().NoError(err, "Resume")

	after, ok := c.st.Get(id)
	ck.Require().True(ok, "resumed child has no store row")
	ck.Eq("sort=price", after.Routing, "the resumed child must keep the STORED spec, not the changed policy")
	ck.Eq("sort=price", after.Labels["rafiki/routing"], "the resumed row's label mirrors the stored spec")
}

// TestRoutingLabel pins the label leg: the daemon writes rafiki/routing
// mirroring the session's stored spec, and writes NOTHING when the resolved
// spec is empty (no noise on a spec-less spawn).
func TestRoutingLabel(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)

	id := spawnRoutingChild(t, c, "openrouter/z-ai/glm-5.3-flash[sort=price]")
	snap, ok := c.st.Get(id)
	ck.Require().True(ok, "child has no store row")
	ck.Eq("sort=price", snap.Labels["rafiki/routing"], "rafiki/routing label")

	id2 := spawnRoutingChild(t, c, "openrouter/z-ai/glm-5.3-flash")
	snap2, ok := c.st.Get(id2)
	ck.Require().True(ok, "second child has no store row")
	_, has := snap2.Labels["rafiki/routing"]
	ck.False(has, "a spec-less spawn must not carry a rafiki/routing label")
}

// TestRoutingAliasOnlyNotInSpec pins what resolveRouting deliberately does
// NOT fold in: a providers.toml alias's `only` pin. The alias pin is applied
// at request time as the pin (used only when the spec has no only) and does
// not bypass bans; folding it into the stored spec would silently make every
// alias pin a ban-bypassing decision.
func TestRoutingAliasOnlyNotInSpec(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.providers = &providers.Set{
		DefaultProvider: "openrouter",
		Providers: map[string]providers.Provider{
			"openrouter": {
				Name: "openrouter", Kind: providers.KindAnthropicOpenRouter,
				Models: map[string]providers.ModelAlias{
					"z-ai/glm-5.3-flash": {ID: "z-ai/glm-5.3-flash", Only: []string{"together"}},
				},
			},
		},
	}
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "z-ai/glm-5.3-flash", Spec: "quant=fp8+"}))

	req, err := resolveVia(t, c, "owner-1", protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   "/tmp/w",
		Model: "openrouter/z-ai/glm-5.3-flash",
	})
	ck.Require().NoError(err, "resolveRouting")
	ck.Eq("quant=fp8+", req.Routing, "the alias's only must not leak into the stored spec")
	ck.False(strings.Contains(req.Routing, "only="), "stored spec must not contain only=")
}

// spawnRoutingChild spawns a claude child (fake-pi.sh, as spawnTestChild does)
// with an explicit model, driving the REAL Spawn path: applyPreset,
// resolveRouting, session insert and label write.
func spawnRoutingChild(t *testing.T, ctrl *Controller, model string) string {
	t.Helper()
	req := protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
		Model:     model,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req, users.Identity{})
	assert.NewAborting(t).NoError(err, "spawn")
	return res.ChildID
}

// TestAgentRuntimeOptionsCarriesRouting pins the child-side plumbing: the
// daemon's resolved spec rides buildAgentArgv as --routing, survives the
// parse back, and lands on RuntimeOptions.Routing verbatim — the value
// clientOptions hands to llm.WithRouting. A spec-less request leaves it empty.
func TestAgentRuntimeOptionsCarriesRouting(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)

	ro, err := c.agentRuntimeOptions(protocol.SpawnRequest{
		Kind:    protocol.KindFundi,
		Cwd:     t.TempDir(),
		Model:   "openrouter/z-ai/glm-5.3-flash",
		Routing: "sort=price,nodata",
	}, "c_routing", false, "", "")
	ck.Require().NoError(err, "agentRuntimeOptions")
	ck.Eq("sort=price,nodata", ro.Routing, "RuntimeOptions.Routing must carry the resolved spec verbatim")

	ro, err = c.agentRuntimeOptions(protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "openrouter/z-ai/glm-5.3-flash",
	}, "c_no_routing", false, "", "")
	ck.Require().NoError(err, "agentRuntimeOptions (spec-less)")
	ck.Eq("", ro.Routing, "a spec-less request must leave RuntimeOptions.Routing empty")
}

// TestRoutingAlreadyResolvedRequestIsKept pins the belt-and-braces marker:
// req.Routing != "" on a request whose Model is already bracket-free means
// "resolved upstream" (a rebuilt resume/respawn request), and resolveRouting
// keeps it verbatim instead of re-resolving against a policy that may have
// changed since. A request with Routing set but a BRACKETED model is NOT
// exempt — that shape is a fresh spawn, parsed normally.
func TestRoutingAlreadyResolvedRequestIsKept(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "z-ai/glm-5.3", Spec: "sort=latency"}))

	// A rebuilt resume request: stored routing, bracket-free model.
	got, err := c.resolveRouting("openrouter/z-ai/glm-5.3", nil, protocol.SpawnRequest{
		Kind:    protocol.KindFundi,
		Cwd:     "/tmp/w",
		Model:   "openrouter/z-ai/glm-5.3",
		Routing: "sort=price",
	})
	ck.Require().NoError(err, "resolveRouting")
	ck.Eq("sort=price", got.Routing, "the stored spec must be kept, not re-resolved")
	ck.Eq("openrouter/z-ai/glm-5.3", got.Model, "Model untouched")

	// A bracketed model with Routing set is still parsed fresh (the marker
	// alone is not trusted) — and the policy row for the line still fills the
	// gaps, exactly as any fresh spawn resolves.
	got, err = c.resolveRouting("openrouter/z-ai/glm-5.3[only=a]", nil, protocol.SpawnRequest{
		Kind:    protocol.KindFundi,
		Cwd:     "/tmp/w",
		Model:   "openrouter/z-ai/glm-5.3[only=a]",
		Routing: "stale",
	})
	ck.Require().NoError(err, "resolveRouting")
	ck.Eq("sort=latency,only=a", got.Routing, "a bracketed model is a fresh spawn: re-resolved, policy still fills")
}

// TestRoutingForPrefersStoredSession pins the proxy-face resolver: a child
// whose session recorded a spec answers with THAT spec even after the policy
// changed; an unknown/empty session id falls through to policy-only
// resolution; a session with no stored spec does the same.
func TestRoutingForPrefersStoredSession(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.SetRoutePolicy(routingPolicy(t, routepolicy.Row{ModelLine: "z-ai/glm-5.3", Spec: "sort=latency"}))
	c.st.Insert(&childstore.Session{
		ChildID: "c_rt_stored",
		Kind:    protocol.KindFundi,
		Cwd:     "/tmp/w",
		Status:  protocol.StatusIdle,
		Model:   "openrouter/z-ai/glm-5.3",
		Routing: "sort=price",
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_rt_bare",
		Kind:    protocol.KindFundi,
		Cwd:     "/tmp/w",
		Status:  protocol.StatusIdle,
		Model:   "openrouter/z-ai/glm-5.3",
	})

	ck.Eq("sort=price", c.RoutingFor("c_rt_stored", "openrouter/z-ai/glm-5.3").String(), "stored session spec wins")
	ck.Eq("sort=latency", c.RoutingFor("c_rt_bare", "openrouter/z-ai/glm-5.3").String(), "no stored spec: policy resolves")
	ck.Eq("sort=latency", c.RoutingFor("c_unknown", "openrouter/z-ai/glm-5.3").String(), "unknown session: policy resolves")
	ck.Eq("sort=latency", c.RoutingFor("", "openrouter/z-ai/glm-5.3").String(), "empty session id: policy resolves")

	// A controller with no policy at all degrades to a zero spec, never a
	// panic (the database-less daemon shape).
	bare := newTestController(t)
	ck.True(bare.RoutingFor("", "m").IsZero(), "no policy: zero spec")
}
