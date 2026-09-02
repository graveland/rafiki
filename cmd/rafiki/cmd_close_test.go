// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestCloseCommandKeepsForgetAsAnAlias(t *testing.T) {
	cmd := newCloseCmd()
	if cmd.Name() != "close" {
		t.Errorf("Name() = %q, want close", cmd.Name())
	}
	var hasForget, hasRM bool
	for _, a := range cmd.Aliases {
		switch a {
		case "forget":
			hasForget = true
		case "rm":
			hasRM = true
		}
	}
	if !hasForget {
		t.Error("`forget` must stay an alias: it is in muscle memory and in scripts")
	}
	if !hasRM {
		t.Error("`rm` was an alias before the rename and must survive it")
	}
}

func TestCloseCmd_HasStopTimeoutFlags(t *testing.T) {
	cmd := newCloseCmd()
	if cmd.Flags().Lookup("shutdown-timeout") == nil {
		t.Error("--shutdown-timeout missing: close must be able to override the stop-first step's timeout")
	}
	if cmd.Flags().Lookup("kill-timeout") == nil {
		t.Error("--kill-timeout missing: close must be able to override the stop-first step's timeout")
	}
}
