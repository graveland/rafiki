// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestCreateNotifyFlagReachesSpawnRequest pins --notify onto both the client
// request and the Connect message built from it; the value is not validated
// client-side (the daemon is the authority).
func TestCreateNotifyFlagReachesSpawnRequest(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("notify", "result"))
	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "buildSpawnRequest")
	c.Eq("result", req.Notify, "request Notify")
	c.Eq("result", connectSpawnRequest(req).GetNotify(), "wire Notify")

	bare, err := buildSpawnRequest(newTestCreateCmd(), nil)
	c.Require().NoError(err, "buildSpawnRequest (no flag)")
	c.Eq("", bare.Notify, "unset flag stays empty")
}
