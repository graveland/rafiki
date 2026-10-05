// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"

	"github.com/multigres/testkit/assert"
)

// fakeRootedLineageSource is a lineage source that can also resolve a subtree
// from a caller-supplied root, standing in for childstoredb.Store's optional
// RootedLineageSource capability.
type fakeRootedLineageSource struct {
	fakeLineageSource
	gotRoot string
}

func (f *fakeRootedLineageSource) LineageWithRoot(_ context.Context, ancestorChildID, rootFallback string) ([]childstore.LineageMember, error) {
	f.gotAncestor = ancestorChildID
	f.gotRoot = rootFallback
	return f.members, f.err
}

// When the ancestor's own row is missing from the database but the live store
// still knows it, subtreeSelector must pass the ancestor's live root down so the
// subtree query still runs and a closed descendant stays in scope.
//
// Fails against the pre-change subtreeSelector, which called Lineage with no
// root fallback: a missing ancestor row returned (nil, nil), so gotRoot is empty.
func TestSubtreeSelectorUsesLiveRootWhenTheAncestorRowIsMissing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3, 3) // c_d0 -> c_d1 -> c_d2
	c.lineage = &fakeRootedLineageSource{fakeLineageSource: fakeLineageSource{
		members: []childstore.LineageMember{{ChildID: "c_d2", SessionID: "s-closed", Closed: true}},
	}}

	sel, err := c.subtreeSelector(context.Background(), "c_d1")
	ck.Require().NoError(err, "selector")

	rooted := c.lineage.(*fakeRootedLineageSource)
	ck.Eq("c_d1", rooted.gotAncestor, "the ancestor asked about")
	ck.Eq("c_d0", rooted.gotRoot, "a live-but-unpersisted ancestor's root must be passed as the fallback")
	ck.Contains(sel.ConversationIDs, "s-closed", "the closed descendant stays in scope via the fallback root; ids=%v", sel.ConversationIDs)
}
