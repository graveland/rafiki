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
func (s *fakePyModuleStore) Get(_ context.Context, repo, name string) (pymodules.Record, error) {
	if s.getErr != nil {
		return pymodules.Record{}, s.getErr
	}
	return s.getRec, nil
}

// A successful Get returns the module's full text: a header naming the
// module, its version and creation stamp, its description, and the exact
// code.
func TestPymoduleGetReturnsCodeAndVersion(t *testing.T) {
	fixed := time.Date(2025, 3, 1, 12, 30, 0, 0, time.UTC)
	store := &fakePyModuleStore{getRec: pymodules.Record{
		ID: 7, Name: "util", Code: "def util(): pass", Description: "d", CreatedAt: fixed,
	}}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"util"}`))
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
func TestPymoduleGetNotFound(t *testing.T) {
	store := &fakePyModuleStore{getErr: pymodules.ErrNotFound}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"nope"}`))
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
func TestPymoduleGetInvalidName(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"9bad"}`))
	if err == nil {
		t.Fatal("Execute(9bad) = nil error, want a validation error")
	}
	if !strings.Contains(err.Error(), "bare Python identifier") {
		t.Errorf("Execute error = %v, want it to mention %q", err, "bare Python identifier")
	}
}

// A non-"local" repo names a git source, which this operation can never
// target (serving a git-sourced get needs an executor round-trip pymodule_run
// already has and get does not): a clear tool-level error, raised before the
// store is touched -- and an omitted repo is the same rejection, since
// "required" is enforced here and not by the schema.
func TestPymoduleGetRejectsNonLocalRepo(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, input := range []string{
		`{"repo":"ops-tools","name":"util"}`,
		`{"name":"util"}`,
	} {
		res, err := tool.Execute(context.Background(), ToolInput(input))
		if err == nil {
			t.Errorf("Execute(%s) = nil error, want the repo rejection", input)
			continue
		}
		if res.Text != "" {
			t.Errorf("Execute(%s) result text = %q, want empty on error", input, res.Text)
		}
		if !strings.Contains(err.Error(), `repo must be "local"`) {
			t.Errorf("Execute(%s) error = %v, want it to name the only legal repo value", input, err)
		}
	}
}

// {"repo":"local"} behaves exactly as it did before the repo parameter
// existed: the regression guard for the required-field change.
func TestPymoduleGetAcceptsLocalRepo(t *testing.T) {
	fixed := time.Date(2025, 3, 1, 12, 30, 0, 0, time.UTC)
	store := &fakePyModuleStore{getRec: pymodules.Record{
		ID: 7, Name: "util", Code: "def util(): pass", Description: "d", CreatedAt: fixed,
	}}
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{PyModules: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"util"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{`"util"`, "version 7", "def util(): pass"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("result text %q does not contain %q", res.Text, want)
		}
	}
}

// With no pymodule store configured the blueprint declines: it never
// registers as a tool that can only fail.
func TestPymoduleGetDeclinedWithoutStore(t *testing.T) {
	tool, err := PyModuleGetBlueprint{}.Materialize(ToolOpts{})
	if tool != nil || err != nil {
		t.Errorf("Materialize with nil PyModules = (%v, %v), want (nil, nil)", tool, err)
	}
}
