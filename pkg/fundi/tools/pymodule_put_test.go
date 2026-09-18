// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakePyModuleStore records every Put call so a test can assert the tool
// called the store (or, for a rejected input, that it did not).
type fakePyModuleStore struct {
	puts   [][3]string // name, code, description, in call order
	nextID int64
	putErr error
}

func (s *fakePyModuleStore) Put(_ context.Context, name, code, description string) (int64, error) {
	if s.putErr != nil {
		return 0, s.putErr
	}
	s.puts = append(s.puts, [3]string{name, code, description})
	s.nextID++
	return s.nextID, nil
}

func TestPymodulePutSavesValidInputToStore(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool == nil {
		t.Fatal("Materialize returned nil tool with a non-nil store")
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"chart_helpers","code":"def chart(): pass","description":"chart helpers"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(store.puts) != 1 {
		t.Fatalf("store Put calls = %d, want 1", len(store.puts))
	}
	if got := store.puts[0]; got[0] != "chart_helpers" || got[1] != "def chart(): pass" || got[2] != "chart helpers" {
		t.Errorf("store got name/code/description = %q/%q/%q, want the tool input verbatim", got[0], got[1], got[2])
	}
	want := `saved "chart_helpers" as version 1`
	if res.Text != want {
		t.Errorf("result text = %q, want %q", res.Text, want)
	}
}

func TestPymodulePutRejectsInvalidNameWithoutCallingStore(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, name := range []string{"foo/bar", "", "1foo", "my-module", strings.Repeat("_", 65)} {
		input := `{"name":"` + name + `","code":"x = 1"}`
		res, err := tool.Execute(context.Background(), ToolInput(input))
		if err == nil {
			t.Errorf("Execute(name=%q) = nil error, want a validation error", name)
		}
		if res.Text != "" {
			t.Errorf("Execute(name=%q) result text = %q, want empty on error", name, res.Text)
		}
	}
	if len(store.puts) != 0 {
		t.Errorf("store Put calls = %d, want 0: an invalid name must be rejected before the store is touched", len(store.puts))
	}
}

func TestPymodulePutRejectsEmptyCode(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{"name":"chart_helpers","code":""}`)); err == nil {
		t.Error("Execute with empty code = nil error, want an error")
	}
	if len(store.puts) != 0 {
		t.Errorf("store Put calls = %d, want 0: empty code must never reach the store", len(store.puts))
	}
}

func TestPymodulePutPropagatesStoreError(t *testing.T) {
	store := &fakePyModuleStore{putErr: errors.New("db down")}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{"name":"chart_helpers","code":"x = 1"}`)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("Execute error = %v, want the store error wrapped through", err)
	}
}

func TestPymodulePutMaterializeDeclinesWithoutStore(t *testing.T) {
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{})
	if tool != nil || err != nil {
		t.Errorf("Materialize with nil PyModules = (%v, %v), want (nil, nil)", tool, err)
	}
}
