// SPDX-License-Identifier: Apache-2.0

// Package quota captures Anthropic's per-account subscription rate-limit
// headers (anthropic-ratelimit-unified-*) off OAuth-passthrough responses to
// api.anthropic.com, and answers the latest captured snapshot per user.
//
// This is scoped to passthrough traffic ONLY. API-token usage (a separate
// billing relationship) has its own usage endpoint and is not this package's
// concern.
package quota

import (
	"net/http"
	"strconv"
	"time"
)

// Window is one rate-limit window's snapshot (the "5h" or "7d" family of
// headers). Utilization and ResetAt are pointers because a header present but
// in an unrecognized shape must stay distinguishable from "Anthropic did not
// send this field" -- both leave the pointer nil, and a client renders both
// as unknown rather than a false zero.
type Window struct {
	Utilization *float64
	ResetAt     *time.Time
	Status      string
}

// Status is one user's full captured snapshot.
type Status struct {
	OrganizationID string
	FiveH          Window
	SevenD         Window
	OverallStatus  string
	// UpdatedAt is set by Store.Get from the stored row's updated_at. Zero
	// when Status is freshly built by ParseHeaders, before it has ever been
	// persisted.
	UpdatedAt time.Time
}

const (
	hdrOrgID    = "Anthropic-Organization-Id"
	hdrOverall  = "Anthropic-Ratelimit-Unified-Status"
	hdr5hUtil   = "Anthropic-Ratelimit-Unified-5h-Utilization"
	hdr5hReset  = "Anthropic-Ratelimit-Unified-5h-Reset"
	hdr5hStatus = "Anthropic-Ratelimit-Unified-5h-Status"
	hdr7dUtil   = "Anthropic-Ratelimit-Unified-7d-Utilization"
	hdr7dReset  = "Anthropic-Ratelimit-Unified-7d-Reset"
	hdr7dStatus = "Anthropic-Ratelimit-Unified-7d-Status"
)

// ParseHeaders extracts a Status from an upstream response's headers. ok is
// false when none of the anthropic-ratelimit-unified-* headers are present --
// the set Anthropic emits only on a genuine OAuth-passthrough response from
// api.anthropic.com, never through OpenRouter or any other routed provider.
//
// Each field parses independently and defensively: a header present but in an
// unexpected shape is simply omitted (left nil/empty) rather than failing the
// whole capture, since this path must never cost a real response anything.
func ParseHeaders(h http.Header) (Status, bool) {
	overall := h.Get(hdrOverall)
	fiveStatus := h.Get(hdr5hStatus)
	sevenStatus := h.Get(hdr7dStatus)
	if overall == "" && fiveStatus == "" && sevenStatus == "" {
		return Status{}, false
	}
	return Status{
		OrganizationID: h.Get(hdrOrgID),
		FiveH: Window{
			Utilization: parseFloat(h.Get(hdr5hUtil)),
			ResetAt:     parseResetTime(h.Get(hdr5hReset)),
			Status:      fiveStatus,
		},
		SevenD: Window{
			Utilization: parseFloat(h.Get(hdr7dUtil)),
			ResetAt:     parseResetTime(h.Get(hdr7dReset)),
			Status:      sevenStatus,
		},
		OverallStatus: overall,
	}, true
}

func parseFloat(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// parseResetTime accepts either an RFC3339 timestamp (the convention
// Anthropic's existing per-key ratelimit-reset headers use) or a bare unix
// second count, since the unified headers' exact format is not yet pinned
// down against a live response.
func parseResetTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		t := time.Unix(secs, 0).UTC()
		return &t
	}
	return nil
}
