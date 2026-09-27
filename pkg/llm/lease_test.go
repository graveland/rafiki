package llm

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestSetLeaseIgnoresStorelessClient: a client with no pool has no stores, so
// SetLease must be a no-op rather than a nil dereference.
func TestSetLeaseIgnoresStorelessClient(t *testing.T) {
	c := &Client{}
	c.SetLease(store.Lease{ConversationID: "conv", Holder: "d", Token: "t"})
	assert.NewCollecting(t).False(c.lease.Held(), "a store-less client recorded a lease it cannot enforce")
}

// TestSetLeaseIsNoOpForZeroLease pins the unfenced path: the proxy face and
// every client-driven conversation run without a lease and must keep working.
func TestSetLeaseIsNoOpForZeroLease(t *testing.T) {
	c := &Client{}
	c.SetLease(store.Lease{})
	assert.NewCollecting(t).False(c.lease.Held(), "a zero lease was recorded as held")
}
