package integration_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// TestSubagentLineagePersistence verifies that spawns with parentChildId
// correctly record the lineage and that it persists through store reads.
func TestSubagentLineagePersistence(t *testing.T) {
	d := bootDaemon(t)
	client := d.control(t)

	// Spawn two top-level siblings.
	topA := d.spawnChild(t)
	topB := d.spawnChild(t)

	// Spawn a child under topA with ParentChildId set.
	kid := d.spawnChildUnder(t, topA)

	// Verify the child appears in the list with its parent.
	found := make(map[string]bool)
	for _, s := range listChildren(t, client) {
		found[s.GetChildId()] = true
	}
	for _, want := range []string{topA, topB, kid} {
		if !found[want] {
			t.Errorf("child %s not found in list: %v", want, found)
		}
	}

	// Verify topA's child has the parent label set.
	snap := getChild(t, client, kid)
	parent, hasParent := snap.GetLabels()["rafiki/parent"]
	if !hasParent || parent != topA {
		t.Errorf("want rafiki/parent=%s, got %s (labels: %v)", topA, parent, snap.GetLabels())
	}

	// Kill topA and its children.
	kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := client.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: topA})); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}
	kcancel()

	// Wait for the child to exit too.
	time.Sleep(500 * time.Millisecond)

	// Clean up sibling.
	bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, _ = client.Kill(bctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: topB}))
	bcancel()
}
