package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/conversationview"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestConversationsStatsCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsStatsCmd()
	for _, name := range []string{"since", "until", "owner", "persona", "source", "model", "path"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestConversationsSearchCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsSearchCmd()
	for _, name := range []string{
		"since", "until", "owner", "persona", "source", "model", "path",
		"status", "min-tokens", "text", "limit",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestConversationsExportCmd_RequiresExactlyOneArg(t *testing.T) {
	cmd := newConversationsExportCmd()
	if err := cmd.Args(cmd, nil); err == nil {
		t.Error("expected error with zero args")
	}
	if err := cmd.Args(cmd, []string{"conv-abc"}); err != nil {
		t.Errorf("expected no error with one arg: %v", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("expected error with two args")
	}
}

// wireResponse marshals v the way the daemon does, so the tests below exercise
// the real round trip rather than a hand-written payload. Pass exactly what
// pkg/control/dispatch.go hands okResponse: the bare domain value for stats and
// export, but the {"rows": ...} envelope for search — a bare slice there passes
// the render assertion while the real client fails on the wire.
func wireResponse(t *testing.T, command string, v any) *protocol.Response {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", command, err)
	}
	return &protocol.Response{Type: protocol.TypeCtrlResponse, Command: command, Success: true, Data: data}
}

func sampleStats() *insightstypes.Stats {
	return &insightstypes.Stats{
		Volume:   insightstypes.VolumeStats{Conversations: 3, Turns: 17},
		Adoption: insightstypes.AdoptionStats{DistinctOwners: 2, PerOwner: []insightstypes.OwnerCount{{Owner: "alice", Conversations: 2, Turns: 11}, {Owner: "", Conversations: 1, Turns: 6}}},
		Tokens:   insightstypes.TokenStats{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 3000, CacheCreationTokens: 500, CacheHitRatio: 0.75},
		Cost: []insightstypes.CostRow{
			{Model: "claude-sonnet-5", Turns: 12, InputTokens: 800, OutputTokens: 150, CacheReadTokens: 2000, CostUSD: 0.042},
			{Model: "claude-haiku-4-5", Turns: 5, InputTokens: 200, OutputTokens: 50, CacheReadTokens: 1000},
		},
		Failures:   insightstypes.FailureStats{Turns: 17, Errors: 1, ErrorRate: 0.058, FailoverRate: 0.11},
		Latency:    insightstypes.LatencyStats{P50: 1200, P95: 4300, P99: 9100},
		CacheWaste: insightstypes.CacheWasteStats{WastedTurns: 2, WastedInputTokens: 9000, Threshold: 4096},
		Prefix:     insightstypes.PrefixStats{DistinctPrefixes: 4, TurnsWithPrefix: 15, ReuseRatio: 3.75, CrossUserPrefixes: 1, DriftedConversations: 2},
		ByPath: map[string]insightstypes.TokenStats{
			"proxy":  {InputTokens: 700, OutputTokens: 140, CacheReadTokens: 2400, CacheHitRatio: 0.77},
			"direct": {InputTokens: 300, OutputTokens: 60, CacheReadTokens: 600, CacheHitRatio: 0.66},
		},
	}
}

// The point of routing rafiki's output through agentcli: `rafiki conversations
// stats` must render byte-for-byte what `rafikid agent stats` renders for the
// same rows. These assert the client's decode+render path against the renderer
// the daemon-side CLI calls directly, so a future change to either surface
// alone fails here.
func TestConversationsStatsRendersLikeAgentCLI(t *testing.T) {
	st := sampleStats()

	var want bytes.Buffer
	if err := conversationview.RenderStats(&want, st); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	resp := wireResponse(t, protocol.TypeCtrlConversationStats, st)
	if err := renderConversationResponse(&got, conversationview.ModeTable, resp, conversationview.RenderStats); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("rendered output differs from rafikid agent stats\n--- got ---\n%s\n--- want ---\n%s", got.String(), want.String())
	}
}

// searchEnvelope is the shape pkg/control/dispatch.go marshals for
// ctrl_conversation_search, kept here so these tests fail if the client stops
// unwrapping it.
type searchEnvelope struct {
	Rows []insightstypes.ConversationSummary `json:"rows"`
}

func sampleSearchRows() []insightstypes.ConversationSummary {
	return []insightstypes.ConversationSummary{
		{
			ID: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", Model: "claude-sonnet-5",
			Status: "completed", DrivenBy: "client", CreatedAt: time.Unix(1716000000, 0),
			Turns: 7, InputTokens: 900, OutputTokens: 120, CacheReadTokens: 2400,
			CacheHitRatio: 0.727, TotalCostUSD: 0.042,
			FirstMessage: "why do the stats disagree",
		},
	}
}

func TestConversationsSearchRendersLikeAgentCLI(t *testing.T) {
	rows := sampleSearchRows()

	var want bytes.Buffer
	if err := conversationview.RenderSearch(&want, rows); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	resp := wireResponse(t, protocol.TypeCtrlConversationSearch, searchEnvelope{Rows: rows})
	if err := renderConversationSearch(&got, conversationview.ModeTable, resp); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("rendered output differs from rafikid agent search\n--- got ---\n%s\n--- want ---\n%s", got.String(), want.String())
	}
}

// The JSON arm must print the rows, not the daemon's envelope, so
// `rafiki conversations search -o json | jq '.[]'` behaves like
// `rafikid agent search -J | jq '.[]'`.
func TestConversationsSearchJSONEmitsBareRows(t *testing.T) {
	rows := sampleSearchRows()
	resp := wireResponse(t, protocol.TypeCtrlConversationSearch, searchEnvelope{Rows: rows})

	var got bytes.Buffer
	if err := renderConversationSearch(&got, conversationview.ModeJSON, resp); err != nil {
		t.Fatal(err)
	}

	var back []insightstypes.ConversationSummary
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output is not a bare rows array: %v\n%s", err, got.String())
	}
	if len(back) != len(rows) || back[0].ID != rows[0].ID {
		t.Errorf("rows did not survive the round trip: got %+v", back)
	}
}

func TestConversationsExportRendersLikeAgentCLI(t *testing.T) {
	tr := &insightstypes.Transcript{
		ConversationID: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", DrivenBy: "client",
		AvailableSkills: []string{"td-go", "td-sql"},
		Turns: []insightstypes.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
			{Ordinal: 2, Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`), OutputTokens: 12, Model: "claude-sonnet-5"},
		},
	}

	var want bytes.Buffer
	if err := conversationview.RenderTranscriptMD(&want, tr); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	resp := wireResponse(t, protocol.TypeCtrlConversationExport, tr)
	if err := renderConversationResponse(&got, conversationview.ModeTable, resp, conversationview.RenderTranscriptMD); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("rendered output differs from rafikid agent export\n--- got ---\n%s\n--- want ---\n%s", got.String(), want.String())
	}
}

// --output json must still produce the daemon's payload verbatim, so existing
// `rafiki conversations stats | jq` pipelines keep working.
func TestConversationsJSONModeRoundTripsPayload(t *testing.T) {
	st := sampleStats()
	resp := wireResponse(t, protocol.TypeCtrlConversationStats, st)

	var got bytes.Buffer
	if err := renderConversationResponse(&got, conversationview.ModeJSON, resp, conversationview.RenderStats); err != nil {
		t.Fatal(err)
	}

	var back insightstypes.Stats
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if back.Volume != st.Volume || back.Tokens != st.Tokens || len(back.Cost) != len(st.Cost) {
		t.Errorf("payload did not survive the round trip: got %+v", back)
	}
}

// The global --output flag must reach conversations, which it only does once
// cobra has merged the root's persistent flags — hence driving the real command
// tree rather than calling conversationsMode on a detached subcommand.
func TestConversationsModeFromOutputFlag(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want conversationview.Mode
	}{
		{"table", conversationview.ModeTable},
		{"json", conversationview.ModeJSON},
	} {
		root := newRootCmd()
		stats, _, err := root.Find([]string{"conversations", "stats"})
		if err != nil {
			t.Fatalf("locate conversations stats: %v", err)
		}

		var got conversationview.Mode
		stats.RunE = func(cmd *cobra.Command, _ []string) error {
			got = conversationsMode(cmd)
			return nil
		}
		root.SetArgs([]string{"conversations", "stats", "--output", tc.flag})
		if err := root.Execute(); err != nil {
			t.Fatalf("--output %s: %v", tc.flag, err)
		}
		if got != tc.want {
			t.Errorf("--output %s: got mode %v, want %v", tc.flag, got, tc.want)
		}
	}
}

func TestUnixOrZero(t *testing.T) {
	if got := unixOrZero(nil); got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
	tm := time.Unix(1716000000, 0)
	if got := unixOrZero(&tm); got != 1716000000 {
		t.Errorf("got %d, want 1716000000", got)
	}
}
