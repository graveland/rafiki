// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"sync"
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

// TestCommitKeepsUpdatedAt pins the other half of the activity write: a commit
// that stores no message must not move last activity. RetractHead is the
// ordinary zero-row commit: it moves the head onto the retracted row's parent
// and writes no row, and a conversation whose newest message is a day old must
// read as a day inactive afterwards.
func TestCommitKeepsUpdatedAt(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)

	handle := conv.NewStore(NewConvBackend(pool), conv.Options{}).Open(convID, conv.AsOwner())
	res, err := handle.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{convNode(t, "user", "head to retract")},
	})
	c.Require().NoError(err, "append")
	c.Require().Len(res.Nodes, 1, "one node appended")

	_, err = pool.Exec(ctx,
		`UPDATE conversations.conversation SET updated_at = now() - interval '1 day' WHERE id = $1::uuid`,
		convID)
	c.Require().NoError(err, "age the conversation's updated_at")
	var before time.Time
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT updated_at FROM conversations.conversation WHERE id = $1::uuid`, convID).Scan(&before),
		"read updated_at before the retract")

	c.Require().NoError(handle.RetractHead(ctx, res.Nodes[0].ID), "retract the head")

	var after time.Time
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT updated_at FROM conversations.conversation WHERE id = $1::uuid`, convID).Scan(&after),
		"read updated_at after the retract")
	c.Eq(before, after, "a retract writes no message, so last activity stays where it was")
}

// A commit that stores a message tells the observer which conversation got it;
// a commit that stores none (a head retract) does not, and a stopped observer
// is never called again.
func TestObserveMessageCommitsFiresOnlyForStoredMessages(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)

	var mu sync.Mutex
	seen := map[string]int{}
	stop := ObserveMessageCommits(func(id string) {
		mu.Lock()
		defer mu.Unlock()
		seen[id]++
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return seen[convID]
	}

	handle := conv.NewStore(NewConvBackend(pool), conv.Options{}).Open(convID, conv.AsOwner())
	res, err := handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{convNode(t, "user", "first")}})
	c.Require().NoError(err, "append")
	c.Eq(1, count(), "one stored commit, one observation")

	c.Require().NoError(handle.RetractHead(ctx, res.Nodes[0].ID), "retract the head")
	c.Eq(1, count(), "a retract stores no message and is not observed")

	stop()
	_, err = handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{convNode(t, "user", "second")}})
	c.Require().NoError(err, "append after stop")
	c.Eq(1, count(), "a stopped observer is not called")
}
