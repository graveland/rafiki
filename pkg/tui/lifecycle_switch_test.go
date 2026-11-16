// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"sync"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	rafikiv1connect "go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// switchClient is the ControlClient the switch composition reaches. Only the
// six RPCs the switch issues are implemented: any other call lands on the
// embedded nil interface and panics, the loud answer to an RPC a test did not
// set up. Every call is recorded in order, so the assertions can check the
// SEQUENCE (the source must be stopped before it is closed) as well as the
// payloads.
type switchClient struct {
	rafikiv1connect.ControlClient

	mu      sync.Mutex
	events  []string
	spawned []*rafikiv1.SpawnRequest
	routing []string
	rename  []string
	// killErr, when set, is returned by the first Kill call.
	killErr error
	killed  int
	// sandboxed marks the fake source as sandboxed.
	sandboxed bool
	// workspaceTools is the mode the fake source reports on GetChild (the mode
	// the daemon resolved at spawn).
	workspaceTools string
}

func (f *switchClient) record(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *switchClient) GetChild(_ context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	f.record("getchild:" + req.Msg.GetChildId())
	src := &rafikiv1.ChildSummary{
		ChildId: req.Msg.GetChildId(),
		Name:    "scout",
		Cwd:     "/work/scout",
		Labels: map[string]string{
			"rafiki/parent": "c_coord",
			"rafiki/preset": "review:fixer",
			"team":          "x",
		},
		Routing:        "sort=price",
		WorkspaceTools: f.workspaceTools,
	}
	if f.sandboxed {
		src.Labels["rafiki/sandbox"] = "1"
	}
	cost := 3.5
	src.MaxCost = &cost
	return connect.NewResponse(&rafikiv1.GetChildResponse{Child: src}), nil
}

func (f *switchClient) Spawn(_ context.Context, req *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	f.mu.Lock()
	f.spawned = append(f.spawned, req.Msg)
	f.mu.Unlock()
	f.record("spawn")
	return connect.NewResponse(&rafikiv1.SpawnResponse{ChildId: "c_new"}), nil
}

func (f *switchClient) Kill(_ context.Context, req *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	f.mu.Lock()
	f.killed++
	killErr := f.killErr
	f.mu.Unlock()
	f.record("kill:" + req.Msg.GetChildId())
	if killErr != nil {
		return nil, killErr
	}
	return connect.NewResponse(&rafikiv1.KillResponse{}), nil
}

func (f *switchClient) Close(_ context.Context, req *connect.Request[rafikiv1.CloseRequest]) (*connect.Response[rafikiv1.CloseResponse], error) {
	f.record("close:" + req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.CloseResponse{}), nil
}

func (f *switchClient) Rename(_ context.Context, req *connect.Request[rafikiv1.RenameRequest]) (*connect.Response[rafikiv1.RenameResponse], error) {
	f.mu.Lock()
	f.rename = append(f.rename, req.Msg.GetName())
	f.mu.Unlock()
	f.record("rename:" + req.Msg.GetName())
	return connect.NewResponse(&rafikiv1.RenameResponse{Name: req.Msg.GetName()}), nil
}

func (f *switchClient) SetRouting(_ context.Context, req *connect.Request[rafikiv1.SetRoutingRequest]) (*connect.Response[rafikiv1.SetRoutingResponse], error) {
	f.mu.Lock()
	f.routing = append(f.routing, req.Msg.GetDelta())
	f.mu.Unlock()
	f.record("routing:" + req.Msg.GetDelta())
	return connect.NewResponse(&rafikiv1.SetRoutingResponse{ChildId: req.Msg.GetChildId(), Routing: req.Msg.GetDelta()}), nil
}

func newSwitchClient() *switchClient { return &switchClient{} }

// switchMsg drives the two commands of a switch against the fake client: the
// spawn step, then the finish step Update dispatches from spawnedMsg, exactly
// the path applySpawned wires.
func switchMsg(t *testing.T, c *Cockpit, p spawnParams) (spawnedMsg, switchedMsg) {
	t.Helper()
	sm, ok := c.switchSpawnCmd(p)().(spawnedMsg)
	assert.NewAborting(t).Require().True(ok, "switchSpawnCmd = %T", c.switchSpawnCmd(p))
	if sm.err != nil {
		return sm, switchedMsg{}
	}
	cm := c.switchTailCmd(sm)
	sw, ok := cm().(switchedMsg)
	assert.NewAborting(t).Require().True(ok, "switchFinishCmd = %T", cm)
	return sm, sw
}

// A switch of a LIVE source: the composition reads the source, spawns the
// fork with the source's cwd/parent/budget (and only its user labels), then
// stops and closes the source — in that order, because Close refuses a live
// child — renames the fork into the source's name, and replays the routing
// spec.
func TestSwitchCompositionCarriesSourceAndStopsBeforeClose(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_x")
	client := newSwitchClient()
	c.client = client

	p := spawnParams{
		kind:         protocol.KindFundi,
		model:        "openrouter/z-ai/glm-5.3-flash",
		cwd:          "/somewhere/else", // the form's client-side guess; the source's wins
		switchSource: "c_src",
		forkFrom:     &protocol.ForkFrom{ChildID: "c_src"},
	}
	sm, sw := switchMsg(t, c, p)
	ck.Require().Eq("c_new", sm.childID, "spawned id")
	ck.Require().NoError(sw.err, "switch finish")

	ck.EqDiff([]string{"getchild:c_src", "spawn", "kill:c_src", "close:c_src", "rename:scout", "routing:sort=price"},
		client.events, "the composition's RPC sequence")

	ck.Require().Len(client.spawned, 1, "spawns")
	req := client.spawned[0]
	ck.Eq("/somewhere/else", req.GetCwd(), "the form's cwd is used: it opens prefilled with the source's")
	ck.Eq("c_coord", req.GetParentChildId(), "a subagent's switch stays under its coordinator")
	ck.Eq("review:fixer", req.GetPreset(),
		"the source's preset re-resolves: its allowlists and prompt are part of the seat")
	ck.Nil(req.MaxCost, "a blank cap row is unlimited, not the source's cap coming back")
	ck.Require().NotNil(req.GetForkFrom(), "fork")
	ck.Eq("c_src", req.GetForkFrom().ChildId, "fork source")
	ck.EqDiff(map[string]string{"team": "x"}, req.GetLabels(),
		"only user labels carry; rafiki/*, fundi/* and the reserved owner label are dropped")

	// The name lands last, after the close: the live-sibling name rule holds
	// the name until the source leaves the live set.
	ck.EqDiff([]string{"kill:c_src", "close:c_src", "rename:scout"}, client.events[2:5], "rename after close")
}

// A live source is what Close refuses: the composition tolerates only the
// already-exited reason on the stop, and any other stop failure aborts BEFORE
// the close, leaving the source untouched.
func TestSwitchCompositionStopFailureAbortsBeforeClose(t *testing.T) {
	ck := assert.NewCollecting(t)

	// A plain refusal: no Close, no rename, the error names the step.
	c := newTestCockpit("c_x")
	client := newSwitchClient()
	client.killErr = connect.NewError(connect.CodePermissionDenied, nil)
	c.client = client
	_, sw := switchMsg(t, c, spawnParams{
		kind: protocol.KindFundi, model: "openrouter/z-ai/glm-5.3-flash",
		switchSource: "c_src", forkFrom: &protocol.ForkFrom{ChildID: "c_src"},
	})
	ck.Require().NotEq(nil, sw.err, "the stop failure must surface")
	ck.StrContains(sw.err.Error(), "stop c_src", "the failure names the step")
	ck.EqDiff([]string{"getchild:c_src", "spawn", "kill:c_src"}, client.events, "nothing runs after a refused stop")

	// Already-exited is tolerated and the switch proceeds.
	c2 := newTestCockpit("c_x")
	client2 := newSwitchClient()
	client2.killErr = rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, nil),
		protocol.ErrChildExited)
	c2.client = client2
	_, sw2 := switchMsg(t, c2, spawnParams{
		kind: protocol.KindFundi, model: "openrouter/z-ai/glm-5.3-flash",
		switchSource: "c_src", forkFrom: &protocol.ForkFrom{ChildID: "c_src"},
	})
	ck.Require().NoError(sw2.err, "ErrChildExited on the stop must not fail the switch")
	ck.EqDiff([]string{"getchild:c_src", "spawn", "kill:c_src", "close:c_src", "rename:scout", "routing:sort=price"},
		client2.events, "the switch proceeds past an already-exited source")
}

// A model switch reconstructs the source's spawn, so the workspace tools mode
// the daemon RESOLVED AT SPAWN rides the fork: a rafiki-mode claude child
// switched onto another model comes back rafiki-mode, and a native source keeps
// the row's native value. The channel is the rail row — ListChildren carries
// ChildSummary.workspace_tools, the seed puts it on the node, switchModelOf
// seeds the form's row from it, and params/buildSpawnRequest put the row on the
// request. Dropping any link silently reverts the continuation to Claude Code's
// built-in tools on its launch host.
//
// A native source's request carries "native" rather than "": the form sends
// the row verbatim (see spawnParams.workspaceTools) and the daemon stores
// "native" as the child's empty mode — so the continuation's persisted mode is
// empty either way, while a preset's rafiki cannot overrule what the row shows.
func TestSwitchKeepsTheSourceWorkspaceTools(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{"rafiki-mode source", protocol.WorkspaceToolsRafiki, protocol.WorkspaceToolsRafiki},
		// The daemon reports "" for a native child; the row seeds to its own
		// native default and the request carries that value.
		{"native source", "", protocol.WorkspaceToolsNative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCockpit("c_x")
			client := newSwitchClient()
			client.workspaceTools = tc.source
			c.client = client
			sum := summaryFor("c_src", "scout", 0)
			sum.Kind = protocol.KindClaude
			sum.Model = "claude-sonnet-4-5"
			sum.WorkspaceTools = tc.source
			c.rail.Seed([]*rafikiv1.ChildSummary{sum})
			c.selected = "c_src"

			c.switchModelOf("c_src", "claude-opus-4-1")
			ck.Require().NotNil(c.form, "the switch form must be open")
			ck.Eq(tc.want, c.form.workspaceTools(), "the form's workspace tools row is seeded from the source")

			// Rename so the switch is a CLONE: a claude-kind switch of a claude
			// source that keeps its name restarts in place (no spawn), and it is
			// the spawn this test is about.
			c.form.inputs[fieldName].SetValue("scout-2")
			p, problem := c.form.params(nil)
			ck.Require().Eq("", problem, "form params")
			ck.Require().True(p.keepSource, "a renamed switch is a clone")
			_, sw := switchMsg(t, c, p)
			ck.Require().NoError(sw.err, "switch finish")

			ck.Require().Len(client.spawned, 1, "spawns")
			ck.Eq(tc.want, client.spawned[0].GetWorkspaceTools(), "workspace_tools on the fork's spawn request")
		})
	}
}

// A sandboxed source refuses the switch outright: the sandbox block cannot be
// carried (ChildSummary does not expose it), so continuing would land the new
// child UNCONFINED on the default executor — a widening, not a neutral loss.
func TestSwitchCompositionRefusesASandboxedSource(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_x")
	client := newSwitchClient()
	c.client = client

	// The sandbox refusal happens on the source read, before any spawn.
	client.sandboxed = true
	p := spawnParams{
		kind: protocol.KindFundi, model: "openrouter/z-ai/glm-5.3-flash",
		switchSource: "c_src", forkFrom: &protocol.ForkFrom{ChildID: "c_src"},
	}
	sm, ok := c.switchSpawnCmd(p)().(spawnedMsg)
	ck.Require().True(ok, "switchSpawnCmd = %T", c.switchSpawnCmd(p))
	ck.Require().NotEq(nil, sm.err, "the sandboxed source must refuse")
	ck.StrContains(sm.err.Error(), "sandbox", "the refusal names the sandbox")
	ck.EqDiff([]string{"getchild:c_src"}, client.events, "nothing spawns after the refusal")
}
