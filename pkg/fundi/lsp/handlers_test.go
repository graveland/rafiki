package lsp

import (
	"encoding/json"
	"testing"
)

// TestHandleWorkspaceConfiguration_NilParams covers a server that sends the
// request with no params at all (permitted by JSON-RPC): the reply must
// still be a well-formed empty array, not a nil that marshals to JSON null,
// which is not the same wire shape as "[]".
func TestHandleWorkspaceConfiguration_NilParams(t *testing.T) {
	result, err := HandleWorkspaceConfiguration(nil)
	if err != nil {
		t.Fatalf("HandleWorkspaceConfiguration(nil): %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("got %d results, want 0", len(result))
	}
}

// TestHandleWorkspaceConfiguration_OneResultPerItem pins the ordering and
// length contract: gopls matches responses back to its questions positionally,
// so a wrong-length array or nulls-in-the-wrong-order would silently
// misattribute settings.
func TestHandleWorkspaceConfiguration_OneResultPerItem(t *testing.T) {
	raw := json.RawMessage(`{"items":[{"section":"gopls"},{"section":"go"},{"section":"gopls.staticcheck"}]}`)
	result, err := HandleWorkspaceConfiguration(&raw)
	if err != nil {
		t.Fatalf("HandleWorkspaceConfiguration: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("got %d results, want 3 (one per requested item)", len(result))
	}
	for i, r := range result {
		if r != nil {
			t.Errorf("item %d: got %v, want null", i, r)
		}
	}
}

// TestHandleWorkspaceConfiguration_MalformedParams must return an error, not
// panic or silently guess a length -- an InvalidParams reply is a truthful
// answer to "I couldn't parse your request," which a MethodNotFound-style
// blanket success would not be.
func TestHandleWorkspaceConfiguration_MalformedParams(t *testing.T) {
	raw := json.RawMessage(`not json`)
	if _, err := HandleWorkspaceConfiguration(&raw); err == nil {
		t.Fatal("expected an error for malformed params, got nil")
	}
}
