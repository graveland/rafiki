// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/childstore"
)

var (
	_ childstore.LineageSource       = (*Store)(nil)
	_ childstore.RootedLineageSource = (*Store)(nil)
)

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
// at any depth, live or closed. An unknown ancestor yields (nil, nil).
//
// This is the database half of childstore.LineageSource: coverage comes from
// the child table (tombstoned rows included), so a closed descendant stays in
// its ancestors' subtree scope and spend accounting.
//
// The parent/root the walk reads are generated columns (migration 0076), so a
// row's labels cannot disagree with its lineage: a non-object labels value or a
// non-string parent/root is refused at the write by
// child_labels_lineage_shape, rather than met here as an undecidable row.
func (s *Store) Lineage(ctx context.Context, ancestorChildID string) ([]childstore.LineageMember, error) {
	return s.lineage(ctx, ancestorChildID, "")
}

// LineageWithRoot is Lineage with a caller-supplied root fallback: when the
// ancestor's OWN row is absent, rootFallback (the ancestor's root, which the
// caller resolved from its live store) is used so the subtree query still runs
// and the ancestor's persisted descendants are still found. An ancestor absent
// from BOTH the table and the caller's live store passes an empty fallback and
// keeps the (nil, nil) contract.
func (s *Store) LineageWithRoot(ctx context.Context, ancestorChildID, rootFallback string) ([]childstore.LineageMember, error) {
	return s.lineage(ctx, ancestorChildID, rootFallback)
}

func (s *Store) lineage(ctx context.Context, ancestorChildID, rootFallback string) ([]childstore.LineageMember, error) {
	if ancestorChildID == "" {
		return nil, nil
	}

	root := rootFallback
	var ancestorRoot string
	err := s.pool.QueryRow(ctx, lineageAncestorSQL, ancestorChildID).Scan(&ancestorRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		// The ancestor's row is missing. With no caller-supplied root there is
		// nothing to scope the subtree by, so the documented (nil, nil) stands.
		if root == "" {
			return nil, nil
		}
	} else if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	} else {
		// The ancestor's root is its stored root, falling back to the ancestor
		// itself for a top-level child. A present row always wins over the
		// caller's fallback.
		if ancestorRoot != "" {
			root = ancestorRoot
		} else {
			root = ancestorChildID
		}
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

	return childstore.DescendantsOf(lineageRows, ancestorChildID), nil
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
