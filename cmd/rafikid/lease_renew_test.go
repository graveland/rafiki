package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/store"
)

// TestRenewThenKillOrdersAndParallelises pins both halves of the fix: every
// lease is renewed before any child is killed, and the kills run concurrently.
// A takeover invalidates leases in bulk, so a serial kill loop can outlast the
// TTL of the leases it has not renewed yet.
func TestRenewThenKillOrdersAndParallelises(t *testing.T) {
	var mu sync.Mutex
	var order []string

	lost := func(childID string) {
		mu.Lock()
		order = append(order, "kill:"+childID)
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	renew := func(childID string) (bool, error) {
		mu.Lock()
		order = append(order, "renew:"+childID)
		mu.Unlock()
		return false, nil
	}

	held := map[string]store.Lease{
		"c1": {ConversationID: "v1", Token: "t1"},
		"c2": {ConversationID: "v2", Token: "t2"},
		"c3": {ConversationID: "v3", Token: "t3"},
	}

	start := time.Now()
	renewThenKill(held, renew, lost)
	elapsed := time.Since(start)

	mu.Lock()
	defer mu.Unlock()

	if len(order) != 6 {
		t.Fatalf("recorded %d events, want 6: %v", len(order), order)
	}
	for i, ev := range order[:3] {
		if !strings.HasPrefix(ev, "renew:") {
			t.Errorf("event %d = %q, want a renew — every renewal must precede every kill", i, ev)
		}
	}
	if elapsed > 120*time.Millisecond {
		t.Errorf("took %v; three 50ms kills appear to be serialized", elapsed)
	}
}
