package childstore_test

import (
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
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
	_, ok := s.ParentOf("nope")
	assert.NewAborting(t).False(ok, "ParentOf on an unknown child should report false")
}

func TestRootOf(t *testing.T) {
	c := assert.NewAborting(t)
	s := tree(t)
	c.Eq("a", s.RootOf("c"), "RootOf(c)")
	c.Eq("a", s.RootOf("a"), "RootOf(a)")
	c.Eq("", s.RootOf("nope"), "RootOf(unknown)")
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
		got := s.IsDescendant(tc.ancestor, tc.candidate)
		assert.NewCollecting(t).Eq(tc.want, got, "IsDescendant(%q,%q) = %v; want", tc.ancestor, tc.candidate, got)
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
			got := s.DescendantDepth(tc.ancestor, tc.cand)
			assert.NewCollecting(t).Eq(tc.want, got, "DescendantDepth(%q,%q) = %d, want", tc.ancestor, tc.cand, got)
		})
	}
}

func TestDescendants(t *testing.T) {
	c := assert.NewCollecting(t)

	s := tree(t)
	got := map[string]bool{}
	for _, snap := range s.Descendants("a") {
		got[snap.ChildID] = true
	}
	want := map[string]bool{"b": true, "c": true, "d": true}
	c.Require().Len(got, len(want), "Descendants(a) = %v; want %v", got, want)
	for id := range want {
		c.False(!got[id], "Descendants(a) missing %q", id)
	}
	c.Empty(s.Descendants("z"), "Descendants(z) should be empty")
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
	assert.NewAborting(t).True(s.IsDescendant("a", "old"), "a legacy fundi/-labelled child should still be found as a descendant")
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

// absoluteChain builds a linear parent chain of n nodes (c_00 at the top,
// each subsequent node parented on the last) and returns the deepest id. The
// root label is left unset: AbsoluteDepth walks rafiki/parent links only.
func absoluteChain(t *testing.T, s *childstore.Store, n int) string {
	t.Helper()
	prev := ""
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c_%02d", i)
		insert(t, s, id, prev, "")
		prev = id
	}
	return prev
}

func TestAbsoluteDepth(t *testing.T) {
	c := assert.NewCollecting(t)
	s := tree(t)
	cases := map[string]int{
		"a": 0, "b": 1, "c": 2, "d": 1, "z": 0,
	}
	for id, want := range cases {
		got := s.AbsoluteDepth(id)
		c.Eq(want, got, "AbsoluteDepth(%s) = %d, want", id, got)
	}
	c.Eq(-1, s.AbsoluteDepth("nope"), "AbsoluteDepth(unknown)")
	c.Eq(-1, s.AbsoluteDepth(""), "AbsoluteDepth(empty)")
}

// A chain exactly as long as the walk bound still resolves, so 64 is a REAL
// depth and the sentinel must not swallow it. (65 nodes = depth 64; the
// literal mirrors childstore's unexported maxChainDepth.)
func TestAbsoluteDepthResolvesChainAtWalkBound(t *testing.T) {
	s := childstore.New()
	deepest := absoluteChain(t, s, 65)
	assert.NewCollecting(t).Eq(64, s.AbsoluteDepth(deepest), "AbsoluteDepth(64-deep chain)")
}

// One hop past the walk bound the walk can no longer confirm a root, and the
// true depth is unknown. It must come back as the refuse sentinel -1 —
// indistinguishable-from-nothing — never as 64, which a legitimate chain can
// also produce. Before the sentinel this fell through as a real depth 64.
func TestAbsoluteDepthRefusesChainPastWalkBound(t *testing.T) {
	s := childstore.New()
	deepest := absoluteChain(t, s, 66) // nodes c_00..c_65, depth 65
	assert.NewCollecting(t).Eq(-1, s.AbsoluteDepth(deepest), "AbsoluteDepth(65-deep chain)")
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
	assert.NewAborting(t).Eq(3, s.LiveDescendantCount("root"), "LiveDescendantCount(root)")
}
