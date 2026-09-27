// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// TestMCPPyModuleListerAdapter tests that the adapter correctly translates
// pymodules.Record to tools.PyModuleInfo.
func TestMCPPyModuleListerAdapter(t *testing.T) {
	c := assert.NewAborting(t)
	f := newPymoduleFixture()
	// fakePymoduleStore is initialized in newPymoduleFixture with alice's data
	f.store.rows["u-alice"] = []pymodules.Record{
		{
			ID:          1,
			OwnerUserID: "u-alice",
			Name:        "helper",
			Description: "a helper",
		},
	}

	ctrl := &Controller{pymoduleStore: f.store}
	lister := newMCPPyModuleLister(ctrl, users.Identity{UserID: "u-alice"})

	infos, err := lister.List(context.Background(), "")
	c.NoError(err, "List")
	c.Len(infos, 1, "List returned %d infos, want 1", len(infos))
	if infos[0].Name != "helper" || infos[0].Description != "a helper" {
		t.Errorf("List returned %+v, want {Name:\"helper\", Description:\"a helper\"}", infos[0])
	}
}

// TestMCPPyModuleListBlueprintDeclines tests that the blueprint declines to
// materialize when opts.PyModuleList is nil.
func TestMCPPyModuleListBlueprintDeclines(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := mcpPyModuleListBlueprint{}
	tool, err := bp.Materialize(tools.ToolOpts{})
	c.Require().NoError(err, "Materialize")
	c.Nil(tool, "Materialize returned non-nil tool, want nil")
}

// stubLister is a minimal tools.PyModuleLister for testing. It ignores the
// repo filter -- the filtering lives in the real adapter, and the render
// tests below only need entries to come back.
type stubLister struct {
	infos []tools.PyModuleInfo
}

func (s *stubLister) List(context.Context, string) ([]tools.PyModuleInfo, error) {
	return s.infos, nil
}

// TestMCPPyModuleListToolRendersEmptyInventory tests that an empty inventory
// produces the expected message.
func TestMCPPyModuleListToolRendersEmptyInventory(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := mcpPyModuleListBlueprint{}
	lister := &stubLister{infos: nil}
	tool, err := bp.Materialize(tools.ToolOpts{PyModuleList: lister})
	c.Require().NoError(err, "Materialize")
	c.Require().NotNil(tool, "Materialize returned nil tool")

	result, err := tool.Execute(context.Background(), tools.ToolInput{})
	c.Require().NoError(err, "Execute")

	want := "No pymodules saved yet. Use pymodule_put to save one."
	c.Eq(want, result.Text, "Execute returned")
}

// TestMCPPyModuleListToolRendersEntries tests that multiple entries are
// rendered with one per line in the format "name — description\n".
func TestMCPPyModuleListToolRendersEntries(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := mcpPyModuleListBlueprint{}
	lister := &stubLister{
		infos: []tools.PyModuleInfo{
			{Name: "a", Description: "b"},
			{Name: "c", Description: "d"},
		},
	}
	tool, err := bp.Materialize(tools.ToolOpts{PyModuleList: lister})
	c.Require().NoError(err, "Materialize")
	c.Require().NotNil(tool, "Materialize returned nil tool")

	result, err := tool.Execute(context.Background(), tools.ToolInput{})
	c.Require().NoError(err, "Execute")

	want := "a — b\nc — d\n"
	c.Eq(want, result.Text, "Execute returned")
}
