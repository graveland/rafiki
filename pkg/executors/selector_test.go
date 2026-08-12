package executors

import "testing"

func TestSelectorSyntax(t *testing.T) {
	cases := []struct {
		sel    string
		labels map[string]string
		want   bool
	}{
		{"", nil, true}, // empty matches everything
		{"os=linux", map[string]string{"os": "linux"}, true},
		{"os=linux", map[string]string{"os": "darwin"}, false},
		{"os!=linux", map[string]string{"os": "darwin"}, true},
		{"os", map[string]string{"os": "linux"}, true},    // key presence
		{"!os", map[string]string{"arch": "arm64"}, true}, // key absence
		{"!os", map[string]string{"os": "linux"}, false},
		{"os in (linux,darwin)", map[string]string{"os": "darwin"}, true},
		{"os notin (linux)", map[string]string{"os": "darwin"}, true},
		{"os=linux,arch=arm64", map[string]string{"os": "linux", "arch": "arm64"}, true},
		{"os=linux,arch=arm64", map[string]string{"os": "linux"}, false}, // AND
	}
	for _, c := range cases {
		s, err := ParseSelector(c.sel)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", c.sel, err)
		}
		if got := s.Matches(c.labels); got != c.want {
			t.Errorf("%q against %v = %v, want %v", c.sel, c.labels, got, c.want)
		}
	}
}

func TestParseSelectorRejectsGarbage(t *testing.T) {
	for _, s := range []string{"os=", "=linux", "os in linux", "os in (", "os!!=x", ","} {
		if _, err := ParseSelector(s); err == nil {
			t.Errorf("ParseSelector(%q) must fail — a selector that silently parses to 'match everything' is a confinement hole", s)
		}
	}
}

// Narrowing is runtime INTERSECTION, never a subset proof. Proving a child's
// selector is a subset of its parent's is decidable for equality matches and a
// logic puzzle the moment notin appears; intersecting the evaluated sets is
// exact, cheap, and correct by construction.
func TestNarrowIsIntersectionNotImplication(t *testing.T) {
	all := []Executor{
		{ID: "a", Labels: map[string]string{"env": "work", "os": "linux"}},
		{ID: "b", Labels: map[string]string{"env": "home", "os": "linux"}},
		{ID: "c", Labels: map[string]string{"env": "home", "os": "darwin"}},
	}
	parent, _ := ParseSelector("env=home")
	child, _ := ParseSelector("os=linux")

	var parentSet []Executor
	for _, e := range all {
		if parent.Matches(e.Labels) {
			parentSet = append(parentSet, e)
		}
	}
	got := Narrow(parentSet, child)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("want only b, got %+v", got)
	}
}

// The property that makes annotations safe: a child can never reach an
// executor its parent could not, whatever its selector says.
func TestChildCannotEscapeItsParentsSet(t *testing.T) {
	parentSet := []Executor{{ID: "b", Labels: map[string]string{"env": "home"}}}
	child, _ := ParseSelector("env=work") // asking for something outside the set
	if got := Narrow(parentSet, child); len(got) != 0 {
		t.Fatalf("a child reached outside its parent's set: %+v", got)
	}
}
