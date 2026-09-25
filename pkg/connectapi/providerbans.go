// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ErrProviderNotBanned is what a ProviderBanManager returns when Unban names a
// provider with no live operator ban. Package-local, like ErrSkillNotFound, so
// this interface doesn't force implementers onto pkg/routing's sentinels.
var ErrProviderNotBanned = errors.New("provider has no active operator ban")

// ErrInvalidProviderBan is what a ProviderBanManager returns for an unusable
// provider slug or duration.
var ErrInvalidProviderBan = errors.New("invalid provider ban")

// ProviderBanRow is one live exclusion of an OpenRouter provider from routing.
type ProviderBanRow struct {
	Provider  string
	ModelLine string
	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time // zero = until lifted
	Note      string
}

// ProviderBanManager is the narrow slice of the daemon needed to list and
// manage provider bans. Implementations enforce who may ban: a mutation from
// an identity that may not is refused with a connect PermissionDenied error,
// which the handlers pass through unchanged.
type ProviderBanManager interface {
	// ListProviderBans returns every live exclusion and whether bans survive
	// a daemon restart.
	ListProviderBans(ctx context.Context) ([]ProviderBanRow, bool, error)
	// BanProvider bans provider from every model line; d == 0 means until
	// lifted.
	BanProvider(ctx context.Context, provider string, d time.Duration, note string) (ProviderBanRow, bool, error)
	UnbanProvider(ctx context.Context, provider string) error
}

// SetProviderBanManager attaches the provider-ban backend. A nil manager is
// refused rather than stored, the same rule as SetSkillManager: storing &m
// for a nil interface would defeat providerBanManager's Unavailable path and
// nil-panic the first handler call.
func (s *Server) SetProviderBanManager(m ProviderBanManager) {
	if m == nil {
		return
	}
	s.providerBans.Store(&m)
}

func (s *Server) providerBanManager() (ProviderBanManager, error) {
	p := s.providerBans.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("provider-ban backend not yet wired"))
	}
	return *p, nil
}

func providerBanError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, ErrProviderNotBanned) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	if errors.Is(err, ErrInvalidProviderBan) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func toProtoProviderBan(r ProviderBanRow) *rafikiv1.ProviderBan {
	out := &rafikiv1.ProviderBan{
		Provider:  r.Provider,
		ModelLine: r.ModelLine,
		Reason:    r.Reason,
		CreatedAt: r.CreatedAt.Unix(),
		Note:      r.Note,
	}
	if !r.ExpiresAt.IsZero() {
		v := r.ExpiresAt.Unix()
		out.ExpiresAt = &v
	}
	return out
}

func (s *Server) ListProviderBans(
	ctx context.Context, _ *connect.Request[rafikiv1.ListProviderBansRequest],
) (*connect.Response[rafikiv1.ListProviderBansResponse], error) {
	m, err := s.providerBanManager()
	if err != nil {
		return nil, err
	}
	rows, persistent, err := m.ListProviderBans(ctx)
	if err != nil {
		return nil, providerBanError(err)
	}
	out := make([]*rafikiv1.ProviderBan, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoProviderBan(r))
	}
	return connect.NewResponse(&rafikiv1.ListProviderBansResponse{Bans: out, Persistent: persistent}), nil
}

func (s *Server) BanProvider(
	ctx context.Context, req *connect.Request[rafikiv1.BanProviderRequest],
) (*connect.Response[rafikiv1.BanProviderResponse], error) {
	m, err := s.providerBanManager()
	if err != nil {
		return nil, err
	}
	provider := strings.TrimSpace(req.Msg.GetProvider())
	if provider == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider is required"))
	}
	var d time.Duration
	if req.Msg.DurationSeconds != nil {
		secs := req.Msg.GetDurationSeconds()
		if secs <= 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("duration_seconds must be positive when set, got %d (omit it to ban until lifted)", secs))
		}
		d = time.Duration(secs) * time.Second
	}
	row, persistent, err := m.BanProvider(ctx, provider, d, req.Msg.GetNote())
	if err != nil {
		return nil, providerBanError(err)
	}
	return connect.NewResponse(&rafikiv1.BanProviderResponse{Ban: toProtoProviderBan(row), Persistent: persistent}), nil
}

func (s *Server) UnbanProvider(
	ctx context.Context, req *connect.Request[rafikiv1.UnbanProviderRequest],
) (*connect.Response[rafikiv1.UnbanProviderResponse], error) {
	m, err := s.providerBanManager()
	if err != nil {
		return nil, err
	}
	provider := strings.TrimSpace(req.Msg.GetProvider())
	if provider == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider is required"))
	}
	if err := m.UnbanProvider(ctx, provider); err != nil {
		return nil, providerBanError(err)
	}
	return connect.NewResponse(&rafikiv1.UnbanProviderResponse{}), nil
}
