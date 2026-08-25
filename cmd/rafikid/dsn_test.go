// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestRequireDSNPrefersExplicitFlag(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://from-env/db")
	got, err := requireDSN("postgres://from-flag/db")
	if err != nil {
		t.Fatalf("requireDSN returned error: %v", err)
	}
	if got != "postgres://from-flag/db" {
		t.Fatalf("want the flag value, got %q", got)
	}
}

func TestRequireDSNFallsBackToEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://from-env/db")
	got, err := requireDSN("")
	if err != nil {
		t.Fatalf("requireDSN returned error: %v", err)
	}
	if got != "postgres://from-env/db" {
		t.Fatalf("want the env value, got %q", got)
	}
}

// The daemon requires a database (Phase C design 2.1). An absent DSN is a
// startup error, not a degraded mode: without it there is no history, no cost
// accounting, no task ledger, no users, and no executor plane.
func TestRequireDSNEmptyIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	_, err := requireDSN("")
	if err == nil {
		t.Fatal("want an error when no DSN is configured, got nil")
	}
	// The message must tell the operator how to fix it.
	if !strings.Contains(err.Error(), "RAFIKI_DB") {
		t.Fatalf("error must name RAFIKI_DB, got: %v", err)
	}
	if !strings.Contains(err.Error(), "timescale/timescaledb") {
		t.Fatalf("error must name the documented docker image, got: %v", err)
	}
}
