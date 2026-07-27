package main

import (
	"testing"
	"time"
)

// TestKillTimeoutDefaults verifies that durOrDefault returns the spec §6.5
// values when the caller passes 0 for both timeout arguments.
func TestKillTimeoutDefaults(t *testing.T) {
	const wantShutdown = 180 * time.Second
	const wantKill = 30 * time.Second

	if got := durOrDefault(0, wantShutdown); got != wantShutdown {
		t.Fatalf("shutdownTimeout default: got %v, want %v", got, wantShutdown)
	}
	if got := durOrDefault(0, wantKill); got != wantKill {
		t.Fatalf("killTimeout default: got %v, want %v", got, wantKill)
	}
}

// TestDurOrDefault verifies that explicit non-zero values are honoured.
func TestDurOrDefault(t *testing.T) {
	if got := durOrDefault(1000, 99*time.Second); got != time.Second {
		t.Fatalf("durOrDefault(1000ms): got %v, want 1s", got)
	}
	if got := durOrDefault(-1, 5*time.Second); got != 5*time.Second {
		t.Fatalf("durOrDefault(-1): got %v, want default", got)
	}
}
