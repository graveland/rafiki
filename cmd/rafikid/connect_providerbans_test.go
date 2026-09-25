// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/server"
)

// TestProviderBanAuthority pins who may ban: the anonymous local socket and an
// admin user credential; never a non-admin user, a child-attributed identity
// (even one whose owner is admin), or a per-child token.
func TestProviderBanAuthority(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    *server.Identity
		allow bool
	}{
		{"anonymous UDS", nil, true},
		{"admin user", &server.Identity{UserID: "u1", Via: server.ProvenanceUser, IsAdmin: true}, true},
		{"non-admin user", &server.Identity{UserID: "u2", Via: server.ProvenanceUser}, false},
		{"child attributed to admin", &server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed, IsAdmin: true}, false},
		{"child token", &server.Identity{UserID: "u1", Via: server.ProvenanceChildToken, ChildID: "c1", IsAdmin: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.id != nil {
				ctx = server.WithIdentity(ctx, tc.id)
			}
			m := connectProviderBans{g: routing.NewProviderGuard(0, slog.New(slog.DiscardHandler))}
			_, _, err := m.BanProvider(ctx, "openinference", 0, "")
			if tc.allow && err != nil {
				t.Fatalf("BanProvider refused: %v", err)
			}
			if !tc.allow {
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("BanProvider code %v, want PermissionDenied", connect.CodeOf(err))
				}
				if err := m.UnbanProvider(ctx, "openinference"); connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("UnbanProvider code %v, want PermissionDenied", connect.CodeOf(err))
				}
			}
		})
	}
}

// TestProviderBanAdapterRoundTrip proves a ban lists with reason operator and
// no expiry, and that the routing sentinels translate to connectapi's.
func TestProviderBanAdapterRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := connectProviderBans{g: routing.NewProviderGuard(0, slog.New(slog.DiscardHandler))}
	if _, _, err := m.BanProvider(ctx, "Open Inference", 0, "spinning"); err != nil {
		t.Fatal(err)
	}
	rows, persistent, err := m.ListProviderBans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if persistent {
		t.Error("a sink-less guard reported persistent bans")
	}
	if len(rows) != 1 || rows[0].Provider != "open-inference" || rows[0].Reason != "operator" ||
		!rows[0].ExpiresAt.IsZero() || rows[0].Note != "spinning" || rows[0].CreatedAt.IsZero() {
		t.Fatalf("rows = %+v", rows)
	}
	if err := m.UnbanProvider(ctx, "open-inference"); err != nil {
		t.Fatal(err)
	}
	if err := m.UnbanProvider(ctx, "open-inference"); !errors.Is(err, connectapi.ErrProviderNotBanned) {
		t.Errorf("second unban = %v, want ErrProviderNotBanned", err)
	}
	if _, _, err := m.BanProvider(ctx, "x", -time.Hour, ""); !errors.Is(err, connectapi.ErrInvalidProviderBan) {
		t.Errorf("negative ban = %v, want ErrInvalidProviderBan", err)
	}
}
