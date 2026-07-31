// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/rafiki/agentcli"
	"git.graveland.dev/brent/rafiki/analyze"
	"git.graveland.dev/brent/rafiki/insights"
	"git.graveland.dev/brent/rafiki/llm"
	"git.graveland.dev/brent/rafiki/store"
)

// The scripted-sender pattern below duplicates analyze/detect_test.go's
// fakeSender/cannedMessage/respond* helpers: those are unexported to package
// analyze and cannot be imported from here.

type analyzeFakeSender struct {
	calls   int
	scripts []func(anthropic.MessageNewParams) (*anthropic.Message, error)
}

func (s *analyzeFakeSender) New(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	i := s.calls
	if i >= len(s.scripts) {
		i = len(s.scripts) - 1
	}
	s.calls++
	return s.scripts[i](params)
}

func analyzeCannedMessage(t *testing.T, raw string) *anthropic.Message {
	t.Helper()
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("analyzeCannedMessage: %v", err)
	}
	return &m
}

// analyzeRespondToolUse replies with a single report_findings tool_use block
// carrying inputJSON.
func analyzeRespondToolUse(t *testing.T, inputJSON string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"content":[{"type":"tool_use","id":"tu_1","name":"report_findings","input":` + inputJSON + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`
	msg := analyzeCannedMessage(t, raw)
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) { return msg, nil }
}

// analyzeRespondToolUseNamed replies with a single tool_use block for the
// given tool name, carrying its own fixed usage (input_tokens=40,
// output_tokens=77) distinct from analyzeRespondToolUse's (100/50) — used for
// Draft's propose_skill_edit responses, which share a *llm.Client (and so an
// analyzeFakeSender's scripted call sequence) with the preceding Detect
// call's report_findings response, so a test folding both stages' usage into
// Summary.Totals can tell them apart.
func analyzeRespondToolUseNamed(t *testing.T, name, inputJSON string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	t.Helper()
	raw := `{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"content":[{"type":"tool_use","id":"tu_2","name":"` + name + `","input":` + inputJSON + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":40,"output_tokens":77}}`
	msg := analyzeCannedMessage(t, raw)
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) { return msg, nil }
}

// analyzeRespondTextOnly replies with plain text (no tool_use block): the
// "detector didn't call the tool" malformed case.
func analyzeRespondTextOnly(text string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
			`"content":[{"type":"text","text":"` + text + `"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":50}}`
		var m anthropic.Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			panic(err)
		}
		return &m, nil
	}
}

func testAnalyzeClient(t *testing.T, sender llm.Sender) *llm.Client {
	t.Helper()
	// WithDefaultModel is required: the client invents no default model, and
	// Profile.Defaults() deliberately leaves DetectorModel/DraftModel unset, so
	// a zero-value Profile resolves its model from this default.
	c, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, sender),
		llm.WithDefaultModel("haiku-latest"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

const analyzeWellFormedInput = `{
	"outcome": "agent diagnosed replication lag from a stuck WAL sender",
	"verdicts": {"skill-gap": "ok", "knowledge-to-persist": "finding", "grind": "ok"},
	"findings": [{
		"axis": "knowledge-to-persist",
		"title": "WAL sender stuck behind a long-running query on the replica",
		"topic_key": "wal-sender-stuck-long-query",
		"evidence": [{"ordinal": 1, "quote": "hi"}],
		"recommendation": {"kind": "memory", "summary": "record the diagnosis pattern"},
		"confidence": 0.8
	}]
}`

// analyzeWellFormedInputNewSkill is analyzeWellFormedInput's sibling with a
// draft-eligible recommendation (kind="new-skill"), for tests that need
// Draft to actually run after Detect.
const analyzeWellFormedInputNewSkill = `{
	"outcome": "agent invented a bespoke pgbouncer restart runbook from scratch",
	"verdicts": {"skill-gap": "finding", "knowledge-to-persist": "ok", "grind": "ok"},
	"findings": [{
		"axis": "skill-gap",
		"title": "missing pgbouncer restart runbook",
		"topic_key": "pgbouncer-restart-runbook",
		"evidence": [{"ordinal": 1, "quote": "hi"}],
		"recommendation": {"kind": "new-skill", "skill_name": "pgbouncer-restart", "summary": "codify the restart steps"},
		"confidence": 0.8
	}]
}`

func drainEvents(ch <-chan agentcli.AnalyzeEvent) []agentcli.AnalyzeEvent {
	var out []agentcli.AnalyzeEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// TestAnalyzeSingleIDFullPipeline covers the happy path end to end: a single
// explicit conversation id runs through Export/Compact/Detect and is stored,
// emitting progress -> analysis -> summary in that order.
func TestAnalyzeSingleIDFullPipeline(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (progress, analysis, summary); got %+v", len(events), events)
	}
	if events[0].Kind != agentcli.EventProgress || events[0].Progress.State != agentcli.StateDone {
		t.Errorf("events[0] = %+v, want progress/done", events[0])
	}
	if events[1].Kind != agentcli.EventAnalysis || events[1].Analysis == nil {
		t.Errorf("events[1] = %+v, want analysis", events[1])
	}
	if events[2].Kind != agentcli.EventSummary || events[2].Summary == nil {
		t.Fatalf("events[2] = %+v, want summary", events[2])
	}
	if events[2].Summary.Analyzed != 1 {
		t.Errorf("Summary.Analyzed = %d, want 1", events[2].Summary.Analyzed)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("conversation_analysis rows = %d, want 1", n)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM conversations.analysis_finding af
		  JOIN conversations.conversation_analysis ca ON ca.id = af.analysis_id
		 WHERE ca.conversation_id = $1::uuid`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("analysis_finding rows = %d, want 1", n)
	}
}

// TestAnalyzeSkipThenForce covers skip-detection: a second run without Force
// reports the conversation as skipped (its stored analysis still feeding
// Rank), and a third run with Force re-analyzes it.
func TestAnalyzeSkipThenForce(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	if err != nil {
		t.Fatalf("Analyze (first run): %v", err)
	}
	first := drainEvents(ch)
	if first[len(first)-1].Summary.Analyzed != 1 {
		t.Fatalf("first run Summary.Analyzed = %d, want 1", first[len(first)-1].Summary.Analyzed)
	}

	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	if err != nil {
		t.Fatalf("Analyze (second run): %v", err)
	}
	second := drainEvents(ch)
	var sawSkip bool
	for _, ev := range second {
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateSkipped {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Errorf("second run: no skipped progress event; events = %+v", second)
	}
	summary := second[len(second)-1].Summary
	if summary.Skipped != 1 || summary.Analyzed != 0 {
		t.Errorf("second run Summary = %+v, want Skipped=1 Analyzed=0", summary)
	}
	if len(summary.Ranked) == 0 {
		t.Error("second run Summary.Ranked is empty, want the skipped conversation's stored finding to still rank")
	}

	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile, Force: true})
	if err != nil {
		t.Fatalf("Analyze (forced run): %v", err)
	}
	third := drainEvents(ch)
	if third[len(third)-1].Summary.Analyzed != 1 {
		t.Errorf("forced run Summary.Analyzed = %d, want 1", third[len(third)-1].Summary.Analyzed)
	}
}

// TestAnalyzeCanonicalizesConversationIDCase covers Fix 1: a conversation id
// passed uppercase-cased must canonicalize to the same lowercase form
// Postgres stores, so a second run recognizes it as the SAME conversation
// already analyzed (skip-detected, not silently re-analyzed as new) — and,
// critically, replaceFindingsPerAnalysis's Conversations-slice matching
// (slices.Contains, string-equality) must not fail across the case
// mismatch and wipe the conversation's existing findings.
func TestAnalyzeCanonicalizesConversationIDCase(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	if err != nil {
		t.Fatalf("Analyze (first run, canonical id): %v", err)
	}
	first := drainEvents(ch)
	if first[len(first)-1].Summary.Analyzed != 1 {
		t.Fatalf("first run Summary.Analyzed = %d, want 1", first[len(first)-1].Summary.Analyzed)
	}
	before := snapshotAnalysisFindings(t, pool)
	if len(before) != 1 {
		t.Fatalf("first run stored %d analysis_finding rows, want 1", len(before))
	}

	upper := strings.ToUpper(convID)
	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{upper}, Profile: profile})
	if err != nil {
		t.Fatalf("Analyze (second run, uppercase id): %v", err)
	}
	second := drainEvents(ch)
	for _, ev := range second {
		if ev.Kind == agentcli.EventError {
			t.Fatalf("unexpected EventError on uppercase-id run: %v", ev.Err)
		}
	}
	summary := second[len(second)-1].Summary
	if summary == nil {
		t.Fatal("second run produced no Summary")
	}
	if summary.Skipped != 1 || summary.Analyzed != 0 {
		t.Errorf("second run (uppercase id) Summary = %+v, want Skipped=1 Analyzed=0", summary)
	}
	if len(summary.Ranked) == 0 {
		t.Error("second run Summary.Ranked is empty, want the carried-over finding to still rank")
	}

	// A normal (non-NoStore) skip is allowed to rewrite the finding row via
	// replaceFindingsPerAnalysis (recomputing ranked findings fresh each
	// run) — only --no-store promises byte-for-byte row identity
	// (TestAnalyzeNoStoreDoesNotRewriteExistingFindings covers that). What
	// Fix 1 guarantees here is that the finding is never lost: same count,
	// same content, across a re-run whose only difference is the input id's
	// case.
	after := snapshotAnalysisFindings(t, pool)
	if len(after) != len(before) {
		t.Fatalf("analysis_finding row count changed across the case-mismatched re-run: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].expectedSavingsTokens != after[i].expectedSavingsTokens || before[i].status != after[i].status {
			t.Errorf("analysis_finding row %d content changed under an uppercase-cased re-run: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

// TestAnalyzeDetectFailure covers a Detect that fails after its internal
// one-retry: the batch keeps going (no channel error), a failed row is
// recorded, and the failure surfaces via progress + Summary.Failed.
func TestAnalyzeDetectFailure(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondTextOnly("forgot the tool"),
		analyzeRespondTextOnly("forgot it again"),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	for _, ev := range events {
		if ev.Kind == agentcli.EventError {
			t.Fatalf("unexpected EventError: %v", ev.Err)
		}
	}
	var failProgress *agentcli.Progress
	for _, ev := range events {
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateFailed {
			failProgress = ev.Progress
		}
	}
	if failProgress == nil || failProgress.Detail == "" {
		t.Fatalf("no failed progress event with non-empty Detail; events = %+v", events)
	}
	summary := events[len(events)-1].Summary
	if summary == nil || summary.Failed != 1 {
		t.Errorf("Summary = %+v, want Failed=1", summary)
	}

	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&status); err != nil {
		t.Fatalf("query status row: %v", err)
	}
	if status != "failed" {
		t.Errorf("stored status = %q, want failed", status)
	}
}

// TestAnalyzeStopAfterCompact covers stop_after=="compact": the run emits a
// Transcript-carrying analysis event per conversation and never writes a
// conversation_analysis row.
func TestAnalyzeStopAfterCompact(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
		StopAfter:       "compact",
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	var sawTranscript bool
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Transcript != nil {
			sawTranscript = true
		}
	}
	if !sawTranscript {
		t.Fatalf("no analysis event carrying a Transcript; events = %+v", events)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("conversation_analysis rows = %d, want 0", n)
	}
}

// TestAnalyzeNoStore covers NoStore: the run still emits an analysis event
// but writes nothing to the database.
func TestAnalyzeNoStore(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
		NoStore:         true,
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	var sawAnalysis bool
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Analysis != nil {
			sawAnalysis = true
		}
	}
	if !sawAnalysis {
		t.Fatalf("no analysis event with a non-nil Analysis; events = %+v", events)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("conversation_analysis rows = %d, want 0", n)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.analysis_finding`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("analysis_finding rows = %d, want 0", n)
	}
}

// analysisFindingSnapshot is (id, expected_savings_tokens, status) for every
// conversations.analysis_finding row — enough to prove a run touched (or
// didn't touch) existing findings: an untouched row keeps its own id (no
// DELETE-then-INSERT cycle occurred), not just equal values.
type analysisFindingSnapshot struct {
	id                    string
	expectedSavingsTokens int64
	status                string
}

func snapshotAnalysisFindings(t *testing.T, pool *pgxpool.Pool) []analysisFindingSnapshot {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, expected_savings_tokens, status FROM conversations.analysis_finding ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot analysis_finding: %v", err)
	}
	defer rows.Close()
	var out []analysisFindingSnapshot
	for rows.Next() {
		var s analysisFindingSnapshot
		if err := rows.Scan(&s.id, &s.expectedSavingsTokens, &s.status); err != nil {
			t.Fatalf("snapshot analysis_finding: scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot analysis_finding: %v", err)
	}
	return out
}

// TestAnalyzeNoStoreDoesNotRewriteExistingFindings covers the destructive-write
// bug: a --no-store run must never call store.ReplaceFindings (DELETE-then-
// INSERT) against a conversation that already has stored findings from an
// earlier, separate run — a prompt-iteration NoStore run silently rescoring
// stored data would contradict NoStore's own contract. The skipped
// conversation's carried-over analysis still feeds Rank (read-only), it just
// must never be written back.
func TestAnalyzeNoStoreDoesNotRewriteExistingFindings(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	// First run: normal (stored) analysis, producing a real finding row.
	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	if err != nil {
		t.Fatalf("Analyze (first run): %v", err)
	}
	first := drainEvents(ch)
	if first[len(first)-1].Summary.Analyzed != 1 {
		t.Fatalf("first run Summary.Analyzed = %d, want 1", first[len(first)-1].Summary.Analyzed)
	}
	before := snapshotAnalysisFindings(t, pool)
	if len(before) == 0 {
		t.Fatal("first run stored no analysis_finding rows to compare against")
	}

	// Second run: same detector key (so this conversation is skipped, not
	// re-detected) but NoStore. Before the fix, storedAnalysesFor's
	// carryOver was appended to forReplace unconditionally, so this call
	// silently rewrote the row above via ReplaceFindings' DELETE+INSERT.
	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID}, Profile: profile, NoStore: true,
	})
	if err != nil {
		t.Fatalf("Analyze (no-store run): %v", err)
	}
	second := drainEvents(ch)
	summary := second[len(second)-1].Summary
	if summary.Skipped != 1 {
		t.Fatalf("no-store run Summary.Skipped = %d, want 1 (conversation must be skip-detected, not re-detected)", summary.Skipped)
	}
	if len(summary.Ranked) == 0 {
		t.Error("no-store run Summary.Ranked is empty, want the carried-over finding to still rank (read-only)")
	}

	after := snapshotAnalysisFindings(t, pool)
	if len(after) != len(before) {
		t.Fatalf("analysis_finding row count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("analysis_finding row %d changed under --no-store: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

// TestAnalyzeInvalidConversationID covers up-front UUID validation: a
// malformed id must fail before any event is produced.
func TestAnalyzeInvalidConversationID(t *testing.T) {
	pool := newTestPool(t)
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{"not-a-uuid"}})
	if err == nil {
		t.Fatal("Analyze: want error for an invalid conversation id, got nil")
	}
	if ch != nil {
		t.Error("Analyze: want a nil channel alongside the error")
	}
}

// TestAnalyzeCorpusDir covers corpus mode: a directory of pre-built
// transcripts, with one broken file, is analyzed without touching the
// database at all (corpus conversations have no conversations.conversation
// row to FK an analysis_finding against, so they're implicitly NoStore).
func TestAnalyzeCorpusDir(t *testing.T) {
	pool := newTestPool(t)
	dir := t.TempDir()

	textContent := func(s string) json.RawMessage {
		b, _ := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		return b
	}
	writeTranscript := func(name, convID string) {
		tr := insights.Transcript{
			ConversationID: convID,
			Owner:          "brent",
			Persona:        "diagnose",
			Source:         "corpus",
			Turns: []insights.TranscriptTurn{
				{Ordinal: 0, Role: "user", Content: textContent("why is replica X lagging?")},
				{Ordinal: 1, Role: "assistant", Content: textContent("investigating..."), Model: "claude-haiku-4-5", InputTokens: 10, OutputTokens: 5},
			},
		}
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("conv-a.json", "corpus-conv-a")
	writeTranscript("conv-b.json", "corpus-conv-b")
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		CorpusDir: dir,
		Profile:   &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	var analyses int
	var brokenDetail string
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Analysis != nil {
			analyses++
		}
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateSkipped {
			brokenDetail = ev.Progress.Detail
		}
	}
	if analyses != 2 {
		t.Errorf("analysis events = %d, want 2", analyses)
	}
	summary := events[len(events)-1].Summary
	if summary == nil || summary.Skipped != 1 {
		t.Errorf("Summary = %+v, want Skipped=1", summary)
	}
	if brokenDetail == "" {
		t.Error("no skipped progress event carrying a reason for the broken file")
	}

	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations.conversation_analysis`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("conversation_analysis rows = %d, want 0 (corpus runs never touch the DB)", n)
	}
}

// TestAnalyzeCorpusDirSkipsArtifactsAndEmptyTranscripts covers Fix 2: a
// corpus run must not re-ingest its own prior-run artifacts
// (*.compact/.detect/.rank/.draft.json siblings of a real conversation file),
// and must treat a well-formed but empty (zero-Turns) transcript as
// unparseable rather than handing Detect nothing to work with.
func TestAnalyzeCorpusDirSkipsArtifactsAndEmptyTranscripts(t *testing.T) {
	pool := newTestPool(t)
	dir := t.TempDir()

	textContent := func(s string) json.RawMessage {
		b, _ := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		return b
	}
	writeTranscript := func(name, convID string, turns []insights.TranscriptTurn) {
		tr := insights.Transcript{ConversationID: convID, Owner: "brent", Persona: "diagnose", Source: "corpus", Turns: turns}
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	realTurns := []insights.TranscriptTurn{
		{Ordinal: 0, Role: "user", Content: textContent("why is replica X lagging?")},
		{Ordinal: 1, Role: "assistant", Content: textContent("investigating..."), Model: "claude-haiku-4-5", InputTokens: 10, OutputTokens: 5},
	}
	writeTranscript("conv-a.json", "corpus-conv-a", realTurns)
	writeTranscript("conv-a.compact.json", "corpus-conv-a", realTurns)
	writeTranscript("conv-a.detect.json", "corpus-conv-a", realTurns)
	writeTranscript("conv-a.rank.json", "corpus-conv-a", realTurns)
	writeTranscript("conv-a.draft.json", "corpus-conv-a", realTurns)
	writeTranscript("empty.json", "corpus-conv-empty", nil)

	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		CorpusDir: dir,
		Profile:   &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	var analyses int
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Analysis != nil {
			analyses++
			if ev.Analysis.ConversationID != "corpus-conv-a" {
				t.Errorf("unexpected conversation analyzed: %s", ev.Analysis.ConversationID)
			}
		}
	}
	if analyses != 1 {
		t.Fatalf("analysis events = %d, want exactly 1 (conv-a only — the compact/detect/rank/draft siblings must be excluded by name)", analyses)
	}

	summary := events[len(events)-1].Summary
	if summary == nil {
		t.Fatal("no Summary event")
	}
	if summary.Population != 2 {
		t.Errorf("Summary.Population = %d, want 2 (conv-a + empty.json; the 4 artifact siblings must not count)", summary.Population)
	}
	if summary.Skipped != 1 {
		t.Errorf("Summary.Skipped = %d, want 1 (empty.json, treated as unparseable)", summary.Skipped)
	}
}

// TestRunAnalyzeAlwaysEmitsTerminalEventOnCancelledContext covers Fix 4: a
// terminal event (EventSummary or EventError) must never be droppable by a
// raced ctx.Done() select. Corpus mode's toProcess loop checks ctx.Err() at
// the top of every iteration, so a pre-cancelled context deterministically
// reaches fail(ctx.Err()) on the very first item — before this fix, that
// path used the same racy `send` as progress/analysis events and so could
// silently drop the terminal event roughly half the time.
func TestRunAnalyzeAlwaysEmitsTerminalEventOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	tr := insights.Transcript{
		ConversationID: "corpus-conv",
		Owner:          "brent",
		Turns: []insights.TranscriptTurn{
			{Ordinal: 0, Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
		},
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conv.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	b := New(Options{LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ch, err := b.Analyze(ctx, agentcli.AnalyzeRequest{CorpusDir: dir, Profile: &analyze.Profile{DetectorModel: "claude-haiku-4-5"}})
		if err != nil {
			t.Fatalf("iteration %d: Analyze: %v", i, err)
		}
		events := drainEvents(ch)
		if len(events) == 0 {
			t.Fatalf("iteration %d: no events at all on a cancelled ctx; want a terminal event", i)
		}
		last := events[len(events)-1]
		if last.Kind != agentcli.EventSummary && last.Kind != agentcli.EventError {
			t.Fatalf("iteration %d: last event = %+v, want EventSummary or EventError", i, last)
		}
	}
}

// TestAnalyzeFoldsDraftUsageIntoTotals covers Fix 5: Draft's LLM usage
// (input/output tokens, cost) must be folded into Summary.Totals alongside
// Detect's, and recorded on the ranked finding's own Draft — not silently
// dropped, which would understate real spend for any run whose findings are
// draft-eligible.
func TestAnalyzeFoldsDraftUsageIntoTotals(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInputNewSkill),
		analyzeRespondToolUseNamed(t, "propose_skill_edit",
			`{"files":[{"path":"skills/pgbouncer-restart/SKILL.md","content":"# PgBouncer Restart\n"}],"rationale":"codify the steps"}`),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)
	summary := events[len(events)-1].Summary
	if summary == nil {
		t.Fatal("no Summary event")
	}
	if len(summary.Ranked) != 1 || summary.Ranked[0].Draft == nil {
		t.Fatalf("want exactly 1 ranked finding with a Draft attached, got %+v", summary.Ranked)
	}
	draft := summary.Ranked[0].Draft

	if summary.Totals.InputTokens != 140 {
		t.Errorf("Totals.InputTokens = %d, want 140 (100 detect + 40 draft)", summary.Totals.InputTokens)
	}
	if summary.Totals.OutputTokens != 127 {
		t.Errorf("Totals.OutputTokens = %d, want 127 (50 detect + 77 draft)", summary.Totals.OutputTokens)
	}
	if draft.InputTokens != 40 || draft.OutputTokens != 77 {
		t.Errorf("Draft usage = %+v, want InputTokens=40 OutputTokens=77", draft)
	}
}

// TestAnalyzeExportNotFoundPinsFKFix covers analyzeOne's insights.ErrNotFound
// branch: a well-formed but nonexistent conversation id must surface as a
// failed progress event WITHOUT attempting to record a failed
// conversation_analysis row — there is no conversation row for one to FK
// against, and recordFailure would itself die on the constraint. This pins
// the FK-bug fix, which was previously unpinned by any test.
func TestAnalyzeExportNotFoundPinsFKFix(t *testing.T) {
	pool := newTestPool(t)
	missingID := uuid.NewString()
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{missingID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	var sawFailed bool
	for _, ev := range events {
		if ev.Kind == agentcli.EventError {
			t.Fatalf("unexpected EventError: %v", ev.Err)
		}
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateFailed && ev.Progress.ConversationID == missingID {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Fatalf("no failed progress event for the missing conversation; events = %+v", events)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, missingID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("conversation_analysis rows for the missing id = %d, want 0", n)
	}
}

// TestAnalyzeForceKeepsDismissedFinding covers the prior-map carry-over: once
// a human dismisses a finding, a later --force re-analysis (which deletes and
// re-inserts the analysis row, cascading away the old finding row) must not
// resurrect it as 'open' just because Detect re-found the same finding.
func TestAnalyzeForceKeepsDismissedFinding(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	if err != nil {
		t.Fatalf("Analyze (first run): %v", err)
	}
	drainEvents(ch)

	findings, err := b.Findings(context.Background(), store.FindingFilter{})
	if err != nil || len(findings) != 1 {
		t.Fatalf("Findings after first run = %+v, %v; want exactly 1", findings, err)
	}
	if _, err := b.SetFindingStatus(context.Background(), findings[0].ID, "dismissed"); err != nil {
		t.Fatalf("SetFindingStatus: %v", err)
	}

	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile, Force: true})
	if err != nil {
		t.Fatalf("Analyze (forced run): %v", err)
	}
	drainEvents(ch)

	// ListFindings defaults to status=='open', which a still-dismissed
	// finding deliberately will not match — query for dismissed explicitly.
	findings, err = b.Findings(context.Background(), store.FindingFilter{Status: "dismissed"})
	if err != nil || len(findings) != 1 {
		t.Fatalf("Findings(status=dismissed) after forced re-run = %+v, %v; want exactly 1", findings, err)
	}
	if findings[0].Status != "dismissed" {
		t.Errorf("finding status after forced re-run = %q, want dismissed", findings[0].Status)
	}
}

// TestAnalyzeLimitRemainingArithmetic covers Summary.Remaining's arithmetic:
// with 2 eligible conversations and Limit 1, exactly 1 is analyzed and 1 is
// left over (population minus skipped minus processed).
func TestAnalyzeLimitRemainingArithmetic(t *testing.T) {
	pool := newTestPool(t)
	seedConversation(t, pool)
	seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		Profile: &analyze.Profile{DetectorModel: "claude-haiku-4-5", Limit: 1},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)
	summary := events[len(events)-1].Summary
	if summary == nil {
		t.Fatalf("no summary event; events = %+v", events)
	}
	if summary.Population != 2 {
		t.Errorf("Summary.Population = %d, want 2", summary.Population)
	}
	if summary.Analyzed != 1 {
		t.Errorf("Summary.Analyzed = %d, want 1", summary.Analyzed)
	}
	if summary.Remaining != 1 {
		t.Errorf("Summary.Remaining = %d, want 1", summary.Remaining)
	}
}

// TestAnalyzeInterestingnessOrder covers population's sort: an error-status
// conversation must be analyzed before a healthy one even when the healthy
// one carries far more tokens — with Limit 1, only the error-status
// conversation's id should end up processed.
func TestAnalyzeInterestingnessOrder(t *testing.T) {
	pool := newTestPool(t)
	errConvID := insertConversation(t, pool, "client", "alice")
	insertTurn(t, pool, errConvID, 0, 10, 5)
	insertMessage(t, pool, errConvID, 0, "user", `[{"type":"text","text":"hi"}]`)
	if _, err := pool.Exec(context.Background(),
		`UPDATE conversations.conversation SET status = 'error' WHERE id = $1::uuid`, errConvID); err != nil {
		t.Fatal(err)
	}

	healthyConvID := insertConversation(t, pool, "client", "alice")
	insertTurn(t, pool, healthyConvID, 0, 10000, 5000)
	insertMessage(t, pool, healthyConvID, 0, "user", `[{"type":"text","text":"hi"}]`)

	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		Profile: &analyze.Profile{DetectorModel: "claude-haiku-4-5", Limit: 1},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)

	var processedID string
	for _, ev := range events {
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateDone {
			processedID = ev.Progress.ConversationID
		}
	}
	if processedID != errConvID {
		t.Errorf("processed conversation = %q, want the error-status one %q (healthy id %q)", processedID, errConvID, healthyConvID)
	}
}

// TestAnalyzeFindingScoreIsRankedNotRaw covers Fix 1's shape: a finding that
// appears in two conversations must be persisted with
// ExpectedSavingsTokens == the ranked finding's Score, on BOTH conversations'
// analysis rows — not each conversation's own raw per-analysis token count
// (here, GrindTokens, which the canned response never sets, so a
// pre-fix implementation would persist 0 on both rows instead of Rank's
// recurrence-boosted Score).
func TestAnalyzeFindingScoreIsRankedNotRaw(t *testing.T) {
	pool := newTestPool(t)
	convA := seedConversation(t, pool)
	convB := seedConversation(t, pool)

	// Both conversations report the exact same (axis, topic_key) finding, so
	// Rank groups them into a single RankedFinding spanning both
	// conversations.
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convA, convB},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	events := drainEvents(ch)
	summary := events[len(events)-1].Summary
	if summary == nil || len(summary.Ranked) != 1 {
		t.Fatalf("Summary.Ranked = %+v, want exactly 1 ranked finding spanning both conversations", summary)
	}
	wantScore := summary.Ranked[0].Score

	rows, err := b.Findings(context.Background(), store.FindingFilter{})
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("finding rows = %d, want 2 (one per conversation's analysis row)", len(rows))
	}
	for _, r := range rows {
		if r.ExpectedSavingsTokens != wantScore {
			t.Errorf("finding row %+v ExpectedSavingsTokens = %d, want ranked Score %d", r, r.ExpectedSavingsTokens, wantScore)
		}
	}
}
