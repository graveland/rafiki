// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

// TestMain isolates the client state directory for the whole package.
//
// buildSpawnRequest consults the remembered model, so without this the tests
// read the developer's real ~/.local/state/rafiki/client-state.json -- and a
// model remembered from ordinary use then leaks into a test's expected
// precedence. That is not hypothetical: it made TestPreset_MergeOrder fail
// with a model no fixture mentions.
//
// Package-wide, because the coupling is inside the request builder rather than
// in any one test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rafiki-cli-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
