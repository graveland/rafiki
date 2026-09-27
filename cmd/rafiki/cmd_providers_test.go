// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestEmitProviderBansJSONLOmitsUnboundedExpiry pins the JSON shape: a ban
// that lasts until lifted has no expires_at, rather than a zero or epoch time
// a script would read as "already expired".
func TestEmitProviderBansJSONLOmitsUnboundedExpiry(t *testing.T) {
	c := assert.NewCollecting(t)
	exp := int64(2000)
	bans := []*rafikiv1.ProviderBan{
		{Provider: "open-inference", ModelLine: "*", Reason: "operator", CreatedAt: 1000, Note: "spinning"},
		{Provider: "CoreWeave", ModelLine: "deepseek/deepseek-v4-pro", Reason: "no_cache", CreatedAt: 1000, ExpiresAt: &exp},
	}
	var buf bytes.Buffer
	c.Require().NoError(emitProviderBans(&buf, bans, outputJSONL, false, time.Unix(1500, 0)))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	c.Require().Len(lines, 2, "got %d lines, want 2:\n%s", len(lines), buf.String())
	c.NotStrContains(lines[0], "expires_at", "unbounded ban carries expires_at")
	c.StrContains(lines[1], `"expires_at"`, "bounded ejection lacks expires_at")
}

func TestEmitProviderBansTable(t *testing.T) {
	c := assert.NewCollecting(t)
	bans := []*rafikiv1.ProviderBan{
		{Provider: "open-inference", ModelLine: "*", Reason: "operator", CreatedAt: 1000},
	}
	var buf bytes.Buffer
	c.Require().NoError(emitProviderBans(&buf, bans, outputAuto, false, time.Unix(1500, 0)))
	for _, want := range []string{"open-inference", "all", "operator", "when lifted"} {
		c.StrContains(buf.String(), want, "table missing")
	}
}
