// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

type fakeProviderBans struct {
	rows     []ProviderBanRow
	banErr   error
	unbanErr error

	bannedFor []time.Duration
	unbanned  []string
}

func (f *fakeProviderBans) ListProviderBans(context.Context) ([]ProviderBanRow, bool, error) {
	return f.rows, true, nil
}

func (f *fakeProviderBans) BanProvider(_ context.Context, provider string, d time.Duration, note string) (ProviderBanRow, bool, error) {
	if f.banErr != nil {
		return ProviderBanRow{}, false, f.banErr
	}
	f.bannedFor = append(f.bannedFor, d)
	row := ProviderBanRow{Provider: provider, ModelLine: "*", Reason: "operator", CreatedAt: time.Unix(100, 0), Note: note}
	if d > 0 {
		row.ExpiresAt = row.CreatedAt.Add(d)
	}
	return row, true, nil
}

func (f *fakeProviderBans) UnbanProvider(_ context.Context, provider string) error {
	if f.unbanErr != nil {
		return f.unbanErr
	}
	f.unbanned = append(f.unbanned, provider)
	return nil
}

func TestSetProviderBanManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetProviderBanManager(nil)
	_, err := s.ListProviderBans(context.Background(), connect.NewRequest(&rafikiv1.ListProviderBansRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("after SetProviderBanManager(nil): got code %v, want Unavailable", connect.CodeOf(err))
	}
}

// TestBanProviderDurationIsTriState proves absent means "until lifted" and an
// explicit zero is refused rather than collapsing into the same meaning.
func TestBanProviderDurationIsTriState(t *testing.T) {
	f := &fakeProviderBans{}
	s := &Server{}
	s.SetProviderBanManager(f)
	ctx := context.Background()

	resp, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{Provider: "openinference"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetBan().ExpiresAt != nil {
		t.Errorf("an unbounded ban carries expires_at %d", resp.Msg.GetBan().GetExpiresAt())
	}

	if _, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{
		Provider: "openinference", DurationSeconds: proto.Int64(3600),
	})); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{0, time.Hour}; fmt.Sprint(f.bannedFor) != fmt.Sprint(want) {
		t.Errorf("manager saw durations %v, want %v", f.bannedFor, want)
	}

	for _, secs := range []int64{0, -5} {
		_, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{
			Provider: "openinference", DurationSeconds: proto.Int64(secs),
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("duration %d: code %v, want InvalidArgument", secs, connect.CodeOf(err))
		}
	}
}

func TestProviderBanErrorMapping(t *testing.T) {
	ctx := context.Background()
	denied := connect.NewError(connect.CodePermissionDenied, errors.New("admins only"))
	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not banned", fmt.Errorf("x: %w", ErrProviderNotBanned), connect.CodeNotFound},
		{"invalid", fmt.Errorf("x: %w", ErrInvalidProviderBan), connect.CodeInvalidArgument},
		{"permission passes through", denied, connect.CodePermissionDenied},
		{"other", errors.New("db down"), connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.SetProviderBanManager(&fakeProviderBans{banErr: tc.err, unbanErr: tc.err})
			_, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{Provider: "p"}))
			if connect.CodeOf(err) != tc.want {
				t.Errorf("BanProvider code %v, want %v", connect.CodeOf(err), tc.want)
			}
			_, err = s.UnbanProvider(ctx, connect.NewRequest(&rafikiv1.UnbanProviderRequest{Provider: "p"}))
			if connect.CodeOf(err) != tc.want {
				t.Errorf("UnbanProvider code %v, want %v", connect.CodeOf(err), tc.want)
			}
		})
	}
}

func TestBanProviderRequiresProvider(t *testing.T) {
	s := &Server{}
	s.SetProviderBanManager(&fakeProviderBans{})
	_, err := s.BanProvider(context.Background(), connect.NewRequest(&rafikiv1.BanProviderRequest{Provider: "  "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code %v, want InvalidArgument", connect.CodeOf(err))
	}
}
