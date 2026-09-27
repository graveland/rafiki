// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestRequireDSNPrefersExplicitFlag(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://from-env/db")
	c := assert.NewAborting(t)
	got, err := requireDSN("postgres://from-flag/db")
	c.NoError(err, "requireDSN returned error")
	c.Eq("postgres://from-flag/db", got, "want the flag value, got")
}

func TestRequireDSNFallsBackToEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://from-env/db")
	c := assert.NewAborting(t)
	got, err := requireDSN("")
	c.NoError(err, "requireDSN returned error")
	c.Eq("postgres://from-env/db", got, "want the env value, got")
}

// The daemon requires a database (Phase C design 2.1). An absent DSN is a
// startup error, not a degraded mode: without it there is no history, no cost
// accounting, no task ledger, no users, and no executor plane.
func TestRequireDSNEmptyIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	c := assert.NewAborting(t)
	_, err := requireDSN("")
	c.Error(err, "want an error when no DSN is configured, got nil")
	// The message must tell the operator how to fix it.
	c.StrContains(err.Error(), "RAFIKI_DB", "error must name RAFIKI_DB, got: %v", err)
	c.StrContains(err.Error(), "timescale/timescaledb", "error must name the documented docker image, got: %v", err)
}
