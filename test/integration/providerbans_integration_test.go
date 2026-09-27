// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestCLI_ProviderBanRoundTrip drives ban → bans → unban through the real
// client over the local socket, which admits the anonymous caller as the
// operator. The slug is unique per run because the suite shares one database:
// a leftover ban would be rehydrated by every later daemon.
func TestCLI_ProviderBanRoundTrip(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d := bootDaemon(t)
	slug := "it-banned-" + time.Now().Format("150405.000000")

	out, err := cliCmd(t, d, "providers", "ban", slug, "--note", "spinning").CombinedOutput()
	c.Require().NoError(err, "providers ban failed: %v\noutput: %s", err, out)
	t.Cleanup(func() { _ = cliCmd(t, d, "providers", "unban", slug).Run() })
	c.Require().StrContains(string(out), "banned "+slug+" from all models until lifted", "ban output = %q", out)

	listed := func() map[string]any {
		t.Helper()
		out, err := cliCmd(t, d, "providers", "bans", "-J").Output()
		c.Require().NoError(err, "providers bans failed")
		for line := range bytes.Lines(out) {
			var row map[string]any
			c.Require().NoError(json.Unmarshal(line, &row), "bans -J line %q", line)
			if row["provider"] == slug {
				return row
			}
		}
		return nil
	}
	row := listed()
	c.Require().NotNil(row, "%s missing from providers bans", slug)
	c.False(row["reason"] != "operator" || row["model_line"] != "*" || row["note"] != "spinning", "listed ban = %v", row)
	_, ok := row["expires_at"]
	c.False(ok, "an unbounded ban lists expires_at: %v", row)

	if out, err := cliCmd(t, d, "providers", "unban", slug).CombinedOutput(); err != nil {
		t.Fatalf("providers unban failed: %v\noutput: %s", err, out)
	}
	c.Nil(listed(), "%s still listed after unban", slug)
	c.Error(cliCmd(t, d, "providers", "unban", slug).Run(), "a second unban succeeded; want not-found")
}
