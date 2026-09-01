// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// A child that ran before this client attached must not read $0.00: the rail
// resumes from the log head, so its past turn_end events are never replayed
// and the seed is the only source for them.
func TestChildSummaryCarriesCost(t *testing.T) {
	cost := 1.25
	got := toProtoChild(protocol.ChildSummary{ChildID: "c1", CostUSD: &cost}, nil, nil)
	if got.CostUsd == nil {
		t.Fatal("cost_usd not carried")
	}
	if *got.CostUsd != 1.25 {
		t.Errorf("cost_usd = %v, want 1.25", *got.CostUsd)
	}
}

// Unset stays unset. Zero means "spent nothing", which is a different fact
// from "no database configured".
func TestChildSummaryOmitsUnknownCost(t *testing.T) {
	got := toProtoChild(protocol.ChildSummary{ChildID: "c1"}, nil, nil)
	if got.CostUsd != nil {
		t.Errorf("cost_usd = %v, want nil for an unreported cost", *got.CostUsd)
	}
}
