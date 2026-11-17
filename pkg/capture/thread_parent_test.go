// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestThreadConversationParent pins the parent write on thread conversations: a
// thread ('<session>:<thread>') carries the session's root conversation as its
// parent from the moment it is created, a thread whose root does not exist yet
// stays top level, and re-resolving an existing thread row never rewrites the
// parent it already has.
func TestThreadConversationParent(t *testing.T) {
	ctx := context.Background()
	cs, pool := newTestStore(t)

	// Sessions carry no colon: the thread suffix is what makes the ref a
	// thread's, and a colon inside the session itself would blur that.
	session := func(tag string) string {
		return fmt.Sprintf("sess-%d-%s", time.Now().UnixNano(), tag)
	}
	ref := func(session, thread string) ConversationRef {
		externalRef := session
		if thread != "" {
			externalRef = session + ":" + thread
		}
		return ConversationRef{
			OriginEntrypoint: "test", DrivenBy: "client", ExternalRef: externalRef,
		}
	}

	// The refs this test creates are plain conversation rows (no messages, no
	// turns), so one delete clears a subtest's whole family; the self-referential
	// parent FK is ON DELETE SET NULL, so order does not matter.
	cleanup := func(session string) {
		_, _ = pool.Exec(ctx,
			`DELETE FROM conversations.conversation WHERE external_ref = $1 OR external_ref LIKE $1 || ':%'`,
			session)
	}

	parentOf := func(t *testing.T, convID string) *string {
		t.Helper()
		var parent *string
		c := assert.NewAborting(t)
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT parent_conversation_id::text FROM conversations.conversation WHERE id = $1::uuid`,
			convID).Scan(&parent), "read parent_conversation_id of %s", convID)
		return parent
	}

	t.Run("thread points at its session's root", func(t *testing.T) {
		c := assert.NewAborting(t)
		sess := session("root")
		t.Cleanup(func() { cleanup(sess) })

		rootID, err := cs.EnsureConversationByExternalRef(ctx, ref(sess, ""))
		c.Require().NoError(err, "create the session's root")

		threadID, err := cs.ResolveThreadConversation(ctx, ref(sess, ""), "t1")
		c.Require().NoError(err, "ResolveThreadConversation")
		c.NotEq(rootID, threadID, "the thread must be its own row")

		parent := parentOf(t, threadID)
		c.Require().NotNil(parent, "a thread whose root exists must carry a parent")
		c.Eq(rootID, *parent, "the thread's parent is the session's root")
		c.Nil(parentOf(t, rootID), "the root stays top level")
	})

	t.Run("thread without a root stays top level", func(t *testing.T) {
		c := assert.NewAborting(t)
		sess := session("orphan")
		t.Cleanup(func() { cleanup(sess) })

		threadID, err := cs.ResolveThreadConversation(ctx, ref(sess, ""), "t2")
		c.Require().NoError(err, "ResolveThreadConversation")
		c.Nil(parentOf(t, threadID), "a thread whose root does not exist stays top level")
	})

	t.Run("re-resolving never rewrites the parent", func(t *testing.T) {
		c := assert.NewAborting(t)
		sess := session("stable")
		t.Cleanup(func() { cleanup(sess) })

		rootID, err := cs.EnsureConversationByExternalRef(ctx, ref(sess, ""))
		c.Require().NoError(err, "create the session's root")
		threadID, err := cs.ResolveThreadConversation(ctx, ref(sess, ""), "t3")
		c.Require().NoError(err, "ResolveThreadConversation (first)")

		// Wipe the parent by hand: if the second resolve re-wrote it, the write
		// is not create-once and an adopted thread would silently re-parent.
		_, err = pool.Exec(ctx,
			`UPDATE conversations.conversation SET parent_conversation_id = NULL WHERE id = $1::uuid`,
			threadID)
		c.Require().NoError(err, "clear the thread's parent")

		again, err := cs.ResolveThreadConversation(ctx, ref(sess, ""), "t3")
		c.Require().NoError(err, "ResolveThreadConversation (repeat)")
		c.Eq(threadID, again, "the same thread resolves to the same row")
		// The root exists for this resolve, so the lookup finds it; still NULL is
		// what proves the write is create-once.
		c.Nil(parentOf(t, again), "an existing thread row keeps whatever parent it has")
		// parentOf fails the subtest when the row is gone, so reading the root
		// proves it survived untouched.
		parentOf(t, rootID)
	})
}
