package childstore_test

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// insert adds a session with the given lineage labels.
func insert(t *testing.T, s *childstore.Store, id, parent, root string) {
	t.Helper()
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	if root != "" {
		labels[childstore.LabelRoot] = root
	}
	s.Insert(&childstore.Session{
		ChildID:   id,
		Status:    protocol.StatusIdle,
		StartedAt: time.Now(),
		Labels:    labels,
	})
}

// tree builds:  a (top)  ->  b  ->  c
//
//	a         ->  d
//	z (top, unrelated)
func tree(t *testing.T) *childstore.Store {
	t.Helper()
	s := childstore.New()
	insert(t, s, "a", "", "")
	insert(t, s, "b", "a", "a")
	insert(t, s, "c", "b", "a")
	insert(t, s, "d", "a", "a")
	insert(t, s, "z", "", "")
	return s
}

func TestParentOf(t *testing.T) {
	s := tree(t)
	if p, ok := s.ParentOf("c"); !ok || p != "b" {
		t.Fatalf("ParentOf(c) = %q,%v; want b,true", p, ok)
	}
	if _, ok := s.ParentOf("a"); ok {
		t.Fatal("ParentOf(a) should report false for a top-level child")
	}
	if _, ok := s.ParentOf("nope"); ok {
		t.Fatal("ParentOf on an unknown child should report false")
	}
}

func TestRootOf(t *testing.T) {
	s := tree(t)
	if got := s.RootOf("c"); got != "a" {
		t.Fatalf("RootOf(c) = %q; want a", got)
	}
	if got := s.RootOf("a"); got != "a" {
		t.Fatalf("RootOf(a) = %q; want a (a top-level child is its own root)", got)
	}
	if got := s.RootOf("nope"); got != "" {
		t.Fatalf("RootOf(unknown) = %q; want empty", got)
	}
}

func TestIsDescendant(t *testing.T) {
	s := tree(t)
	cases := []struct {
		ancestor, candidate string
		want                bool
	}{
		{"a", "b", true},
		{"a", "c", true},
		{"a", "d", true},
		{"b", "c", true},
		{"b", "d", false},
		{"c", "b", false},
		{"a", "z", false},
		{"a", "a", false},
		{"nope", "c", false},
		{"a", "nope", false},
	}
	for _, tc := range cases {
		if got := s.IsDescendant(tc.ancestor, tc.candidate); got != tc.want {
			t.Errorf("IsDescendant(%q,%q) = %v; want %v", tc.ancestor, tc.candidate, got, tc.want)
		}
	}
}

func TestDescendantDepth(t *testing.T) {
	s := childstore.New()
	insert(t, s, "c_root", "", "")
	insert(t, s, "c_mid", "c_root", "c_root")
	insert(t, s, "c_leaf", "c_mid", "c_root")

	for _, tc := range []struct {
		name           string
		ancestor, cand string
		want           int
	}{
		{"direct child", "c_root", "c_mid", 1},
		{"grandchild", "c_root", "c_leaf", 2},
		{"self is not a descendant", "c_root", "c_root", -1},
		{"upward is not a descendant", "c_leaf", "c_root", -1},
		{"unknown candidate", "c_root", "c_nope", -1},
		{"unknown ancestor", "c_nope", "c_leaf", -1},
		{"empty ids", "", "", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.DescendantDepth(tc.ancestor, tc.cand); got != tc.want {
				t.Errorf("DescendantDepth(%q,%q) = %d, want %d", tc.ancestor, tc.cand, got, tc.want)
			}
		})
	}
}

func TestDescendants(t *testing.T) {

	s := tree(t)
	got := map[string]bool{}
	for _, snap := range s.Descendants("a") {
		got[snap.ChildID] = true
	}
	want := map[string]bool{"b": true, "c": true, "d": true}
	if len(got) != len(want) {
		t.Fatalf("Descendants(a) = %v; want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("Descendants(a) missing %q", id)
		}
	}
	if len(s.Descendants("z")) != 0 {
		t.Error("Descendants(z) should be empty")
	}
}

func TestLegacyFundiPrefixTolerated(t *testing.T) {
	s := childstore.New()
	s.Insert(&childstore.Session{
		ChildID:   "old",
		Status:    protocol.StatusIdle,
		StartedAt: time.Now(),
		Labels:    map[string]string{"fundi/parent": "a", "fundi/root": "a"},
	})
	insert(t, s, "a", "", "")
	if p, ok := s.ParentOf("old"); !ok || p != "a" {
		t.Fatalf("ParentOf on a legacy fundi/ record = %q,%v; want a,true", p, ok)
	}
	if !s.IsDescendant("a", "old") {
		t.Fatal("a legacy fundi/-labelled child should still be found as a descendant")
	}
}

func TestCycleTerminates(t *testing.T) {
	s := childstore.New()
	insert(t, s, "x", "y", "x")
	insert(t, s, "y", "x", "x")
	done := make(chan bool, 1)
	go func() { done <- s.IsDescendant("x", "y") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("IsDescendant did not terminate on a cyclic parent chain")
	}
}

func TestAbsoluteDepth(t *testing.T) {
	s := tree(t)
	cases := map[string]int{
		"a": 0, "b": 1, "c": 2, "d": 1, "z": 0,
	}
	for id, want := range cases {
		if got := s.AbsoluteDepth(id); got != want {
			t.Errorf("AbsoluteDepth(%s) = %d, want %d", id, got, want)
		}
	}
	if got := s.AbsoluteDepth("nope"); got != -1 {
		t.Errorf("AbsoluteDepth(unknown) = %d, want -1", got)
	}
	if got := s.AbsoluteDepth(""); got != -1 {
		t.Errorf("AbsoluteDepth(empty) = %d, want -1", got)
	}
}

func TestLiveDescendantCount(t *testing.T) {
	s := childstore.New()
	insert(t, s, "root", "", "")
	// 3 live children + 1 exited
	for _, id := range []string{"c1", "c2", "c3"} {
		insert(t, s, id, "root", "root")
	}
	s.Insert(&childstore.Session{
		ChildID: "c_dead", Status: protocol.StatusExited, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "root", childstore.LabelRoot: "root"},
	})
	if got := s.LiveDescendantCount("root"); got != 3 {
		t.Fatalf("LiveDescendantCount(root) = %d, want 3", got)
	}
}
