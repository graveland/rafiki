// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"go.graveland.dev/rafiki/pkg/childstore"
)

var (
	_ childstore.LineageSource       = (*Store)(nil)
	_ childstore.RootedLineageSource = (*Store)(nil)
)

// lineageAncestorSQL reads the ancestor's own labels, closed or not. A lineage
// read must see a tombstoned ancestor: a closed coordinator's subtree is still
// scoped and still spends.
const lineageAncestorSQL = `
SELECT labels
  FROM conversations.child
 WHERE child_id = $1`

// lineageSubtreeSQL selects every row sharing the ancestor's root — the root's
// own row, plus every child whose rafiki/root or legacy fundi/root label is
// that root. It deliberately does NOT filter on closed_at: tombstones are part
// of the lineage. The @> containment is served by child_labels_idx (GIN).
const lineageSubtreeSQL = `
SELECT child_id, COALESCE(session_id, ''), COALESCE(conversation_id::text, ''),
       closed_at IS NOT NULL, labels
  FROM conversations.child
 WHERE child_id = $1 OR labels @> $2::jsonb OR labels @> $3::jsonb`

// legacyRootKey is the pre-rename spelling of childstore.LabelRoot. The
// database implementation must tolerate rows written before the fundi ->
// rafiki consolidation, so the subtree query matches both keys.
const legacyRootKey = "fundi/root"

// Lineage returns the ancestor's own row (when it exists) and every descendant
// at any depth, live or closed. An unknown ancestor yields (nil, nil).
//
// This is the database half of childstore.LineageSource: coverage comes from
// the child table (tombstoned rows included), so a closed descendant stays in
// its ancestors' subtree scope and spend accounting.
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
	var ancestorLabels []byte
	err := s.pool.QueryRow(ctx, lineageAncestorSQL, ancestorChildID).Scan(&ancestorLabels)
	if errors.Is(err, pgx.ErrNoRows) {
		// The ancestor's row is missing. With no caller-supplied root there is
		// nothing to scope the subtree by, so the documented (nil, nil) stands.
		if root == "" {
			return nil, nil
		}
	} else if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	} else {
		var labels map[string]string
		if err := json.Unmarshal(ancestorLabels, &labels); err != nil {
			return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
		}
		// The ancestor's root is its stored root label, falling back to the
		// ancestor itself for a top-level child (which carries no root label).
		// A present row always wins over the caller's fallback.
		if r := childstore.RootLabel(labels); r != "" {
			root = r
		} else {
			root = ancestorChildID
		}
	}

	rootJSON, err := json.Marshal(map[string]string{childstore.LabelRoot: root})
	if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}
	legacyRootJSON, err := json.Marshal(map[string]string{legacyRootKey: root})
	if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}

	rows, err := s.pool.Query(ctx, lineageSubtreeSQL, root, rootJSON, legacyRootJSON)
	if err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}
	defer rows.Close()

	var (
		lineageRows     []childstore.LineageRow
		skippedBadLabel int
		firstSkippedID  string
	)
	for rows.Next() {
		var (
			member     childstore.LineageMember
			memberJSON []byte
		)
		if err := rows.Scan(&member.ChildID, &member.SessionID, &member.ConversationID, &member.Closed, &memberJSON); err != nil {
			return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
		}
		var memberLabels map[string]string
		if err := json.Unmarshal(memberJSON, &memberLabels); err != nil {
			// One unparseable row must not fail the whole tree: it cannot be
			// proven a descendant, so it is skipped rather than allowed to take
			// every other row down with it (which would make every budget sweep
			// for this tree skip forever). Counted and logged once per call, and
			// named by child id only — the parse error is the same shape for every
			// row and would only add noise.
			if skippedBadLabel == 0 {
				firstSkippedID = member.ChildID
			}
			skippedBadLabel++
			continue
		}
		lineageRows = append(lineageRows, childstore.LineageRow{Member: member, Labels: memberLabels})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}
	if skippedBadLabel > 0 {
		slog.Warn("childstoredb: lineage skipped rows with unparseable labels",
			"ancestor", ancestorChildID, "childId", firstSkippedID, "count", skippedBadLabel)
	}

	return childstore.DescendantsOf(lineageRows, ancestorChildID), nil
}
