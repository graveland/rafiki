// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeOwnerGuard stands in for the daemon's owner guard in handler tests.
// owned names the ids Authorize admits and Owns keeps; convs names the owned
// conversation ids. Authorize's error carries the same text for every refused
// id, as the daemon's does.
type fakeOwnerGuard struct {
	owned map[string]bool
	convs map[string]bool
	calls []string
}

func (f *fakeOwnerGuard) Authorize(id string) error {
	f.calls = append(f.calls, id)
	if f.owned[id] {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
		"agent %s is not owned by you; you may only view, steer or kill agents you own", id))
}

func (f *fakeOwnerGuard) Owns(id string) bool { return f.owned[id] }

func (f *fakeOwnerGuard) OwnsConversation(id string) bool { return f.convs[id] }

// TestOwnerGuardResolverUnwiredAndWired pins the two shapes a Server resolves
// with no real guard behind it: no source wired at all (the zero value and
// every pre-seam fixture) and a source that resolves nil for the caller (an
// admin or the anonymous unix socket). Both must serve the unrestricted path,
// or a wiring gap would silently empty every operator's list.
//
// ownerGuard is unexported, so the assertion is behavioural: ListChildren's
// operator branch answers every summary the lister holds in both shapes.
func TestOwnerGuardResolverUnwiredAndWired(t *testing.T) {
	summaries := []protocol.ChildSummary{
		{ChildID: "c_mine", Name: "mine"},
		{ChildID: "c_theirs", Name: "theirs"},
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		wire func(*connectapi.Server)
	}{
		{"no source wired", func(*connectapi.Server) {}},
		{"source resolves nil", func(s *connectapi.Server) {
			s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			s := connectapi.NewServer(nil)
			s.SetChildLister(&fakeLister{all: summaries})
			tc.wire(s)

			resp, err := s.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
			c.NoError(err, "ListChildren")
			c.Len(resp.Msg.GetChildren(), 2, "got %d children, want the whole fleet", len(resp.Msg.GetChildren()))
		})
	}
}
