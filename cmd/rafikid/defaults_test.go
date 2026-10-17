package main

import (
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestKillTimeoutDefaults verifies that durOrDefault returns the spec §6.5
// values when the caller passes 0 for both timeout arguments.
func TestKillTimeoutDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(2*time.Second, durOrDefault(0, defaultKillShutdown), "shutdownTimeout default: got")
	c.Eq(2500*time.Millisecond, durOrDefault(0, defaultKillTimeout), "killTimeout default: got")
}

// TestDurOrDefault verifies that explicit non-zero values are honoured.
func TestDurOrDefault(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(time.Second, durOrDefault(time.Second, 99*time.Second), "durOrDefault(1s): got")
	c.Eq(5*time.Second, durOrDefault(-1, 5*time.Second), "durOrDefault(-1): got")
}
