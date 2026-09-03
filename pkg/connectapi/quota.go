// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// RateLimitWindow is one Anthropic subscription rate-limit window's snapshot.
// A package-local mirror of quota.Window rather than that type directly, the
// same reason ModelRow mirrors routing's pricing type: this interface should
// not force every implementer to link pkg/quota (and, through it, pgx).
type RateLimitWindow struct {
	Utilization *float64
	ResetAt     *time.Time
	Status      string
}

// RateLimitStatus is one user's latest captured Anthropic subscription
// snapshot.
type RateLimitStatus struct {
	OrganizationID string
	FiveH          RateLimitWindow
	SevenD         RateLimitWindow
	OverallStatus  string
	UpdatedAt      time.Time
}

// QuotaReader answers the CALLER's own latest rate-limit snapshot -- never an
// arbitrary user's, which is why it takes no id: the daemon resolves who is
// asking from ctx, the same way the Spawn adapter's spawnOwner does.
type QuotaReader interface {
	RateLimitStatus(ctx context.Context) (RateLimitStatus, bool, error)
}

// SetQuotaReader attaches the quota source. Post-construction setter for the
// same reason as SetChildLister: the Controller is built after this Server.
func (s *Server) SetQuotaReader(q QuotaReader) { s.quota.Store(&q) }

// GetRateLimitStatus returns the caller's own latest captured snapshot.
// NotFound (not an empty message) means this user has never made an
// OAuth-passthrough call -- expected, not an error.
func (s *Server) GetRateLimitStatus(
	ctx context.Context,
	_ *connect.Request[rafikiv1.GetRateLimitStatusRequest],
) (*connect.Response[rafikiv1.GetRateLimitStatusResponse], error) {
	p := s.quota.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("quota reader not yet wired"))
	}
	st, ok, err := (*p).RateLimitStatus(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			errors.New("no rate-limit data captured for this user yet"))
	}
	return connect.NewResponse(&rafikiv1.GetRateLimitStatusResponse{
		OrganizationId: st.OrganizationID,
		FiveH:          toProtoWindow(st.FiveH),
		SevenD:         toProtoWindow(st.SevenD),
		OverallStatus:  st.OverallStatus,
		UpdatedAt:      st.UpdatedAt.Unix(),
	}), nil
}

func toProtoWindow(w RateLimitWindow) *rafikiv1.RateLimitWindow {
	out := &rafikiv1.RateLimitWindow{Status: w.Status}
	if w.Utilization != nil {
		v := *w.Utilization
		out.Utilization = &v
	}
	if w.ResetAt != nil {
		v := w.ResetAt.Unix()
		out.ResetAt = &v
	}
	return out
}
