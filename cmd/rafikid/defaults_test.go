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
	const wantShutdown = 180 * time.Second
	const wantKill = 30 * time.Second

	c.Eq(wantShutdown, durOrDefault(0, wantShutdown), "shutdownTimeout default: got")
	c.Eq(wantKill, durOrDefault(0, wantKill), "killTimeout default: got")
}

// TestDurOrDefault verifies that explicit non-zero values are honoured.
func TestDurOrDefault(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(time.Second, durOrDefault(time.Second, 99*time.Second), "durOrDefault(1s): got")
	c.Eq(5*time.Second, durOrDefault(-1, 5*time.Second), "durOrDefault(-1): got")
}
