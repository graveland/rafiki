package msg_test

import (
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/msg"
)

// TestStopReasonRoundTrip pins the wire spellings: String and ParseStopReason
// must invert each other, and anything unmodelled must read as StopUnknown
// rather than a wrong reason.
func TestStopReasonRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)

	cases := []struct {
		reason msg.StopReason
		wire   string
	}{
		{reason: msg.StopUnknown, wire: ""},
		{reason: msg.StopEndTurn, wire: "end_turn"},
		{reason: msg.StopMaxTokens, wire: "max_tokens"},
		{reason: msg.StopToolUse, wire: "tool_use"},
		{reason: msg.StopStopSequence, wire: "stop_sequence"},
		{reason: msg.StopRefusal, wire: "refusal"},
		{reason: msg.StopPause, wire: "pause_turn"},
	}
	for _, tc := range cases {
		c.Eq(tc.wire, tc.reason.String(), "%v renders its wire spelling", tc.reason)
		c.Eq(tc.reason, msg.ParseStopReason(tc.wire), "ParseStopReason(%q)", tc.wire)
	}

	c.Eq(msg.StopUnknown, msg.ParseStopReason("stop"), `"stop" is not a reason this package models`)
	c.Eq(msg.StopUnknown, msg.ParseStopReason(""), "an absent stop reason")
	c.Eq(msg.StopUnknown, msg.ParseStopReason("END_TURN"), "parsing is exact")
}

// TestUsageValPtr pins the nil-means-unreported contract: a nil count reads as
// zero, and Ptr hands back a cell the caller owns.
func TestUsageValPtr(t *testing.T) {
	c := assert.NewAborting(t)

	c.Eq(int64(0), msg.Val(nil), "a count the provider did not report reads as zero")

	p := msg.Ptr(42)
	c.Eq(int64(42), msg.Val(p))
	c.Eq(int64(42), *p)

	*p = 7
	c.Eq(int64(7), msg.Val(p), "Ptr points at storage the caller owns")

	c.NotSame(msg.Ptr(1), msg.Ptr(1), "each Ptr is its own cell")
}
