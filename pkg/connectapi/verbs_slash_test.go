// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"

	"github.com/multigres/testkit/assert"
)

// The child-scope check precedes the command layer, so a /exit from a child
// outside the caller's subtree is refused before anything is accepted — Send
// cannot reach further than Kill.
func TestSendSlashExitFromOutOfScopeChildIsRefusedBeforeAccept(t *testing.T) {
	c := assert.NewAborting(t)
	s := emptyScopeServer(t)
	acc := &fakeAccepter{}
	s.SetInbox(acc)

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_other",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("/exit"),
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Send(/exit) from an out-of-scope child = %v, want", err)
	c.EqDeep(inbox.Inbound{}, acc.got, "the scope check must precede the command layer; the accepter ran")
}
