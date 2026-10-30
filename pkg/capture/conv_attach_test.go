// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"fmt"
	"testing"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestCaptureClearPendingReadFresh: clear_pending is read from the handle on
// every attach, never from anything an earlier read left behind — a /clear
// marked after the conversation's entry was already open still opens the next
// root and consumes the flag.
func TestCaptureClearPendingReadFresh(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "clear-pending-fresh")

	runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"before"}]}`), 1, 1)
	st, err := s.handle(convID).State(ctx)
	c.Require().NoError(err, "State")
	c.False(st.ClearPending, "clear_pending before the mark")

	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")
	head := runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"after"}]}`), 1, 1)

	kind, _, depth := mustNodeKind(t, ctx, pool, head)
	c.Eq("clear", kind, "kind")
	c.Eq(0, depth, "root depth")
	c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending is consumed with the root")
}

// TestCaptureUnknownRoleFailsAttach: a request message whose role has no native
// form fails the attach, naming the message, rather than being stored under an
// invented role.
func TestCaptureUnknownRoleFailsAttach(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := newConv(t, ctx, s)

	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent")
	_, err = s.DecomposeRequest(ctx, convID, turnID, createdAt,
		[]byte(`{"messages":[{"role":"narrator","content":"hi"}]}`), "", "")
	c.Require().Error(err, "an unknown role fails the attach")
	c.StrContains(err.Error(), "attach: message 0", "the error names the message")
	c.StrContains(err.Error(), "narrator", "the error names the role")
	c.Eq(0, mustNodeCount(t, ctx, pool, convID), "no node was written")
}

// TestCaptureSharedStoreServesReaders: an attach through a shared conversation
// store leaves the nodes it wrote in that store's cache, so a reader's working
// path costs no backend fetch at all.
func TestCaptureSharedStoreServesReaders(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := newConv(t, ctx, s)

	recovered := c.Panics(func() { s.WithConv(nil) }, "WithConv(nil) is refused")
	c.Eq("capture: WithConv(nil)", fmt.Sprint(recovered), "the panic names the setter")

	counted := convtest.NewCounting(store.NewConvBackend(pool))
	shared := conv.NewStore(counted, conv.Options{})
	cs := s.WithConv(shared)

	runTurn(t, ctx, cs, convID,
		[]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"there"}]}`), 1, 1)
	c.Greater(0, counted.Counts()["Commit"], "the attach wrote through the shared store")

	counted.Reset()
	path, err := shared.Open(convID).Working(ctx)
	c.Require().NoError(err, "Working")
	c.Eq(2, len(path), "the working path")
	c.Eq(0, counted.Counts()["Fetch"], "a reader on the shared store fetched nothing")
}
