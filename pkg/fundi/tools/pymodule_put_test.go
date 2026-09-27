// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/pymodules"

	"github.com/multigres/testkit/assert"
)

// fakePyModuleStore records every Put call so a test can assert the tool
// called the store (or, for a rejected input, that it did not). putNotice
// and delNotice are the advisory notices the fake returns, so a test can
// drive the tool's notice-surfacing without a real store.
type fakePyModuleStore struct {
	puts      [][4]string // repo, name, code, description, in call order
	deletes   [][2]string // repo, name Delete was called with, in call order
	getRec    pymodules.Record
	getErr    error
	nextID    int64
	putErr    error
	delErr    error
	putNotice string
	delNotice string
}

func (s *fakePyModuleStore) Put(_ context.Context, repo, name, code, description string) (int64, string, error) {
	if s.putErr != nil {
		return 0, "", s.putErr
	}
	s.puts = append(s.puts, [4]string{repo, name, code, description})
	s.nextID++
	return s.nextID, s.putNotice, nil
}

func TestPymodulePutSavesValidInputToStore(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	c.Require().NotNil(tool, "Materialize returned nil tool with a non-nil store")
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers","code":"def chart(): pass","description":"chart helpers"}`))
	c.Require().NoError(err, "Execute")
	c.Require().Len(store.puts, 1, "store Put calls = %d, want 1", len(store.puts))
	if got := store.puts[0]; got[0] != "local" || got[1] != "chart_helpers" || got[2] != "def chart(): pass" || got[3] != "chart helpers" {
		t.Errorf("store got repo/name/code/description = %q/%q/%q/%q, want the tool input verbatim", got[0], got[1], got[2], got[3])
	}
	want := `saved "chart_helpers" as version 1`
	c.Eq(want, res.Text, "result text")
}

func TestPymodulePutRejectsInvalidNameWithoutCallingStore(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	for _, name := range []string{"foo/bar", "", "1foo", "my-module", strings.Repeat("_", 65)} {
		input := `{"repo":"local","name":"` + name + `","code":"x = 1"}`
		res, err := tool.Execute(context.Background(), ToolInput(input))
		c.Error(err, "Execute(name=%q) = nil error, want a validation error", name)
		c.Eq("", res.Text, "Execute(name=%q) result text = %q, want empty on error", name, res.Text)
	}
	c.Empty(store.puts, "store Put calls = %d, want 0: an invalid name must be rejected before the store is touched", len(store.puts))
}

func TestPymodulePutRejectsEmptyCode(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	if _, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers","code":""}`)); err == nil {
		t.Error("Execute with empty code = nil error, want an error")
	}
	c.Empty(store.puts, "store Put calls = %d, want 0: empty code must never reach the store", len(store.puts))
}

func TestPymodulePutPropagatesStoreError(t *testing.T) {
	store := &fakePyModuleStore{putErr: errors.New("db down")}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	assert.NewAborting(t).NoError(err, "Materialize")
	if _, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers","code":"x = 1"}`)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("Execute error = %v, want the store error wrapped through", err)
	}
}

// A non-"local" repo names a git source, which this operation can never
// target: a clear tool-level error, raised before the store is touched --
// and an omitted repo is the same rejection, since "required" is enforced
// here and not by the schema.
func TestPymodulePutRejectsNonLocalRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	for _, input := range []string{
		`{"repo":"ops-tools","name":"chart_helpers","code":"x = 1"}`,
		`{"name":"chart_helpers","code":"x = 1"}`,
	} {
		res, err := tool.Execute(context.Background(), ToolInput(input))
		if err == nil {
			t.Errorf("Execute(%s) = nil error, want the repo rejection", input)
			continue
		}
		c.Eq("", res.Text, "Execute(%s) result text = %q, want empty on error", input, res.Text)
		c.StrContains(err.Error(), `repo must be "local"`, "Execute(%s) error = %v, want it to name the only legal repo value", input, err)
	}
	c.Empty(store.puts, "store Put calls = %d, want 0: a non-local repo must be rejected before the store is touched", len(store.puts))
}

// {"repo":"local"} behaves exactly as it did before the repo parameter
// existed: the regression guard for the required-field change.
func TestPymodulePutAcceptsLocalRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers","code":"x = 1"}`))
	c.Require().NoError(err, "Execute")
	if len(store.puts) != 1 || store.puts[0][0] != "local" || store.puts[0][1] != "chart_helpers" {
		t.Errorf("store Put calls = %v, want exactly [local chart_helpers]", store.puts)
	}
	want := `saved "chart_helpers" as version 1`
	c.Eq(want, res.Text, "result text")
}

// A non-empty notice from the store must ride the result text, after the
// saved confirmation -- advisory findings are useless if the agent never
// sees them.
func TestPymodulePutIncludesNoticeInResult(t *testing.T) {
	c := assert.NewCollecting(t)
	const notice = "ruff found:\nfake.py:1:1: F401 'os' imported but unused"
	store := &fakePyModuleStore{putNotice: notice}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers","code":"import os\n"}`))
	c.Require().NoError(err, "Execute")
	c.StrContains(res.Text, `saved "chart_helpers" as version 1`, "result text")
	c.StrContains(res.Text, notice, "result text")
}

// An empty notice means nothing to report: the result is exactly the plain
// saved-as-version form, with no trailing blank block.
func TestPymodulePutOmitsNoticeWhenEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakePyModuleStore{putNotice: ""}
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{PyModules: store})
	c.Require().NoError(err, "Materialize")
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo":"local","name":"chart_helpers","code":"x = 1"}`))
	c.Require().NoError(err, "Execute")
	want := `saved "chart_helpers" as version 1`
	c.Eq(want, res.Text, "result text")
}

func TestPymodulePutMaterializeDeclinesWithoutStore(t *testing.T) {
	tool, err := PyModulePutBlueprint{}.Materialize(ToolOpts{})
	assert.NewCollecting(t).False(tool != nil || err != nil, "Materialize with nil PyModules = (%v, %v), want (nil, nil)", tool, err)
}

// The put description is the only place an agent learns the marker syntax,
// on every face the tool is served from, so it must name it.
func TestPymodulePutDescriptionMentionsRequirementsMarker(t *testing.T) {
	for _, want := range []string{pymodules.RequirementsMarker, "per-module venv", "pymodule_run"} {
		assert.NewCollecting(t).StrContains(pymodulePutDescription, want, "pymodule_put description should mention")
	}
}
