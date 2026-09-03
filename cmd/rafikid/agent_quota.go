// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/quota"
)

// controllerQuotaReader implements tools.QuotaReader for one user.
//
// userID is closed over at construction and never arrives as an argument —
// the same reasoning as controllerSpawner's selfID: a caller-supplied id
// would let one agent read another user's usage.
type controllerQuotaReader struct {
	store  *quota.Store
	userID string
}

// newControllerQuotaReader builds a reader for userID. userID may be empty
// (owner unknown, or unresolved on a resume path) — RateLimitStatus then
// always answers not-found rather than querying with an empty key.
func newControllerQuotaReader(c *Controller, userID string) *controllerQuotaReader {
	return &controllerQuotaReader{store: quota.NewStore(c.pool), userID: userID}
}

func (r *controllerQuotaReader) RateLimitStatus(ctx context.Context) (tools.QuotaStatus, bool, error) {
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
