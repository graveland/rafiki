// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestSendRequestShape(t *testing.T) {
	c := assert.NewCollecting(t)
	req := &rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_STEER,
		Blocks: []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi"}},
		}},
	}
	c.Eq(rafikiv1.SendMode_SEND_MODE_STEER, req.GetMode(), "Mode")
	c.Require().Len(req.GetBlocks(), 1, "Blocks length = %d, want 1", len(req.GetBlocks()))
	c.Eq("hi", req.GetBlocks()[0].GetText().GetText(), "block text")
}

func TestSpawnRequestBudgetsArePresenceSensitive(t *testing.T) {
	c := assert.NewCollecting(t)
	unset := &rafikiv1.SpawnRequest{Cwd: "/tmp"}
	c.False(unset.MaxDepth != nil || unset.MaxCost != nil || unset.MaxChildren != nil, "unset budgets must be nil pointers, not zero values")

	zeroDepth := int32(0)
	zeroCost := float64(0)
	set := &rafikiv1.SpawnRequest{Cwd: "/tmp", MaxDepth: &zeroDepth, MaxCost: &zeroCost}
	c.False(set.MaxDepth == nil || set.GetMaxDepth() != 0, "an explicitly-zero MaxDepth must be distinguishable from unset")
	c.False(set.MaxCost == nil || set.GetMaxCost() != 0, "an explicitly-zero MaxCost must be distinguishable from unset")
}

func TestChildSummaryShape(t *testing.T) {
	c := assert.NewCollecting(t)
	pid := int32(4242)
	s := &rafikiv1.ChildSummary{
		ChildId: "c_1", Name: "scout", Kind: "fundi", Status: "idle",
		Model: "claude-opus-5", Cwd: "/tmp", Pid: &pid,
		Labels: map[string]string{"rafiki/parent": "c_0"},
	}
	c.False(s.GetPid() != 4242 || s.GetLabels()["rafiki/parent"] != "c_0", "ChildSummary round-trip failed: %+v", s)
	// Pid is optional: an exited child has none, and 0 is a real pid value.
	c.Nil((&rafikiv1.ChildSummary{}).Pid, "ChildSummary.Pid must be nil when unset")
}

func TestListChildrenResponseShape(t *testing.T) {
	r := &rafikiv1.ListChildrenResponse{Children: []*rafikiv1.ChildSummary{{ChildId: "c_1"}}}
	assert.NewAborting(t).Len(r.GetChildren(), 1, "Children length = %d, want 1", len(r.GetChildren()))
}
