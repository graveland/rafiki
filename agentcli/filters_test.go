// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"testing"
	"time"

	"github.com/timescale/rafiki/insights"
)

func TestParseTime(t *testing.T) {
	if ts, err := ParseTime(""); err != nil || ts != nil {
		t.Fatalf(`ParseTime("") = %v, %v; want nil, nil`, ts, err)
	}
	ts, err := ParseTime("2026-07-01T00:00:00Z")
	if err != nil || ts == nil || ts.Year() != 2026 {
		t.Fatalf("RFC3339 parse failed: %v, %v", ts, err)
	}
	ts, err = ParseTime("24h")
	if err != nil || ts == nil || time.Since(*ts) < 23*time.Hour {
		t.Fatalf("duration parse failed: %v, %v", ts, err)
	}
	if _, err := ParseTime("24 hours"); err == nil {
		t.Fatal("unparseable time must error, not silently drop the bound")
	}
}

func TestBindSearchFilter(t *testing.T) {
	f, err := BindSearchFilter(FilterVals{Owner: "alice", Path: "proxy", MinTokens: 500, Limit: 7, Since: "24h", Entrypoint: "proxy", ExcludeEntrypoint: "analyze"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Owner != "alice" || f.MinTokens != 500 || f.Limit != 7 || f.Since == nil {
		t.Fatalf("fields lost: %+v", f)
	}
	if f.Path != insights.PathProxy {
		t.Fatalf("path = %q, want proxy", f.Path)
	}
	if f.Entrypoint != "proxy" || f.ExcludeEntrypoint != "analyze" {
		t.Fatalf("entrypoint fields lost: Entrypoint=%q, ExcludeEntrypoint=%q", f.Entrypoint, f.ExcludeEntrypoint)
	}
	if _, err := BindSearchFilter(FilterVals{Path: "client"}); err == nil {
		t.Fatal("raw driven_by value must be rejected before the DB call")
	}
}

func TestBindStatsFilter(t *testing.T) {
	f, err := BindStatsFilter(FilterVals{Persona: "team-platform-default", Model: "claude-haiku-4-5", Until: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Persona == "" || f.Model == "" || f.Until == nil {
		t.Fatalf("fields lost: %+v", f)
	}
}
