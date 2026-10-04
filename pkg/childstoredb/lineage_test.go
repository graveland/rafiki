package childstoredb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// lineagePrefix returns a per-run unique child-id prefix. The test database is
// shared and never reset, so a fixed literal would collide with an earlier
// run's rows and a global row count would be wrong; every id below is
// prefixed and hard-deleted in t.Cleanup.
func lineagePrefix() string {
	return fmt.Sprintf("c_lineage%d", time.Now().UnixNano())
}

// cleanupLineage hard-deletes the rows a test inserted. A hard DELETE (not the
// close tombstone) is deliberate: this is test scaffolding, and leaving
// tombstones behind would accumulate forever.
func cleanupLineage(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.child WHERE child_id = ANY($1)`, ids)
	})
}

// insertLineageChild writes a child row with the given lineage labels,
// optionally closing it (stamping the tombstone).
func insertLineageChild(t *testing.T, s *Store, id, parent, root string, closed bool) {
	t.Helper()
	c := assert.NewAborting(t)
	labels := map[string]string{}
	if root != "" {
		labels[childstore.LabelRoot] = root
	}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	rec := childstore.ChildRecord{
		ChildID:   id,
		Kind:      protocol.KindFundi,
		Status:    string(protocol.StatusIdle),
		SpawnedAt: time.Now(),
		Labels:    labels,
	}
	c.NoError(s.Upsert(context.Background(), rec), "upsert %s", id)
	if closed {
		c.NoError(s.Delete(context.Background(), id), "close %s", id)
	}
}

func lineageIDs(ms []childstore.LineageMember) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ChildID)
	}
	return out
}

// TestLineageIncludesTombstonedDescendants pins the headline behaviour: a
// closed child and its grandchild stay in the root's lineage, because the
// query covers tombstones. Fails if the subtree SQL adds a closed_at filter
// (the closed child and grandchild vanish) or forgets the @> label match.
func TestLineageIncludesTombstonedDescendants(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, live, closed, grand := p+"_root", p+"_live", p+"_closed", p+"_grand"
	cleanupLineage(t, pool, root, live, closed, grand)

	insertLineageChild(t, s, root, "", "", false)
	insertLineageChild(t, s, live, root, root, false)
	insertLineageChild(t, s, closed, root, root, true)
	insertLineageChild(t, s, grand, closed, root, false)

	got, err := s.Lineage(ctx, root)
	c.Require().NoError(err, "Lineage(%s)", root)
	c.ElementsMatch([]string{root, live, closed, grand}, lineageIDs(got), "Lineage = %v", lineageIDs(got))

	closedByID := map[string]bool{}
	for _, m := range got {
		closedByID[m.ChildID] = m.Closed
	}
	c.True(closedByID[closed], "the closed child must come back Closed=true")
	c.False(closedByID[live], "a live child must not be marked closed")
	c.False(closedByID[grand], "the grandchild beneath a closed child must not be marked closed")
}

// TestLineageExcludesOtherRoots pins that a second tree's rows are absent.
func TestLineageExcludesOtherRoots(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, child, otherRoot, otherChild := p+"_root", p+"_child", p+"_oroot", p+"_ochild"
	cleanupLineage(t, pool, root, child, otherRoot, otherChild)

	insertLineageChild(t, s, root, "", "", false)
	insertLineageChild(t, s, child, root, root, false)
	insertLineageChild(t, s, otherRoot, "", "", false)
	insertLineageChild(t, s, otherChild, otherRoot, otherRoot, false)

	got, err := s.Lineage(ctx, root)
	c.Require().NoError(err, "Lineage(%s)", root)
	c.ElementsMatch([]string{root, child}, lineageIDs(got), "Lineage = %v; the other tree must be absent", lineageIDs(got))
}

// TestLineageOfUnknownAncestorIsEmpty pins the (nil, nil) contract.
func TestLineageOfUnknownAncestorIsEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)

	got, err := s.Lineage(context.Background(), lineagePrefix()+"_never_inserted")
	c.Require().NoError(err, "Lineage(unknown)")
	c.Nil(got, "an unknown ancestor must yield a nil slice")
}

// TestLineageOfAClosedAncestor pins that a tombstoned ancestor still resolves:
// its own row (Closed=true) and its subtree are returned, so a closed
// coordinator's scope and spend remain accountable.
func TestLineageOfAClosedAncestor(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, child := p+"_root", p+"_child"
	cleanupLineage(t, pool, root, child)

	insertLineageChild(t, s, root, "", "", true) // ancestor is closed
	insertLineageChild(t, s, child, root, root, false)

	got, err := s.Lineage(ctx, root)
	c.Require().NoError(err, "Lineage(%s)", root)
	c.ElementsMatch([]string{root, child}, lineageIDs(got), "Lineage = %v; a closed ancestor must still resolve its subtree", lineageIDs(got))

	for _, m := range got {
		if m.ChildID == root {
			c.True(m.Closed, "the closed ancestor must be returned Closed=true")
		}
	}
}

// TestLineageCarriesSessionAndConversationIds pins the member fields: a child
// with a session id and a conversation id carries both through, with a child
// that has neither resolving to empty strings.
func TestLineageCarriesSessionAndConversationIds(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, child, bare := p+"_root", p+"_child", p+"_bare"
	cleanupLineage(t, pool, root, child, bare)

	convID := insertConversation(t, pool)
	insertLineageChild(t, s, root, "", "", false)

	rec := childstore.ChildRecord{
		ChildID: child, Kind: protocol.KindFundi,
		Status: string(protocol.StatusIdle), SpawnedAt: time.Now(),
		SessionID: "sess-123", ConversationID: convID,
		Labels: map[string]string{childstore.LabelRoot: root, childstore.LabelParent: root},
	}
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert %s", child)
	insertLineageChild(t, s, bare, root, root, false)

	got, err := s.Lineage(ctx, root)
	c.Require().NoError(err, "Lineage(%s)", root)

	byID := map[string]childstore.LineageMember{}
	for _, m := range got {
		byID[m.ChildID] = m
	}
	c.Eq("sess-123", byID[child].SessionID, "SessionID")
	c.Eq(convID, byID[child].ConversationID, "ConversationID")
	c.Eq("", byID[bare].SessionID, "SessionID for a child with none")
	c.Eq("", byID[bare].ConversationID, "ConversationID for a child with none")
}

func TestLineageQueryUsesTheLabelIndex(t *testing.T) {
	c := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels)
		SELECT 'c_lineageseed' || g, 'fundi', 'idle', now(),
		       jsonb_build_object('rafiki/root', 'r' || (g % 50))
		  FROM generate_series(1, 2000) g`)
	c.NoError(err, "seed rows")
	_, err = pool.Exec(ctx, `ANALYZE conversations.child`)
	c.NoError(err, "analyze")

	var plan string
	err = pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+lineageSubtreeSQL,
		"x", []byte(`{"rafiki/root":"x"}`), []byte(`{"fundi/root":"x"}`)).Scan(&plan)
	c.NoError(err, "explain")
	c.StrContains(plan, "child_labels_idx", "the lineage query must use the label index; plan:\n%s", plan)
}
