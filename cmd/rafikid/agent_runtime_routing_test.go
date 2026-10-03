// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// routingSourceController builds the minimal Controller liveRoutingSource needs.
func routingSourceController() *Controller {
	return &Controller{st: childstore.New(), cm: newChildManager()}
}

// TestRoutingSourceFallsBackToSpawnSpecWhenRowMissing pins the fallback: when
// the child's row is gone the closure returns the spawn-time spec rather than
// the zero spec.
func TestRoutingSourceFallsBackToSpawnSpecWhenRowMissing(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingSourceController()
	want, err := routing.ParseSpec("sort=price")
	ck.NoError(err, "ParseSpec spawn spec")

	src := c.liveRoutingSource("c_missing", "sort=price")
	ck.EqDeep(want, src(), "a missing row must fall back to the spawn-time spec")
}

// TestRoutingSourceReflectsStoreUpdate pins the point of the whole feature: an
// operator's steering write (childstore.Store.SetRouting, which
// Controller.SetChildRouting drives) is visible on the NEXT read, not frozen at
// spawn.
func TestRoutingSourceReflectsStoreUpdate(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingSourceController()
	c.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusIdle, Kind: protocol.KindFundi, StartedAt: time.Now(),
	})
	ck.NoError(c.st.SetRouting("c1", "sort=price,nodata"), "seed stored spec")

	src := c.liveRoutingSource("c1", "sort=price")
	first, err := routing.ParseSpec("sort=price,nodata")
	ck.NoError(err, "ParseSpec first")
	ck.EqDeep(first, src(), "the closure must read the stored spec")

	ck.NoError(c.st.SetRouting("c1", "sort=throughput"), "steer")
	second, err := routing.ParseSpec("sort=throughput")
	ck.NoError(err, "ParseSpec second")
	ck.EqDeep(second, src(), "after a steer the closure must return the new spec")
}

// TestRoutingSourceConcurrentReadsAndWritesAreRaceFree runs the closure's cache
// against a concurrent steering writer under -race: eight readers vs a writer
// calling SetRouting.
func TestRoutingSourceConcurrentReadsAndWritesAreRaceFree(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingSourceController()
	c.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusIdle, Kind: protocol.KindFundi, StartedAt: time.Now(),
	})
	ck.NoError(c.st.SetRouting("c1", "sort=price"), "seed stored spec")

	src := c.liveRoutingSource("c1", "sort=price")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = src()
				}
			}
		}()
	}

	for i := range 200 {
		spec := "sort=price"
		if i%2 == 1 {
			spec = "sort=throughput"
		}
		if err := c.st.SetRouting("c1", spec); err != nil {
			t.Fatalf("SetRouting: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	want, err := routing.ParseSpec("sort=throughput")
	ck.NoError(err, "ParseSpec final")
	ck.EqDeep(want, src(), "the final read must reflect the last write")
}
