// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"net/http"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestParseHeadersAbsentMeansNotPassthrough(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	_, ok := ParseHeaders(h)
	assert.NewAborting(t).False(ok, "ParseHeaders reported ok on a response with no unified-ratelimit headers")
}

func TestParseHeadersFullSet(t *testing.T) {
	c := assert.NewCollecting(t)
	h := http.Header{}
	h.Set("Anthropic-Organization-Id", "org_123")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "2026-09-03T18:00:00Z")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.81")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1798934400") // fallback unix-seconds form
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed_warning")

	st, ok := ParseHeaders(h)
	c.Require().True(ok, "ParseHeaders reported ok=false on a full header set")
	c.Eq("org_123", st.OrganizationID, "OrganizationID")
	c.Eq("allowed_warning", st.OverallStatus, "OverallStatus")
	c.False(st.FiveH.Utilization == nil || *st.FiveH.Utilization != 0.42, "FiveH.Utilization = %v, want 0.42", st.FiveH.Utilization)
	c.Eq("allowed", st.FiveH.Status, "FiveH.Status")
	wantReset := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	c.False(st.FiveH.ResetAt == nil || !st.FiveH.ResetAt.Equal(wantReset), "FiveH.ResetAt = %v, want %v", st.FiveH.ResetAt, wantReset)
	c.False(st.SevenD.Utilization == nil || *st.SevenD.Utilization != 0.81, "SevenD.Utilization = %v, want 0.81", st.SevenD.Utilization)
	c.False(st.SevenD.ResetAt == nil || st.SevenD.ResetAt.Unix() != 1798934400, "SevenD.ResetAt = %v, want unix 1798934400", st.SevenD.ResetAt)
	c.Eq("allowed_warning", st.SevenD.Status, "SevenD.Status")
}

func TestParseHeadersMalformedFieldsOmittedNotFatal(t *testing.T) {
	c := assert.NewCollecting(t)
	// Only a status header present -- still a real passthrough response
	// (Anthropic may omit utilization/reset on some responses), and a
	// garbage utilization value must not sink the whole capture.
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "not-a-number")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "not-a-time")

	st, ok := ParseHeaders(h)
	c.Require().True(ok, "ParseHeaders reported ok=false when a status header was present")
	c.Eq("allowed", st.FiveH.Status, "FiveH.Status")
	c.Nil(st.FiveH.Utilization, "FiveH.Utilization")
	c.Nil(st.FiveH.ResetAt, "FiveH.ResetAt")
}
