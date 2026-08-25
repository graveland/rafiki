// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestSendRequestShape(t *testing.T) {
	req := &rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_STEER,
		Blocks: []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi"}},
		}},
	}
	if req.GetMode() != rafikiv1.SendMode_SEND_MODE_STEER {
		t.Errorf("Mode = %v, want STEER", req.GetMode())
	}
	if len(req.GetBlocks()) != 1 {
		t.Fatalf("Blocks length = %d, want 1", len(req.GetBlocks()))
	}
	if got := req.GetBlocks()[0].GetText().GetText(); got != "hi" {
		t.Errorf("block text = %q, want %q", got, "hi")
	}
}

func TestSpawnRequestBudgetsArePresenceSensitive(t *testing.T) {
	unset := &rafikiv1.SpawnRequest{Cwd: "/tmp"}
	if unset.MaxDepth != nil || unset.MaxCost != nil || unset.MaxChildren != nil {
		t.Error("unset budgets must be nil pointers, not zero values")
	}

	zeroDepth := int32(0)
	zeroCost := float64(0)
	set := &rafikiv1.SpawnRequest{Cwd: "/tmp", MaxDepth: &zeroDepth, MaxCost: &zeroCost}
	if set.MaxDepth == nil || set.GetMaxDepth() != 0 {
		t.Error("an explicitly-zero MaxDepth must be distinguishable from unset")
	}
	if set.MaxCost == nil || set.GetMaxCost() != 0 {
		t.Error("an explicitly-zero MaxCost must be distinguishable from unset")
	}
}

func TestChildSummaryShape(t *testing.T) {
	pid := int32(4242)
	s := &rafikiv1.ChildSummary{
		ChildId: "c_1", Name: "scout", Kind: "fundi", Status: "idle",
		Model: "claude-opus-5", Cwd: "/tmp", Pid: &pid,
		Labels: map[string]string{"rafiki/parent": "c_0"},
	}
	if s.GetPid() != 4242 || s.GetLabels()["rafiki/parent"] != "c_0" {
		t.Errorf("ChildSummary round-trip failed: %+v", s)
	}
	// Pid is optional: an exited child has none, and 0 is a real pid value.
	if (&rafikiv1.ChildSummary{}).Pid != nil {
		t.Error("ChildSummary.Pid must be nil when unset")
	}
}

func TestListChildrenResponseShape(t *testing.T) {
	r := &rafikiv1.ListChildrenResponse{Children: []*rafikiv1.ChildSummary{{ChildId: "c_1"}}}
	if len(r.GetChildren()) != 1 {
		t.Fatalf("Children length = %d, want 1", len(r.GetChildren()))
	}
}
