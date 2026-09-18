// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// Delete is the delete half of fakePyModuleStore (the fake's struct and its
// Put half live in pymodule_put_test.go; both tools share it). It records
// every name Delete was called with and reports delErr when configured.
// Records nothing when delErr is set -- an erroring fake models a failed call.
func (s *fakePyModuleStore) Delete(_ context.Context, name string) error {
	if s.delErr != nil {
		return s.delErr
	}
	s.deletes = append(s.deletes, name)
	return nil
}

// An invalid name must be rejected by the tool's own validation, before the
// store is touched at all.
func TestPymoduleDeleteRejectsInvalidName(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{"name":"bad/name"}`)); err == nil {
		t.Error("Execute(bad/name) = nil error, want a validation error")
	}
	if len(store.deletes) != 0 {
		t.Errorf("store Delete calls = %v, want none: an invalid name must be rejected before the store is touched", store.deletes)
	}
}

// A valid name goes to the store verbatim and the result reports the deletion.
func TestPymoduleDeleteCallsStoreAndReports(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"chart_helpers"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(store.deletes) != 1 || store.deletes[0] != "chart_helpers" {
		t.Errorf("store Delete calls = %v, want exactly [chart_helpers]", store.deletes)
	}
	if !strings.Contains(res.Text, "deleted") {
		t.Errorf("result text = %q, want it to contain %q", res.Text, "deleted")
	}
}

// A not-found from the store is a user-facing "no such module" error, not an
// internal failure: the message must name the missing module.
func TestPymoduleDeleteReportsNotFound(t *testing.T) {
	store := &fakePyModuleStore{delErr: pymodules.ErrNotFound}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"name":"gone"}`))
	if err == nil {
		t.Fatal("Execute(gone) = nil error, want a not-found error")
	}
	if !strings.Contains(err.Error(), `no module named "gone"`) {
		t.Errorf("Execute error = %v, want it to contain %q", err, `no module named "gone"`)
	}
}

// With no pymodule store configured the blueprint declines: it never
// registers as a tool that can only fail.
func TestPymoduleDeleteMaterializeDeclinesWithoutStore(t *testing.T) {
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{})
	if tool != nil || err != nil {
		t.Errorf("Materialize with nil PyModules = (%v, %v), want (nil, nil)", tool, err)
	}
}
