// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestRenderRateLimitStatus(t *testing.T) {
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
		if !strings.Contains(out, want) {
			t.Errorf("render output missing %q; got:\n%s", want, out)
		}
	}
	// 7d has no utilization reported -- must show the unknown marker, not "0%".
	if strings.Contains(out, "0% used") {
		t.Errorf("rendered a false 0%% for an unreported utilization; got:\n%s", out)
	}
}

func TestRenderRateLimitStatusAllUnknown(t *testing.T) {
	st := &rafikiv1.GetRateLimitStatusResponse{
		FiveH:  &rafikiv1.RateLimitWindow{},
		SevenD: &rafikiv1.RateLimitWindow{},
	}
	var sb strings.Builder
	renderRateLimitStatus(&sb, st)
	out := sb.String()
	if !strings.Contains(out, "—") {
		t.Errorf("expected the unknown marker for absent fields; got:\n%s", out)
	}
}
