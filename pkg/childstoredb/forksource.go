// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/childstore"
)

// forkSourceSQL reads the durable child row whether or not it is closed: a
// closed child is exactly the case a fork or a tag still needs, because its
// conversation history outlives its process.
//
// The conversation id falls back to the child's root thread, because a claude
// child leaves child.conversation_id NULL and links by external_ref. The bare
// child id is the root thread; a "<child>:<thread>" ref does not match this
// equality and is deliberately not picked.
//
// Parent and root come from their generated columns (migration 0076), not from
// a labels decode: the columns are what the lineage walk and the sibling-name
// index use, so this cannot disagree with them.
const forkSourceSQL = `
SELECT c.child_id,
       COALESCE(c.owner_user_id::text, ''),
       c.kind,
       COALESCE(c.conversation_id::text,
                (SELECT v.id::text
                   FROM conversations.conversation v
                  WHERE v.external_ref = c.child_id
                  ORDER BY v.created_at
                  LIMIT 1), ''),
       COALESCE(c.parent_id, ''),
       COALESCE(c.root_id, ''),
       c.closed_at IS NOT NULL
  FROM conversations.child c
 WHERE c.child_id = $1`

// ForkSource returns what forking or tagging a child needs to know about it.
// ok is false for an unknown child.
func (s *Store) ForkSource(ctx context.Context, childID string) (childstore.ForkSource, bool, error) {
	var src childstore.ForkSource
	err := s.pool.QueryRow(ctx, forkSourceSQL, childID).Scan(
		&src.ChildID, &src.OwnerUserID, &src.Kind, &src.ConversationID,
		&src.ParentID, &src.RootID, &src.Closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return childstore.ForkSource{}, false, nil
	}
	if err != nil {
		return childstore.ForkSource{}, false, fmt.Errorf("childstoredb: fork source %s: %w", childID, err)
	}
	return src, true, nil
}
