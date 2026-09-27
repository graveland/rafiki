package childstore_test

import (
	"sort"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func newSess(id, name, cwd string) *childstore.Session {
	return &childstore.Session{
		ChildID:   id,
		Name:      name,
		Cwd:       cwd,
		Status:    protocol.StatusIdle,
		StartedAt: time.Now(),
	}
}

func TestStore_InsertAndGet(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "foo", "/x"))

	snap, ok := s.Get("c_1")
	c.True(ok, "missing after insert")
	c.False(snap.Name != "foo" || snap.Cwd != "/x", "got %+v", snap)
}

func TestStore_FindByName_Multiple(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "afk", "/a"))
	s.Insert(newSess("c_2", "afk", "/b"))
	s.Insert(newSess("c_3", "other", "/c"))

	got := s.FindByName("afk")
	c.Len(got, 2, "got %d, want 2", len(got))
	ids := []string{got[0].ChildID, got[1].ChildID}
	sort.Strings(ids)
	c.False(ids[0] != "c_1" || ids[1] != "c_2", "got %v", ids)
}

func TestStore_Rename_UpdatesIndex(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "old", "/x"))

	c.NoError(s.Rename("c_1", "new"))

	c.Empty(s.FindByName("old"), "old name still found")
	if got := s.FindByName("new"); len(got) != 1 || got[0].ChildID != "c_1" {
		t.Fatalf("new name lookup: %v", got)
	}
}

func TestStore_FindByCwd(t *testing.T) {
	s := childstore.New()
	s.Insert(newSess("c_1", "a", "/x"))
	s.Insert(newSess("c_2", "b", "/x"))
	s.Insert(newSess("c_3", "c", "/y"))
	got := s.FindByCwd("/x")
	assert.NewAborting(t).Len(got, 2, "got %d, want 2", len(got))
}

func TestStore_Delete_RemovesFromAllIndexes(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "afk", "/x"))
	s.Delete("c_1")
	_, ok := s.Get("c_1")
	c.False(ok, "still in primary")
	c.Empty(s.FindByName("afk"), "name index leak")
	c.Empty(s.FindByCwd("/x"), "cwd index leak")
	c.Empty(s.FindByStatus(protocol.StatusIdle), "status index leak")
}

func TestStore_VerifyOnRead_FiltersStaleIndex(t *testing.T) {
	c := assert.NewAborting(t)
	// Direct unit test of the verify path: insert under one name,
	// mutate the session's name field through Update, then ensure
	// the old-name lookup returns nothing (verify-on-read filters it).
	s := childstore.New()
	s.Insert(newSess("c_1", "old", "/x"))
	c.NoError(s.Update("c_1", func(sess *childstore.Session) {
		sess.Name = "new"
	})) // bypasses the index update intentionally for this test
	got := s.FindByName("old")
	c.Empty(got, "verify-on-read failed")
}

func TestStore_SetStatus(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "a", "/x"))
	prev, ok := s.SetStatus("c_1", protocol.StatusStreaming)
	c.True(ok, "missing child")
	c.Eq(protocol.StatusIdle, prev, "prev")
	snap, _ := s.Get("c_1")
	c.Eq(protocol.StatusStreaming, snap.Status, "status")
	// Old status should no longer find this child.
	c.Empty(s.FindByStatus(protocol.StatusIdle), "old status index not cleared")
	// New status should find it.
	if got := s.FindByStatus(protocol.StatusStreaming); len(got) != 1 || got[0].ChildID != "c_1" {
		t.Fatalf("new status index missing: %v", got)
	}
}

func TestStore_NotFoundPaths(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()

	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get on missing returned ok")
	}
	if err := s.Update("missing", func(*childstore.Session) {}); err != childstore.ErrNotFound {
		t.Fatalf("Update on missing: got %v, want ErrNotFound", err)
	}
	err := s.Rename("missing", "new")
	c.False(err != childstore.ErrNotFound, "Rename on missing: got %v, want ErrNotFound", err)
	_, ok := s.SetStatus("missing", protocol.StatusIdle)
	c.False(ok, "SetStatus on missing returned ok")
	// Delete on missing is a no-op — confirm it doesn't panic.
	s.Delete("missing")
}

func TestStore_SetLabels_SetAndRemove(t *testing.T) {
	c := assert.NewCollecting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "a", "/x"))

	// Set some labels.
	merged, err := s.SetLabels("c_1", map[string]string{"env": "prod", "tier": "fast"}, nil)
	c.Require().NoError(err)
	c.Require().False(merged["env"] != "prod" || merged["tier"] != "fast", "set: got %v", merged)

	// Update one, add one, remove one.
	merged, err = s.SetLabels("c_1", map[string]string{"env": "staging", "owner": "brent"}, []string{"tier"})
	c.Require().NoError(err)
	c.Eq("staging", merged["env"], "update: env")
	c.Eq("brent", merged["owner"], "add: owner")
	_, ok := merged["tier"]
	c.False(ok, "remove: tier still present")

	// Snapshot reflects latest labels.
	snap, _ := s.Get("c_1")
	c.Require().False(snap.Labels["env"] != "staging" || snap.Labels["owner"] != "brent", "snapshot labels: %v", snap.Labels)

	// Returned map is a defensive copy.
	merged["env"] = "MUTATED"
	snap2, _ := s.Get("c_1")
	c.Require().Eq("staging", snap2.Labels["env"], "returned map was not a defensive copy")
}

func TestStore_SetLabels_NotFound(t *testing.T) {
	s := childstore.New()
	_, err := s.SetLabels("missing", map[string]string{"k": "v"}, nil)
	assert.NewAborting(t).False(err != childstore.ErrNotFound, "expected ErrNotFound, got %v", err)
}

func TestStore_SetLabels_RemoveOnly(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "a", "/x"))

	// Remove on empty labels is a no-op.
	merged, err := s.SetLabels("c_1", nil, []string{"nonexistent"})
	c.NoError(err)
	c.Empty(merged, "expected empty, got")
}

func TestStore_ListSortedByStartedAtDesc(t *testing.T) {
	s := childstore.New()
	now := time.Now()
	a := newSess("c_a", "a", "/")
	a.StartedAt = now.Add(-2 * time.Hour)
	b := newSess("c_b", "b", "/")
	b.StartedAt = now.Add(-1 * time.Hour)
	s.Insert(a)
	s.Insert(b)
	got := s.List()
	assert.NewAborting(t).Len(got, 2, "got %d", len(got))
	if got[0].ChildID != "c_b" || got[1].ChildID != "c_a" {
		t.Fatalf("sort wrong: %v %v", got[0].ChildID, got[1].ChildID)
	}
}

func TestStore_SetMaxCost(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "a", "/x"))

	c.NoError(s.SetMaxCost("c_1", 12.5), "SetMaxCost")
	snap, _ := s.Get("c_1")
	c.Eq(12.5, snap.MaxCost, "MaxCost")

	// 0 means unlimited, same convention as every other MaxCost write in
	// this codebase — confirm it round-trips, not just non-zero values.
	c.NoError(s.SetMaxCost("c_1", 0), "SetMaxCost(0)")
	snap, _ = s.Get("c_1")
	c.Eq(0, snap.MaxCost, "MaxCost")
}

func TestStore_SetMaxCost_NotFound(t *testing.T) {
	s := childstore.New()
	err := s.SetMaxCost("missing", 5.0)
	assert.NewAborting(t).False(err != childstore.ErrNotFound, "SetMaxCost on missing id: got %v, want ErrNotFound", err)
}
