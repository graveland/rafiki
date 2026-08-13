package executors

import (
	"fmt"
	"strings"
)

// Selector is a parsed label selector for matching executors or narrowing
// a parent's effective set.
type Selector struct {
	terms []selectorTerm
}

type selectorOp int

const (
	opEq        selectorOp = iota // key = value
	opNeq                         // key != value
	opIn                          // key in (v1,v2)
	opNotIn                       // key notin (v1,v2)
	opExists                      // key (presence)
	opNotExists                   // !key (absence)
)

type selectorTerm struct {
	key    string
	op     selectorOp
	values []string
}

// ParseSelector parses a label selector string. An empty string matches
// everything. A malformed selector is an error, never a permissive default.
//
// Supported forms, joined by comma (AND):
//
//	os=linux          equality
//	os!=linux         inequality
//	os                key presence
//	!os               key absence
//	os in (linux,darwin)  set membership
//	os notin (linux)      set non-membership
func ParseSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Selector{}, nil
	}

	var terms []selectorTerm
	for _, part := range splitSelector(s) {
		t, err := parseTerm(part)
		if err != nil {
			return Selector{}, fmt.Errorf("parse selector %q: %w", s, err)
		}
		terms = append(terms, t)
	}
	return Selector{terms: terms}, nil
}

// Matches returns true when labels satisfies every term.
func (s Selector) Matches(labels map[string]string) bool {
	for _, t := range s.terms {
		if !t.matches(labels) {
			return false
		}
	}
	return true
}

// Narrow returns the executors in parentSet that ALSO match child's
// selector. It evaluates the sets, never tries to prove implication —
// intersection is exact and correct by construction.
func Narrow(parentSet []Executor, child Selector) []Executor {
	if len(parentSet) == 0 {
		return nil
	}
	var out []Executor
	for _, e := range parentSet {
		if child.Matches(e.Labels) {
			out = append(out, e)
		}
	}
	return out
}

// Explain returns "" when labels satisfies every term, otherwise the first
// failing term in human words against the actual labels.
func (s Selector) Explain(labels map[string]string) string {
	for _, t := range s.terms {
		if explain := t.explain(labels); explain != "" {
			return explain
		}
	}
	return ""
}

func (t selectorTerm) matches(labels map[string]string) bool {
	v, exists := labels[t.key]
	switch t.op {
	case opEq:
		return exists && v == t.values[0]
	case opNeq:
		return !exists || v != t.values[0]
	case opExists:
		return exists
	case opNotExists:
		return !exists
	case opIn:
		if !exists {
			return false
		}
		for _, want := range t.values {
			if v == want {
				return true
			}
		}
		return false
	case opNotIn:
		if !exists {
			return true // absent is not in any set
		}
		for _, excl := range t.values {
			if v == excl {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// explain returns "" when the term matches, otherwise a human-readable
// description of why it failed against the actual labels.
func (t selectorTerm) explain(labels map[string]string) string {
	v, exists := labels[t.key]
	switch t.op {
	case opEq:
		if !exists {
			return fmt.Sprintf("no %s label, wanted %s=%s", t.key, t.key, t.values[0])
		}
		if v != t.values[0] {
			return fmt.Sprintf("%s=%s, wanted %s=%s", t.key, v, t.key, t.values[0])
		}
		return ""
	case opNeq:
		if exists && v == t.values[0] {
			return fmt.Sprintf("%s=%s, wanted %s!=%s", t.key, v, t.key, t.values[0])
		}
		return ""
	case opExists:
		if !exists {
			return fmt.Sprintf("no %s label, wanted it present", t.key)
		}
		return ""
	case opNotExists:
		if exists {
			return fmt.Sprintf("has %s=%s, wanted no %s label", t.key, v, t.key)
		}
		return ""
	case opIn:
		if !exists {
			return fmt.Sprintf("no %s label, wanted %s in (%s)", t.key, t.key, strings.Join(t.values, ","))
		}
		for _, want := range t.values {
			if v == want {
				return ""
			}
		}
		return fmt.Sprintf("%s=%s, wanted %s in (%s)", t.key, v, t.key, strings.Join(t.values, ","))
	case opNotIn:
		if !exists {
			return ""
		}
		for _, excl := range t.values {
			if v == excl {
				return fmt.Sprintf("%s=%s, wanted %s notin (%s)", t.key, v, t.key, strings.Join(t.values, ","))
			}
		}
		return ""
	}
	return ""
}

// splitSelector splits on comma, respecting parentheses.
func splitSelector(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}

func parseTerm(s string) (selectorTerm, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return selectorTerm{}, fmt.Errorf("empty term")
	}

	// !key — absence
	if strings.HasPrefix(s, "!") && !strings.Contains(s, "=") {
		key := strings.TrimSpace(s[1:])
		if key == "" {
			return selectorTerm{}, fmt.Errorf("empty key in %q", s)
		}
		return selectorTerm{key: key, op: opNotExists}, nil
	}

	// key in (v1,v2) / key notin (v1,v2)
	if idx := strings.Index(s, " in ("); idx >= 0 {
		key := strings.TrimSpace(s[:idx])
		rest := s[idx+len(" in ("):]
		if !strings.HasSuffix(rest, ")") {
			return selectorTerm{}, fmt.Errorf("unclosed parenthesis in %q", s)
		}
		vals := splitValues(strings.TrimSuffix(rest, ")"))
		if len(vals) == 0 {
			return selectorTerm{}, fmt.Errorf("empty value list in %q", s)
		}
		return selectorTerm{key: key, op: opIn, values: vals}, nil
	}
	if idx := strings.Index(s, " notin ("); idx >= 0 {
		key := strings.TrimSpace(s[:idx])
		rest := s[idx+len(" notin ("):]
		if !strings.HasSuffix(rest, ")") {
			return selectorTerm{}, fmt.Errorf("unclosed parenthesis in %q", s)
		}
		vals := splitValues(strings.TrimSuffix(rest, ")"))
		if len(vals) == 0 {
			return selectorTerm{}, fmt.Errorf("empty value list in %q", s)
		}
		return selectorTerm{key: key, op: opNotIn, values: vals}, nil
	}

	// key!=value
	if idx := strings.Index(s, "!="); idx >= 0 {
		key := strings.TrimSpace(s[:idx])
		val := strings.TrimSpace(s[idx+2:])
		if key == "" || val == "" {
			return selectorTerm{}, fmt.Errorf("empty key or value in %q", s)
		}
		// Reject doubled operators like "os!!=x" — "!=" is the only valid form.
		if strings.Contains(key, "!") {
			return selectorTerm{}, fmt.Errorf("unexpected operator in %q", s)
		}
		return selectorTerm{key: key, op: opNeq, values: []string{val}}, nil
	}

	// key=value
	if idx := strings.Index(s, "="); idx >= 0 {
		key := strings.TrimSpace(s[:idx])
		val := strings.TrimSpace(s[idx+1:])
		if key == "" || val == "" {
			return selectorTerm{}, fmt.Errorf("empty key or value in %q", s)
		}
		return selectorTerm{key: key, op: opEq, values: []string{val}}, nil
	}

	// key — presence
	// A key must be a single identifier — reject anything that looks like a
	// broken compound (e.g. "os in linux" without parentheses).
	if s != "" {
		if strings.Contains(s, " ") {
			return selectorTerm{}, fmt.Errorf("unexpected space in %q — is this a broken compound?", s)
		}
		return selectorTerm{key: s, op: opExists}, nil
	}

	return selectorTerm{}, fmt.Errorf("cannot parse %q", s)
}

func splitValues(s string) []string {
	var vals []string
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			vals = append(vals, v)
		}
	}
	return vals
}
