// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// legacyParentKey is the pre-rename spelling of childstore.LabelParent.
const legacyParentKey = "fundi/parent"

// lineageWalkLabelKeys are the ONLY labels the parent walk reads. A row's
// other labels are irrelevant to it and are never inspected, so a non-string
// value under any other key cannot affect ancestry. A row is ambiguous only
// when one of THESE keys is present with a non-string value — the chain through
// that row then cannot be determined — or when its labels are not a JSON
// object at all.
var lineageWalkLabelKeys = [...]string{
	childstore.LabelParent,
	legacyParentKey,
	childstore.LabelRoot,
	legacyRootKey,
}

// parseLineageLabels decodes a child row's labels tolerantly, returning only
// the walk-relevant keys (parent/root, and their legacy spellings) as strings.
// Every other label is ignored, so an unrelated non-string value never breaks
// the row: {"rafiki/root":"x","rafiki/parent":"p","n":1} parses fine and the
// row stays in the tree. It errors only on genuine ambiguity: labels that are
// not a JSON object, or a walk key present with a non-string value (including
// JSON null). A MISSING walk key is not ambiguity — a top-level row or an
// orphan, exactly as DescendantsOf already treats it.
func parseLineageLabels(raw []byte) (map[string]string, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		// A JSON null unmarshals into a nil map with no error, but it is not the
		// object shape a labels column must hold: refuse it rather than reading it
		// as an empty label set.
		return nil, errors.New("labels is not a JSON object")
	}
	out := make(map[string]string, len(lineageWalkLabelKeys))
	for _, key := range lineageWalkLabelKeys {
		v, ok := obj[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("label %q is present but not a string", key)
		}
		out[key] = s
	}
	return out, nil
}

// Lineage returns the ancestor's own row (when it exists) and every descendant
// at any depth, live or closed. An unknown ancestor yields (nil, nil).
//
// This is the database half of childstore.LineageSource: coverage comes from
// the child table (tombstoned rows included), so a closed descendant stays in
// its ancestors' subtree scope and spend accounting.
//
// Label parsing is tolerant per key: a returned row carrying unrelated
// non-string labels is IGNORED and stays in the tree, while genuine ambiguity
// about a returned row — labels that are not a JSON object, or a parent/root
// key present with a non-string value — is an ERROR, so a budget caller fails
// closed rather than silently dropping a subtree.
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
		labels, err := parseLineageLabels(ancestorLabels)
		if err != nil {
			// The ancestor's own row is also a RETURNED row: an ambiguous parent or
			// root key on it makes the whole chain undeterminable, so fail closed.
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

	var lineageRows []childstore.LineageRow
	for rows.Next() {
		var (
			member     childstore.LineageMember
			memberJSON []byte
		)
		if err := rows.Scan(&member.ChildID, &member.SessionID, &member.ConversationID, &member.Closed, &memberJSON); err != nil {
			return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
		}
		// Every returned row is already proven to be in the tree — the query
		// matched it by root containment or by child_id — so a row can never be
		// dropped without losing its whole sub-subtree from a budget rollup. Parse
		// tolerantly; only genuine ambiguity about a returned row fails closed.
		memberLabels, err := parseLineageLabels(memberJSON)
		if err != nil {
			return nil, fmt.Errorf("childstoredb: lineage %s: %w", member.ChildID, err)
		}
		lineageRows = append(lineageRows, childstore.LineageRow{Member: member, Labels: memberLabels})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("childstoredb: lineage %s: %w", ancestorChildID, err)
	}

	return childstore.DescendantsOf(lineageRows, ancestorChildID), nil
}
