// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// Get is the get half of fakePyModuleStore (the fake's struct and its Put
// half live in pymodule_put_test.go; both tools share it). It returns the
// canned record, or getErr when configured -- an erroring fake models a
// failed call.
func (s *fakePyModuleStore) Get(_ context.Context, name string) (pymodules.Record, error) {
	if s.getErr != nil {
		return pymodules.Record{}, s.getErr
	}
	return s.getRec, nil
}

// A successful Get returns the module's full text: a header naming the
// module, its version and creation stamp, its description, and the exact
// code.
func TestPyModuleGetReturnsCodeAndVersion(t *testing.T) {
	fixed := time.Date(2025, 3, 1, 12, 30, 0, 0, time.UTC)
	store := &fakePyModuleStore{getRec: pymodules.Record{
		ID: 7, Name: "util", Code: "def util(): pass", Description: "d", CreatedAt: fixed,
	}}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"util"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{`"util"`, "version 7", fixed.UTC().Format(time.RFC3339), "description: d", "def util(): pass"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("result text %q does not contain %q", res.Text, want)
		}
	}
}

// A not-found from the store is a user-facing "no such module" error, not an
// internal failure: the message must name the missing module.
func TestPyModuleGetNotFound(t *testing.T) {
	store := &fakePyModuleStore{getErr: pymodules.ErrNotFound}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"name":"nope"}`))
	if err == nil {
		t.Fatal("Execute(nope) = nil error, want a not-found error")
	}
	want := `pymodule_get: no module named "nope" in your store`
	if err.Error() != want {
		t.Errorf("Execute error = %q, want exactly %q", err.Error(), want)
	}
}

// An invalid name must be rejected by the tool's own validation, before the
// store is touched at all.
func TestPyModuleGetInvalidName(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"name":"9bad"}`))
	if err == nil {
		t.Fatal("Execute(9bad) = nil error, want a validation error")
	}
	if !strings.Contains(err.Error(), "bare Python identifier") {
		t.Errorf("Execute error = %v, want it to mention %q", err, "bare Python identifier")
	}
}

// With no pymodule store configured the blueprint declines: it never
// registers as a tool that can only fail.
func TestPyModuleGetDeclinedWithoutStore(t *testing.T) {
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{})
	if tool != nil || err != nil {
		t.Errorf("Materialize with nil PyModules = (%v, %v), want (nil, nil)", tool, err)
	}
}
