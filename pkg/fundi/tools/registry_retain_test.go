package tools

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// TestRegistryRetainToolAllowlist covers Registry.Retain, the primitive the
// fundi runtime's built-in tool allowlist applies through: a keep list of
// registered-and-unknown names retains only the registered ones, reports the
// unknown ones sorted, leaves the dropped tools unreachable through Execute,
// and a nil keep empties the registry entirely.
func TestRegistryRetainToolAllowlist(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"a", "b", "c"} {
		r.Register(&testEchoTool{name: name, desc: "test " + name, schema: Schema{Type: "object"}})
	}

	missing := r.Retain([]string{"a", "zzz"})
	if !slices.Equal(missing, []string{"zzz"}) {
		t.Fatalf("Retain([a zzz]) missing = %v, want [zzz]", missing)
	}

	var names []string
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names = append(names, def.OfTool.Name)
		}
	}
	if !slices.Equal(names, []string{"a"}) {
		t.Fatalf("Definitions after Retain([a zzz]) = %v, want [a]", names)
	}

	// The dropped tool must be gone from the executable face too, not merely
	// hidden from the definitions list.
	if _, err := r.Execute(context.Background(), "b", json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute(b) after Retain returned nil error; the dropped tool is still registered")
	}
	if _, err := r.Execute(context.Background(), "a", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute(a) after Retain: %v", err)
	}

	// A nil keep drops everything.
	if missing := r.Retain(nil); missing != nil {
		t.Errorf("Retain(nil) missing = %v, want nil", missing)
	}
	if got := r.Definitions(); len(got) != 0 {
		t.Fatalf("Retain(nil) left %d tools registered, want 0: %v", len(got), toolNames(got))
	}
}
