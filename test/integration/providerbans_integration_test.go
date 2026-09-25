// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCLI_ProviderBanRoundTrip drives ban → bans → unban through the real
// client over the local socket, which admits the anonymous caller as the
// operator. The slug is unique per run because the suite shares one database:
// a leftover ban would be rehydrated by every later daemon.
func TestCLI_ProviderBanRoundTrip(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)
	slug := "it-banned-" + time.Now().Format("150405.000000")

	out, err := cliCmd(t, d, "providers", "ban", slug, "--note", "spinning").CombinedOutput()
	if err != nil {
		t.Fatalf("providers ban failed: %v\noutput: %s", err, out)
	}
	t.Cleanup(func() { _ = cliCmd(t, d, "providers", "unban", slug).Run() })
	if !strings.Contains(string(out), "banned "+slug+" from all models until lifted") {
		t.Fatalf("ban output = %q", out)
	}

	listed := func() map[string]any {
		t.Helper()
		out, err := cliCmd(t, d, "providers", "bans", "-J").Output()
		if err != nil {
			t.Fatalf("providers bans failed: %v", err)
		}
		for line := range bytes.Lines(out) {
			var row map[string]any
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatalf("bans -J line %q: %v", line, err)
			}
			if row["provider"] == slug {
				return row
			}
		}
		return nil
	}
	row := listed()
	if row == nil {
		t.Fatalf("%s missing from providers bans", slug)
	}
	if row["reason"] != "operator" || row["model_line"] != "*" || row["note"] != "spinning" {
		t.Errorf("listed ban = %v", row)
	}
	if _, ok := row["expires_at"]; ok {
		t.Errorf("an unbounded ban lists expires_at: %v", row)
	}

	if out, err := cliCmd(t, d, "providers", "unban", slug).CombinedOutput(); err != nil {
		t.Fatalf("providers unban failed: %v\noutput: %s", err, out)
	}
	if row := listed(); row != nil {
		t.Errorf("%s still listed after unban: %v", slug, row)
	}
	if err := cliCmd(t, d, "providers", "unban", slug).Run(); err == nil {
		t.Error("a second unban succeeded; want not-found")
	}
}
