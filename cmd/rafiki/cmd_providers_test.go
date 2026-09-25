// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// TestEmitProviderBansJSONLOmitsUnboundedExpiry pins the JSON shape: a ban
// that lasts until lifted has no expires_at, rather than a zero or epoch time
// a script would read as "already expired".
func TestEmitProviderBansJSONLOmitsUnboundedExpiry(t *testing.T) {
	exp := int64(2000)
	bans := []*rafikiv1.ProviderBan{
		{Provider: "open-inference", ModelLine: "*", Reason: "operator", CreatedAt: 1000, Note: "spinning"},
		{Provider: "CoreWeave", ModelLine: "deepseek/deepseek-v4-pro", Reason: "no_cache", CreatedAt: 1000, ExpiresAt: &exp},
	}
	var buf bytes.Buffer
	if err := emitProviderBans(&buf, bans, outputJSONL, false, time.Unix(1500, 0)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	if strings.Contains(lines[0], "expires_at") {
		t.Errorf("unbounded ban carries expires_at: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"expires_at"`) {
		t.Errorf("bounded ejection lacks expires_at: %s", lines[1])
	}
}

func TestEmitProviderBansTable(t *testing.T) {
	bans := []*rafikiv1.ProviderBan{
		{Provider: "open-inference", ModelLine: "*", Reason: "operator", CreatedAt: 1000},
	}
	var buf bytes.Buffer
	if err := emitProviderBans(&buf, bans, outputAuto, false, time.Unix(1500, 0)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"open-inference", "all", "operator", "when lifted"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("table missing %q:\n%s", want, buf.String())
		}
	}
}
