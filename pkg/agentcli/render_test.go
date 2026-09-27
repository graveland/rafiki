// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/insights"

	"github.com/multigres/testkit/assert"
)

func TestRenderStats(t *testing.T) {
	c := assert.NewCollecting(t)
	st := &insights.Stats{
		Volume:   insights.VolumeStats{Conversations: 2, Turns: 40},
		Tokens:   insights.TokenStats{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 9000, CacheHitRatio: 0.9},
		Adoption: insights.AdoptionStats{DistinctOwners: 1, PerOwner: []insights.OwnerCount{{Owner: "alice", Conversations: 2, Turns: 40}}},
		Cost:     []insights.CostRow{{Model: "claude-haiku-4-5", Turns: 40, InputTokens: 1000, CostUSD: 0.25}},
		ByPath:   map[string]insights.TokenStats{"proxy": {InputTokens: 1000, CacheHitRatio: 0.9}},
	}
	var b bytes.Buffer
	c.Require().NoError(RenderStats(&b, st))
	out := b.String()
	for _, want := range []string{"Conversations: 2", "alice", "claude-haiku-4-5", "TOTAL", "90.0%", "Latency"} {
		c.StrContains(out, want, "stats output missing")
	}
}

func TestRenderStatsEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	var b bytes.Buffer
	c.Require().NoError(RenderStats(&b, &insights.Stats{}))
	c.StrContains(b.String(), "no captured turns", "empty stats should say so, got")
}

func TestRenderSearch(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []insights.ConversationSummary{{
		ID: "019f-aaaa", Owner: "bob", Source: "claude", Model: "m", Status: "active",
		DrivenBy: "client", CreatedAt: time.Now(), Turns: 5, InputTokens: 100, CacheReadTokens: 50,
		CacheHitRatio: 0.33, TotalCostUSD: 0.42, FirstMessage: "hello there",
	}}
	var b bytes.Buffer
	c.Require().NoError(RenderSearch(&b, rows))
	for _, want := range []string{"019f-aaaa", "bob", "hello there"} {
		c.StrContains(b.String(), want, "search output missing")
	}
}

func TestRenderTranscriptMD(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := &insights.Transcript{
		ConversationID: "c1", Owner: "alice", AvailableSkills: []string{"sc-diagnose-service"},
		Turns: []insights.TranscriptTurn{{
			Ordinal: 0, Role: "user",
			Content: json.RawMessage(`[{"type":"text","text":"check the replica"}]`),
		}, {
			Ordinal: 1, Role: "assistant", Model: "claude-haiku-4-5", OutputTokens: i64(12),
			Content: json.RawMessage(`[{"type":"tool_use","id":"tu_1","name":"service_status","input":{"id":"x"}}]`),
			Skills:  []string{"sc-diagnose-service"},
		}},
	}
	var b bytes.Buffer
	c.Require().NoError(RenderTranscriptMD(&b, tr))
	out := b.String()
	for _, want := range []string{"c1", "alice", "sc-diagnose-service", "check the replica", "service_status"} {
		c.StrContains(out, want, "transcript output missing")
	}
}

func TestRenderTranscriptMDPlainStringContent(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := &insights.Transcript{
		ConversationID: "c1",
		Turns: []insights.TranscriptTurn{{
			Ordinal: 0, Role: "user",
			Content: json.RawMessage(`"just a plain string"`),
		}},
	}
	var b bytes.Buffer
	c.Require().NoError(RenderTranscriptMD(&b, tr))
	out := b.String()
	c.StrContains(out, "just a plain string", "transcript output missing plain string content:\n")
	c.NotStrContains(out, `"just a plain string"`, "plain string content should render bare, not quoted:\n")
}

func TestRenderStatsNil(t *testing.T) {
	c := assert.NewCollecting(t)
	var b bytes.Buffer
	c.Require().NoError(RenderStats(&b, nil))
	c.NotEq(0, b.Len(), "nil stats should render a one-line message, not nothing")
}

func TestRenderTranscriptMDNil(t *testing.T) {
	c := assert.NewCollecting(t)
	var b bytes.Buffer
	c.Require().NoError(RenderTranscriptMD(&b, nil))
	c.StrContains(b.String(), "no transcript", "nil transcript should say so, got")
}

func TestRenderJSONIndent(t *testing.T) {
	c := assert.NewCollecting(t)
	var b bytes.Buffer
	c.Require().NoError(RenderJSON(&b, map[string]int{"a": 1}, true))
	c.StrContains(b.String(), "\n  \"a\"", "indent mode should pretty-print, got")
}

func i64(v int64) *int64 { return &v }
