// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/childstore"
)

var _ childstore.LineageSource = (*Store)(nil)

// lineageAncestorSQL reads the ancestor's own root, closed or not. A lineage
// read must see a tombstoned ancestor: a closed coordinator's subtree is still
// scoped and still spends. root_id is the generated column migration 0076
// derives from labels->>'rafiki/root'; an empty value means a top-level child,
// which is its own root.
const lineageAncestorSQL = `
SELECT COALESCE(root_id, '')
  FROM conversations.child
 WHERE child_id = $1`

// lineageSubtreeSQL selects every row sharing the ancestor's root: the root's
// own row, plus every child whose generated root_id is that root. It
// deliberately does NOT filter on closed_at: tombstones are part of the
// lineage. child_root_idx serves the root_id arm.
const lineageSubtreeSQL = `
SELECT child_id, COALESCE(session_id, ''), COALESCE(conversation_id::text, ''),
       closed_at IS NOT NULL, COALESCE(parent_id, ''), COALESCE(root_id, '')
  FROM conversations.child
 WHERE child_id = $1 OR root_id = $1`

// Lineage returns the ancestor's own row (when it exists) and every descendant
// at any depth, live or closed, with the parent/root the rows were read with —
// so a caller can select a deeper subtree from one read
// (childstore.DescendantsOf). An unknown ancestor yields (nil, nil).
//
// This is the database half of childstore.LineageSource: coverage comes from
// the child table (tombstoned rows included), so a closed descendant stays in
// its ancestors' subtree scope and spend accounting. The database is the only
// lineage source — a native subagent has a row like any other child — so there
// is no second, in-memory definition of what a subtree contains.
//
// The parent/root the walk reads are generated columns (migration 0076), so a
// row's labels cannot disagree with its lineage: a non-object labels value or a
// non-string parent/root is refused at the write by
// child_labels_lineage_shape, rather than met here as an undecidable row.
func (s *Store) Lineage(ctx context.Context, ancestorChildID string) ([]childstore.LineageRow, error) {
	if ancestorChildID == "" {
		return nil, nil
	}

	// The ancestor's own row scopes the read. Without it there is nothing to
	// scope by, and the documented (nil, nil) stands.
	var ancestorRoot string
	err := s.pool.QueryRow(ctx, lineageAncestorSQL, ancestorChildID).Scan(&ancestorRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}
	// The ancestor's root is its stored root, falling back to the ancestor
	// itself for a top-level child.
	root := ancestorRoot
	if root == "" {
		root = ancestorChildID
	}

	rows, err := s.pool.Query(ctx, lineageSubtreeSQL, root)
	if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}
	defer rows.Close()

	var lineageRows []childstore.LineageRow
	for rows.Next() {
		var (
			member  childstore.LineageMember
			parent  string
			rowRoot string
		)
		if err := rows.Scan(&member.ChildID, &member.SessionID, &member.ConversationID,
			&member.Closed, &parent, &rowRoot); err != nil {
			return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
		}
		lineageRows = append(lineageRows, childstore.LineageRow{
			Member: member,
			Labels: lineageLabels(parent, rowRoot),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}

	// The root arm reaches rows that are NOT beneath this ancestor: a sibling
	// branch of the same tree. Trim to the ancestor's own line, so reading a
	// mid-tree child never widens a scope to its cousins.
	return childstore.LineageRowsOf(lineageRows, ancestorChildID), nil
}

// lineageLabels rebuilds the label map childstore.DescendantsOf walks from a
// row's two generated columns. Only non-empty values are present, so a
// top-level child carries neither key — exactly the shape DescendantsOf treats
// as "no parent".
func lineageLabels(parent, root string) map[string]string {
	labels := make(map[string]string, 2)
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	if root != "" {
		labels[childstore.LabelRoot] = root
	}
	return labels
}
