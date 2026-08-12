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
		{"os notin (linux,darwin)", map[string]string{"os": "darwin"}, false},
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
	for _, s := range []string{"os=", "=linux", "os in linux", "os in (", "os!!=x", "os != x"} {
		if _, err := ParseSelector(s); err == nil {
			t.Errorf("ParseSelector(%q) must fail", s)
		}
	}
}

// Narrowing is runtime INTERSECTION, never a subset proof.
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

// The property that makes annotations safe.
func TestChildCannotEscapeItsParentsSet(t *testing.T) {
	parentSet := []Executor{{ID: "b", Labels: map[string]string{"env": "home"}}}
	child, _ := ParseSelector("env=work")
	if got := Narrow(parentSet, child); len(got) != 0 {
		t.Fatalf("a child reached outside its parent's set: %+v", got)
	}
}

func TestSelectorExplain(t *testing.T) {
	// eq mismatch
	s, _ := ParseSelector("env=work")
	if got := s.Explain(map[string]string{"env": "home"}); got != "env=home, wanted env=work" {
		t.Errorf("got %q", got)
	}

	// missing key
	if got := s.Explain(map[string]string{}); got != "no env label, wanted env=work" {
		t.Errorf("got %q", got)
	}

	// key absence violated
	s, _ = ParseSelector("!os")
	if got := s.Explain(map[string]string{"os": "linux"}); got != "has os label" {
		t.Errorf("got %q", got)
	}

	// in list mismatch
	s, _ = ParseSelector("os in (linux)")
	if got := s.Explain(map[string]string{"os": "darwin"}); got != "os=darwin, wanted os in (linux)" {
		t.Errorf("got %q", got)
	}

	// match -> empty string
	if got := s.Explain(map[string]string{"os": "linux"}); got != "" {
		t.Errorf("match must return empty, got %q", got)
	}
}

func TestSelectorStringRoundTrip(t *testing.T) {
	for _, s := range []string{"", "os=linux", "os!=darwin", "os in (linux,darwin)", "os notin (windows)", "os,arch=arm64", "os=linux,!arch"} {
		sel, err := ParseSelector(s)
		if err != nil {
			t.Errorf("ParseSelector(%q): %v", s, err)
			continue
		}
		if got := sel.String(); got != s {
			t.Errorf("String round-trip: %q -> %q", s, got)
		}
	}
}
