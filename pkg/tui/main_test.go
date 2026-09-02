// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"testing"
)

// TestMain isolates the client state directory for the whole package.
//
// NewCockpit reads the remembered model query at construction, so without this
// every cockpit test reads -- and every one that closes the query panel WRITES
// -- the developer's real ~/.local/state/rafiki/client-state.json. That is bad
// twice over: a test clobbers a real preference, and one test's saved query
// leaks into the next test's fresh cockpit, which is exactly how
// TestSpaceCyclesASortCellThroughOffAscDesc started failing on a sort key it
// never set.
//
// Package-wide rather than per-test: the coupling is in the CONSTRUCTOR, so
// any test that builds a Cockpit is exposed whether or not it thinks about
// state, including every test written from here on.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rafiki-tui-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
