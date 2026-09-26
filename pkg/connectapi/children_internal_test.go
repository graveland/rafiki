// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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

func TestSpawnRequestScriptSpecMapsOntoParams(t *testing.T) {
	absent := connectapiSpawnParams(&rafikiv1.SpawnRequest{Cwd: "/tmp"})
	if absent.Script != nil {
		t.Fatalf("a request without a script spec must map to nil, got %+v", absent.Script)
	}

	got := connectapiSpawnParams(&rafikiv1.SpawnRequest{Cwd: "/tmp", Kind: "script",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{
			Repo:    "local",
			Script:  "driver",
			Modules: []string{"helper"},
			Args:    []string{"--one"},
		}})
	if got.Script == nil {
		t.Fatal("the script spec was dropped by the mapping")
	}
	if got.Script.Repo != "local" || got.Script.Script != "driver" ||
		strings.Join(got.Script.Modules, ",") != "helper" ||
		strings.Join(got.Script.Args, ",") != "--one" {
		t.Fatalf("script spec round trip mismatch: %+v", got.Script)
	}
}
