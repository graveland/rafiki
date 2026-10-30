// SPDX-License-Identifier: Apache-2.0

package convtest

import (
	"context"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
)

// Counting wraps a Backend and counts the calls it receives, so a test can
// assert that a read issued no backend traffic at all.
type Counting struct {
	backend conv.Backend
	mu      sync.Mutex
	counts  map[string]int
}

// NewCounting returns a counting wrapper around b.
func NewCounting(b conv.Backend) *Counting {
	return &Counting{backend: b, counts: make(map[string]int)}
}

// Counts returns a copy of the per-method call counters, keyed by method name.
func (c *Counting) Counts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// Reset zeroes every counter.
func (c *Counting) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = make(map[string]int)
}

func (c *Counting) record(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[method]++
}

// State counts and delegates.
func (c *Counting) State(ctx context.Context, convID string) (conv.State, error) {
	c.record("State")
	return c.backend.State(ctx, convID)
}

// Walk counts and delegates.
func (c *Counting) Walk(ctx context.Context, convID, fromID string, crossRoots bool) ([]conv.Ref, error) {
	c.record("Walk")
	return c.backend.Walk(ctx, convID, fromID, crossRoots)
}

// Fetch counts and delegates.
func (c *Counting) Fetch(ctx context.Context, convID string, ids []string) ([]*conv.Node, error) {
	c.record("Fetch")
	return c.backend.Fetch(ctx, convID, ids)
}

// Lookup counts and delegates.
func (c *Counting) Lookup(ctx context.Context, convID string, hashes []convtree.Hash) (map[convtree.Hash]conv.Ref, error) {
	c.record("Lookup")
	return c.backend.Lookup(ctx, convID, hashes)
}

// Roots counts and delegates.
func (c *Counting) Roots(ctx context.Context, convID string) ([]*conv.Node, error) {
	c.record("Roots")
	return c.backend.Roots(ctx, convID)
}

// ByProviderMessageID counts and delegates.
func (c *Counting) ByProviderMessageID(ctx context.Context, convID, providerMessageID string) (*conv.Node, bool, error) {
	c.record("ByProviderMessageID")
	return c.backend.ByProviderMessageID(ctx, convID, providerMessageID)
}

// HasChild counts and delegates.
func (c *Counting) HasChild(ctx context.Context, convID, parentID string) (bool, error) {
	c.record("HasChild")
	return c.backend.HasChild(ctx, convID, parentID)
}

// Commit counts and delegates.
func (c *Counting) Commit(ctx context.Context, commit conv.Commit) (time.Time, error) {
	c.record("Commit")
	return c.backend.Commit(ctx, commit)
}
