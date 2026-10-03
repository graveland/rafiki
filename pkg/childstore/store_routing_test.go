// SPDX-License-Identifier: Apache-2.0

package childstore_test

import (
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"

	"github.com/multigres/testkit/assert"
)

// TestStoreSetRoutingRoundTripsThroughSnapshot pins the persistence half of
// steering: a spec written with SetRouting survives the record round trip the
// daemon uses on restart (RecordFromSnapshot -> SessionFromRecord), and an
// unknown id is ErrNotFound rather than a silent no-op.
func TestStoreSetRoutingRoundTripsThroughSnapshot(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "worker", "/x"))

	const spec = "sort=price,prefer=fireworks,nodata"
	c.NoError(s.SetRouting("c_1", spec), "SetRouting")

	snap, ok := s.Get("c_1")
	c.True(ok, "missing after SetRouting")
	c.Eq(spec, snap.Routing, "snapshot Routing")

	rec := childstore.RecordFromSnapshot(snap)
	c.Eq(spec, rec.Config.Routing, "record Config.Routing")

	back := childstore.SessionFromRecord(rec)
	c.Eq(spec, back.Routing, "Routing after SessionFromRecord")

	c.Eq(childstore.ErrNotFound, s.SetRouting("c_missing", spec), "SetRouting on an unknown id")
	c.True(errors.Is(s.SetRouting("c_missing", spec), childstore.ErrNotFound), "unknown id Is ErrNotFound")
}
