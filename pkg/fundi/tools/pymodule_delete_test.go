// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/pymodules"

	"github.com/multigres/testkit/assert"
)

// Delete is the delete half of fakePyModuleStore (the fake's struct and its
// Put half live in pymodule_put_test.go; both tools share it). It records
// every repo/name Delete was called with and reports delErr when configured.
// Records nothing when delErr is set -- an erroring fake models a failed call.
// delNotice is the advisory notice the fake returns alongside success.
func (s *fakePyModuleStore) Delete(_ context.Context, repo, name string) (string, error) {
	if s.delErr != nil {
		return "", s.delErr
	}
	s.deletes = append(s.deletes, [2]string{repo, name})
	return s.delNotice, nil
}

// An invalid name must be rejected by the tool's own validation, before the
// store is touched at all.
func TestPymoduleDeleteRejectsInvalidName(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	if _, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"bad/name"}`)); err == nil {
		t.Error("Execute(bad/name) = nil error, want a validation error")
	}
	c.Empty(store.deletes, "store Delete calls")
}

// A valid name goes to the store verbatim and the result reports the deletion.
func TestPymoduleDeleteCallsStoreAndReports(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers"}`))
	c.Require().NoError(err, "Execute")
	if len(store.deletes) != 1 || store.deletes[0] != [2]string{"local", "chart_helpers"} {
		t.Errorf("store Delete calls = %v, want exactly [{local chart_helpers}]", store.deletes)
	}
	c.StrContains(res.Text, "deleted", "result text")
}

// A non-empty delete notice must ride the result text, after the deleted
// confirmation -- same surfacing rule as pymodule_put's.
func TestPymoduleDeleteIncludesNoticeInResult(t *testing.T) {
	c := assert.NewCollecting(t)
	const notice = "dependency install failed on chart_helpers: boom"
	store := &fakePyModuleStore{delNotice: notice}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers"}`))
	c.Require().NoError(err, "Execute")
	c.StrContains(res.Text, "deleted", "result text")
	c.StrContains(res.Text, notice, "result text")
}

// A not-found from the store is a user-facing "no such module" error, not an
// internal failure: the message must name the missing module.
func TestPymoduleDeleteReportsNotFound(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{delErr: pymodules.ErrNotFound}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	_, err = tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"gone"}`))
	c.Require().Error(err, "Execute(gone) = nil error, want a not-found error")
	c.StrContains(err.Error(), `no module named "gone"`, "Execute error = %v, want it to contain", err)
}

// A non-"local" repo names a git source, which this operation can never
// target: a clear tool-level error, raised before the store is touched --
// and an omitted repo is the same rejection, since "required" is enforced
// here and not by the schema.
func TestPymoduleDeleteRejectsNonLocalRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	for _, input := range []string{
		`{"repo":"ops-tools","name":"chart_helpers"}`,
		`{"name":"chart_helpers"}`,
	} {
		res, err := tool.Execute(context.Background(), ToolInput(input))
		if err == nil {
			t.Errorf("Execute(%s) = nil error, want the repo rejection", input)
			continue
		}
		c.Eq("", res.Text, "Execute(%s) result text = %q, want empty on error", input, res.Text)
		c.StrContains(err.Error(), `repo must be "local"`, "Execute(%s) error = %v, want it to name the only legal repo value", input, err)
	}
	c.Empty(store.deletes, "store Delete calls")
}

// {"repo":"local"} behaves exactly as it did before the repo parameter
// existed: the regression guard for the required-field change.
func TestPymoduleDeleteAcceptsLocalRepo(t *testing.T) {
	store := &fakePyModuleStore{}
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{PyModules: store})
	assert.NewAborting(t).NoError(err, "Materialize")
	if _, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(store.deletes) != 1 || store.deletes[0] != [2]string{"local", "chart_helpers"} {
		t.Errorf("store Delete calls = %v, want exactly [{local chart_helpers}]", store.deletes)
	}
}

// With no pymodule store configured the blueprint declines: it never
// registers as a tool that can only fail.
func TestPymoduleDeleteMaterializeDeclinesWithoutStore(t *testing.T) {
	tool, err := PyModuleDeleteBlueprint{}.Materialize(ToolOpts{})
	assert.NewCollecting(t).False(tool != nil || err != nil, "Materialize with nil PyModules = (%v, %v), want (nil, nil)", tool, err)
}
