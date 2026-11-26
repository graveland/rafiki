// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestChildSummaryCarriesParentChildID(t *testing.T) {
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_parent", Kind: protocol.KindFundi})
	st.Insert(&childstore.Session{
		ChildID: "c_child", Kind: protocol.KindFundi,
		Labels: map[string]string{childstore.LabelParent: "c_parent"},
	})
	// A row carrying only the pre-rename key (which migration 0076 rewrites)
	// reports no parent: the summary reads the one canonical label, so a stale
	// spelling can never forge a lineage edge.
	st.Insert(&childstore.Session{
		ChildID: "c_legacy", Kind: protocol.KindFundi,
		Labels: map[string]string{"fundi/parent": "c_parent"},
	})
	c := &Controller{st: st}

	want := map[string]string{"c_parent": "", "c_child": "c_parent", "c_legacy": ""}

	t.Run("GetChild", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		for id, parent := range want {
			sum, ok := c.GetChild(id)
			ck.Require().True(ok, "GetChild(%s) ok", id)
			ck.Eq(parent, sum.ParentChildID, "GetChild(%s).ParentChildID", id)
		}
	})

	t.Run("ListChildren", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		got := map[string]string{}
		for _, s := range c.ListChildren(nil) {
			got[s.ChildID] = s.ParentChildID
		}
		ck.EqDeep(want, got, "ParentChildID by child")
	})
}

// TestChildSummaryCarriesWorkspaceTools pins the session→summary half of the
// mapping the cockpit's model switch reconstructs a spawn from: the mode is
// resolved once at spawn and persisted on the session, so the summary must
// report the STORED value (never a re-resolution) or a continuation silently
// reverts a rafiki-mode claude child to its built-in tools.
func TestChildSummaryCarriesWorkspaceTools(t *testing.T) {
	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: "c_ws", Kind: protocol.KindClaude,
		WorkspaceTools: protocol.WorkspaceToolsRafiki,
	})
	st.Insert(&childstore.Session{ChildID: "c_native", Kind: protocol.KindClaude})
	c := &Controller{st: st}

	ck := assert.NewCollecting(t)
	got, ok := c.GetChild("c_ws")
	ck.Require().True(ok, "GetChild(c_ws) ok")
	ck.Eq(protocol.WorkspaceToolsRafiki, got.WorkspaceTools, "GetChild(c_ws).WorkspaceTools")

	byID := map[string]string{}
	for _, s := range c.ListChildren(nil) {
		byID[s.ChildID] = s.WorkspaceTools
	}
	ck.Eq(protocol.WorkspaceToolsRafiki, byID["c_ws"], "ListChildren c_ws workspace tools")
	ck.Eq("", byID["c_native"], "a native child reports no mode")
}
