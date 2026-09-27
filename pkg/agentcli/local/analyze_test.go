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

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NoError(json.Unmarshal([]byte(raw), &m), "analyzeCannedMessage")
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
		llm.WithProviderSender("anthropic", sender),
		llm.WithDefaultModel("haiku-latest"),
	)
	assert.NewAborting(t).NoError(err, "NewClient")
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
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	c.Require().Len(events, 3, "events = %d, want 3 (progress, analysis, summary); got", len(events))
	if events[0].Kind != agentcli.EventProgress || events[0].Progress.State != agentcli.StateDone {
		t.Errorf("events[0] = %+v, want progress/done", events[0])
	}
	if events[1].Kind != agentcli.EventAnalysis || events[1].Analysis == nil {
		t.Errorf("events[1] = %+v, want analysis", events[1])
	}
	if events[2].Kind != agentcli.EventSummary || events[2].Summary == nil {
		t.Fatalf("events[2] = %+v, want summary", events[2])
	}
	c.Eq(1, events[2].Summary.Analyzed, "Summary.Analyzed")

	var n int
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&n))
	c.Eq(1, n, "conversation_analysis rows")
	c.Require().NoError(pool.QueryRow(context.Background(), `
		SELECT count(*) FROM conversations.analysis_finding af
		  JOIN conversations.conversation_analysis ca ON ca.id = af.analysis_id
		 WHERE ca.conversation_id = $1::uuid`, convID).Scan(&n))
	c.Eq(1, n, "analysis_finding rows")
}

// TestAnalyzeSkipThenForce covers skip-detection: a second run without Force
// reports the conversation as skipped (its stored analysis still feeding
// Rank), and a third run with Force re-analyzes it.
func TestAnalyzeSkipThenForce(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	c.Require().NoError(err, "Analyze (first run)")
	first := drainEvents(ch)
	c.Require().Eq(1, first[len(first)-1].Summary.Analyzed, "first run Summary.Analyzed")

	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	c.Require().NoError(err, "Analyze (second run)")
	second := drainEvents(ch)
	var sawSkip bool
	for _, ev := range second {
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateSkipped {
			sawSkip = true
		}
	}
	c.True(sawSkip, "second run: no skipped progress event; events = %+v", second)
	summary := second[len(second)-1].Summary
	c.False(summary.Skipped != 1 || summary.Analyzed != 0, "second run Summary = %+v, want Skipped=1 Analyzed=0", summary)
	c.NotEmpty(summary.Ranked, "second run Summary.Ranked is empty, want the skipped conversation's stored finding to still rank")

	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile, Force: true})
	c.Require().NoError(err, "Analyze (forced run)")
	third := drainEvents(ch)
	c.Eq(1, third[len(third)-1].Summary.Analyzed, "forced run Summary.Analyzed")
}

// TestAnalyzeCanonicalizesConversationIDCase covers Fix 1: a conversation id
// passed uppercase-cased must canonicalize to the same lowercase form
// Postgres stores, so a second run recognizes it as the SAME conversation
// already analyzed (skip-detected, not silently re-analyzed as new) — and,
// critically, replaceFindingsPerAnalysis's Conversations-slice matching
// (slices.Contains, string-equality) must not fail across the case
// mismatch and wipe the conversation's existing findings.
func TestAnalyzeCanonicalizesConversationIDCase(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	c.Require().NoError(err, "Analyze (first run, canonical id)")
	first := drainEvents(ch)
	c.Require().Eq(1, first[len(first)-1].Summary.Analyzed, "first run Summary.Analyzed")
	before := snapshotAnalysisFindings(t, pool)
	c.Require().Len(before, 1, "first run stored %d analysis_finding rows, want 1", len(before))

	upper := strings.ToUpper(convID)
	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{upper}, Profile: profile})
	c.Require().NoError(err, "Analyze (second run, uppercase id)")
	second := drainEvents(ch)
	for _, ev := range second {
		c.Require().NotEq(agentcli.EventError, ev.Kind, "unexpected EventError on uppercase-id run: %v", ev.Err)
	}
	summary := second[len(second)-1].Summary
	c.Require().NotNil(summary, "second run produced no Summary")
	c.False(summary.Skipped != 1 || summary.Analyzed != 0, "second run (uppercase id) Summary = %+v, want Skipped=1 Analyzed=0", summary)
	c.NotEmpty(summary.Ranked, "second run Summary.Ranked is empty, want the carried-over finding to still rank")

	// A normal (non-NoStore) skip is allowed to rewrite the finding row via
	// replaceFindingsPerAnalysis (recomputing ranked findings fresh each
	// run) — only --no-store promises byte-for-byte row identity
	// (TestAnalyzeNoStoreDoesNotRewriteExistingFindings covers that). What
	// Fix 1 guarantees here is that the finding is never lost: same count,
	// same content, across a re-run whose only difference is the input id's
	// case.
	after := snapshotAnalysisFindings(t, pool)
	c.Require().Len(after, len(before), "analysis_finding row count changed across the case-mismatched re-run: before=%d after=%d", len(before), len(after))
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
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	for _, ev := range events {
		c.Require().NotEq(agentcli.EventError, ev.Kind, "unexpected EventError: %v", ev.Err)
	}
	var failProgress *agentcli.Progress
	for _, ev := range events {
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateFailed {
			failProgress = ev.Progress
		}
	}
	c.Require().False(failProgress == nil || failProgress.Detail == "", "no failed progress event with non-empty Detail; events = %+v", events)
	summary := events[len(events)-1].Summary
	c.False(summary == nil || summary.Failed != 1, "Summary = %+v, want Failed=1", summary)

	var status string
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT status FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&status), "query status row")
	c.Eq("failed", status, "stored status")
}

// TestAnalyzeStopAfterCompact covers stop_after=="compact": the run emits a
// Transcript-carrying analysis event per conversation and never writes a
// conversation_analysis row.
func TestAnalyzeStopAfterCompact(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
		StopAfter:       "compact",
	})
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	var sawTranscript bool
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Transcript != nil {
			sawTranscript = true
		}
	}
	c.Require().True(sawTranscript, "no analysis event carrying a Transcript; events = %+v", events)

	var n int
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&n))
	c.Eq(0, n, "conversation_analysis rows")
}

// TestAnalyzeNoStore covers NoStore: the run still emits an analysis event
// but writes nothing to the database.
func TestAnalyzeNoStore(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	var sawAnalysis bool
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Analysis != nil {
			sawAnalysis = true
		}
	}
	c.Require().True(sawAnalysis, "no analysis event with a non-nil Analysis; events = %+v", events)

	var n int
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, convID).Scan(&n))
	c.Eq(0, n, "conversation_analysis rows")
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.analysis_finding`).Scan(&n))
	c.Eq(0, n, "analysis_finding rows")
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
	c := assert.NewAborting(t)
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, expected_savings_tokens, status FROM conversations.analysis_finding ORDER BY id`)
	c.NoError(err, "snapshot analysis_finding")
	defer rows.Close()
	var out []analysisFindingSnapshot
	for rows.Next() {
		var s analysisFindingSnapshot
		c.NoError(rows.Scan(&s.id, &s.expectedSavingsTokens, &s.status), "snapshot analysis_finding: scan")
		out = append(out, s)
	}
	c.NoError(rows.Err(), "snapshot analysis_finding")
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
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	// First run: normal (stored) analysis, producing a real finding row.
	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	c.Require().NoError(err, "Analyze (first run)")
	first := drainEvents(ch)
	c.Require().Eq(1, first[len(first)-1].Summary.Analyzed, "first run Summary.Analyzed")
	before := snapshotAnalysisFindings(t, pool)
	c.Require().NotEmpty(before, "first run stored no analysis_finding rows to compare against")

	// Second run: same detector key (so this conversation is skipped, not
	// re-detected) but NoStore. Before the fix, storedAnalysesFor's
	// carryOver was appended to forReplace unconditionally, so this call
	// silently rewrote the row above via ReplaceFindings' DELETE+INSERT.
	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{convID}, Profile: profile, NoStore: true,
	})
	c.Require().NoError(err, "Analyze (no-store run)")
	second := drainEvents(ch)
	summary := second[len(second)-1].Summary
	c.Require().Eq(1, summary.Skipped, "no-store run Summary.Skipped")
	c.NotEmpty(summary.Ranked, "no-store run Summary.Ranked is empty, want the carried-over finding to still rank (read-only)")

	after := snapshotAnalysisFindings(t, pool)
	c.Require().Len(after, len(before), "analysis_finding row count changed: before=%d after=%d", len(before), len(after))
	for i := range before {
		c.Eq(after[i], before[i], "analysis_finding row %d changed under --no-store: before=%+v after=", i, before[i])
	}
}

// TestAnalyzeInvalidConversationID covers up-front UUID validation: a
// malformed id must fail before any event is produced.
func TestAnalyzeInvalidConversationID(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{"not-a-uuid"}})
	c.Require().Error(err, "Analyze: want error for an invalid conversation id, got nil")
	c.Nil(ch, "Analyze: want a nil channel alongside the error")
}

// TestAnalyzeCorpusDir covers corpus mode: a directory of pre-built
// transcripts, with one broken file, is analyzed without touching the
// database at all (corpus conversations have no conversations.conversation
// row to FK an analysis_finding against, so they're implicitly NoStore).
func TestAnalyzeCorpusDir(t *testing.T) {
	c := assert.NewCollecting(t)
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
				{Ordinal: 1, Role: "assistant", Content: textContent("investigating..."), Model: "claude-haiku-4-5", InputTokens: i64(10), OutputTokens: i64(5)},
			},
		}
		raw, err := json.Marshal(tr)
		c.Require().NoError(err)
		c.Require().NoError(os.WriteFile(filepath.Join(dir, name), raw, 0o644))
	}
	writeTranscript("conv-a.json", "corpus-conv-a")
	writeTranscript("conv-b.json", "corpus-conv-b")
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not valid json"), 0o644))

	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		CorpusDir: dir,
		Profile:   &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	c.Require().NoError(err, "Analyze")
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
	c.Eq(2, analyses, "analysis events")
	summary := events[len(events)-1].Summary
	c.False(summary == nil || summary.Skipped != 1, "Summary = %+v, want Skipped=1", summary)
	c.NotEq("", brokenDetail, "no skipped progress event carrying a reason for the broken file")

	var n int
	c.Require().NoError(pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations.conversation_analysis`).Scan(&n))
	c.Eq(0, n, "conversation_analysis rows")
}

// TestAnalyzeCorpusDirSkipsArtifactsAndEmptyTranscripts covers Fix 2: a
// corpus run must not re-ingest its own prior-run artifacts
// (*.compact/.detect/.rank/.draft.json siblings of a real conversation file),
// and must treat a well-formed but empty (zero-Turns) transcript as
// unparseable rather than handing Detect nothing to work with.
func TestAnalyzeCorpusDirSkipsArtifactsAndEmptyTranscripts(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	dir := t.TempDir()

	textContent := func(s string) json.RawMessage {
		b, _ := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		return b
	}
	writeTranscript := func(name, convID string, turns []insights.TranscriptTurn) {
		tr := insights.Transcript{ConversationID: convID, Owner: "brent", Persona: "diagnose", Source: "corpus", Turns: turns}
		raw, err := json.Marshal(tr)
		c.Require().NoError(err)
		c.Require().NoError(os.WriteFile(filepath.Join(dir, name), raw, 0o644))
	}
	realTurns := []insights.TranscriptTurn{
		{Ordinal: 0, Role: "user", Content: textContent("why is replica X lagging?")},
		{Ordinal: 1, Role: "assistant", Content: textContent("investigating..."), Model: "claude-haiku-4-5", InputTokens: i64(10), OutputTokens: i64(5)},
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	var analyses int
	for _, ev := range events {
		if ev.Kind == agentcli.EventAnalysis && ev.Analysis != nil {
			analyses++
			c.Eq("corpus-conv-a", ev.Analysis.ConversationID, "unexpected conversation analyzed")
		}
	}
	c.Require().Eq(1, analyses, "analysis events")

	summary := events[len(events)-1].Summary
	c.Require().NotNil(summary, "no Summary event")
	c.Eq(2, summary.Population, "Summary.Population")
	c.Eq(1, summary.Skipped, "Summary.Skipped")
}

// TestRunAnalyzeAlwaysEmitsTerminalEventOnCancelledContext covers Fix 4: a
// terminal event (EventSummary or EventError) must never be droppable by a
// raced ctx.Done() select. Corpus mode's toProcess loop checks ctx.Err() at
// the top of every iteration, so a pre-cancelled context deterministically
// reaches fail(ctx.Err()) on the very first item — before this fix, that
// path used the same racy `send` as progress/analysis events and so could
// silently drop the terminal event roughly half the time.
func TestRunAnalyzeAlwaysEmitsTerminalEventOnCancelledContext(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	tr := insights.Transcript{
		ConversationID: "corpus-conv",
		Owner:          "brent",
		Turns: []insights.TranscriptTurn{
			{Ordinal: 0, Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
		},
	}
	raw, err := json.Marshal(tr)
	c.NoError(err)
	c.NoError(os.WriteFile(filepath.Join(dir, "conv.json"), raw, 0o644))

	b := New(Options{LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ch, err := b.Analyze(ctx, agentcli.AnalyzeRequest{CorpusDir: dir, Profile: &analyze.Profile{DetectorModel: "claude-haiku-4-5"}})
		c.NoError(err, "iteration %d: Analyze", i)
		events := drainEvents(ch)
		c.NotEmpty(events, "iteration %d: no events at all on a cancelled ctx; want a terminal event", i)
		last := events[len(events)-1]
		c.False(last.Kind != agentcli.EventSummary && last.Kind != agentcli.EventError, "iteration %d: last event = %+v, want EventSummary or EventError", i, last)
	}
}

// TestAnalyzeFoldsDraftUsageIntoTotals covers Fix 5: Draft's LLM usage
// (input/output tokens, cost) must be folded into Summary.Totals alongside
// Detect's, and recorded on the ranked finding's own Draft — not silently
// dropped, which would understate real spend for any run whose findings are
// draft-eligible.
func TestAnalyzeFoldsDraftUsageIntoTotals(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)
	summary := events[len(events)-1].Summary
	c.Require().NotNil(summary, "no Summary event")
	if len(summary.Ranked) != 1 || summary.Ranked[0].Draft == nil {
		t.Fatalf("want exactly 1 ranked finding with a Draft attached, got %+v", summary.Ranked)
	}
	draft := summary.Ranked[0].Draft

	c.Eq(140, summary.Totals.InputTokens, "Totals.InputTokens")
	c.Eq(127, summary.Totals.OutputTokens, "Totals.OutputTokens")
	c.False(draft.InputTokens != 40 || draft.OutputTokens != 77, "Draft usage = %+v, want InputTokens=40 OutputTokens=77", draft)
}

// TestAnalyzeExportNotFoundPinsFKFix covers analyzeOne's insights.ErrNotFound
// branch: a well-formed but nonexistent conversation id must surface as a
// failed progress event WITHOUT attempting to record a failed
// conversation_analysis row — there is no conversation row for one to FK
// against, and recordFailure would itself die on the constraint. This pins
// the FK-bug fix, which was previously unpinned by any test.
func TestAnalyzeExportNotFoundPinsFKFix(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	missingID := uuid.NewString()
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, &analyzeFakeSender{})})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{missingID},
		Profile:         &analyze.Profile{DetectorModel: "claude-haiku-4-5"},
	})
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	var sawFailed bool
	for _, ev := range events {
		c.Require().NotEq(agentcli.EventError, ev.Kind, "unexpected EventError: %v", ev.Err)
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateFailed && ev.Progress.ConversationID == missingID {
			sawFailed = true
		}
	}
	c.Require().True(sawFailed, "no failed progress event for the missing conversation; events = %+v", events)

	var n int
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_analysis WHERE conversation_id = $1::uuid`, missingID).Scan(&n))
	c.Eq(0, n, "conversation_analysis rows for the missing id")
}

// TestAnalyzeForceKeepsDismissedFinding covers the prior-map carry-over: once
// a human dismisses a finding, a later --force re-analysis (which deletes and
// re-inserts the analysis row, cascading away the old finding row) must not
// resurrect it as 'open' just because Detect re-found the same finding.
func TestAnalyzeForceKeepsDismissedFinding(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{Pool: pool, LLM: testAnalyzeClient(t, sender)})
	profile := &analyze.Profile{DetectorModel: "claude-haiku-4-5"}

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile})
	c.Require().NoError(err, "Analyze (first run)")
	drainEvents(ch)

	findings, err := b.Findings(context.Background(), store.FindingFilter{})
	c.Require().False(err != nil || len(findings) != 1, "Findings after first run = %+v, %v; want exactly 1", findings, err)
	if _, err := b.SetFindingStatus(context.Background(), findings[0].ID, "dismissed"); err != nil {
		t.Fatalf("SetFindingStatus: %v", err)
	}

	ch, err = b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{convID}, Profile: profile, Force: true})
	c.Require().NoError(err, "Analyze (forced run)")
	drainEvents(ch)

	// ListFindings defaults to status=='open', which a still-dismissed
	// finding deliberately will not match — query for dismissed explicitly.
	findings, err = b.Findings(context.Background(), store.FindingFilter{Status: "dismissed"})
	c.Require().False(err != nil || len(findings) != 1, "Findings(status=dismissed) after forced re-run = %+v, %v; want exactly 1", findings, err)
	c.Eq("dismissed", findings[0].Status, "finding status after forced re-run")
}

// TestAnalyzeLimitRemainingArithmetic covers Summary.Remaining's arithmetic:
// with 2 eligible conversations and Limit 1, exactly 1 is analyzed and 1 is
// left over (population minus skipped minus processed).
func TestAnalyzeLimitRemainingArithmetic(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)
	summary := events[len(events)-1].Summary
	c.Require().NotNil(summary, "no summary event; events = %+v", events)
	c.Eq(2, summary.Population, "Summary.Population")
	c.Eq(1, summary.Analyzed, "Summary.Analyzed")
	c.Eq(1, summary.Remaining, "Summary.Remaining")
}

// TestAnalyzeInterestingnessOrder covers population's sort: an error-status
// conversation must be analyzed before a healthy one even when the healthy
// one carries far more tokens — with Limit 1, only the error-status
// conversation's id should end up processed.
func TestAnalyzeInterestingnessOrder(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)

	var processedID string
	for _, ev := range events {
		if ev.Kind == agentcli.EventProgress && ev.Progress.State == agentcli.StateDone {
			processedID = ev.Progress.ConversationID
		}
	}
	c.Eq(errConvID, processedID, "processed conversation = %q, want the error-status one %q (healthy id %q)", processedID, errConvID, healthyConvID)
}

// TestAnalyzeFindingScoreIsRankedNotRaw covers Fix 1's shape: a finding that
// appears in two conversations must be persisted with
// ExpectedSavingsTokens == the ranked finding's Score, on BOTH conversations'
// analysis rows — not each conversation's own raw per-analysis token count
// (here, GrindTokens, which the canned response never sets, so a
// pre-fix implementation would persist 0 on both rows instead of Rank's
// recurrence-boosted Score).
func TestAnalyzeFindingScoreIsRankedNotRaw(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Analyze")
	events := drainEvents(ch)
	summary := events[len(events)-1].Summary
	c.Require().False(summary == nil || len(summary.Ranked) != 1, "Summary.Ranked = %+v, want exactly 1 ranked finding spanning both conversations", summary)
	wantScore := summary.Ranked[0].Score

	rows, err := b.Findings(context.Background(), store.FindingFilter{})
	c.Require().NoError(err, "Findings")
	c.Require().Len(rows, 2, "finding rows = %d, want 2 (one per conversation's analysis row)", len(rows))
	for _, r := range rows {
		c.Eq(wantScore, r.ExpectedSavingsTokens, "finding row %+v ExpectedSavingsTokens = %d, want ranked Score", r, r.ExpectedSavingsTokens)
	}
}

func i64(v int64) *int64 { return &v }
