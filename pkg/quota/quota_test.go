// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"net/http"
	"testing"
	"time"
)

func TestParseHeadersAbsentMeansNotPassthrough(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	_, ok := ParseHeaders(h)
	if ok {
		t.Fatal("ParseHeaders reported ok on a response with no unified-ratelimit headers")
	}
}

func TestParseHeadersFullSet(t *testing.T) {
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
	if !ok {
		t.Fatal("ParseHeaders reported ok=false on a full header set")
	}
	if st.OrganizationID != "org_123" {
		t.Errorf("OrganizationID = %q, want org_123", st.OrganizationID)
	}
	if st.OverallStatus != "allowed_warning" {
		t.Errorf("OverallStatus = %q, want allowed_warning", st.OverallStatus)
	}
	if st.FiveH.Utilization == nil || *st.FiveH.Utilization != 0.42 {
		t.Errorf("FiveH.Utilization = %v, want 0.42", st.FiveH.Utilization)
	}
	if st.FiveH.Status != "allowed" {
		t.Errorf("FiveH.Status = %q, want allowed", st.FiveH.Status)
	}
	wantReset := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	if st.FiveH.ResetAt == nil || !st.FiveH.ResetAt.Equal(wantReset) {
		t.Errorf("FiveH.ResetAt = %v, want %v", st.FiveH.ResetAt, wantReset)
	}
	if st.SevenD.Utilization == nil || *st.SevenD.Utilization != 0.81 {
		t.Errorf("SevenD.Utilization = %v, want 0.81", st.SevenD.Utilization)
	}
	if st.SevenD.ResetAt == nil || st.SevenD.ResetAt.Unix() != 1798934400 {
		t.Errorf("SevenD.ResetAt = %v, want unix 1798934400", st.SevenD.ResetAt)
	}
	if st.SevenD.Status != "allowed_warning" {
		t.Errorf("SevenD.Status = %q, want allowed_warning", st.SevenD.Status)
	}
}

func TestParseHeadersMalformedFieldsOmittedNotFatal(t *testing.T) {
	// Only a status header present -- still a real passthrough response
	// (Anthropic may omit utilization/reset on some responses), and a
	// garbage utilization value must not sink the whole capture.
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "not-a-number")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "not-a-time")

	st, ok := ParseHeaders(h)
	if !ok {
		t.Fatal("ParseHeaders reported ok=false when a status header was present")
	}
	if st.FiveH.Status != "allowed" {
		t.Errorf("FiveH.Status = %q, want allowed", st.FiveH.Status)
	}
	if st.FiveH.Utilization != nil {
		t.Errorf("FiveH.Utilization = %v, want nil for an unparseable value", st.FiveH.Utilization)
	}
	if st.FiveH.ResetAt != nil {
		t.Errorf("FiveH.ResetAt = %v, want nil for an unparseable value", st.FiveH.ResetAt)
	}
}
