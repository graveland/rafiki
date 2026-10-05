package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/conversationview"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

func TestConversationsStatsCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsStatsCmd()
	for _, name := range []string{"since", "until", "owner", "persona", "source", "model", "path"} {
		assert.NewCollecting(t).NotNil(cmd.Flags().Lookup(name), "flag --%s not registered", name)
	}
}

func TestConversationsSearchCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsSearchCmd()
	for _, name := range []string{
		"since", "until", "owner", "persona", "source", "model", "path",
		"status", "min-tokens", "text", "limit",
	} {
		assert.NewCollecting(t).NotNil(cmd.Flags().Lookup(name), "flag --%s not registered", name)
	}
}

func TestConversationsExportCmd_RequiresExactlyOneArg(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newConversationsExportCmd()
	c.Error(cmd.Args(cmd, nil), "expected error with zero args")
	c.NoError(cmd.Args(cmd, []string{"conv-abc"}), "expected no error with one arg")
	c.Error(cmd.Args(cmd, []string{"a", "b"}), "expected error with two args")
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

// newConversationsHarness wires an isolated profile at a Connect stub served
// on the profile's own socket, and points the process at it.
func newConversationsHarness(t *testing.T) *conversationStub {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp with a short prefix, not t.TempDir(): the harness serves a
	// unix socket, whose path must fit sun_path (104 bytes on darwin) —
	// t.TempDir() embeds the test name and blows past it. Same shape as
	// newReviewHarness.
	dir, err := os.MkdirTemp("", "cv")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	stub := &conversationStub{}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	sock := filepath.Join(dir, "controller.sock")
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
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
	assert.NewAborting(t).NoError(err, "marshal stats payload")
	return &rafikiv1.ConversationStatsResponse{StatsJson: string(raw)}
}

// The point of routing rafiki's output through conversationview: `rafiki
// conversations stats` must render byte-for-byte what `rafikid agent stats`
// renders for the same rows. The wire carries the opaque stats_json; the table
// decodes the same bytes the daemon marshalled, so a future change to either
// surface alone fails here.
func TestConversationsStatsRendersLikeAgentCLI(t *testing.T) {
	c := assert.NewCollecting(t)
	st := sampleStats()

	var want bytes.Buffer
	c.Require().NoError(conversationview.RenderStats(&want, st))

	var got bytes.Buffer
	c.Require().NoError(renderStatsResponse(&got, conversationview.ModeTable, statsResponse(t, st)))

	c.Eq(want.String(), got.String(), "rendered output differs from rafikid agent stats\n--- got ---\n")
}

// -j must decode the opaque stats_json into a generic map and print THAT —
// the payload as the daemon wrote it, not a client-side re-shape. The values
// still round-trip into the domain shape, so `rafiki conversations stats -j |
// jq` keeps working.
func TestConversationsStatsJSONDecodesOpaquePayload(t *testing.T) {
	c := assert.NewCollecting(t)
	st := sampleStats()
	raw, err := json.Marshal(st)
	c.Require().NoError(err)

	var got bytes.Buffer
	c.Require().NoError(renderStatsResponse(&got, conversationview.ModeJSON, statsResponse(t, st)))

	var gotMap map[string]any
	if err := json.Unmarshal(got.Bytes(), &gotMap); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.String())
	}
	var wantMap map[string]any
	c.Require().NoError(json.Unmarshal(raw, &wantMap))
	c.EqDeep(wantMap, gotMap, "-j output differs from the daemon's stats_json content")

	var back insightstypes.Stats
	c.Require().NoError(json.Unmarshal(got.Bytes(), &back), "output does not round-trip into the domain shape")
	c.False(back.Volume != st.Volume || back.Tokens != st.Tokens || len(back.Cost) != len(st.Cost), "payload did not survive the round trip: got %+v", back)
}

// -J renders one compact record: the whole stats bundle on a single line.
func TestConversationsStatsJSONCompactIsOneLine(t *testing.T) {
	c := assert.NewCollecting(t)
	var got bytes.Buffer
	c.Require().NoError(renderStatsResponse(&got, conversationview.ModeJSONCompact, statsResponse(t, sampleStats())))
	n := strings.Count(strings.TrimSpace(got.String()), "\n") + 1
	c.Eq(1, n, "jsonl rendered %d lines, want one:\n%s", n, got.String())
}

// The global --output flag must reach conversations, which it only does once
// cobra has merged the root's persistent flags — hence driving the real command
// tree rather than calling conversationsMode on a detached subcommand.
func TestConversationsModeFromOutputFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		flag string
		want conversationview.Mode
	}{
		{"table", conversationview.ModeTable},
		{"json", conversationview.ModeJSON},
	} {
		root := newRootCmd()
		stats, _, err := root.Find([]string{"conversations", "stats"})
		c.Require().NoError(err, "locate conversations stats")

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
		c.Require().NoError(root.Execute(), "--output %s", tc.flag)
		c.Eq(tc.want, got, "--output %s: got mode %v, want", tc.flag, got)
	}
}

// Every filter flag must reach the wire verbatim, and --since resolves to the
// request's Timestamp (a present message), with an absent --until staying
// unset (a nil message, never a zero Timestamp).
func TestConversationsStatsSendsFiltersOverConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.statsResp = statsResponse(t, sampleStats())

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{
		"--owner", "alice", "--persona", "reviewer", "--source", "cli",
		"--model", "claude-sonnet-5", "--path", "proxy", "--since", "24h",
	})
	c.Require().NoError(cmd.Execute(), "conversations stats")

	calls := stub.recorded()
	c.Require().Len(calls, 1, "got %d ConversationStats calls, want 1", len(calls))
	req := calls[0]
	c.Eq("", req.ConversationId, "ConversationId")
	c.False(req.Owner != "alice" || req.Persona != "reviewer" || req.Source != "cli" ||
		req.Model != "claude-sonnet-5" || req.Path != "proxy", "filters did not reach the wire: %+v", req)
	c.Require().NotNil(req.Since, "Since on the wire")
	c.True(req.Since.AsTime().Unix() > 0, "Since = %v, want the resolved --since", req.Since)
	c.Nil(req.Until, "an unset --until must ride as a nil message, got %v", req.Until)
}

// A positional conversation id switches to the per-conversation stats and the
// filter flags are not bound at all.
func TestConversationsStatsSendsConversationID(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.statsResp = statsResponse(t, sampleStats())

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{"conv-abc"})
	c.Require().NoError(cmd.Execute(), "conversations stats conv-abc")

	calls := stub.recorded()
	c.Require().Len(calls, 1, "got %d ConversationStats calls, want 1", len(calls))
	c.Eq("conv-abc", calls[0].GetConversationId(), "ConversationId")
}

// The conversation path attaches rafiki's precise reasons as ErrorInfo
// details, which connect's own rendering drops; the stderr shape must surface
// the reason (`<reason>: <message>`), and it must survive a real client round
// trip — the stub serves the decorated error, the command decodes it.
func TestConversationsStatsErrorCarriesReason(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.statsErr = rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child c_1 already exited")),
		protocol.ErrChildExited)

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{"conv-abc"})
	err := cmd.Execute()
	c.Require().Error(err, "expected an error")
	want := "child_exited: child c_1 already exited"
	c.Eq(want, err.Error(), "error")
}

// An error with no attached reason renders as connect's own `<code>:
// <message>` — the same shape the other Connect-backed verbs print.
func TestConversationsSearchErrorWithoutReason(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.searchErr = connect.NewError(connect.CodeNotFound, errors.New("no such conversation"))

	cmd := newConversationsSearchCmd()
	err := cmd.Execute()
	c.Require().Error(err, "expected an error")
	c.Eq("not_found: no such conversation", err.Error(), "error")
}

// The three infrastructure codes keep diagnoseConnectError's endpoint advice:
// "cannot reach the daemon" must name the socket so a wrong profile is
// diagnosable, instead of collapsing to a bare "unavailable".
func TestConversationsStatsUnreachableNamesTheSocket(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()

	// A profile whose socket answers nothing.
	dir := t.TempDir()
	c.Require().NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: filepath.Join(dir, "controller.sock")},
	}}), "Save")
	c.Require().NoError(profile.SavePointer("scratch"), "SavePointer")

	cmd := newConversationsStatsCmd()
	cmd.SetArgs([]string{"conv-abc"})
	err := cmd.Execute()
	c.Require().Error(err, "expected an error")
	for _, want := range []string{"cannot reach the rafiki daemon at", "is rafikid running", filepath.Join(dir, "controller.sock")} {
		c.StrContains(err.Error(), want, "error")
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
			Status: "completed", DrivenBy: "client", CreatedAt: timestamppb.New(time.Unix(1716000000, 0)),
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
	c := assert.NewCollecting(t)
	rows := sampleSummaries()

	var want bytes.Buffer
	c.Require().NoError(conversationview.RenderSearch(&want, rows))

	var got bytes.Buffer
	c.Require().NoError(renderSearchResponse(&got, conversationview.ModeTable, searchResponse(sampleProtoSummaries())))

	c.Eq(want.String(), got.String(), "rendered output differs from rafikid agent search\n--- got ---\n")
}

// The JSON arm must print the rows bare — no envelope — so
// `rafiki conversations search -o json | jq '.[]'` behaves like
// `rafikid agent search -J | jq '.[]'`, with every field the wire carries
// intact (including the rebuilt timestamp).
func TestConversationsSearchJSONEmitsBareRows(t *testing.T) {
	c := assert.NewCollecting(t)
	var got bytes.Buffer
	c.Require().NoError(renderSearchResponse(&got, conversationview.ModeJSON, searchResponse(sampleProtoSummaries())))

	var back []insightstypes.ConversationSummary
	err := json.Unmarshal(got.Bytes(), &back)
	c.Require().NoError(err, "output is not a bare rows array: %v\n%s", err, got.String())
	want := sampleSummaries()
	c.Require().Len(back, len(want), "got %d rows, want", len(back))
	for i := range want {
		if !back[i].CreatedAt.Equal(want[i].CreatedAt) {
			t.Errorf("row %d CreatedAt = %v, want %v", i, back[i].CreatedAt, want[i].CreatedAt)
		}
		back[i].CreatedAt = want[i].CreatedAt
		c.EqDiff(want[i], back[i], "row %d did not survive the round trip:\n got %+v\nwant", i, back[i])
	}
}

// An empty result renders the renderer's honest empty line, and the JSON arm
// emits `[]`, not null.
func TestConversationsSearchEmptyRendersEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	var tableBuf bytes.Buffer
	c.Require().NoError(renderSearchResponse(&tableBuf, conversationview.ModeTable, searchResponse(nil)))
	c.Eq("no conversations found\n", tableBuf.String(), "empty table rendered")

	var jsn bytes.Buffer
	c.Require().NoError(renderSearchResponse(&jsn, conversationview.ModeJSON, searchResponse(nil)))
	c.Eq("[]", strings.TrimSpace(jsn.String()), "empty JSON rendered %q, want []", jsn.String())
}

// Search's optional since/until must stay ABSENT when unset (nil pointers on
// the wire) and present when set — the wire's optional shape exists so unset
// never reads as measured.
func TestConversationsSearchSendsFiltersOverConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.searchResp = searchResponse(sampleProtoSummaries())

	cmd := newConversationsSearchCmd()
	cmd.SetArgs([]string{
		"--owner", "alice", "--path", "proxy", "--status", "completed",
		"--min-tokens", "5", "--text", "stats", "--limit", "3", "--since", "24h",
		"--until", "2026-01-02T03:04:05Z",
	})
	c.Require().NoError(cmd.Execute(), "conversations search")

	calls := stub.searched()
	c.Require().Len(calls, 1, "got %d ConversationSearch calls, want 1", len(calls))
	req := calls[0]
	c.False(req.Owner != "alice" || req.Path != "proxy" || req.Status != "completed" ||
		req.MinTokens != 5 || req.Text != "stats" || req.Limit != 3, "filters did not reach the wire: %+v", req)
	if req.Since == nil || req.Since.AsTime().Unix() <= 0 {
		t.Errorf("Since = %v, want the resolved --since", req.Since)
	}
	until, err := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	c.Require().NoError(err)
	if req.Until == nil || req.GetUntil().AsTime().Unix() != until.Unix() {
		t.Errorf("Until = %v, want the parsed RFC3339 --until (%d)", req.Until, until.Unix())
	}

	// Without the flags: absent, not present-zero.
	stub = newConversationsHarness(t)
	stub.searchResp = searchResponse(sampleProtoSummaries())
	cmd = newConversationsSearchCmd()
	c.Require().NoError(cmd.Execute(), "conversations search (no filters)")
	calls = stub.searched()
	c.Require().Len(calls, 1, "got %d ConversationSearch calls, want 1", len(calls))
	req = calls[0]
	if req.Since != nil || req.Until != nil {
		t.Errorf("unset --since/--until must ride absent, got %v/%v", req.Since, req.Until)
	}
}

// --limit is narrowed to the proto's int32; a value that would truncate must
// error rather than silently send the wrong number.
func TestConversationsSearchLimitBeyondInt32(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.searchResp = searchResponse(nil)

	cmd := newConversationsSearchCmd()
	cmd.SetArgs([]string{"--limit", "9999999999"})
	err := cmd.Execute()
	c.Require().Error(err, "expected --limit beyond int32 to error")
	c.StrContains(err.Error(), "--limit", "error")
	c.Empty(stub.searched(), "a rejected request must not reach the wire, got %d calls", len(stub.searched()))
}

// The removed closed filter must leave no CLI surface: --open and --closed are
// not registered flags on `conversations search` any more.
func TestConversationsSearchHasNoOpenClosedFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newConversationsSearchCmd()
	c.Nil(cmd.Flags().Lookup("open"), "--open must be gone")
	c.Nil(cmd.Flags().Lookup("closed"), "--closed must be gone")
}

// closed_at is a Timestamp: present means the conversation was closed (at that
// moment), absent means it is still open and must map to a nil ClosedAt, never
// a zero time.
func TestSummaryFromProtoMapsClosedAt(t *testing.T) {
	c := assert.NewCollecting(t)
	want := time.Unix(1716003600, 0).UTC()
	got := summaryFromProto(&rafikiv1.ConversationSummary{
		Id: "conv-closed", CreatedAt: timestamppb.New(time.Unix(1716000000, 0)),
		ClosedAt: timestamppb.New(time.Unix(1716003600, 0)),
	})
	c.Require().NotNil(got.ClosedAt, "ClosedAt")
	c.True(got.ClosedAt.Equal(want), "ClosedAt = %v, want %v", got.ClosedAt, want)

	open := summaryFromProto(&rafikiv1.ConversationSummary{Id: "conv-open", CreatedAt: timestamppb.New(time.Unix(1716000000, 0))})
	c.Nil(open.ClosedAt, "an unset closed_at must map to a nil ClosedAt")
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
				Latency: durationpb.New(1500 * time.Millisecond), ServedProvider: "openrouter"},
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
	c := assert.NewCollecting(t)
	var want bytes.Buffer
	c.Require().NoError(conversationview.RenderTranscriptMD(&want, sampleTranscript()))

	var got bytes.Buffer
	c.Require().NoError(renderExportResponse(&got, conversationview.ModeTable, sampleProtoExport()))

	c.Eq(want.String(), got.String(), "rendered output differs from rafikid agent export\n--- got ---\n")
}

// JSON mode must round-trip EVERY field the wire carries — including the
// nil-vs-measured-zero distinction on the optional metrics, which the markdown
// renderer does not print and so cannot pin. Content is compared semantically:
// json.MarshalIndent re-indents embedded json.RawMessage (old -j behavior too,
// so the output stays byte-identical to before), so raw byte equality of
// Content would fail on whitespace that carries no meaning.
func TestConversationsExportJSONRoundTripsEveryField(t *testing.T) {
	c := assert.NewCollecting(t)
	var got bytes.Buffer
	c.Require().NoError(renderExportResponse(&got, conversationview.ModeJSON, sampleProtoExport()))

	var back insightstypes.Transcript
	if err := json.Unmarshal(got.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.String())
	}
	want := *sampleTranscript()
	c.Require().Len(back.Turns, len(want.Turns), "got %d turns, want", len(back.Turns))
	for i := range want.Turns {
		var gotBlocks, wantBlocks []any
		c.Require().NoError(json.Unmarshal(back.Turns[i].Content, &gotBlocks), "turn %d content is not a JSON array", i)
		c.Require().NoError(json.Unmarshal(want.Turns[i].Content, &wantBlocks), "want turn %d content", i)
		c.EqDeep(wantBlocks, gotBlocks, "turn %d content did not survive the round trip", i)
		// Everything but Content must be exactly equal — the pointers on the
		// optional metrics are the point: nil is "not reported", 0 measured.
		back.Turns[i].Content, want.Turns[i].Content = nil, nil
		c.EqDiff(want.Turns[i], back.Turns[i], "turn %d did not survive the round trip:\n got %+v\nwant", i, back.Turns[i])
	}
	back.Turns, want.Turns = nil, nil
	c.EqDiff(want, back, "transcript header did not survive the round trip:\n got")
}

func TestConversationsExportSendsIDOverConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.exportResp = sampleProtoExport()

	cmd := newConversationsExportCmd()
	cmd.SetArgs([]string{"conv-abc"})
	c.Require().NoError(cmd.Execute(), "conversations export")

	calls := stub.exported()
	c.Require().Len(calls, 1, "got %d ConversationExport calls, want 1", len(calls))
	c.Eq("conv-abc", calls[0].GetConversationId(), "ConversationId")
}

// A scope miss or a missing conversation reads as not_found (the daemon makes
// the two indistinguishable), rendered in connect's code-and-message shape.
func TestConversationsExportNotFound(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.exportErr = connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))

	cmd := newConversationsExportCmd()
	cmd.SetArgs([]string{"conv-abc"})
	err := cmd.Execute()
	c.Require().Error(err, "expected an error")
	got := err.Error()
	c.Eq("not_found: conversation not found", got, "error = %q, want \"not_found: conversation not found\"", got)
}

// ─── query ──────────────────────────────────────────────────────────────────

func TestConversationsQueryCmd_FlagsRegistered(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newConversationsQueryCmd()
	for _, name := range []string{"since", "until", "owner", "persona", "source", "model", "path"} {
		c.NotNil(cmd.Flags().Lookup(name), "flag --%s not registered", name)
	}
	c.Require().NotNil(cmd.Args, "Args validator not set")
	c.NoError(cmd.Args(cmd, []string{"tools"}), "one arg should validate")
	c.Error(cmd.Args(cmd, nil), "expected error with zero args")

	root := newRootCmd()
	_, _, err := root.Find([]string{"conversations", "query"})
	c.NoError(err, "query not reachable from the command tree")
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
	c := assert.NewCollecting(t)
	var got bytes.Buffer
	c.Require().NoError(renderQueryResponse(&got, conversationview.ModeTable, sampleQueryResponse()))
	for _, tc := range []struct {
		name, want string
	}{
		{"string cell", "bash"},
		{"int cell renders bare, no decimals", "606"},
		{"usd format", "$0.0042"},
		{"pct format", "50%"},
		{"unformatted float", "0.73"},
	} {
		c.StrContains(got.String(), tc.want, "%s: output missing %q:\n", tc.name, tc.want)
	}
}

// The JSON path must emit real typed values — int and float cells decode as
// numbers, never strings — which is the whole reason Q2/Q3 typed the wire.
func TestRenderQueryResponseJSONTypedValues(t *testing.T) {
	c := assert.NewCollecting(t)
	var got bytes.Buffer
	c.Require().NoError(renderQueryResponse(&got, conversationview.ModeJSON, sampleQueryResponse()))

	var back struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	err := json.Unmarshal(got.Bytes(), &back)
	c.Require().NoError(err, "output is not valid JSON: %v\n%s", err, got.String())
	c.False(len(back.Columns) != 5 || back.Columns[2] != "cost", "columns did not survive: %+v", back.Columns)
	row := back.Rows[0]
	c.Require().Len(row, 5, "row width %d, want 5", len(row))
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
	c := assert.NewCollecting(t)
	cmd := newConversationsReviewCmd()
	for _, name := range []string{"stage", "model", "analyzer-profile", "budget-usd", "min-turns", "force"} {
		c.NotNil(cmd.Flags().Lookup(name), "flag --%s not registered", name)
	}
	// The analyzer-profile flag must not be named "profile": profile
	// resolution (profile_glue.go) reads a flag by that name off the
	// command's own flag set as the DAEMON profile selection, so a local one
	// would make `--profile sql` exit(2) on "unknown profile sql" instead of
	// naming the analyzer profile the request field wants.
	c.Nil(cmd.Flags().Lookup("profile"), "a local --profile shadows the client's global -P/--profile; the analyzer-profile flag is --analyzer-profile")
	c.Eq("detect", cmd.Flags().Lookup("stage").DefValue, "--stage default")
	if _, _, err := newRootCmd().Find([]string{"conversations", "review"}); err != nil {
		t.Errorf("review not reachable from the command tree: %v", err)
	}
	_, _, err := newRootCmd().Find([]string{"conversations", "findings"})
	c.NoError(err, "findings not reachable from the command tree")
}

func TestConversationsFindingsCmd_FlagsRegistered(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newConversationsFindingsCmd()
	for _, name := range []string{"axis", "skill", "status", "limit"} {
		c.NotNil(cmd.Flags().Lookup(name), "flag --%s not registered", name)
	}
	c.Error(cmd.Args(cmd, []string{"extra"}), "expected error for a positional arg (findings takes none)")
	c.NoError(cmd.Args(cmd, nil), "no args should validate")
}

// --stage is a closed set: anything else errors before any dial.
func TestConversationsReviewStageValidated(t *testing.T) {
	c := assert.NewCollecting(t)
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
		c.Require().NoError(cmd.Flags().Set("stage", tc.stage), "set stage %q", tc.stage)
		got, err := reviewStageFlag(cmd)
		c.False(tc.ok && err != nil, "stage %q: unexpected error %v", tc.stage, err)
		c.False(!tc.ok && err == nil, "stage %q: expected an error, got none", tc.stage)
		c.Eq(tc.want, got, "stage %q: got %v, want", tc.stage, got)
	}
}

// The config file fills what the flags leave unset and never overwrites a
// flag — design §3's resolution order, over the real wire. The target is
// resolved by NAME through the Connect stub's ListChildren: review resolves
// through resolveTargetConnect like every other child-target verb, so the
// request's ids are real child ids resolved over Connect.
func TestConversationsReviewMergesConfigFileButFlagsWin(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.children = []*rafikiv1.ChildSummary{{
		ChildId: "c_9", Name: "my-agent", Status: "exited",
	}}
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_9", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	}}}

	cfgPath := filepath.Join(t.TempDir(), "review.json")
	c.Require().NoError(os.WriteFile(cfgPath, []byte(
		`{"model":"file-model","profile":"file-profile","budget_usd":2.5,"min_turns":4}`), 0o600))
	t.Setenv("RAFIKI_REVIEW_CONFIG", cfgPath)

	cmd := newConversationsReviewCmd()
	// --analyzer-profile is the review verb's own flag (the analyzer
	// profile): it must reach the request, not the daemon-profile selection.
	cmd.SetArgs([]string{"my-agent", "--model", "flag-model", "--min-turns", "8", "--analyzer-profile", "flag-profile"})
	c.Require().NoError(cmd.Execute(), "conversations review")

	calls := stub.reviews()
	c.Require().Len(calls, 1, "got %d ConversationReview calls, want 1", len(calls))
	req := calls[0]
	if ids := req.GetConversationIds(); len(ids) != 1 || ids[0] != "c_9" {
		t.Errorf("ConversationIds = %v, want [c_9] (resolved from the name)", ids)
	}
	c.Eq(rafikiv1.ReviewStage_REVIEW_STAGE_DETECT, req.Stage, "Stage")
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
	c.Nil(req.Force, "Force")
}

// An unresolvable review target fails with resolveTargetConnect's matching
// error, naming the identifier that matched nothing, and must not send the
// review.
func TestConversationsReviewUnresolvedTarget(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newConversationsHarness(t)
	stub.children = []*rafikiv1.ChildSummary{{
		ChildId: "c_9", Name: "my-agent", Status: "exited",
	}}

	cmd := newConversationsReviewCmd()
	cmd.SetArgs([]string{"no-such-agent"})
	err := cmd.Execute()
	c.Require().Error(err, "expected an error")
	want := `resolve "no-such-agent": no child matches "no-such-agent"`
	c.Eq(want, err.Error(), "error")
	c.Empty(stub.reviews(), "a failed resolution must not send the review, got %d calls", len(stub.reviews()))
}

func TestConversationsReviewRendersPerIDStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	err := renderReviewAccepts(&buf, []*rafikiv1.ConversationReviewAccept{
		{ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED},
		{ConversationId: "c_2", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ALREADY_RUNNING},
		{ConversationId: "c_3", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL},
	})
	c.Require().NoError(err)
	want := "c_1: enqueued\nc_2: already running\nc_3: queue full\n"
	c.Eq(want, buf.String(), "rendered")
}

// --force rides the request, and findings filters reach the wire verbatim:
// status passes through (the daemon defaults "" to open), limit 0 means the
// daemon's default.
func TestConversationsFindingsSendsFilters(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.findingsResp = &rafikiv1.ConversationFindingsResponse{}

	cmd := newConversationsFindingsCmd()
	cmd.SetArgs([]string{"--axis", "prompt", "--skill", "sql", "--status", "dismissed", "--limit", "5"})
	c.Require().NoError(cmd.Execute(), "conversations findings")

	calls := stub.findings()
	c.Require().Len(calls, 1, "got %d ConversationFindings calls, want 1", len(calls))
	req := calls[0]
	c.False(req.Axis != "prompt" || req.Skill != "sql" || req.Status != "dismissed" || req.Limit != 5, "filters did not reach the wire: %+v", req)
}

func TestConversationsFindingsRendersRows(t *testing.T) {
	c := assert.NewCollecting(t)
	resp := &rafikiv1.ConversationFindingsResponse{
		Findings: []*rafikiv1.ReviewFinding{
			{Id: "f1", ConversationId: "conv-a", Axis: "prompt", SkillName: "sql",
				Title: "N+1 queries", ExpectedSavingsTokens: 12000, Status: "open"},
			{Id: "f2", ConversationId: "conv-b", Axis: "tools",
				Title: "Redundant greps", ExpectedSavingsTokens: 300, Status: "open"},
		},
		Analyses: []*rafikiv1.ReviewAnalysis{{
			Id: "a1", ConversationId: "conv-a", Model: "anthropic/claude-sonnet-5",
			Status: "ok", CostUsd: 0.0123, CreatedAt: timestamppb.New(time.Unix(1716000000, 0)),
		}},
	}

	var buf bytes.Buffer
	c.Require().NoError(renderFindingsResponse(&buf, outputTable, resp))
	out := buf.String()
	for _, want := range []string{
		"Axis", "Skill", "Title", "Savings", "Status", "ID",
		"N+1 queries", "12,000", "open", "f1", "f2",
		"Analysis", "Conversation", "Model", "Cost", "Created",
		"a1", "conv-a", "anthropic/claude-sonnet-5", "$0.0123",
	} {
		c.StrContains(out, want, "findings table missing")
	}

	// Neither collection present: the honest "no findings" line, matching
	// rafikid agent findings' empty rendering.
	var empty bytes.Buffer
	c.Require().NoError(renderFindingsResponse(&empty, outputTable, &rafikiv1.ConversationFindingsResponse{}))
	c.Eq("no findings\n", empty.String(), "empty response rendered %q, want \"no findings\\n\"", empty.String())
}

// JSON mode carries the WHOLE response — findings and analyses — so jq sees
// the same shape the daemon sent, and JSONL is one finding per line.
func TestConversationsFindingsJSONModes(t *testing.T) {
	c := assert.NewCollecting(t)
	resp := &rafikiv1.ConversationFindingsResponse{
		Findings: []*rafikiv1.ReviewFinding{{
			Id: "f1", ConversationId: "conv-a", Axis: "prompt", Title: "t",
			ExpectedSavingsTokens: 10, Status: "open",
		}},
		Analyses: []*rafikiv1.ReviewAnalysis{{Id: "a1", ConversationId: "conv-a", Status: "ok"}},
	}

	var pretty bytes.Buffer
	c.Require().NoError(renderFindingsResponse(&pretty, outputJSON, resp))
	var back struct {
		Findings []map[string]any `json:"findings"`
		Analyses []map[string]any `json:"analyses"`
	}
	if err := json.Unmarshal(pretty.Bytes(), &back); err != nil {
		t.Fatalf("json output invalid: %v\n%s", err, pretty.String())
	}
	c.False(len(back.Findings) != 1 || len(back.Analyses) != 1, "json output lost rows: %+v", back)

	var lines bytes.Buffer
	c.Require().NoError(renderFindingsResponse(&lines, outputJSONL, resp))
	n := strings.Count(strings.TrimSpace(lines.String()), "\n") + 1
	c.Eq(1, n, "jsonl rendered %d lines, want one finding per line:\n%s", n, lines.String())
}

func TestUnixOrZero(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(0, unixOrZero(nil), "nil: got")
	tm := time.Unix(1716000000, 0)
	c.Eq(1716000000, unixOrZero(&tm), "got")
}

func TestTimeTS(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Nil(timeTS(nil), "nil: got")
	tm := time.Unix(1716000000, 0)
	got := timeTS(&tm)
	c.False(got == nil || got.AsTime().Unix() != 1716000000, "got %v, want 1716000000", got)
}

func int64Ptr(v int64) *int64 { return &v }
