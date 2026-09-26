package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/conversationview"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
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

// wireResponse marshals v the way the framed daemon does, so tests exercising
// framed-client render paths (see cmd_user_test.go) drive the real round trip
// rather than a hand-written payload. The conversation verbs are on Connect
// now, but their old helpers served other files too.
func wireResponse(t *testing.T, command string, v any) *protocol.Response {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", command, err)
	}
	return &protocol.Response{Type: protocol.TypeCtrlResponse, Command: command, Success: true, Data: data}
}

// ─── the Connect stub ───────────────────────────────────────────────────────

// conversationStub serves the conversation Control RPCs and records every
// request, so the tests assert what reached the wire rather than what a mock
// echoed. UnimplementedControlHandler keeps the verbs a test does not serve at
// unimplemented — a call to one fails the request loudly instead of silently.
type conversationStub struct {
	rafikiv1connect.UnimplementedControlHandler
	mu         sync.Mutex
	children   []*rafikiv1.ChildSummary
	statsResp  *rafikiv1.ConversationStatsResponse
	statsErr   error
	searchResp *rafikiv1.ConversationSearchResponse
	searchErr  error
	exportResp *rafikiv1.ConversationExportResponse
	exportErr  error
	reviewResp *rafikiv1.ConversationReviewResponse

	statsCalls  []*rafikiv1.ConversationStatsRequest
	searchCalls []*rafikiv1.ConversationSearchRequest
	exportCalls []*rafikiv1.ConversationExportRequest
	reviewCalls []*rafikiv1.ConversationReviewRequest
}

func (s *conversationStub) ConversationStats(
	_ context.Context,
	req *connect.Request[rafikiv1.ConversationStatsRequest],
) (*connect.Response[rafikiv1.ConversationStatsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statsCalls = append(s.statsCalls, req.Msg)
	if s.statsErr != nil {
		return nil, s.statsErr
	}
	return connect.NewResponse(s.statsResp), nil
}

func (s *conversationStub) ConversationSearch(
	_ context.Context,
	req *connect.Request[rafikiv1.ConversationSearchRequest],
) (*connect.Response[rafikiv1.ConversationSearchResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchCalls = append(s.searchCalls, req.Msg)
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return connect.NewResponse(s.searchResp), nil
}

func (s *conversationStub) ConversationExport(
	_ context.Context,
	req *connect.Request[rafikiv1.ConversationExportRequest],
) (*connect.Response[rafikiv1.ConversationExportResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exportCalls = append(s.exportCalls, req.Msg)
	if s.exportErr != nil {
		return nil, s.exportErr
	}
	return connect.NewResponse(s.exportResp), nil
}

// ListChildren backs resolveTargetConnect, which review now resolves through.
func (s *conversationStub) ListChildren(
	_ context.Context,
	_ *connect.Request[rafikiv1.ListChildrenRequest],
) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: s.children}), nil
}

func (s *conversationStub) ConversationReview(
	_ context.Context,
	req *connect.Request[rafikiv1.ConversationReviewRequest],
) (*connect.Response[rafikiv1.ConversationReviewResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviewCalls = append(s.reviewCalls, req.Msg)
	if s.reviewResp == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no review response stubbed"))
	}
	return connect.NewResponse(s.reviewResp), nil
}

func (s *conversationStub) recorded() []*rafikiv1.ConversationStatsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ConversationStatsRequest(nil), s.statsCalls...)
}

func (s *conversationStub) searched() []*rafikiv1.ConversationSearchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ConversationSearchRequest(nil), s.searchCalls...)
}

func (s *conversationStub) exported() []*rafikiv1.ConversationExportRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ConversationExportRequest(nil), s.exportCalls...)
}

func (s *conversationStub) reviews() []*rafikiv1.ConversationReviewRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ConversationReviewRequest(nil), s.reviewCalls...)
}

// newConversationsHarness wires an isolated profile at a Connect stub on
// connect.sock and points the process at it. No framed fake is served: none of
// these verbs dials controller.sock any more — the profile's socket path
// exists only as the anchor connect.sock is resolved beside.
func newConversationsHarness(t *testing.T) *conversationStub {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp with a short prefix, not t.TempDir(): the harness serves a
	// unix socket, whose path must fit sun_path (104 bytes on darwin) —
	// t.TempDir() embeds the test name and blows past it. Same shape as
	// newReviewHarness.
	dir, err := os.MkdirTemp("", "cv")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	stub := &conversationStub{}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, filepath.Join(dir, "connect.sock"), routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: filepath.Join(dir, "controller.sock")},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
	return stub
}

// ─── stats ──────────────────────────────────────────────────────────────────

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

// statsResponse is the Connect response carrying the daemon's opaque
// insights.Stats JSON (stats_json), marshalled exactly as the daemon does.
func statsResponse(t *testing.T, st *insightstypes.Stats) *rafikiv1.ConversationStatsResponse {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal stats payload: %v", err)
	}
	return &rafikiv1.ConversationStatsResponse{StatsJson: string(raw)}
}

// The point of routing rafiki's output through conversationview: `rafiki
// conversations stats` must render byte-for-byte what `rafikid agent stats`
// renders for the same rows. The wire carries the opaque stats_json; the table
// decodes the same bytes the daemon marshalled, so a future change to either
// surface alone fails here.
func TestConversationsStatsRendersLikeAgentCLI(t *testing.T) {
	st := sampleStats()

	var want bytes.Buffer
	if err := conversationview.RenderStats(&want, st); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := renderStatsResponse(&got, conversationview.ModeTable, statsResponse(t, st)); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("rendered output differs from rafikid agent stats\n--- got ---\n%s\n--- want ---\n%s", got.String(), want.String())
	}
}

// -j must decode the opaque stats_json into a generic map and print THAT —
// the payload as the daemon wrote it, not a client-side re-shape. The values
// still round-trip into the domain shape, so `rafiki conversations stats -j |
// jq` keeps working.
func TestConversationsStatsJSONDecodesOpaquePayload(t *testing.T) {
	st := sampleStats()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := renderStatsResponse(&got, conversationview.ModeJSON, statsResponse(t, st)); err != nil {
		t.Fatal(err)
	}

	var gotMap map[string]any
	if err := json.Unmarshal(got.Bytes(), &gotMap); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.String())
	}
	var wantMap map[string]any
	if err := json.Unmarshal(raw, &wantMap); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Errorf("-j output differs from the daemon's stats_json content")
	}

	var back insightstypes.Stats
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output does not round-trip into the domain shape: %v", err)
	}
	if back.Volume != st.Volume || back.Tokens != st.Tokens || len(back.Cost) != len(st.Cost) {
		t.Errorf("payload did not survive the round trip: got %+v", back)
	}
}

// -J renders one compact record: the whole stats bundle on a single line.
func TestConversationsStatsJSONCompactIsOneLine(t *testing.T) {
	var got bytes.Buffer
	if err := renderStatsResponse(&got, conversationview.ModeJSONCompact, statsResponse(t, sampleStats())); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(got.String()), "\n") + 1; n != 1 {
		t.Errorf("jsonl rendered %d lines, want one:\n%s", n, got.String())
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
			m, err := conversationsMode(cmd)
			if err != nil {
				return err
			}
			got = m
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

// Every filter flag must reach the wire verbatim, and --since resolves to the
// request's Unix seconds with an absent --until staying a present zero (the
// stats request's since/until are plain int64, where 0 means unset).
func TestConversationsStatsSendsFiltersOverConnect(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.statsResp = statsResponse(t, sampleStats())

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{
		"--owner", "alice", "--persona", "reviewer", "--source", "cli",
		"--model", "claude-sonnet-5", "--path", "proxy", "--since", "24h",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations stats: %v", err)
	}

	calls := stub.recorded()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationStats calls, want 1", len(calls))
	}
	req := calls[0]
	if req.ConversationId != "" {
		t.Errorf("ConversationId = %q, want empty for the global stats", req.ConversationId)
	}
	if req.Owner != "alice" || req.Persona != "reviewer" || req.Source != "cli" ||
		req.Model != "claude-sonnet-5" || req.Path != "proxy" {
		t.Errorf("filters did not reach the wire: %+v", req)
	}
	if req.SinceUnix <= 0 {
		t.Errorf("SinceUnix = %d, want the resolved --since (positive)", req.SinceUnix)
	}
	if req.UntilUnix != 0 {
		t.Errorf("UntilUnix = %d, want 0 (unset)", req.UntilUnix)
	}
}

// A positional conversation id switches to the per-conversation stats and the
// filter flags are not bound at all.
func TestConversationsStatsSendsConversationID(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.statsResp = statsResponse(t, sampleStats())

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{"conv-abc"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations stats conv-abc: %v", err)
	}

	calls := stub.recorded()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationStats calls, want 1", len(calls))
	}
	if got := calls[0].GetConversationId(); got != "conv-abc" {
		t.Errorf("ConversationId = %q, want conv-abc", got)
	}
}

// The conversation path attaches rafiki's precise reasons as ErrorInfo
// details, which connect's own rendering drops; the stderr shape must surface
// the reason (`<reason>: <message>`), and it must survive a real client round
// trip — the stub serves the decorated error, the command decodes it.
func TestConversationsStatsErrorCarriesReason(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.statsErr = rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child c_1 already exited")),
		protocol.ErrChildExited)

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{"conv-abc"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error")
	}
	want := "child_exited: child c_1 already exited"
	if err.Error() != want {
		t.Errorf("error = %q, want %q (the rafiki reason over the connect code)", err.Error(), want)
	}
}

// An error with no attached reason renders as connect's own `<code>:
// <message>` — the same shape the other Connect-backed verbs print.
func TestConversationsSearchErrorWithoutReason(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.searchErr = connect.NewError(connect.CodeNotFound, errors.New("no such conversation"))

	cmd := newConversationsSearchCmd()
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got != "not_found: no such conversation" {
		t.Errorf("error = %q, want connect's code-and-message rendering", got)
	}
}

// The three infrastructure codes keep diagnoseConnectError's endpoint advice:
// "cannot reach the daemon" must name the socket so a wrong profile is
// diagnosable, instead of collapsing to a bare "unavailable".
func TestConversationsStatsUnreachableNamesTheSocket(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// A profile whose sibling connect.sock answers nothing.
	dir := t.TempDir()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: filepath.Join(dir, "controller.sock")},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{"conv-abc"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"cannot reach the rafiki daemon at", "is rafikid running", filepath.Join(dir, "connect.sock")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// ─── search ─────────────────────────────────────────────────────────────────

func sampleSummaries() []insightstypes.ConversationSummary {
	return []insightstypes.ConversationSummary{
		{
			ID: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", Model: "claude-sonnet-5",
			Status: "completed", DrivenBy: "client", CreatedAt: time.Unix(1716000000, 0).UTC(),
			Turns: 7, InputTokens: 900, OutputTokens: 120, CacheReadTokens: 2400,
			CacheHitRatio: 0.727, TotalCostUSD: 0.042,
			FirstMessage: "why do the stats disagree",
		},
	}
}

// sampleProtoSummaries is the wire twin of sampleSummaries — what the daemon's
// ConversationSearchResponse carries for those rows. If the conversion below
// drops a field, the render-parity and round-trip tests both fail.
func sampleProtoSummaries() []*rafikiv1.ConversationSummary {
	return []*rafikiv1.ConversationSummary{
		{
			Id: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", Model: "claude-sonnet-5",
			Status: "completed", DrivenBy: "client", CreatedAtUnix: 1716000000,
			Turns: 7, InputTokens: 900, OutputTokens: 120, CacheReadTokens: 2400,
			CacheHitRatio: 0.727, TotalCostUsd: 0.042,
			FirstMessage: "why do the stats disagree",
		},
	}
}

func searchResponse(rows []*rafikiv1.ConversationSummary) *rafikiv1.ConversationSearchResponse {
	return &rafikiv1.ConversationSearchResponse{Rows: rows}
}

// The table must render byte-for-byte what `rafikid agent search` renders for
// the same rows — both surfaces route through conversationview.RenderSearch.
func TestConversationsSearchRendersLikeAgentCLI(t *testing.T) {
	rows := sampleSummaries()

	var want bytes.Buffer
	if err := conversationview.RenderSearch(&want, rows); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := renderSearchResponse(&got, conversationview.ModeTable, searchResponse(sampleProtoSummaries())); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("rendered output differs from rafikid agent search\n--- got ---\n%s\n--- want ---\n%s", got.String(), want.String())
	}
}

// The JSON arm must print the rows bare — no envelope — so
// `rafiki conversations search -o json | jq '.[]'` behaves like
// `rafikid agent search -J | jq '.[]'`, with every field the wire carries
// intact (including the rebuilt timestamp).
func TestConversationsSearchJSONEmitsBareRows(t *testing.T) {
	var got bytes.Buffer
	if err := renderSearchResponse(&got, conversationview.ModeJSON, searchResponse(sampleProtoSummaries())); err != nil {
		t.Fatal(err)
	}

	var back []insightstypes.ConversationSummary
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output is not a bare rows array: %v\n%s", err, got.String())
	}
	want := sampleSummaries()
	if len(back) != len(want) {
		t.Fatalf("got %d rows, want %d", len(back), len(want))
	}
	for i := range want {
		if !back[i].CreatedAt.Equal(want[i].CreatedAt) {
			t.Errorf("row %d CreatedAt = %v, want %v", i, back[i].CreatedAt, want[i].CreatedAt)
		}
		back[i].CreatedAt = want[i].CreatedAt
		if !reflect.DeepEqual(back[i], want[i]) {
			t.Errorf("row %d did not survive the round trip:\n got %+v\nwant %+v", i, back[i], want[i])
		}
	}
}

// An empty result renders the renderer's honest empty line, and the JSON arm
// emits `[]`, not null.
func TestConversationsSearchEmptyRendersEmpty(t *testing.T) {
	var tableBuf bytes.Buffer
	if err := renderSearchResponse(&tableBuf, conversationview.ModeTable, searchResponse(nil)); err != nil {
		t.Fatal(err)
	}
	if tableBuf.String() != "no conversations found\n" {
		t.Errorf("empty table rendered %q", tableBuf.String())
	}

	var jsn bytes.Buffer
	if err := renderSearchResponse(&jsn, conversationview.ModeJSON, searchResponse(nil)); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(jsn.String()) != "[]" {
		t.Errorf("empty JSON rendered %q, want []", jsn.String())
	}
}

// Search's optional since/until must stay ABSENT when unset (nil pointers on
// the wire) and present when set — the wire's optional shape exists so unset
// never reads as measured.
func TestConversationsSearchSendsFiltersOverConnect(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.searchResp = searchResponse(sampleProtoSummaries())

	cmd := newConversationsSearchCmd()
	cmd.SetArgs([]string{
		"--owner", "alice", "--path", "proxy", "--status", "completed",
		"--min-tokens", "5", "--text", "stats", "--limit", "3", "--since", "24h",
		"--until", "2026-01-02T03:04:05Z",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations search: %v", err)
	}

	calls := stub.searched()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationSearch calls, want 1", len(calls))
	}
	req := calls[0]
	if req.Owner != "alice" || req.Path != "proxy" || req.Status != "completed" ||
		req.MinTokens != 5 || req.Text != "stats" || req.Limit != 3 {
		t.Errorf("filters did not reach the wire: %+v", req)
	}
	if req.SinceUnix == nil || *req.SinceUnix <= 0 {
		t.Errorf("SinceUnix = %v, want the resolved --since", req.SinceUnix)
	}
	until, err := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	if req.UntilUnix == nil || req.GetUntilUnix() != until.Unix() {
		t.Errorf("UntilUnix = %v, want the parsed RFC3339 --until (%d)", req.UntilUnix, until.Unix())
	}

	// Without the flags: absent, not present-zero.
	stub = newConversationsHarness(t)
	stub.searchResp = searchResponse(sampleProtoSummaries())
	cmd = newConversationsSearchCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations search (no filters): %v", err)
	}
	calls = stub.searched()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationSearch calls, want 1", len(calls))
	}
	req = calls[0]
	if req.SinceUnix != nil || req.UntilUnix != nil {
		t.Errorf("unset --since/--until must ride absent, got %v/%v", req.SinceUnix, req.UntilUnix)
	}
}

// --limit is narrowed to the proto's int32; a value that would truncate must
// error rather than silently send the wrong number.
func TestConversationsSearchLimitBeyondInt32(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.searchResp = searchResponse(nil)

	cmd := newConversationsSearchCmd()
	cmd.SetArgs([]string{"--limit", "9999999999"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --limit beyond int32 to error")
	}
	if !strings.Contains(err.Error(), "--limit") {
		t.Errorf("error = %q, want a --limit message", err.Error())
	}
	if len(stub.searched()) != 0 {
		t.Errorf("a rejected request must not reach the wire, got %d calls", len(stub.searched()))
	}
}

// ─── export ─────────────────────────────────────────────────────────────────

func sampleProtoExport() *rafikiv1.ConversationExportResponse {
	return &rafikiv1.ConversationExportResponse{
		ConversationId: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", DrivenBy: "client",
		AvailableSkills: []string{"td-go", "td-sql"},
		Turns: []*rafikiv1.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: []byte(`[{"type":"text","text":"hello"}]`)},
			// One turn with metrics reported — including an explicit
			// measured zero — so the conversion's nil-vs-zero handling is
			// exercised from both sides.
			{Ordinal: 2, Role: "assistant", Content: []byte(`[{"type":"text","text":"hi"}]`),
				OutputTokens: int64Ptr(12), InputTokens: int64Ptr(0), Model: "claude-sonnet-5",
				LatencyMs: int32Ptr(1500), ServedProvider: "openrouter"},
		},
	}
}

// sampleTranscript is the domain twin the converter must produce for
// sampleProtoExport.
func sampleTranscript() *insightstypes.Transcript {
	return &insightstypes.Transcript{
		ConversationID: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", DrivenBy: "client",
		AvailableSkills: []string{"td-go", "td-sql"},
		Turns: []insightstypes.TranscriptTurn{
			{Ordinal: 1, Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
			{Ordinal: 2, Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`),
				OutputTokens: int64Ptr(12), InputTokens: int64Ptr(0), Model: "claude-sonnet-5",
				LatencyMS: intPtr(1500), ServedProvider: "openrouter"},
		},
	}
}

// The markdown transcript must render byte-for-byte what `rafikid agent
// export` renders for the same conversation — header identity, skills, roles
// and content blocks.
func TestConversationsExportRendersLikeAgentCLI(t *testing.T) {
	var want bytes.Buffer
	if err := conversationview.RenderTranscriptMD(&want, sampleTranscript()); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := renderExportResponse(&got, conversationview.ModeTable, sampleProtoExport()); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("rendered output differs from rafikid agent export\n--- got ---\n%s\n--- want ---\n%s", got.String(), want.String())
	}
}

// JSON mode must round-trip EVERY field the wire carries — including the
// nil-vs-measured-zero distinction on the optional metrics, which the markdown
// renderer does not print and so cannot pin. Content is compared semantically:
// json.MarshalIndent re-indents embedded json.RawMessage (old -j behavior too,
// so the output stays byte-identical to before), so raw byte equality of
// Content would fail on whitespace that carries no meaning.
func TestConversationsExportJSONRoundTripsEveryField(t *testing.T) {
	var got bytes.Buffer
	if err := renderExportResponse(&got, conversationview.ModeJSON, sampleProtoExport()); err != nil {
		t.Fatal(err)
	}

	var back insightstypes.Transcript
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.String())
	}
	want := *sampleTranscript()
	if len(back.Turns) != len(want.Turns) {
		t.Fatalf("got %d turns, want %d", len(back.Turns), len(want.Turns))
	}
	for i := range want.Turns {
		var gotBlocks, wantBlocks []any
		if err := json.Unmarshal(back.Turns[i].Content, &gotBlocks); err != nil {
			t.Fatalf("turn %d content is not a JSON array: %v", i, err)
		}
		if err := json.Unmarshal(want.Turns[i].Content, &wantBlocks); err != nil {
			t.Fatalf("want turn %d content: %v", i, err)
		}
		if !reflect.DeepEqual(gotBlocks, wantBlocks) {
			t.Errorf("turn %d content did not survive the round trip", i)
		}
		// Everything but Content must be exactly equal — the pointers on the
		// optional metrics are the point: nil is "not reported", 0 measured.
		back.Turns[i].Content, want.Turns[i].Content = nil, nil
		if !reflect.DeepEqual(back.Turns[i], want.Turns[i]) {
			t.Errorf("turn %d did not survive the round trip:\n got %+v\nwant %+v", i, back.Turns[i], want.Turns[i])
		}
	}
	back.Turns, want.Turns = nil, nil
	if !reflect.DeepEqual(back, want) {
		t.Errorf("transcript header did not survive the round trip:\n got %+v\nwant %+v", back, want)
	}
}

func TestConversationsExportSendsIDOverConnect(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.exportResp = sampleProtoExport()

	cmd := newConversationsExportCmd()
	cmd.SetArgs([]string{"conv-abc"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations export: %v", err)
	}

	calls := stub.exported()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationExport calls, want 1", len(calls))
	}
	if got := calls[0].GetConversationId(); got != "conv-abc" {
		t.Errorf("ConversationId = %q, want conv-abc", got)
	}
}

// A scope miss or a missing conversation reads as not_found (the daemon makes
// the two indistinguishable), rendered in connect's code-and-message shape.
func TestConversationsExportNotFound(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.exportErr = connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))

	cmd := newConversationsExportCmd()
	cmd.SetArgs([]string{"conv-abc"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got != "not_found: conversation not found" {
		t.Errorf("error = %q, want \"not_found: conversation not found\"", got)
	}
}

// ─── query ──────────────────────────────────────────────────────────────────

func TestConversationsQueryCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsQueryCmd()
	for _, name := range []string{"since", "until", "owner", "persona", "source", "model", "path"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
	if cmd.Args == nil {
		t.Fatal("Args validator not set")
	}
	if err := cmd.Args(cmd, []string{"tools"}); err != nil {
		t.Errorf("one arg should validate: %v", err)
	}
	if err := cmd.Args(cmd, nil); err == nil {
		t.Error("expected error with zero args")
	}

	root := newRootCmd()
	if _, _, err := root.Find([]string{"conversations", "query"}); err != nil {
		t.Errorf("query not reachable from the command tree: %v", err)
	}
}

// sampleQueryResponse is one response exercising every cell shape the wire
// carries: a string, a bare int, and floats in all three render formats.
func sampleQueryResponse() *rafikiv1.ConversationQueryResponse {
	return &rafikiv1.ConversationQueryResponse{
		Columns: []*rafikiv1.QueryColumn{
			{Name: "tool", Kind: "string"},
			{Name: "calls", Kind: "int"},
			{Name: "cost", Kind: "float", Format: "usd"},
			{Name: "hit", Kind: "float", Format: "pct"},
			{Name: "ratio", Kind: "float"},
		},
		Rows: []*rafikiv1.QueryRow{
			{Cells: []*rafikiv1.QueryValue{
				{V: &rafikiv1.QueryValue_StrValue{StrValue: "bash"}},
				{V: &rafikiv1.QueryValue_IntValue{IntValue: 606}},
				{V: &rafikiv1.QueryValue_FloatValue{FloatValue: 0.0042}},
				{V: &rafikiv1.QueryValue_FloatValue{FloatValue: 0.5}},
				{V: &rafikiv1.QueryValue_FloatValue{FloatValue: 0.727}},
			}},
		},
	}
}

func TestRenderQueryResponseTable(t *testing.T) {
	var got bytes.Buffer
	if err := renderQueryResponse(&got, conversationview.ModeTable, sampleQueryResponse()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
	}{
		{"string cell", "bash"},
		{"int cell renders bare, no decimals", "606"},
		{"usd format", "$0.0042"},
		{"pct format", "50%"},
		{"unformatted float", "0.73"},
	} {
		if !strings.Contains(got.String(), tc.want) {
			t.Errorf("%s: output missing %q:\n%s", tc.name, tc.want, got.String())
		}
	}
}

// The JSON path must emit real typed values — int and float cells decode as
// numbers, never strings — which is the whole reason Q2/Q3 typed the wire.
func TestRenderQueryResponseJSONTypedValues(t *testing.T) {
	var got bytes.Buffer
	if err := renderQueryResponse(&got, conversationview.ModeJSON, sampleQueryResponse()); err != nil {
		t.Fatal(err)
	}

	var back struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.String())
	}
	if len(back.Columns) != 5 || back.Columns[2] != "cost" {
		t.Errorf("columns did not survive: %+v", back.Columns)
	}
	row := back.Rows[0]
	if len(row) != 5 {
		t.Fatalf("row width %d, want 5", len(row))
	}
	if s, ok := row[0].(string); !ok || s != "bash" {
		t.Errorf("cell 0: got %#v, want string \"bash\"", row[0])
	}
	if _, ok := row[1].(float64); !ok {
		t.Errorf("cell 1 (int): got %#v, want a number, not a string", row[1])
	}
	for i := 2; i < 5; i++ {
		if _, ok := row[i].(float64); !ok {
			t.Errorf("cell %d (float): got %#v, want a number, not a string", i, row[i])
		}
	}
}

// ─── conversations review & findings ────────────────────────────────────────

func TestConversationsReviewCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsReviewCmd()
	for _, name := range []string{"stage", "model", "analyzer-profile", "budget-usd", "min-turns", "force"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
	// The analyzer-profile flag must not be named "profile": profile
	// resolution (profile_glue.go) reads a flag by that name off the
	// command's own flag set as the DAEMON profile selection, so a local one
	// would make `--profile sql` exit(2) on "unknown profile sql" instead of
	// naming the analyzer profile the request field wants.
	if cmd.Flags().Lookup("profile") != nil {
		t.Error("a local --profile shadows the client's global -P/--profile; the analyzer-profile flag is --analyzer-profile")
	}
	if got := cmd.Flags().Lookup("stage").DefValue; got != "detect" {
		t.Errorf("--stage default = %q, want detect", got)
	}
	if _, _, err := newRootCmd().Find([]string{"conversations", "review"}); err != nil {
		t.Errorf("review not reachable from the command tree: %v", err)
	}
	if _, _, err := newRootCmd().Find([]string{"conversations", "findings"}); err != nil {
		t.Errorf("findings not reachable from the command tree: %v", err)
	}
}

func TestConversationsFindingsCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsFindingsCmd()
	for _, name := range []string{"axis", "skill", "status", "limit"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("expected error for a positional arg (findings takes none)")
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("no args should validate: %v", err)
	}
}

// --stage is a closed set: anything else errors before any dial.
func TestConversationsReviewStageValidated(t *testing.T) {
	for _, tc := range []struct {
		stage string
		want  rafikiv1.ReviewStage
		ok    bool
	}{
		{"detect", rafikiv1.ReviewStage_REVIEW_STAGE_DETECT, true},
		{"rank", rafikiv1.ReviewStage_REVIEW_STAGE_RANK, true},
		{"draft", rafikiv1.ReviewStage_REVIEW_STAGE_UNSPECIFIED, false},
		{"", rafikiv1.ReviewStage_REVIEW_STAGE_UNSPECIFIED, false},
		{"DETECT", rafikiv1.ReviewStage_REVIEW_STAGE_UNSPECIFIED, false},
	} {
		cmd := newConversationsReviewCmd()
		if err := cmd.Flags().Set("stage", tc.stage); err != nil {
			t.Fatalf("set stage %q: %v", tc.stage, err)
		}
		got, err := reviewStageFlag(cmd)
		if tc.ok && err != nil {
			t.Errorf("stage %q: unexpected error %v", tc.stage, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("stage %q: expected an error, got none", tc.stage)
		}
		if got != tc.want {
			t.Errorf("stage %q: got %v, want %v", tc.stage, got, tc.want)
		}
	}
}

// The config file fills what the flags leave unset and never overwrites a
// flag — design §3's resolution order, over the real wire. The target is
// resolved by NAME through the Connect stub's ListChildren: review resolves
// through resolveTargetConnect like every other child-target verb, so the
// request's ids are real child ids resolved over Connect.
func TestConversationsReviewMergesConfigFileButFlagsWin(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.children = []*rafikiv1.ChildSummary{{
		ChildId: "c_9", Name: "my-agent", Status: "exited",
	}}
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_9", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	}}}

	cfgPath := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(cfgPath, []byte(
		`{"model":"file-model","profile":"file-profile","budget_usd":2.5,"min_turns":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_REVIEW_CONFIG", cfgPath)

	cmd := newConversationsReviewCmd()
	// --analyzer-profile is the review verb's own flag (the analyzer
	// profile): it must reach the request, not the daemon-profile selection.
	cmd.SetArgs([]string{"my-agent", "--model", "flag-model", "--min-turns", "8", "--analyzer-profile", "flag-profile"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations review: %v", err)
	}

	calls := stub.reviews()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationReview calls, want 1", len(calls))
	}
	req := calls[0]
	if ids := req.GetConversationIds(); len(ids) != 1 || ids[0] != "c_9" {
		t.Errorf("ConversationIds = %v, want [c_9] (resolved from the name)", ids)
	}
	if req.Stage != rafikiv1.ReviewStage_REVIEW_STAGE_DETECT {
		t.Errorf("Stage = %v, want DETECT (the flag default)", req.Stage)
	}
	if req.Model == nil || *req.Model != "flag-model" {
		t.Errorf("Model = %v, want the flag's value, not the file's", req.Model)
	}
	if req.MinTurns == nil || *req.MinTurns != 8 {
		t.Errorf("MinTurns = %v, want the flag's 8, not the file's 4", req.MinTurns)
	}
	if req.Profile == nil || *req.Profile != "flag-profile" {
		t.Errorf("Profile = %v, want the flag's value (flags win over the file)", req.Profile)
	}
	if req.BudgetUsd == nil || *req.BudgetUsd != 2.5 {
		t.Errorf("BudgetUsd = %v, want the file's 2.5 (no flag set)", req.BudgetUsd)
	}
	if req.Force != nil {
		t.Errorf("Force = %v, want nil (neither flag nor file set it)", req.Force)
	}
}

// An unresolvable review target fails with resolveTargetConnect's matching
// error, naming the identifier that matched nothing, and must not send the
// review.
func TestConversationsReviewUnresolvedTarget(t *testing.T) {
	stub := newConversationsHarness(t)
	stub.children = []*rafikiv1.ChildSummary{{
		ChildId: "c_9", Name: "my-agent", Status: "exited",
	}}

	cmd := newConversationsReviewCmd()
	cmd.SetArgs([]string{"no-such-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error")
	}
	want := `resolve "no-such-agent": no child matches "no-such-agent"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if len(stub.reviews()) != 0 {
		t.Errorf("a failed resolution must not send the review, got %d calls", len(stub.reviews()))
	}
}

func TestConversationsReviewRendersPerIDStatus(t *testing.T) {
	var buf bytes.Buffer
	err := renderReviewAccepts(&buf, []*rafikiv1.ConversationReviewAccept{
		{ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED},
		{ConversationId: "c_2", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ALREADY_RUNNING},
		{ConversationId: "c_3", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "c_1: enqueued\nc_2: already running\nc_3: queue full\n"
	if buf.String() != want {
		t.Errorf("rendered %q, want %q", buf.String(), want)
	}
}

// --force rides the request, and findings filters reach the wire verbatim:
// status passes through (the daemon defaults "" to open), limit 0 means the
// daemon's default.
func TestConversationsFindingsSendsFilters(t *testing.T) {
	_, stub := newReviewHarness(t)
	stub.findingsResp = &rafikiv1.ConversationFindingsResponse{}

	cmd := newConversationsFindingsCmd()
	cmd.SetArgs([]string{"--axis", "prompt", "--skill", "sql", "--status", "dismissed", "--limit", "5"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("conversations findings: %v", err)
	}

	calls := stub.findings()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationFindings calls, want 1", len(calls))
	}
	req := calls[0]
	if req.Axis != "prompt" || req.Skill != "sql" || req.Status != "dismissed" || req.Limit != 5 {
		t.Errorf("filters did not reach the wire: %+v", req)
	}
}

func TestConversationsFindingsRendersRows(t *testing.T) {
	resp := &rafikiv1.ConversationFindingsResponse{
		Findings: []*rafikiv1.ReviewFinding{
			{Id: "f1", ConversationId: "conv-a", Axis: "prompt", SkillName: "sql",
				Title: "N+1 queries", ExpectedSavingsTokens: 12000, Status: "open"},
			{Id: "f2", ConversationId: "conv-b", Axis: "tools",
				Title: "Redundant greps", ExpectedSavingsTokens: 300, Status: "open"},
		},
		Analyses: []*rafikiv1.ReviewAnalysis{{
			Id: "a1", ConversationId: "conv-a", Model: "anthropic/claude-sonnet-5",
			Status: "ok", CostUsd: 0.0123, CreatedAtUnix: 1716000000,
		}},
	}

	var buf bytes.Buffer
	if err := renderFindingsResponse(&buf, outputTable, resp); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Axis", "Skill", "Title", "Savings", "Status", "ID",
		"N+1 queries", "12,000", "open", "f1", "f2",
		"Analysis", "Conversation", "Model", "Cost", "Created",
		"a1", "conv-a", "anthropic/claude-sonnet-5", "$0.0123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("findings table missing %q:\n%s", want, out)
		}
	}

	// Neither collection present: the honest "no findings" line, matching
	// rafikid agent findings' empty rendering.
	var empty bytes.Buffer
	if err := renderFindingsResponse(&empty, outputTable, &rafikiv1.ConversationFindingsResponse{}); err != nil {
		t.Fatal(err)
	}
	if empty.String() != "no findings\n" {
		t.Errorf("empty response rendered %q, want \"no findings\\n\"", empty.String())
	}
}

// JSON mode carries the WHOLE response — findings and analyses — so jq sees
// the same shape the daemon sent, and JSONL is one finding per line.
func TestConversationsFindingsJSONModes(t *testing.T) {
	resp := &rafikiv1.ConversationFindingsResponse{
		Findings: []*rafikiv1.ReviewFinding{{
			Id: "f1", ConversationId: "conv-a", Axis: "prompt", Title: "t",
			ExpectedSavingsTokens: 10, Status: "open",
		}},
		Analyses: []*rafikiv1.ReviewAnalysis{{Id: "a1", ConversationId: "conv-a", Status: "ok"}},
	}

	var pretty bytes.Buffer
	if err := renderFindingsResponse(&pretty, outputJSON, resp); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Findings []map[string]any `json:"findings"`
		Analyses []map[string]any `json:"analyses"`
	}
	if err := json.Unmarshal(pretty.Bytes(), &back); err != nil {
		t.Fatalf("json output invalid: %v\n%s", err, pretty.String())
	}
	if len(back.Findings) != 1 || len(back.Analyses) != 1 {
		t.Errorf("json output lost rows: %+v", back)
	}

	var lines bytes.Buffer
	if err := renderFindingsResponse(&lines, outputJSONL, resp); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(lines.String()), "\n") + 1; n != 1 {
		t.Errorf("jsonl rendered %d lines, want one finding per line:\n%s", n, lines.String())
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

func TestUnixPtrOrNil(t *testing.T) {
	if got := unixPtrOrNil(nil); got != nil {
		t.Errorf("nil: got %v, want nil", got)
	}
	tm := time.Unix(1716000000, 0)
	got := unixPtrOrNil(&tm)
	if got == nil || *got != 1716000000 {
		t.Errorf("got %v, want 1716000000", got)
	}
}

func int64Ptr(v int64) *int64 { return &v }
