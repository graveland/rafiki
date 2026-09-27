// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestConnectSpawnPrefillMapped pins the wire mapping only: entries arrive on
// SpawnParams as protocol.PrefillRead with path/start/end intact, and a
// request without prefill yields nil — not an empty slice — so the daemon's
// "len == 0 means no pre-fill" checks stay simple. Validation happens in the
// controller (Task 2.2); this layer must not duplicate prefill.Validate's
// rules, so deliberately invalid shapes map through untouched.
func TestConnectSpawnPrefillMapped(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/work",
		Prefill: []*rafikiv1.PrefillRead{
			{Path: "CLAUDE.md", Start: 10, End: 40},
			{Path: "src/**/*.rs"},
			{Path: "notes.txt", Start: 200},
		},
	}))
	c.Require().NoError(err, "Spawn")

	want := []protocol.PrefillRead{
		{Path: "CLAUDE.md", Start: 10, End: 40},
		{Path: "src/**/*.rs"},
		{Path: "notes.txt", Start: 200},
	}
	c.EqDiff(want, f.got.Prefill, "Prefill")
}

func TestConnectSpawnPrefillEmptyStaysNil(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	c.Require().NoError(err, "Spawn")
	c.Nil(f.got.Prefill, "Prefill")
}
