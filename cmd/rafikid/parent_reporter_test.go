// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"

	"github.com/multigres/testkit/assert"
)

// prLongMessage returns a string whose JSON encoding is exactly n bytes: the
// two surrounding quotes, a two-byte escape for the trailing quote, and n-4
// filler characters (the trailing quote is the escaping hazard the real cap
// is about, so the last character forces an escape).
func prLongMessage(n int) string {
	if n < 5 {
		return ""
	}
	return strings.Repeat("a", n-4) + `"`
}

// TestParentReporterBoundsEncodedLength pins the encoded-size caps: the cap
// is on the JSON-ENCODED bytes — the shape the Connect verb's validateJSONValue
// measures — not on the raw string, so a message full of quote escapes tips
// over the limit a byte count of the raw text would miss. Exactly at the
// limit is accepted; one over is refused with the exact error, never
// truncated. Both tools' strings (report message, result) are capped the
// same way.
func TestParentReporterBoundsEncodedLength(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scriptHubFixture(t)
	scriptParented(c)
	rep := newParentReporter(c, "c_script")
	ctx := context.Background()

	// Report: one byte over the encoded cap, refused with the exact error.
	err := rep.Report(ctx, "message", prLongMessage(connectapi.MaxReportDataBytes+1))
	ck.Require().Error(err, "an over-cap message must be refused")
	ck.Eq(err.Error(),
		"message is 4097 bytes encoded; the limit is 4096",
		"exact refusal text")
	// Sanity: the fixture string really does encode to the over-cap size.
	enc, _ := json.Marshal(prLongMessage(connectapi.MaxReportDataBytes + 1))
	ck.Eq(len(enc), connectapi.MaxReportDataBytes+1, "fixture encodes to cap+1")

	// Report: exactly at the cap is accepted.
	ck.NoError(rep.Report(ctx, "message", prLongMessage(connectapi.MaxReportDataBytes)),
		"a message encoding to exactly the cap must be accepted")

	// SetResult: same rule, its own constant and its own error text.
	err = rep.SetResult(ctx, prLongMessage(connectapi.MaxResultBytes+1))
	ck.Require().Error(err, "an over-cap result must be refused")
	ck.Eq(err.Error(),
		"result is 4097 bytes encoded; the limit is 4096",
		"exact refusal text")
	ck.NoError(rep.SetResult(ctx, prLongMessage(connectapi.MaxResultBytes)),
		"a result encoding to exactly the cap must be accepted")
}

// TestParentReporterReportReachesParent pins the binding end to end: a
// parented child's report through parentReporter lands in its PARENT's event
// buffer, rendered with the same `agent … reported message:` fragment the
// Connect verb produces — the tool layer and the Connect verb share one path
// (the hub), so there is no second wording to drift.
func TestParentReporterReportReachesParent(t *testing.T) {
	ck := assert.NewAborting(t)
	c, cap, _, clk := scriptHubFixture(t)
	scriptParented(c)
	rep := newParentReporter(c, "c_script")

	ck.NoError(rep.Report(context.Background(), "message", "hello from worker"), "Report")
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Require().Len(batches, 1, "want 1 batch to the parent, got %d", len(batches))
	ck.Eq(batches[0].childID, "c_coord", "the batch must reach the parent")
	ck.Eq(batches[0].source, scriptEventSource, "batch source")
	ck.Require().Len(batches[0].fragments, 1, "one report, one fragment")
	// The JSON-string payload is decoded for the frame: the parent reads the
	// raw message, not its quoted encoding.
	ck.StrContains(batches[0].fragments[0], "agent c_script (worker) reported message:",
		"the fragment must carry the shared wording")
	ck.StrContains(batches[0].fragments[0], "hello from worker",
		"the message must arrive decoded, not as a quoted JSON string")
}

// TestParentReporterSetResultStoresOnSelf pins the self-half of the binding:
// SetResult writes the caller's OWN row, verbatim JSON — the encoded string —
// exactly as the Connect verb would have stored it.
func TestParentReporterSetResultStoresOnSelf(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scriptHubFixture(t)
	scriptParented(c)
	rep := newParentReporter(c, "c_script")

	ck.NoError(rep.SetResult(context.Background(), "DONE — all checks green"), "SetResult")

	snap, ok := c.st.Get("c_script")
	ck.Require().True(ok, "child vanished")
	ck.Eq(snap.Result, `"DONE — all checks green"`,
		"the result must be stored as the JSON value the hub expects (a quoted string)")
	// And nobody else's row was touched.
	other, ok := c.st.Get("c_coord")
	ck.Require().True(ok, "parent vanished")
	ck.Eq(other.Result, "", "a self-position verb must never write the parent's row")
}
