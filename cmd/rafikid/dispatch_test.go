package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// main() dispatches these on os.Args[1] before any daemon startup. Each is a
// separate process mode and must not fall through into the daemon's own
// flag parsing.
func TestSubcommandsAreDispatchable(t *testing.T) {
	for _, name := range []string{protocol.KindFundi, "agent", "migrate"} {
		if !isSubcommand(name) {
			t.Errorf("%q should be dispatched as a subcommand, not fall through to daemon startup", name)
		}
	}
	for _, name := range []string{"", "--dev", "-config"} {
		if isSubcommand(name) {
			t.Errorf("%q is a daemon flag or empty, not a subcommand", name)
		}
	}
}
