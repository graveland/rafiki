package main

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func ssWiringPool(t *testing.T, executors ...string) *ssFakePool {
	t.Helper()
	pool := &ssFakePool{live: ssLive(executors...), clients: map[string]executorpbconnect.ExecutorServiceClient{}}
	for _, id := range executors {
		_, client := ssScriptedServer(t, ssRun(ssResultText("out-"+id)))
		pool.clients[id] = client
	}
	return pool
}

// A sender step from a child-bound spawner resolves the spawner's own
// snapshot: selfID is the caller, never an argument.
func TestSendStepsControllerSpawnerPassesSelfAsCaller(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := ssWiringPool(t, "e-caller", "e-target")
	snaps := map[string]childstore.Snapshot{
		"c_mine":       ssFundi("/w", "e-caller", ""),
		"c_grandchild": ssFundi("/w", "e-target", ""),
	}
	sp := newControllerSpawner(spawnerFixture(t), "c_mine")
	sp.runner = ssRunner(pool, snaps)

	// The delivery itself fails (no live child process); only the step
	// routing is under test.
	_, _ = sp.Send(context.Background(), tools.SendSpec{
		ChildID: "c_grandchild", Message: "hi",
		Steps: []protocol.SendStep{ssBashStep(protocol.StepSiteSender, "echo x")},
	})
	c.True(slices.Equal([]string{"e-caller"}, pool.dialed), "executors dialed: %v", pool.dialed)
}

func TestSendStepsUserSpawnerRefusesSenderSteps(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := ssWiringPool(t, "e-target")
	snaps := map[string]childstore.Snapshot{"c_mine": ssFundi("/w", "e-target", "")}
	sp := &userSpawner{c: spawnerFixture(t), runner: ssRunner(pool, snaps)}

	_, err := sp.Send(context.Background(), tools.SendSpec{
		ChildID: "c_mine", Message: "hi",
		Steps: []protocol.SendStep{ssBashStep(protocol.StepSiteSender, "echo x")},
	})
	c.Error(err, "sender step from an operator caller")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Empty(pool.dialed, "no step may run when the send is refused")
}

func TestSendStepsSpawnerAppendsRenderedBlock(t *testing.T) {
	c := assert.NewCollecting(t)
	snaps := map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-target", "")}
	steps := []protocol.SendStep{ssBashStep(protocol.StepSiteChild, "echo x")}

	rendered, want, err := ssRunner(ssWiringPool(t, "e-target"), snaps).RunSendSteps(context.Background(), "", "t-1", steps)
	c.Require().NoError(err)

	frame, result, err := buildSendFrame(context.Background(), ssRunner(ssWiringPool(t, "e-target"), snaps), "", tools.SendSpec{
		ChildID: "t-1", Message: "msg", Steps: steps,
	})
	c.Require().NoError(err)
	var got map[string]string
	c.Require().NoError(json.Unmarshal(frame, &got))
	c.Eq("prompt", got["type"], "frame type")
	c.Eq("msg\n\n"+rendered, got["message"], "frame message")
	c.True(reflect.DeepEqual(want, result.Steps), "summaries: got %v want %v", result.Steps, want)
}

func TestSendStepsNoStepsLeavesFrameUntouched(t *testing.T) {
	c := assert.NewCollecting(t)
	frame, result, err := buildSendFrame(context.Background(), nil, "", tools.SendSpec{ChildID: "t-1", Message: "msg"})
	c.Require().NoError(err)
	var got map[string]string
	c.Require().NoError(json.Unmarshal(frame, &got))
	c.Eq("msg", got["message"], "message")
	c.Empty(result.Steps, "summaries")
}
