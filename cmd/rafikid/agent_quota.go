// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/quota"
	"go.graveland.dev/rafiki/pkg/users"
)

// quotaReader implements tools.QuotaReader for one owner, shared by the fundi
// and MCP surfaces. userID is resolved at construction and never arrives as an
// argument — the same reasoning as controllerSpawner's selfID: a caller-supplied
// id would let one agent read another user's usage. Which constructor built the
// reader is the binding story, and the two stay separate: the fundi surface
// derives the owner from the child's spawn-time caller (empty on the resume
// paths, where quota_status answers "no data captured" rather than guess); the
// MCP surface binds the request's authenticated identity directly.
type quotaReader struct {
	store  *quota.Store
	userID string
}

// newControllerQuotaReader builds a reader for userID. userID may be empty
// (owner unknown, or unresolved on a resume path) — RateLimitStatus then
// always answers not-found rather than querying with an empty key.
func newControllerQuotaReader(c *Controller, userID string) *quotaReader {
	return &quotaReader{store: quota.NewStore(c.pool), userID: userID}
}

// newMCPQuotaReader builds a reader bound to the authenticated MCP caller.
func newMCPQuotaReader(store *quota.Store, owner users.Identity) *quotaReader {
	return &quotaReader{store: store, userID: owner.UserID}
}

func (r *quotaReader) RateLimitStatus(ctx context.Context) (tools.QuotaStatus, bool, error) {
	if r.userID == "" {
		return tools.QuotaStatus{}, false, nil
	}
	st, ok, err := r.store.Get(ctx, r.userID)
	if err != nil || !ok {
		return tools.QuotaStatus{}, ok, err
	}
	return tools.QuotaStatus{
		OrganizationID: st.OrganizationID,
		FiveH: tools.QuotaWindow{
			Utilization: st.FiveH.Utilization, ResetAt: st.FiveH.ResetAt, Status: st.FiveH.Status,
		},
		SevenD: tools.QuotaWindow{
			Utilization: st.SevenD.Utilization, ResetAt: st.SevenD.ResetAt, Status: st.SevenD.Status,
		},
		OverallStatus: st.OverallStatus,
		UpdatedAt:     st.UpdatedAt,
	}, true, nil
}
