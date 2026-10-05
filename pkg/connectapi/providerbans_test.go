// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetProviderBanManager(nil): got code")
}

// TestBanProviderDurationIsTriState proves absent means "until lifted" and an
// explicit zero is refused rather than collapsing into the same meaning.
func TestBanProviderDurationIsTriState(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeProviderBans{}
	s := &Server{}
	s.SetProviderBanManager(f)
	ctx := context.Background()

	resp, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{Provider: "openinference"}))
	c.Require().NoError(err)
	c.Nil(resp.Msg.GetBan().ExpiresAt, "an unbounded ban must carry no expires_at")

	if _, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{
		Provider: "openinference", Duration: durationpb.New(time.Hour),
	})); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{0, time.Hour}
	c.Eq(fmt.Sprint(want), fmt.Sprint(f.bannedFor), "manager saw durations %v, want %v", f.bannedFor, want)

	for _, dur := range []time.Duration{0, -5 * time.Second} {
		_, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{
			Provider: "openinference", Duration: durationpb.New(dur),
		}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "duration %s: code %v, want InvalidArgument", dur, connect.CodeOf(err))
	}
}

// TestBanProviderOutOfRangeDurationRefused pins CheckValid on the received
// Duration: a span past the representable range is the caller's fault.
func TestBanProviderOutOfRangeDurationRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeProviderBans{}
	s := &Server{}
	s.SetProviderBanManager(f)
	_, err := s.BanProvider(context.Background(), connect.NewRequest(&rafikiv1.BanProviderRequest{
		Provider: "openinference", Duration: &durationpb.Duration{Seconds: 400000000000},
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Empty(f.bannedFor, "manager reached despite the rejection")
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
			c := assert.NewCollecting(t)
			s := &Server{}
			s.SetProviderBanManager(&fakeProviderBans{banErr: tc.err, unbanErr: tc.err})
			_, err := s.BanProvider(ctx, connect.NewRequest(&rafikiv1.BanProviderRequest{Provider: "p"}))
			c.Eq(tc.want, connect.CodeOf(err), "BanProvider code")
			_, err = s.UnbanProvider(ctx, connect.NewRequest(&rafikiv1.UnbanProviderRequest{Provider: "p"}))
			c.Eq(tc.want, connect.CodeOf(err), "UnbanProvider code")
		})
	}
}

func TestBanProviderRequiresProvider(t *testing.T) {
	s := &Server{}
	s.SetProviderBanManager(&fakeProviderBans{})
	_, err := s.BanProvider(context.Background(), connect.NewRequest(&rafikiv1.BanProviderRequest{Provider: "  "}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}
