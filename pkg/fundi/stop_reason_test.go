// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// A halt wins over the cost guardrail even when both would fire: the halt
// reason is the one returned, not the child's own budget message.
func TestStopReasonHaltWinsOverCost(t *testing.T) {
	ck := assert.NewAborting(t)
	halted := func() (bool, string) { return true, "agent tree budget exhausted on c_root" }
	stop, why := stopReason(halted, 10, 2)
	ck.True(stop, "a halt must stop the turn")
	ck.Eq("agent tree budget exhausted on c_root", why, "the halt reason must win over the cost guardrail's")
}

// Not halted falls through to the cost guardrail, which fires on the same
// threshold it always did.
func TestStopReasonFallsBackToCostGuardrail(t *testing.T) {
	ck := assert.NewAborting(t)
	halted := func() (bool, string) { return false, "" }
	stop, why := stopReason(halted, 2, 2)
	ck.True(stop, "running total at the cap must stop the turn")
	ck.StrContains(why, "cost budget", "the cost guardrail's reason; got %q", why)

	stop, why = stopReason(halted, 1, 2)
	ck.False(stop, "under the cap must not stop")
	ck.Eq("", why, "reason when not stopping")
}

// Nil Halted is the old behavior exactly: only the cost guardrail decides.
func TestStopReasonNilHalted(t *testing.T) {
	ck := assert.NewAborting(t)
	stop, why := stopReason(nil, 1, 2)
	ck.False(stop, "nil halted, under cap")
	ck.Eq("", why, "reason")
	stop, why = stopReason(nil, 3, 2)
	ck.True(stop, "nil halted, over cap still stops via the guardrail")
	ck.StrContains(why, "cost budget", "reason")
}
