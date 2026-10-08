package tools

import (
	"encoding/json"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestAgentSendDefaultsWhereToChild: `where` is defaulted at the TOOL and
// nowhere else — an author who leaves it out means the agent's own workspace,
// so the daemon must never receive an unset site.
func TestAgentSendDefaultsWhereToChild(t *testing.T) {
	c := assert.NewAborting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "agent_send", json.RawMessage(
		`{"agent":"c_a","message":"m","steps":[{"read":{"path":"/r.md"}},{"where":"sender","bash":{"command":"git status"}}]}`))
	c.Require().NoError(err, "agent_send")
	c.Require().Len(sp.sent, 1, "want 1 send, got %d", len(sp.sent))
	steps := sp.sent[0].Steps
	c.Require().Len(steps, 2, "want 2 steps, got %d", len(steps))
	c.Eq(protocol.StepSiteChild, steps[0].Where, "an unset where must arrive as child; got")
	c.Eq(protocol.StepSiteSender, steps[1].Where, "an explicit sender must survive; got")
}

// Every step kind's fields must arrive at the spawner intact — the tool is a
// pass-through, and losing a field here would silently send a different step
// than the model asked for.
func TestAgentSendPassesAllStepKinds(t *testing.T) {
	c := assert.NewAborting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "agent_send", json.RawMessage(`{`+
		`"agent":"c_a","message":"m","steps":[`+
		`{"read":{"path":"/r.md","start":10,"end":20}},`+
		`{"bash":{"command":"git status","timeout_ms":5000}},`+
		`{"pymodule_run":{"repo":"local","script":"driver","modules":["util"],"args":["-x"],"cwd":"/w"}}]}`))
	c.Require().NoError(err, "agent_send")
	c.Require().Len(sp.sent, 1, "want 1 send, got %d", len(sp.sent))
	steps := sp.sent[0].Steps
	c.Require().Len(steps, 3, "want 3 steps, got %d", len(steps))
	c.False(steps[0].Read == nil || steps[0].Read.Path != "/r.md" ||
		steps[0].Read.Start != 10 || steps[0].Read.End != 20,
		"read step not intact: %+v", steps[0].Read)
	c.False(steps[1].Bash == nil || steps[1].Bash.Command != "git status" ||
		steps[1].Bash.TimeoutMs != 5000,
		"bash step not intact: %+v", steps[1].Bash)
	pm := steps[2].PymoduleRun
	c.False(pm == nil || pm.Repo != "local" || pm.Script != "driver" ||
		len(pm.Modules) != 1 || pm.Modules[0] != "util" ||
		len(pm.Args) != 1 || pm.Args[0] != "-x" || pm.Cwd != "/w",
		"pymodule_run step not intact: %+v", pm)
}

func TestAgentSendRendersStepSummaries(t *testing.T) {
	c := assert.NewAborting(t)
	sp := &fakeSpawner{sendResult: SendResult{Steps: []protocol.StepSummary{
		{Index: 0, Tool: "read", Where: protocol.StepSiteChild, Outcome: "ok", Bytes: 2048, Truncated: true},
		{Index: 1, Tool: "bash", Where: protocol.StepSiteSender, Outcome: "ok", Bytes: 42,
			Echo: "On branch send-steps\nnothing to commit"},
	}}}
	reg, ctx := newAgentTools(t, sp)
	outRes, err := reg.Execute(ctx, "agent_send", json.RawMessage(
		`{"agent":"c_a","message":"m","steps":[{"echo":true,"read":{"path":"/r.md"}}]}`))
	out := outRes.Text
	c.Require().NoError(err, "agent_send")
	want := "delivered to c_a\n" +
		"step 0 read (child): ok, 2048 bytes (truncated)\n" +
		"step 1 bash (sender): ok, 42 bytes\n" +
		"  On branch send-steps\n" +
		"  nothing to commit\n"
	c.Eq(want, out, "agent_send result text")
}

// No steps in, no steps out: the pre-steps result text is byte-identical and
// the spec the daemon sees carries a nil Steps slice, not an empty one.
func TestAgentSendWithoutStepsUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	outRes, err := reg.Execute(ctx, "agent_send", json.RawMessage(`{"agent":"c_worker","message":"next step"}`))
	out := outRes.Text
	c.Require().NoError(err, "agent_send")
	c.Eq("delivered to c_worker\n", out, "result text")
	c.Require().Len(sp.sent, 1, "want 1 send, got %d", len(sp.sent))
	c.Nil(sp.sent[0].Steps, "an input without steps must leave Steps nil, got")
}

// The `steps` schema must carry the shape the model authors against: an array
// of objects whose `where` is enum-constrained to child/sender, and whose
// nested read/bash/pymodule_run objects expose the step fields. This pins the
// Schema builder's nested-object rendering the send-steps design depends on.
func TestAgentSendStepsSchemaShape(t *testing.T) {
	c := assert.NewCollecting(t)
	var schema map[string]any
	if err := json.Unmarshal(AgentSendBlueprint{}.InputSchema().JSON(), &schema); err != nil {
		t.Fatalf("steps schema is not JSON: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	c.Require().True(ok, "schema has no properties map; got %v", schema)
	steps, ok := props["steps"].(map[string]any)
	c.Require().True(ok, "steps property missing; got %v", props)
	c.Eq("array", steps["type"], "steps type")
	items, ok := steps["items"].(map[string]any)
	c.Require().True(ok, "steps has no items schema; got %v", steps)
	itemProps, ok := items["properties"].(map[string]any)
	c.Require().True(ok, "step item has no properties; got %v", items)
	for _, name := range []string{"where", "echo", "read", "bash", "pymodule_run"} {
		c.HasKey(itemProps, name, "step item property")
	}
	where, ok := itemProps["where"].(map[string]any)
	c.Require().True(ok, "where property missing; got %v", itemProps)
	enum, ok := where["enum"].([]any)
	c.Require().True(ok, "where has no enum; got %v", where)
	c.Len(enum, 2, "where enum")
	c.Eq("child", enum[0], "where enum[0]")
	c.Eq("sender", enum[1], "where enum[1]")
	for step, fields := range map[string][]string{
		"read":         {"path", "start", "end"},
		"bash":         {"command", "timeout_ms"},
		"pymodule_run": {"repo", "script", "cwd", "modules", "args"},
	} {
		obj, ok := itemProps[step].(map[string]any)
		c.Require().True(ok, "%s property missing; got %v", step, itemProps)
		p, ok := obj["properties"].(map[string]any)
		c.Require().True(ok, "%s has no nested properties; got %v", step, obj)
		for _, f := range fields {
			c.HasKey(p, f, "%s field", step)
		}
	}
	// The description is the contract the model authors steps against; it is
	// pinned verbatim (whitespace-normalised, as every schema description is).
	stepsDesc, _ := steps["description"].(string)
	c.StrContains(stepsDesc, "Optional tool calls the daemon runs when the message is sent", "steps description")
	c.StrContains(stepsDesc, "never put state-changing or long-running commands", "steps description")
}
