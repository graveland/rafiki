// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestQuotaReadoutEmptyWithNoPoll(t *testing.T) {
	c := newTestCockpit("")
	if got := c.quotaReadout(); got != "" {
		t.Errorf("quotaReadout() before any poll = %q, want empty", got)
	}
}

func TestQuotaReadoutShowsFreshData(t *testing.T) {
	c := newTestCockpit("")
	util := 0.42
	c.quota = &rafikiv1.GetRateLimitStatusResponse{
		FiveH:     &rafikiv1.RateLimitWindow{Utilization: &util},
		SevenD:    &rafikiv1.RateLimitWindow{},
		UpdatedAt: time.Now().Unix(),
	}
	got := c.quotaReadout()
	if !strings.Contains(got, "42%") {
		t.Errorf("quotaReadout() = %q, want it to contain 42%%", got)
	}
	if !strings.Contains(got, "5h") || !strings.Contains(got, "7d") {
		t.Errorf("quotaReadout() = %q, want both 5h and 7d labels", got)
	}
}

func TestQuotaReadoutHidesStaleData(t *testing.T) {
	c := newTestCockpit("")
	util := 0.42
	c.quota = &rafikiv1.GetRateLimitStatusResponse{
		FiveH:     &rafikiv1.RateLimitWindow{Utilization: &util},
		UpdatedAt: time.Now().Add(-1 * time.Hour).Unix(), // well past quotaStaleAfter
	}
	if got := c.quotaReadout(); got != "" {
		t.Errorf("quotaReadout() with a stale snapshot = %q, want empty", got)
	}
}
