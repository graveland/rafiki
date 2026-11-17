// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
)

// stampConversationParentSQL fills a child's root conversation with the root
// conversation of the child that spawned it. A conversation row is created
// before the daemon learns which child it belongs to, so the link is written
// here, from the child row, rather than at insert. It only fills a NULL.
const stampConversationParentSQL = `
WITH me AS (
    SELECT ch.child_id,
           COALESCE(ch.labels->>'rafiki/parent', ch.labels->>'fundi/parent') AS parent_id,
           COALESCE(ch.conversation_id,
                    (SELECT v.id FROM conversations.conversation v
                      WHERE v.external_ref = ch.child_id
                      ORDER BY v.created_at LIMIT 1)) AS conv_id
      FROM conversations.child ch WHERE ch.child_id = $1
), parent AS (
    SELECT COALESCE(p.conversation_id,
                    (SELECT v.id FROM conversations.conversation v
                      WHERE v.external_ref = p.child_id
                      ORDER BY v.created_at LIMIT 1)) AS conv_id
      FROM conversations.child p JOIN me ON p.child_id = me.parent_id
)
UPDATE conversations.conversation c
   SET parent_conversation_id = parent.conv_id
  FROM me, parent
 WHERE c.id = me.conv_id
   AND c.parent_conversation_id IS NULL
   AND parent.conv_id IS NOT NULL
   AND parent.conv_id <> c.id`

// stampConversationParentDoneSQL answers whether the child's conversation
// already carries a parent, resolving the conversation the same two ways the
// stamp does: the stored conversation_id, else the oldest conversation whose
// external_ref is the child id. It separates the two zero-row outcomes of the
// stamp -- already stamped, worth memoizing, and parent conversation missing,
// worth retrying.
const stampConversationParentDoneSQL = `
SELECT EXISTS (
    SELECT 1
      FROM conversations.child ch
      JOIN conversations.conversation c
        ON c.id = COALESCE(ch.conversation_id,
               (SELECT v.id FROM conversations.conversation v
                 WHERE v.external_ref = ch.child_id
                 ORDER BY v.created_at LIMIT 1))
     WHERE ch.child_id = $1
       AND c.parent_conversation_id IS NOT NULL)`

// stampConversationParent records, on a child's root conversation, the root
// conversation of the child that spawned it. It only fills a NULL, and a
// parent whose conversation does not exist yet is retried on the child's next
// row write.
//
// A claude child leaves conversations.child.conversation_id NULL and links by
// external_ref, so both sides resolve their conversation the same two-arm way:
// the stored conversation_id, else the oldest conversation whose external_ref
// is the child id. The guard refuses a self-parent (a conversation that
// resolved to itself) and never touches updated_at -- the stamp is
// bookkeeping about the tree's shape, not activity on it.
//
// Once the conversation carries a parent there is nothing left to do, so the
// outcome is memoized per child id on the Controller and later row writes for
// that child skip the lookups entirely. A zero-row stamp is memoized only
// when the conversation is already stamped; the parent-conversation-missing
// case stays unmemoized and keeps retrying.
func (c *Controller) stampConversationParent(ctx context.Context, childID string) error {
	if c.pool == nil {
		return nil
	}
	if _, done := c.stampedParents.Load(childID); done {
		return nil
	}
	tag, err := c.pool.Exec(ctx, stampConversationParentSQL, childID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		c.stampedParents.Store(childID, struct{}{})
		slog.Debug("stamped conversation parent", "childId", childID)
		return nil
	}
	var done bool
	if err := c.pool.QueryRow(ctx, stampConversationParentDoneSQL, childID).Scan(&done); err != nil {
		return err
	}
	if done {
		c.stampedParents.Store(childID, struct{}{})
	}
	return nil
}
