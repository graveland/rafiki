// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/server"
)

// connectProviderBans adapts the provider cache guard to
// connectapi.ProviderBanManager. The guard is the one holder of every
// exclusion — its own ejections and operator bans alike — so a ban reaches
// both OpenRouter request paths on the very next request.
type connectProviderBans struct{ g *routing.ProviderGuard }

func (c connectProviderBans) ListProviderBans(context.Context) ([]connectapi.ProviderBanRow, bool, error) {
	recs := c.g.Ejected(time.Now())
	out := make([]connectapi.ProviderBanRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, toProviderBanRow(r))
	}
	return out, c.g.Persistent(), nil
}

func (c connectProviderBans) BanProvider(ctx context.Context, provider string, d time.Duration, note string) (connectapi.ProviderBanRow, bool, error) {
	if err := requireBanAuthority(ctx); err != nil {
		return connectapi.ProviderBanRow{}, false, err
	}
	rec, err := c.g.Ban(ctx, time.Now(), provider, d, note)
	if err != nil {
		return connectapi.ProviderBanRow{}, false, providerBanErr(err)
	}
	return toProviderBanRow(rec), c.g.Persistent(), nil
}

func (c connectProviderBans) UnbanProvider(ctx context.Context, provider string) error {
	if err := requireBanAuthority(ctx); err != nil {
		return err
	}
	return providerBanErr(c.g.Lift(ctx, time.Now(), provider))
}

func toProviderBanRow(r routing.EjectionRecord) connectapi.ProviderBanRow {
	return connectapi.ProviderBanRow{
		Provider: r.Provider, ModelLine: r.ModelLine, Reason: string(r.Reason),
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, Note: r.Note,
	}
}

// providerBanErr translates routing's sentinels to connectapi's, so the
// handler can map them to NotFound/InvalidArgument without importing routing.
func providerBanErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, routing.ErrNoBan):
		return fmt.Errorf("%w: %v", connectapi.ErrProviderNotBanned, err)
	case errors.Is(err, routing.ErrInvalidBan):
		return fmt.Errorf("%w: %v", connectapi.ErrInvalidProviderBan, err)
	default:
		return err
	}
}

// requireBanAuthority admits a ban or unban from an admin user credential, or
// from the anonymous local socket (no identity: the socket itself is the
// credential, the same trust boundary requireUserCredential honours). A ban
// reroutes every user's children, so a non-admin user may not, and a
// child-attributed identity never may — an agent that thinks a provider is
// misbehaving should say so, not act on it.
func requireBanAuthority(ctx context.Context) error {
	id := server.IdentityFromContext(ctx)
	if id == nil || (id.IsUserCredential() && id.IsAdmin) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied,
		errors.New("provider bans affect every user's routing and require an admin user credential"))
}
