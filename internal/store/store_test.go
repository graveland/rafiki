package store_test

import (
	"sort"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/protocol"
	"graveland.dev/pi-controller/internal/store"
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
