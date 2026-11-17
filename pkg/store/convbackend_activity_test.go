// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
)

// TestCommitBumpsUpdatedAt pins Commit's activity write: appending through the
// conv path moves the conversation's updated_at forward, which is what keeps
// ListConversations' most-recently-active sort honest for tree-written
// conversations.
func TestCommitBumpsUpdatedAt(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)

	_, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET updated_at = now() - interval '1 day' WHERE id = $1::uuid`,
		convID)
	c.Require().NoError(err, "age the conversation's updated_at")

	handle := conv.NewStore(NewConvBackend(pool), conv.Options{}).Open(convID, conv.AsOwner())
	_, err = handle.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{convNode(t, "user", "one message through the normal path")},
	})
	c.Require().NoError(err, "append")

	var at time.Time
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT updated_at FROM conversations.conversation WHERE id = $1::uuid`,
		convID).Scan(&at), "read updated_at back")
	c.True(at.After(time.Now().Add(-time.Minute)),
		"updated_at = %s, want moved forward to the commit's now()", at)
}
