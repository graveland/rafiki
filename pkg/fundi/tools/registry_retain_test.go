package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestRegistryRetainToolAllowlist covers Registry.Retain, the primitive the
// fundi runtime's built-in tool allowlist applies through: a keep list of
// registered-and-unknown names retains only the registered ones, reports the
// unknown ones sorted, leaves the dropped tools unreachable through Execute,
// and a nil keep empties the registry entirely.
func TestRegistryRetainToolAllowlist(t *testing.T) {
	c := assert.NewCollecting(t)
	r := NewRegistry()
	for _, name := range []string{"a", "b", "c"} {
		r.Register(&testEchoTool{name: name, desc: "test " + name, schema: Schema{Type: "object"}})
	}

	missing := r.Retain([]string{"a", "zzz"})
	c.Require().EqDiff([]string{"zzz"}, missing, "Retain([a zzz]) missing")

	var names []string
	for _, def := range r.Definitions() {
		if def.OfTool != nil {
			names = append(names, def.OfTool.Name)
		}
	}
	c.Require().EqDiff([]string{"a"}, names, "Definitions after Retain([a zzz])")

	// The dropped tool must be gone from the executable face too, not merely
	// hidden from the definitions list.
	if _, err := r.Execute(context.Background(), "b", json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute(b) after Retain returned nil error; the dropped tool is still registered")
	}
	_, err := r.Execute(context.Background(), "a", json.RawMessage(`{}`))
	c.Require().NoError(err, "Execute(a) after Retain")

	// A nil keep drops everything.
	c.Nil(r.Retain(nil), "Retain(nil) missing")
	got := r.Definitions()
	c.Require().Empty(got, "Retain(nil) left %d tools registered, want 0: %v", len(got), toolNames(got))
}
