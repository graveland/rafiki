package executors

import (
	"fmt"
	"strings"
)

// Selector is a parsed label selector over executor labels.
// Zero value (empty) matches every executor.
type Selector struct {
	terms []term
}

type term struct {
	key    string
	op     string // "=", "!=", "in", "notin", "exists", "notexists"
	values []string
}

// ParseSelector parses a label selector string.
//
// Supported forms, joined by comma (AND):
//   - "key"         — key existence
//   - "!key"        — key absence
//   - "key=value"   — exact match
//   - "key!=value"  — not-equal match
//   - "key in (a,b)"   — set membership
//   - "key notin (a,b)" — set exclusion
//
// An empty selector matches everything.
// A malformed selector is an error — never a permissive default.
func ParseSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Selector{}, nil
	}
	parts := splitSelectorTerms(s)
	var terms []term
	for _, part := range parts {
		t, err := parseTerm(part)
		if err != nil {
			return Selector{}, fmt.Errorf("invalid selector %q: %w", s, err)
		}
		terms = append(terms, t)
	}
	return Selector{terms: terms}, nil
}

// splitSelectorTerms splits on commas that are not inside parentheses.
func splitSelectorTerms(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
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

func parseTerm(s string) (term, error) {
	// "key in (values)" or "key notin (values)"
	if strings.Contains(s, " in (") {
		key, values, err := parseIn(s, "in")
		if err != nil {
			return term{}, err
		}
		if !validKey(key) {
			return term{}, fmt.Errorf("invalid key %q", key)
		}
		return term{key: key, op: "in", values: values}, nil
	}
	if strings.Contains(s, " notin (") {
		key, values, err := parseIn(s, "notin")
		if err != nil {
			return term{}, err
		}
		if !validKey(key) {
			return term{}, fmt.Errorf("invalid key %q", key)
		}
		return term{key: key, op: "notin", values: values}, nil
	}
	// "!key"
	if strings.HasPrefix(s, "!") {
		key := s[1:]
		if key == "" || strings.ContainsAny(key, "=!(), ") {
			return term{}, fmt.Errorf("invalid key absence %q", s)
		}
		return term{key: key, op: "notexists"}, nil
	}
	// "key!=value"
	if i := strings.Index(s, "!="); i >= 0 {
		key, value := s[:i], s[i+2:]
		if key == "" || value == "" || !validKey(key) {
			return term{}, fmt.Errorf("invalid not-equal %q", s)
		}
		return term{key: key, op: "!=", values: []string{value}}, nil
	}
	// "key=value"
	if i := strings.Index(s, "="); i >= 0 {
		key, value := s[:i], s[i+1:]
		if key == "" || value == "" || !validKey(key) {
			return term{}, fmt.Errorf("invalid equality %q", s)
		}
		return term{key: key, op: "=", values: []string{value}}, nil
	}
	// plain "key"
	if !validKey(s) {
		return term{}, fmt.Errorf("invalid key %q", s)
	}
	return term{key: s, op: "exists"}, nil
}

// validKey reports whether k is a valid label key: non-empty, no whitespace,
// no punctuation except hyphen and slash.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if r == ' ' || r == '!' || r == '=' || r == '(' || r == ')' || r == ',' {
			return false
		}
	}
	return true
}

func parseIn(s, op string) (string, []string, error) {
	idx := strings.Index(s, " "+op+" (")
	if idx < 0 {
		return "", nil, fmt.Errorf("expected 'key %s (values)'", op)
	}
	key := s[:idx]
	if key == "" {
		return "", nil, fmt.Errorf("empty key in %q", s)
	}
	rest := s[idx+len(" "+op+" ("):]
	if !strings.HasSuffix(rest, ")") {
		return "", nil, fmt.Errorf("unterminated %s list in %q", op, s)
	}
	list := rest[:len(rest)-1]
	if list == "" {
		return "", nil, fmt.Errorf("empty %s list", op)
	}
	values := strings.Split(list, ",")
	for i, v := range values {
		values[i] = strings.TrimSpace(v)
		if values[i] == "" {
			return "", nil, fmt.Errorf("empty value in %s list", op)
		}
	}
	return key, values, nil
}

// Matches reports whether labels satisfy every term in the selector.
func (s Selector) Matches(labels map[string]string) bool {
	for _, t := range s.terms {
		val, exists := labels[t.key]
		switch t.op {
		case "exists":
			if !exists {
				return false
			}
		case "notexists":
			if exists {
				return false
			}
		case "=":
			if !exists || val != t.values[0] {
				return false
			}
		case "!=":
			if !exists || val != t.values[0] {
				continue
			}
			return false
		case "in":
			if !exists {
				return false
			}
			found := false
			for _, v := range t.values {
				if val == v {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "notin":
			if !exists {
				continue
			}
			for _, v := range t.values {
				if val == v {
					return false
				}
			}
		}
	}
	return true
}

// Narrow returns the executors in parentSet that also satisfy child.
// Used for intersecting a parent's effective executor set with a child's
// selector — never replaced by a subset proof over selector text.
func Narrow(parentSet []Executor, child Selector) []Executor {
	var out []Executor
	for _, e := range parentSet {
		if child.Matches(e.Labels) {
			out = append(out, e)
		}
	}
	return out
}

// Explain returns a human-readable explanation of the first failing term,
// or "" when the selector matches.
func (s Selector) Explain(labels map[string]string) string {
	for _, t := range s.terms {
		val, exists := labels[t.key]
		switch t.op {
		case "exists":
			if !exists {
				return fmt.Sprintf("no %s label", t.key)
			}
		case "notexists":
			if exists {
				return fmt.Sprintf("has %s label", t.key)
			}
		case "=":
			if !exists {
				return fmt.Sprintf("no %s label, wanted %s=%s", t.key, t.key, t.values[0])
			}
			if val != t.values[0] {
				return fmt.Sprintf("%s=%s, wanted %s=%s", t.key, val, t.key, t.values[0])
			}
		case "!=":
			if exists && val == t.values[0] {
				return fmt.Sprintf("%s=%s, wanted %s!=%s", t.key, val, t.key, t.values[0])
			}
		case "in":
			if !exists {
				return fmt.Sprintf("no %s label, wanted %s in (%s)", t.key, t.key, strings.Join(t.values, ","))
			}
			found := false
			for _, v := range t.values {
				if val == v {
					found = true
					break
				}
			}
			if !found {
				return fmt.Sprintf("%s=%s, wanted %s in (%s)", t.key, val, t.key, strings.Join(t.values, ","))
			}
		case "notin":
			if !exists {
				continue
			}
			for _, v := range t.values {
				if val == v {
					return fmt.Sprintf("%s=%s, wanted %s notin (%s)", t.key, val, t.key, strings.Join(t.values, ","))
				}
			}
		}
	}
	return ""
}

// String returns the selector's text representation.
func (s Selector) String() string {
	if len(s.terms) == 0 {
		return ""
	}
	parts := make([]string, len(s.terms))
	for i, t := range s.terms {
		switch t.op {
		case "exists":
			parts[i] = t.key
		case "notexists":
			parts[i] = "!" + t.key
		case "=":
			parts[i] = t.key + "=" + t.values[0]
		case "!=":
			parts[i] = t.key + "!=" + t.values[0]
		case "in":
			parts[i] = t.key + " in (" + strings.Join(t.values, ",") + ")"
		case "notin":
			parts[i] = t.key + " notin (" + strings.Join(t.values, ",") + ")"
		}
	}
	return strings.Join(parts, ",")
}
