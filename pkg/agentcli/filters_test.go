// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/insights"

	"github.com/multigres/testkit/assert"
)

func TestParseTime(t *testing.T) {
	c := assert.NewAborting(t)
	if ts, err := ParseTime(""); err != nil || ts != nil {
		t.Fatalf(`ParseTime("") = %v, %v; want nil, nil`, ts, err)
	}
	ts, err := ParseTime("2026-07-01T00:00:00Z")
	c.False(err != nil || ts == nil || ts.Year() != 2026, "RFC3339 parse failed: %v, %v", ts, err)
	ts, err = ParseTime("24h")
	c.False(err != nil || ts == nil || time.Since(*ts) < 23*time.Hour, "duration parse failed: %v, %v", ts, err)
	if _, err := ParseTime("24 hours"); err == nil {
		t.Fatal("unparseable time must error, not silently drop the bound")
	}
}

func TestBindSearchFilter(t *testing.T) {
	c := assert.NewAborting(t)
	f, err := BindSearchFilter(FilterVals{Owner: "alice", Path: "proxy", MinTokens: 500, Limit: 7, Since: "24h", Entrypoint: "proxy", ExcludeEntrypoint: "analyze"})
	c.NoError(err)
	c.False(f.Owner != "alice" || f.MinTokens != 500 || f.Limit != 7 || f.Since == nil, "fields lost: %+v", f)
	c.Eq(insights.PathProxy, f.Path, "path")
	c.False(f.Entrypoint != "proxy" || f.ExcludeEntrypoint != "analyze", "entrypoint fields lost: Entrypoint=%q, ExcludeEntrypoint=%q", f.Entrypoint, f.ExcludeEntrypoint)
	if _, err := BindSearchFilter(FilterVals{Path: "client"}); err == nil {
		t.Fatal("raw driven_by value must be rejected before the DB call")
	}
}

func TestBindStatsFilter(t *testing.T) {
	c := assert.NewAborting(t)
	f, err := BindStatsFilter(FilterVals{Persona: "team-platform-default", Model: "claude-haiku-4-5", Until: "1h"})
	c.NoError(err)
	c.False(f.Persona == "" || f.Model == "" || f.Until == nil, "fields lost: %+v", f)
}
