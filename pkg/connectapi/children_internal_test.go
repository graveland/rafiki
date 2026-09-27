// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// A child that ran before this client attached must not read $0.00: the rail
// resumes from the log head, so its past turn_end events are never replayed
// and the seed is the only source for them.
func TestChildSummaryCarriesCost(t *testing.T) {
	c := assert.NewCollecting(t)
	cost := 1.25
	got := toProtoChild(protocol.ChildSummary{ChildID: "c1", CostUSD: &cost}, nil, nil)
	c.Require().NotNil(got.CostUsd, "cost_usd not carried")
	c.Eq(1.25, *got.CostUsd, "cost_usd")
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
	c := assert.NewAborting(t)
	absent := connectapiSpawnParams(&rafikiv1.SpawnRequest{Cwd: "/tmp"})
	c.Nil(absent.Script, "a request without a script spec must map to nil, got")

	got := connectapiSpawnParams(&rafikiv1.SpawnRequest{Cwd: "/tmp", Kind: "script",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{
			Repo:    "local",
			Script:  "driver",
			Modules: []string{"helper"},
			Args:    []string{"--one"},
		}})
	c.NotNil(got.Script, "the script spec was dropped by the mapping")
	c.False(got.Script.Repo != "local" || got.Script.Script != "driver" ||
		strings.Join(got.Script.Modules, ",") != "helper" ||
		strings.Join(got.Script.Args, ",") != "--one", "script spec round trip mismatch: %+v", got.Script)
}
