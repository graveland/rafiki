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

func TestKillKeepsNoForgetAsADeprecatedFlagAlias(t *testing.T) {
	cmd := newKillCmd()
	if cmd.Flags().Lookup("no-close") == nil {
		t.Error("--no-close missing")
	}
	f := cmd.Flags().Lookup("no-forget")
	if f == nil {
		t.Fatal("--no-forget must survive as a deprecated alias")
	}
	if f.Deprecated == "" {
		t.Error("--no-forget should be marked deprecated so --help stops advertising it")
	}
}
