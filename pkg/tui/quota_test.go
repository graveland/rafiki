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

// A percentage says how close the limit is; the reset says whether to wait it
// out. Both windows carry their own countdown, since they reset on very
// different clocks and a bare percentage would leave that to arithmetic.
func TestQuotaReadoutShowsTimeToReset(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	util5, util7 := 0.94, 0.60
	c.quota = &rafikiv1.GetRateLimitStatusResponse{
		FiveH: &rafikiv1.RateLimitWindow{
			Utilization: &util5, ResetAt: timestamppb.New(time.Now().Add(3*time.Hour + 3*time.Minute)),
		},
		SevenD: &rafikiv1.RateLimitWindow{
			Utilization: &util7, ResetAt: timestamppb.New(time.Now().Add(50 * time.Hour)),
		},
		UpdatedAt: timestamppb.New(time.Now()),
	}
	got := c.quotaReadout()
	ck.StrContains(got, "5h 94% (resets in 3h", "quotaReadout() = %q, want the 5h countdown", got)
	ck.StrContains(got, "7d 60% (resets in 2d", "quotaReadout() = %q, want the 7d countdown", got)
}

// A reset the snapshot already passed describes a window that has since
// rolled over, so a countdown would be a lie; the utilization is shown alone
// and left to go stale with the snapshot.
func TestQuotaReadoutOmitsPastReset(t *testing.T) {
	c := newTestCockpit("")
	util := 0.10
	c.quota = &rafikiv1.GetRateLimitStatusResponse{
		FiveH: &rafikiv1.RateLimitWindow{
			Utilization: &util, ResetAt: timestamppb.New(time.Now().Add(-time.Minute)),
		},
		UpdatedAt: timestamppb.New(time.Now()),
	}
	got := c.quotaReadout()
	assert.NewCollecting(t).NotStrContains(got, "resets in", "quotaReadout() = %q, want no countdown for a past reset", got)
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
