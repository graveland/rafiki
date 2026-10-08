package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// The description is the model's contract for the verb, so it is pinned
// verbatim rather than left to drift from the plan's wording.
const wantAgentRouteDescription = "Change where a child's model requests are served, " +
	"effective on its next request. spec is a bracket-free routing spec such as " +
	"\"prefer=fireworks\" (try that provider first, fall back to the rest) or " +
	"\"sort=throughput,quant=fp8+\". It merges over the child's current routing. You " +
	"may steer your own subtree only, and you may set prefer, sort and quant — only= " +
	"(a hard provider pin) is reserved for the operator."

func TestAgentRouteBlueprintSchemaAndMaterialize(t *testing.T) {
	ck := assert.NewAborting(t)
	bp := AgentRouteBlueprint{}
	ck.Eq("agent_route", bp.Name(), "tool name")
	ck.Eq(wantAgentRouteDescription, bp.Description(), "description")

	schema := bp.InputSchema()
	ck.Eq("object", schema.Type, "schema type")
	props := map[string]SchemaProperty{}
	for _, p := range schema.Properties {
		props[p.Name] = p
	}
	ck.Eq("string", props["agent_id"].Type, "agent_id type")
	ck.Eq("string", props["spec"].Type, "spec type")
	ck.EqDeep([]string{"agent_id", "spec"}, schema.Required, "required properties")

	// A nil spawner must decline the tool rather than register one that can
	// only answer "not configured" — the same rule every agent_* tool follows.
	tool, err := bp.Materialize(ToolOpts{})
	ck.NoError(err, "Materialize without a spawner")
	ck.Nil(tool, "a nil spawner must decline the tool")

	tool, err = bp.Materialize(ToolOpts{Agents: &fakeSpawner{}})
	ck.NoError(err, "Materialize with a spawner")
	ck.NotNil(tool, "a bound spawner must materialize the tool")
}

func TestAgentRouteMergesAndReportsCanonicalSpec(t *testing.T) {
	ck := assert.NewAborting(t)
	sp := &fakeSpawner{routingResult: "sort=throughput,quant=fp8+,prefer=fireworks"}
	reg, ctx := newAgentTools(t, sp)

	outRes, err := reg.Execute(ctx, "agent_route", json.RawMessage(
		`{"agent_id":"c_a","spec":"prefer=fireworks"}`))
	out := outRes.Text
	ck.NoError(err, "agent_route")
	ck.Len(sp.routed, 1, "want 1 SetRouting call, got %d", len(sp.routed))
	ck.Eq("c_a", sp.routed[0].ChildID, "target passed through")
	ck.Eq("prefer=fireworks", sp.routed[0].Spec, "spec delta passed through")
	// The result reports the SINGLE canonical spec the daemon merged and
	// stored, not the delta echoed back.
	ck.StrContains(out, "routing for c_a is now: sort=throughput,quant=fp8+,prefer=fireworks", "got:\n")
}

func TestAgentRouteRequiresAnAgentIDAndASpec(t *testing.T) {
	ck := assert.NewCollecting(t)
	reg, ctx := newAgentTools(t, &fakeSpawner{})
	_, err := reg.Execute(ctx, "agent_route", json.RawMessage(`{"spec":"prefer=fireworks"}`))
	ck.Error(err, "agent_route with no agent_id must fail")
	_, err = reg.Execute(ctx, "agent_route", json.RawMessage(`{"agent_id":"c_a"}`))
	ck.Error(err, "agent_route with no spec must fail")
}

// A refusal from the daemon (not in subtree, only= from a child, unknown
// slug) must reach the model as an error naming the target, not be flattened
// into a generic failure.
func TestAgentRouteSurfacesRefusal(t *testing.T) {
	ck := assert.NewAborting(t)
	sp := &fakeSpawner{setRoutingErr: errors.New("agent c_stranger is not in your subtree; use agent_list to see it")}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "agent_route", json.RawMessage(`{"agent_id":"c_stranger","spec":"prefer=fireworks"}`))
	ck.False(err == nil || !strings.Contains(err.Error(), "c_stranger"), "want a refusal naming c_stranger, got %v", err)
}
