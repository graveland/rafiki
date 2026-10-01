// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestDescendantIDsAreDeepestFirstAndSkipNatives pins the order a cascade ends
// children in (each before its parent) and that synthetic thread children,
// which end with their parent on their own, are not in the list.
func TestDescendantIDsAreDeepestFirstAndSkipNatives(t *testing.T) {
	c := assert.NewAborting(t)
	st := childstore.New()
	ins := func(id, parent, root string, native bool) {
		labels := map[string]string{}
		if parent != "" {
			labels[childstore.LabelParent] = parent
			labels[childstore.LabelRoot] = root
		}
		st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(),
			Labels: labels, Native: native,
		})
	}
	ins("a", "", "", false)
	ins("b", "a", "a", false)
	ins("c", "b", "a", false)
	ins("d", "a", "a", false)
	ins("t", "a", "a", true)
	ins("z", "", "", false)

	ctrl := &Controller{st: st}
	c.EqDeep([]string{"c", "b", "d"}, ctrl.DescendantIDs("a"), "a's descendants")
	c.EqDeep([]string{"c"}, ctrl.DescendantIDs("b"), "b's descendants")
	c.Eq(0, len(ctrl.DescendantIDs("z")), "a childless child")
}
