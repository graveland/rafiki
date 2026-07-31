// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/rafiki/insights"
)

func TestRenderStats(t *testing.T) {
	st := &insights.Stats{
		Volume:   insights.VolumeStats{Conversations: 2, Turns: 40},
		Tokens:   insights.TokenStats{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 9000, CacheHitRatio: 0.9},
		Adoption: insights.AdoptionStats{DistinctOwners: 1, PerOwner: []insights.OwnerCount{{Owner: "alice", Conversations: 2, Turns: 40}}},
		Cost:     []insights.CostRow{{Model: "claude-haiku-4-5", Turns: 40, InputTokens: 1000, CostUSD: 0.25}},
		ByPath:   map[string]insights.TokenStats{"proxy": {InputTokens: 1000, CacheHitRatio: 0.9}},
	}
	var b bytes.Buffer
	if err := RenderStats(&b, st); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"Conversations: 2", "alice", "claude-haiku-4-5", "TOTAL", "90.0%", "Latency"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderStatsEmpty(t *testing.T) {
	var b bytes.Buffer
	if err := RenderStats(&b, &insights.Stats{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no captured turns") {
		t.Errorf("empty stats should say so, got: %q", b.String())
	}
}

func TestRenderSearch(t *testing.T) {
	rows := []insights.ConversationSummary{{
		ID: "019f-aaaa", Owner: "bob", Source: "claude", Model: "m", Status: "active",
		DrivenBy: "client", CreatedAt: time.Now(), Turns: 5, InputTokens: 100, FirstMessage: "hello there",
	}}
	var b bytes.Buffer
	if err := RenderSearch(&b, rows); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"019f-aaaa", "bob", "hello there"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("search output missing %q:\n%s", want, b.String())
		}
	}
}

func TestRenderTranscriptMD(t *testing.T) {
	tr := &insights.Transcript{
		ConversationID: "c1", Owner: "alice", AvailableSkills: []string{"sc-diagnose-service"},
		Turns: []insights.TranscriptTurn{{
			Ordinal: 0, Role: "user",
			Content: json.RawMessage(`[{"type":"text","text":"check the replica"}]`),
		}, {
			Ordinal: 1, Role: "assistant", Model: "claude-haiku-4-5", OutputTokens: 12,
			Content: json.RawMessage(`[{"type":"tool_use","id":"tu_1","name":"service_status","input":{"id":"x"}}]`),
			Skills:  []string{"sc-diagnose-service"},
		}},
	}
	var b bytes.Buffer
	if err := RenderTranscriptMD(&b, tr); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"c1", "alice", "sc-diagnose-service", "check the replica", "service_status"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTranscriptMDPlainStringContent(t *testing.T) {
	tr := &insights.Transcript{
		ConversationID: "c1",
		Turns: []insights.TranscriptTurn{{
			Ordinal: 0, Role: "user",
			Content: json.RawMessage(`"just a plain string"`),
		}},
	}
	var b bytes.Buffer
	if err := RenderTranscriptMD(&b, tr); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "just a plain string") {
		t.Errorf("transcript output missing plain string content:\n%s", out)
	}
	if strings.Contains(out, `"just a plain string"`) {
		t.Errorf("plain string content should render bare, not quoted:\n%s", out)
	}
}

func TestRenderStatsNil(t *testing.T) {
	var b bytes.Buffer
	if err := RenderStats(&b, nil); err != nil {
		t.Fatal(err)
	}
	if b.Len() == 0 {
		t.Error("nil stats should render a one-line message, not nothing")
	}
}

func TestRenderTranscriptMDNil(t *testing.T) {
	var b bytes.Buffer
	if err := RenderTranscriptMD(&b, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no transcript") {
		t.Errorf("nil transcript should say so, got: %q", b.String())
	}
}

func TestRenderJSONIndent(t *testing.T) {
	var b bytes.Buffer
	if err := RenderJSON(&b, map[string]int{"a": 1}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\n  \"a\"") {
		t.Errorf("indent mode should pretty-print, got %q", b.String())
	}
}
