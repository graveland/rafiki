package store_test

import (
	"sort"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/protocol"
	"git.graveland.dev/brent/pi-controller/internal/store"
)

func newSess(id, name, cwd string) *store.Session {
	return &store.Session{
		ChildID:   id,
		Name:      name,
		Cwd:       cwd,
		Status:    protocol.StatusIdle,
		StartedAt: time.Now(),
	}
}

func TestStore_InsertAndGet(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "foo", "/x"))

	snap, ok := s.Get("c_1")
	if !ok {
		t.Fatal("missing after insert")
	}
	if snap.Name != "foo" || snap.Cwd != "/x" {
		t.Fatalf("got %+v", snap)
	}
}

func TestStore_FindByName_Multiple(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "afk", "/a"))
	s.Insert(newSess("c_2", "afk", "/b"))
	s.Insert(newSess("c_3", "other", "/c"))

	got := s.FindByName("afk")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	ids := []string{got[0].ChildID, got[1].ChildID}
	sort.Strings(ids)
	if ids[0] != "c_1" || ids[1] != "c_2" {
		t.Fatalf("got %v", ids)
	}
}

func TestStore_Rename_UpdatesIndex(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "old", "/x"))

	if err := s.Rename("c_1", "new"); err != nil {
		t.Fatal(err)
	}

	if got := s.FindByName("old"); len(got) != 0 {
		t.Fatalf("old name still found: %v", got)
	}
	if got := s.FindByName("new"); len(got) != 1 || got[0].ChildID != "c_1" {
		t.Fatalf("new name lookup: %v", got)
	}
}

func TestStore_FindByCwd(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "a", "/x"))
	s.Insert(newSess("c_2", "b", "/x"))
	s.Insert(newSess("c_3", "c", "/y"))
	if got := s.FindByCwd("/x"); len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestStore_Delete_RemovesFromAllIndexes(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "afk", "/x"))
	s.Delete("c_1")
	if _, ok := s.Get("c_1"); ok {
		t.Fatal("still in primary")
	}
	if got := s.FindByName("afk"); len(got) != 0 {
		t.Fatalf("name index leak: %v", got)
	}
	if got := s.FindByCwd("/x"); len(got) != 0 {
		t.Fatalf("cwd index leak: %v", got)
	}
	if got := s.FindByStatus(protocol.StatusIdle); len(got) != 0 {
		t.Fatalf("status index leak: %v", got)
	}
}

func TestStore_VerifyOnRead_FiltersStaleIndex(t *testing.T) {
	// Direct unit test of the verify path: insert under one name,
	// mutate the session's name field through Update, then ensure
	// the old-name lookup returns nothing (verify-on-read filters it).
	s := store.New()
	s.Insert(newSess("c_1", "old", "/x"))
	s.Update("c_1", func(sess *store.Session) {
		sess.Name = "new" // bypasses the index update intentionally for this test
	})
	got := s.FindByName("old")
	if len(got) != 0 {
		t.Fatalf("verify-on-read failed: %v", got)
	}
}

func TestStore_SetStatus(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "a", "/x"))
	prev, ok := s.SetStatus("c_1", protocol.StatusStreaming)
	if !ok {
		t.Fatal("missing child")
	}
	if prev != protocol.StatusIdle {
		t.Fatalf("prev: %v", prev)
	}
	snap, _ := s.Get("c_1")
	if snap.Status != protocol.StatusStreaming {
		t.Fatalf("status: %v", snap.Status)
	}
	// Old status should no longer find this child.
	if got := s.FindByStatus(protocol.StatusIdle); len(got) != 0 {
		t.Fatalf("old status index not cleared: %v", got)
	}
	// New status should find it.
	if got := s.FindByStatus(protocol.StatusStreaming); len(got) != 1 || got[0].ChildID != "c_1" {
		t.Fatalf("new status index missing: %v", got)
	}
}

func TestStore_NotFoundPaths(t *testing.T) {
	s := store.New()

	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get on missing returned ok")
	}
	if err := s.Update("missing", func(*store.Session) {}); err != store.ErrNotFound {
		t.Fatalf("Update on missing: got %v, want ErrNotFound", err)
	}
	if err := s.Rename("missing", "new"); err != store.ErrNotFound {
		t.Fatalf("Rename on missing: got %v, want ErrNotFound", err)
	}
	if _, ok := s.SetStatus("missing", protocol.StatusIdle); ok {
		t.Fatal("SetStatus on missing returned ok")
	}
	// Delete on missing is a no-op — confirm it doesn't panic.
	s.Delete("missing")
}

func TestStore_SetLabels_SetAndRemove(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "a", "/x"))

	// Set some labels.
	merged, err := s.SetLabels("c_1", map[string]string{"env": "prod", "tier": "fast"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged["env"] != "prod" || merged["tier"] != "fast" {
		t.Fatalf("set: got %v", merged)
	}

	// Update one, add one, remove one.
	merged, err = s.SetLabels("c_1", map[string]string{"env": "staging", "owner": "brent"}, []string{"tier"})
	if err != nil {
		t.Fatal(err)
	}
	if merged["env"] != "staging" {
		t.Errorf("update: env = %q, want staging", merged["env"])
	}
	if merged["owner"] != "brent" {
		t.Errorf("add: owner = %q, want brent", merged["owner"])
	}
	if _, ok := merged["tier"]; ok {
		t.Errorf("remove: tier still present")
	}

	// Snapshot reflects latest labels.
	snap, _ := s.Get("c_1")
	if snap.Labels["env"] != "staging" || snap.Labels["owner"] != "brent" {
		t.Fatalf("snapshot labels: %v", snap.Labels)
	}

	// Returned map is a defensive copy.
	merged["env"] = "MUTATED"
	snap2, _ := s.Get("c_1")
	if snap2.Labels["env"] != "staging" {
		t.Fatal("returned map was not a defensive copy")
	}
}

func TestStore_SetLabels_NotFound(t *testing.T) {
	s := store.New()
	_, err := s.SetLabels("missing", map[string]string{"k": "v"}, nil)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_SetLabels_RemoveOnly(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "a", "/x"))

	// Remove on empty labels is a no-op.
	merged, err := s.SetLabels("c_1", nil, []string{"nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 0 {
		t.Fatalf("expected empty, got %v", merged)
	}
}

func TestStore_ListSortedByStartedAtDesc(t *testing.T) {
	s := store.New()
	now := time.Now()
	a := newSess("c_a", "a", "/")
	a.StartedAt = now.Add(-2 * time.Hour)
	b := newSess("c_b", "b", "/")
	b.StartedAt = now.Add(-1 * time.Hour)
	s.Insert(a)
	s.Insert(b)
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].ChildID != "c_b" || got[1].ChildID != "c_a" {
		t.Fatalf("sort wrong: %v %v", got[0].ChildID, got[1].ChildID)
	}
}
