package tasks

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestStatusTerminal(t *testing.T) {
	c := assert.NewCollecting(t)
	// StatusOrphaned must NOT be terminal — it is reassignable by design.
	c.False(StatusOrphaned.Terminal(), "orphaned must not be terminal; it exists to be reassigned")
	c.True(StatusDropped.Terminal(), "dropped must be terminal")
	c.False(StatusPending.Terminal() || StatusBlocked.Terminal(), "pending and blocked are not terminal")
	c.False(Status("nonsense").Valid(), "unknown status must be invalid")
}
