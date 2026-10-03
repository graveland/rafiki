// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// routingTree builds the tree the steering tests address:
//
//	c_root
//	 └── c_mid   (the caller)
//	      └── c_leaf   (a descendant the caller may steer)
//	c_other      (another top-level tree, not in c_mid's subtree)
func routingTree(t *testing.T) *Controller {
	t.Helper()
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	insert := func(id, parent, root string) {
		labels := map[string]string{}
		if parent != "" {
			labels[childstore.LabelParent] = parent
			labels[childstore.LabelRoot] = root
		}
		c.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, Kind: protocol.KindFundi,
			StartedAt: time.Now(), Labels: labels,
		})
	}
	insert("c_root", "", "")
	insert("c_mid", "c_root", "c_root")
	insert("c_leaf", "c_mid", "c_root")
	insert("c_other", "", "")
	return c
}

func setStoredRouting(t *testing.T, c *Controller, id, spec string) {
	t.Helper()
	if err := c.st.SetRouting(id, spec); err != nil {
		t.Fatalf("seed routing for %s: %v", id, err)
	}
}

func storedRouting(t *testing.T, c *Controller, id string) string {
	t.Helper()
	snap, ok := c.st.Get(id)
	if !ok {
		t.Fatalf("no snapshot for %s", id)
	}
	return snap.Routing
}

func TestSetChildRoutingOperatorMergesDeltaOverStored(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_leaf", "sort=price,nodata")

	got, err := c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "prefer=fireworks")
	ck.NoError(err, "SetChildRoutingAsOperator")
	ck.Eq("sort=price,prefer=fireworks,nodata", got, "merged spec")
	ck.Eq(got, storedRouting(t, c, "c_leaf"), "stored spec")
}

func TestSetChildRoutingOperatorMaySetOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)

	got, err := c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "only=deepinfra")
	ck.NoError(err, "operator may set only=")
	ck.Eq("only=deepinfra", got, "merged spec")
	ck.Eq("only=deepinfra", storedRouting(t, c, "c_leaf"), "stored spec")
}

func TestSetChildRoutingChildMayNotSetOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_leaf", "prefer=fireworks")

	_, err := c.SetChildRouting(context.Background(), "c_mid", "c_leaf", "only=deepinfra")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "child only= refusal")
	ck.Eq("prefer=fireworks", storedRouting(t, c, "c_leaf"), "stored spec unchanged")
}

func TestSetChildRoutingChildMayNotChangeStoredOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_leaf", "only=deepinfra")

	_, err := c.SetChildRouting(context.Background(), "c_mid", "c_leaf", "prefer=fireworks")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "child may not change an operator only=")
	ck.Eq("only=deepinfra", storedRouting(t, c, "c_leaf"), "stored spec unchanged")
}

func TestSetChildRoutingChildPreservesStoredNoDataAndZDR(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_leaf", "nodata,zdr")

	got, err := c.SetChildRouting(context.Background(), "c_mid", "c_leaf", "prefer=fireworks")
	ck.NoError(err, "SetChildRouting")
	ck.Eq("prefer=fireworks,nodata,zdr", got, "nodata/zdr are monotone and the grammar cannot un-set them")
}

func TestSetChildRoutingChildMayPreferSortQuant(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_leaf", "nodata")

	got, err := c.SetChildRouting(context.Background(), "c_mid", "c_leaf", "sort=throughput,quant=fp8+,prefer=fireworks")
	ck.NoError(err, "child may set prefer/sort/quant")
	ck.Eq("sort=throughput,quant=fp8+,prefer=fireworks,nodata", got, "merged spec")
	ck.Eq(got, storedRouting(t, c, "c_leaf"), "stored spec")
}

func TestSetChildRoutingChildOutsideSubtreeIsDenied(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	setStoredRouting(t, c, "c_other", "nodata")

	_, err := c.SetChildRouting(context.Background(), "c_mid", "c_other", "prefer=fireworks")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "child steering outside its subtree")
	ck.Eq("nodata", storedRouting(t, c, "c_other"), "stored spec unchanged")

	// A child is not a descendant of itself: the same guard refuses self-steer.
	_, err = c.SetChildRouting(context.Background(), "c_mid", "c_mid", "prefer=fireworks")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(connectapi.ConnectErr(err)), "child steering itself")
}

func TestSetChildRoutingUnknownChildIsNotFound(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)

	_, err := c.SetChildRoutingAsOperator(context.Background(), "c_ghost", "prefer=fireworks")
	ck.Eq(connect.CodeNotFound, connect.CodeOf(connectapi.ConnectErr(err)), "unknown child")
	var ce *connectapi.ControllerError
	ck.Require().True(errors.As(err, &ce), "want a ControllerError, got %T", err)
	ck.Eq("agent c_ghost is not registered", ce.Message, "message")
}

func TestSetChildRoutingBadDeltaIsInvalidArgument(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)

	_, err := c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "bogus=1")
	ck.Eq(connect.CodeInvalidArgument, connect.CodeOf(connectapi.ConnectErr(err)), "unparseable delta")
}

// routingGuardWithDirectory builds the daemon's provider guard around a test
// directory, the seam canonicalizeRoutingSlugs resolves through.
func routingGuardWithDirectory(t *testing.T, client *http.Client, url string) *routing.ProviderGuard {
	t.Helper()
	g := routing.NewProviderGuard(0, slog.New(slog.DiscardHandler))
	g.SetDirectory(routing.NewProviderDirectoryForTest(client, url, slog.New(slog.DiscardHandler)))
	return g
}

func TestSetChildRoutingUnknownSlugIsRejectedBeforePersisting(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"OpenInference","slug":"open-inference"}]}`))
	}))
	t.Cleanup(srv.Close)
	c.SetProviderGuard(routingGuardWithDirectory(t, srv.Client(), srv.URL))
	setStoredRouting(t, c, "c_leaf", "nodata")

	_, err := c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "prefer=bogus")
	ck.Eq(connect.CodeInvalidArgument, connect.CodeOf(connectapi.ConnectErr(err)), "unknown provider slug")
	ck.Eq("nodata", storedRouting(t, c, "c_leaf"), "stored spec unchanged after refusal")
}

func TestSetChildRoutingDegradedDirectoryAcceptsSlug(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c.SetProviderGuard(routingGuardWithDirectory(t, srv.Client(), srv.URL))

	got, err := c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "prefer=whatever")
	ck.NoError(err, "a degraded directory must accept the slug")
	ck.Eq("prefer=whatever", got, "merged spec")
}

// TestSetChildRoutingStoresCanonicalSlugNotDisplayName pins N5: a display-name
// prefer must be persisted as the ProviderDirectory's canonical slug, not the
// raw string OpenRouter would silently ignore.
func TestSetChildRoutingStoresCanonicalSlugNotDisplayName(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"Fireworks AI","slug":"fireworks"}]}`))
	}))
	t.Cleanup(srv.Close)
	c.SetProviderGuard(routingGuardWithDirectory(t, srv.Client(), srv.URL))

	got, err := c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "prefer=Fireworks AI")
	ck.NoError(err, "a display name resolves through the directory")
	ck.Eq("prefer=fireworks", got, "canonical slug returned")
	ck.Eq("prefer=fireworks", storedRouting(t, c, "c_leaf"), "canonical slug stored, not the raw display name")
}

// TestSetChildRoutingChildCanonicalizesSlug pins that the child path resolves
// slugs too, not just the operator path.
func TestSetChildRoutingChildCanonicalizesSlug(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"Fireworks AI","slug":"fireworks"}]}`))
	}))
	t.Cleanup(srv.Close)
	c.SetProviderGuard(routingGuardWithDirectory(t, srv.Client(), srv.URL))
	setStoredRouting(t, c, "c_leaf", "nodata")

	got, err := c.SetChildRouting(context.Background(), "c_mid", "c_leaf", "prefer=Fireworks AI")
	ck.NoError(err, "child steer resolves slugs too")
	ck.Eq("prefer=fireworks,nodata", got, "canonical slug merged over stored")
}

// TestSetChildRoutingOperatorFlagSurvivesRacingChildSteer is the N1
// regression: the child steer's read-modify-write must run inside
// childstore.Store.Update, so an operator write of the monotone `nodata` flag
// racing a child merge can never be read-then-dropped. A lost update needs the
// child write to land AFTER an operator `nodata` write, having read the stored
// spec BEFORE it; racing one operator/child pair per round (with the spec reset
// between rounds) gives each pair the widest window to interleave that way, and
// asserting the flag after EVERY round makes the loss observable -- a later
// operator write would otherwise re-add it. This detects the two-operation
// steer (it lost 114/3000 rounds in the review); the in-lock steer loses none.
// Run under -race.
func TestSetChildRoutingOperatorFlagSurvivesRacingChildSteer(t *testing.T) {
	ck := assert.NewAborting(t)
	c := routingTree(t)

	const rounds = 3000
	losses := 0
	for round := 0; round < rounds; round++ {
		setStoredRouting(t, c, "c_leaf", "")

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = c.SetChildRoutingAsOperator(context.Background(), "c_leaf", "nodata")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = c.SetChildRouting(context.Background(), "c_mid", "c_leaf", "prefer=deepinfra")
		}()
		close(start)
		wg.Wait()

		stored := storedRouting(t, c, "c_leaf")
		spec, err := routing.ParseSpec(stored)
		ck.NoError(err, "round %d: stored spec %q parses", round, stored)
		if !spec.NoData {
			losses++
		}
	}
	ck.Eq(0, losses, "operator nodata lost in %d/%d rounds: the steer read-modify-write is not atomic", losses, rounds)
}

// TestControlPolicySetRoutingIsChildScoped pins the gate classification: a
// missing or wrong entry is what the completeness test catches, but the policy
// itself (childScoped, not userOnly) is what lets a child steer at all.
func TestControlPolicySetRoutingIsChildScoped(t *testing.T) {
	ck := assert.NewAborting(t)
	ck.Eq(policyChildScoped, controlPolicyTable["SetRouting"], "controlPolicyTable[SetRouting]")
}
