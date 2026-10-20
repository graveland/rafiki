// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// gateStubTool records Execute calls so a test can tell whether the call
// reached the stub or was refused by the gate.
type gateStubTool struct {
	name  string
	calls int
}

func (s *gateStubTool) Name() string        { return s.name }
func (s *gateStubTool) Description() string { return "stub " + s.name }
func (s *gateStubTool) InputSchema() tools.Schema {
	return tools.Schema{Type: "object"}
}
func (s *gateStubTool) Execute(ctx context.Context, in tools.ToolInput) (tools.ToolResult, error) {
	s.calls++
	return tools.NewTextResult("ran " + s.name), nil
}

func gateStubTools() (*gateStubTool, *gateStubTool, *gateStubTool, []tools.Tool) {
	spawn, report, result := &gateStubTool{name: "agent_spawn"}, &gateStubTool{name: "agent_report"}, &gateStubTool{name: "agent_result"}
	return spawn, report, result, []tools.Tool{spawn, report, result}
}

// A halted child's tool calls are refused with the budget message — except the
// two hand-back tools, which are exactly how a halted subagent returns its
// results.
func TestMCPBudgetGateRefusesChildToolsExceptReportAndResult(t *testing.T) {
	ck := assert.NewAborting(t)
	spawn, report, result, ts := gateStubTools()
	gated := gateToolsOnBudget(ts, func() (string, bool) { return "c_root", true })
	ck.Len(gated, len(ts), "gateToolsOnBudget must return one tool per input tool")

	_, err := gated[0].Execute(context.Background(), tools.ToolInput(`{}`))
	ck.Require().Error(err, "a halted child must not run agent_spawn")
	ck.StrContains(err.Error(), "budget exhausted on c_root", "refusal must name the breached root; got %v", err)
	ck.Eq(0, spawn.calls, "agent_spawn must be refused before reaching the tool")

	// The hand-back tools reach the stub.
	for i, want := range []string{"agent_report", "agent_result"} {
		idx := i + 1
		res, err := gated[idx].Execute(context.Background(), tools.ToolInput(`{}`))
		ck.NoError(err, "%s must stay callable while halted", want)
		ck.Eq("ran "+want, res.Text, want)
	}
	ck.Eq(1, report.calls, "agent_report calls")
	ck.Eq(1, result.calls, "agent_result calls")

	// The exemption is by NAME, and Name() is not overridden by the wrapper.
	for i, want := range []string{"agent_spawn", "agent_report", "agent_result"} {
		ck.Eq(want, gated[i].Name(), "wrapped tool must keep its name")
	}
}

// When the child is not halted, every tool reaches its stub: the gate is a
// no-op wrapper and must not eat calls.
func TestMCPBudgetGateLiftsWhenNotHalted(t *testing.T) {
	ck := assert.NewAborting(t)
	spawn, report, result, ts := gateStubTools()
	gated := gateToolsOnBudget(ts, func() (string, bool) { return "", false })
	for i, want := range []string{"agent_spawn", "agent_report", "agent_result"} {
		res, err := gated[i].Execute(context.Background(), tools.ToolInput(`{}`))
		ck.NoError(err, "%s must run when not halted", want)
		ck.Eq("ran "+want, res.Text, want)
	}
	ck.Eq(1, spawn.calls, "agent_spawn calls")
	ck.Eq(1, report.calls, "agent_report calls")
	ck.Eq(1, result.calls, "agent_result calls")
}

// The gate is wired on the MCP face for CHILD callers only: a child whose
// position in c.breaches is marked gets the budget refusal on task_list, while
// the same call through a user-credential server is unaffected — a user has no
// position in a budget tree and is never gated.
func TestMCPBudgetGateWiredForChildCallersOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	face, _ := mcpFaceFixture(t)
	ctrl := face.controller()
	registerMCPChild(t, ctrl, "u-owner", "c-child")
	ctrl.breaches.mark("c-child")

	childReq := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	childReq = childReq.WithContext(server.WithIdentity(childReq.Context(), &server.Identity{
		UserID: "u-owner", ChildID: "c-child", Via: server.ProvenanceChildToken,
	}))
	childSess := mcpConnect(t, face.getServer(childReq))
	res, err := childSess.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_list"})
	ck.NoError(err, "task_list over the child server")
	ck.True(res.IsError, "a breached child's task_list must be an error result")
	ck.StrContains(mcpToolResultText(t, res), "budget exhausted", "child refusal text; got %q", mcpToolResultText(t, res))

	userSess := mcpConnect(t, face.getServer(mcpRequestFor("u-alice")))
	res, err = userSess.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_list"})
	ck.NoError(err, "task_list over the user server")
	ck.False(res.IsError, "a user credential is never budget-gated")
	ck.NotStrContains(mcpToolResultText(t, res), "budget exhausted", "user result must not carry the gate message")
}
