// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestQuotaReadoutEmptyWithNoPoll(t *testing.T) {
	c := newTestCockpit("")
	assert.NewCollecting(t).Eq("", c.quotaReadout(), "quotaReadout() before any poll")
}

func TestQuotaReadoutShowsFreshData(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	util := 0.42
	c.quota = &rafikiv1.GetRateLimitStatusResponse{
		FiveH:     &rafikiv1.RateLimitWindow{Utilization: &util},
		SevenD:    &rafikiv1.RateLimitWindow{},
		UpdatedAt: timestamppb.New(time.Now()),
	}
	got := c.quotaReadout()
	ck.StrContains(got, "42%", "quotaReadout() = %q, want it to contain 42%%", got)
	ck.False(!strings.Contains(got, "5h") || !strings.Contains(got, "7d"), "quotaReadout() = %q, want both 5h and 7d labels", got)
}

func TestQuotaReadoutHidesStaleData(t *testing.T) {
	c := newTestCockpit("")
	util := 0.42
	c.quota = &rafikiv1.GetRateLimitStatusResponse{
		FiveH:     &rafikiv1.RateLimitWindow{Utilization: &util},
		UpdatedAt: timestamppb.New(time.Now().Add(-1 * time.Hour)), // well past quotaStaleAfter
	}
	assert.NewCollecting(t).Eq("", c.quotaReadout(), "quotaReadout() with a stale snapshot")
}
