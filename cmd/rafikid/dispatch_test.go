package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// cobra recognises these as the direct subcommands of the rafikid root.
// Every known subcommand must be present, and no daemon flag must be mistaken
// for one — otherwise `rafikid --dev` would try to dispatch to a non-existent
// subcommand instead of starting the daemon.
func TestSubcommandsAreRegistered(t *testing.T) {
	root := newRootCmd()

	want := map[string]bool{protocol.KindFundi: true, "agent": true, "migrate": true}
	for _, c := range root.Commands() {
		if !want[c.Name()] {
			t.Errorf("unexpected subcommand %q", c.Name())
		}
		delete(want, c.Name())
	}
	for name := range want {
		t.Errorf("missing subcommand %q", name)
	}
}
