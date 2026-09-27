// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestRenderRateLimitStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	util5 := 0.42
	reset5 := time.Now().Add(2 * time.Hour).Unix()
	st := &rafikiv1.GetRateLimitStatusResponse{
		OrganizationId: "org_123",
		FiveH:          &rafikiv1.RateLimitWindow{Utilization: &util5, ResetAt: &reset5, Status: "allowed"},
		SevenD:         &rafikiv1.RateLimitWindow{Status: "allowed_warning"}, // no utilization/reset reported
		OverallStatus:  "allowed_warning",
		UpdatedAt:      time.Now().Unix(),
	}

	var sb strings.Builder
	renderRateLimitStatus(&sb, st)
	out := sb.String()

	for _, want := range []string{"org_123", "42%", "allowed_warning", "5h ", "7d "} {
		c.StrContains(out, want, "render output missing")
	}
	// 7d has no utilization reported -- must show the unknown marker, not "0%".
	c.NotStrContains(out, "0% used", "rendered a false 0%% for an unreported utilization; got:\n")
}

func TestRenderRateLimitStatusAllUnknown(t *testing.T) {
	st := &rafikiv1.GetRateLimitStatusResponse{
		FiveH:  &rafikiv1.RateLimitWindow{},
		SevenD: &rafikiv1.RateLimitWindow{},
	}
	var sb strings.Builder
	renderRateLimitStatus(&sb, st)
	out := sb.String()
	assert.NewCollecting(t).StrContains(out, "—", "expected the unknown marker for absent fields; got:\n")
}
