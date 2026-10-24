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
	st.Insert(&childstore.Session{
		ChildID: "c_legacy", Kind: protocol.KindFundi,
		Labels: map[string]string{"fundi/parent": "c_parent"},
	})
	c := &Controller{st: st}

	want := map[string]string{"c_parent": "", "c_child": "c_parent", "c_legacy": "c_parent"}

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
