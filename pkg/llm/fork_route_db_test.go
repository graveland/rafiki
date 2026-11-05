package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
)

// TestForkRoutesThroughTheConversationPath drives real stored forks through
// Conversation.Send, streamed and not, and checks the session id on the
// CONTEXT (what the header transports read) as well as in EncodeOptions (what
// the Responses codec reads). Two forks of one node share the routing id; a
// Clear restarts the fork's working path at a fresh root, after which it no
// longer carries the shared prefix and routes on its own id; the source never
// routes as a fork.
func TestForkRoutesThroughTheConversationPath(t *testing.T) {
	c := assert.NewAborting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){respondText("ok")}}
	client := testClient(t, pool, sender)

	src, err := client.Conversation(ctx, NewConversation("", "test"))
	c.NoError(err)
	_, err = src.Send(ctx, UserText("seed"))
	c.NoError(err)
	history, err := src.loadHistory(ctx)
	c.NoError(err)
	node := history[len(history)-1].ID

	check := func(label, want string) {
		t.Helper()
		i := len(sender.lastCtx) - 1
		got, _ := sessionIDFromContext(sender.lastCtx[i])
		c.Eq(want, got, label+": context session id")
		c.Eq(want, sender.lastOpts[i].SessionID, label+": EncodeOptions session id")
	}
	check("source", src.ID)

	a, err := client.Conversation(ctx, NewConversation("", "test"), ForkFrom(node))
	c.NoError(err)
	bRef := "fork-route-b-" + node
	b, err := client.Conversation(ctx, NewConversation("", "test"), ByExternalRef(bRef), ForkFrom(node))
	c.NoError(err)

	_, err = a.Send(ctx, UserText("a1"))
	c.NoError(err)
	sum := sha256.Sum256([]byte(node + "\x00" + sender.lastReq[len(sender.lastReq)-1].Model))
	shared := fmt.Sprintf("fork-%x", sum[:16])
	check("fork a, sent", shared)

	_, err = a.Send(ctx, UserText("a2"), WithStreamHandler(func(*msg.Response) {}))
	c.NoError(err)
	check("fork a, streamed", shared)

	_, err = b.Send(ctx, UserText("b1"), WithStreamHandler(func(*msg.Response) {}))
	c.NoError(err)
	check("fork b, a sibling", shared)

	c.NoError(a.Clear(ctx))
	_, err = a.Send(ctx, UserText("a3"))
	c.NoError(err)
	check("fork a after clear", a.ID)

	// A reopen carries no spawn-time fork option: the route comes from the
	// stored fork edge alone.
	reopened, err := client.Conversation(ctx, NewConversation("", "test"), ByExternalRef(bRef))
	c.NoError(err)
	c.Eq(b.ID, reopened.ID, "reopen by external ref")
	_, err = reopened.Send(ctx, UserText("b2"))
	c.NoError(err)
	check("fork b reopened", shared)
}
