// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"github.com/multigres/testkit/assert"
)

// fakeResolveClient is a ControlClient whose ListChildren returns canned
// children. Every other method stays on the embedded nil interface and panics
// if reached — resolveTargetConnect must only ever call ListChildren.
type fakeResolveClient struct {
	rafikiv1connect.ControlClient

	children []*rafikiv1.ChildSummary
	err      error

	listCalls int
}

func (f *fakeResolveClient) ListChildren(_ context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	f.listCalls++
	// Resolution must see every child: the request carries no filter.
	if req.Msg.GetStatuses() != nil || req.Msg.GetName() != "" || req.Msg.GetNameContains() != "" ||
		req.Msg.GetCwdContains() != "" || req.Msg.GetSince() != nil || req.Msg.GetLabels() != nil ||
		req.Msg.GetHasLabel() != nil {
		return nil, errors.New("fakeResolveClient: ListChildren called with a filter")
	}
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: f.children}), nil
}

// resolveFixtures must not hand out more c_-prefixed ids than the fast path
// can see through: any input starting with c_ is returned as-is before
// ListChildren, so every list-based branch below is exercised with inputs
// that do NOT start with c_ (the daemon's ids all do today, but the copied
// ResolveWith semantics match exact ids and id prefixes regardless).
func resolveFixtures() []*rafikiv1.ChildSummary {
	return []*rafikiv1.ChildSummary{
		{ChildId: "c_alpha", Name: "alpha"},
		{ChildId: "c_alphabet", Name: "alphabet"},
		{ChildId: "c_beta-one", Name: "beta-one"},
		{ChildId: "c_beta-two", Name: "beta-two"},
		{ChildId: "zzz-9", Name: "unusual"}, // not a c_ id: reaches the exact-id branch
		{ChildId: "nn-1", Name: ""},         // unnamed: the ambiguity message falls back to ids
		{ChildId: "nn-2", Name: ""},
		{ChildId: "qq-1", Name: "quux"}, // unique id prefix its name does not share
	}
}

func TestResolveTargetConnect(t *testing.T) {
	fresh := func(err error) *fakeResolveClient {
		return &fakeResolveClient{children: resolveFixtures(), err: err}
	}
	listless := &fakeResolveClient{} // ListChildren would error if reached

	tests := []struct {
		name          string
		input         string
		activeMarker  string // written to the profile's active file when non-empty
		fake          *fakeResolveClient
		want          string
		wantErr       string
		wantListCalls int
	}{
		{
			name:  "c_ prefix returns as-is without a round trip",
			input: "c_definitely-an-id",
			fake:  listless,
			want:  "c_definitely-an-id",
		},
		{
			name:          "exact name wins",
			input:         "beta-one",
			fake:          fresh(nil),
			want:          "c_beta-one",
			wantListCalls: 1,
		},
		{
			name:          "exact name beats a longer name sharing it as a prefix",
			input:         "alpha",
			fake:          fresh(nil),
			want:          "c_alpha",
			wantListCalls: 1,
		},
		{
			name:          "a name may resolve to a non-c_ id",
			input:         "unusual",
			fake:          fresh(nil),
			want:          "zzz-9",
			wantListCalls: 1,
		},
		{
			name:          "exact child id",
			input:         "zzz-9",
			fake:          fresh(nil),
			want:          "zzz-9",
			wantListCalls: 1,
		},
		{
			name:          "unique name prefix",
			input:         "beta-o",
			fake:          fresh(nil),
			want:          "c_beta-one",
			wantListCalls: 1,
		},
		{
			name:          "unique child id prefix its name does not share",
			input:         "qq-",
			fake:          fresh(nil),
			want:          "qq-1",
			wantListCalls: 1,
		},
		{
			name:          "ambiguous prefix names every candidate by name",
			input:         "beta",
			fake:          fresh(nil),
			wantErr:       `ambiguous identifier "beta" matches: beta-one, beta-two`,
			wantListCalls: 1,
		},
		{
			name:          "ambiguous prefix falls back to child ids for unnamed children",
			input:         "nn-",
			fake:          fresh(nil),
			wantErr:       `ambiguous identifier "nn-" matches: nn-1, nn-2`,
			wantListCalls: 1,
		},
		{
			name:          "no match",
			input:         "nomatch",
			fake:          fresh(nil),
			wantErr:       `no child matches "nomatch"`,
			wantListCalls: 1,
		},
		{
			name:          "list failure is wrapped",
			input:         "beta",
			fake:          fresh(connect.NewError(connect.CodeUnavailable, errors.New("socket gone"))),
			wantErr:       "cannot reach the rafiki daemon at test-endpoint — is rafikid running? (`rafiki status`): unavailable: socket gone",
			wantListCalls: 1,
		},
		{
			name:         "empty input uses the profile's active marker",
			input:        "",
			activeMarker: "c_marked",
			fake:         listless,
			want:         "c_marked",
		},
		{
			name:          "empty input with a named active marker resolves through the list",
			input:         "",
			activeMarker:  "beta-one",
			fake:          fresh(nil),
			want:          "c_beta-one",
			wantListCalls: 1,
		},
		{
			name:    "empty input without an active marker",
			input:   "",
			fake:    listless,
			wantErr: "no child specified and no active marker; run `rafiki list` to see options",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			isolateProfiles(t)
			if tt.activeMarker != "" {
				c.NoError(setActive("resolve-test", tt.activeMarker), "setActive")
			}

			got, err := resolveTargetConnect(context.Background(), tt.fake, "resolve-test", tt.input, "test-endpoint")
			if tt.wantErr != "" {
				c.Error(err, "resolveTargetConnect(%q) = %q, want error %q", tt.input, got, tt.wantErr)
				c.Eq(tt.wantErr, err.Error(), "resolveTargetConnect(%q) error = %q, want", tt.input, err.Error())
			} else {
				c.NoError(err, "resolveTargetConnect(%q)", tt.input)
				c.Eq(tt.want, got, "resolveTargetConnect(%q) = %q, want", tt.input, got)
			}
			c.Eq(tt.wantListCalls, tt.fake.listCalls, "ListChildren called")
		})
	}
}

// TestResolveTargetConnectFastPathSkipsTheList pins that the c_ fast path and
// the active-marker fallback never round-trip. listCalls is the direct
// assertion; the request-shape check inside ListChildren is a second guard.
func TestResolveTargetConnectFastPathSkipsTheList(t *testing.T) {
	ck := assert.NewAborting(t)
	isolateProfiles(t)
	ck.NoError(setActive("resolve-test", "c_marked"), "setActive")
	c := &fakeResolveClient{}

	got, err := resolveTargetConnect(context.Background(), c, "resolve-test", "", "test-endpoint")
	ck.NoError(err, "resolveTargetConnect(\"\")")
	ck.Eq("c_marked", got, "active marker not honored: got")
	ck.Eq(0, c.listCalls, "ListChildren called")

	got, err = resolveTargetConnect(context.Background(), c, "resolve-test", "c_direct", "test-endpoint")
	ck.NoError(err, "resolveTargetConnect(c_direct)")
	ck.Eq("c_direct", got, "c_ fast path not honored: got")
	ck.Eq(0, c.listCalls, "ListChildren called")
}
