// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

// captureStdout runs fn with os.Stdout redirected to an in-memory pipe and
// returns whatever fn wrote there, alongside fn's own error. Every agent
// subcommand writes directly to os.Stdout (no io.Writer injection point), so
// this is the only way to assert on their output from outside the package.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	c := assert.NewAborting(t)
	r, w, err := os.Pipe()
	c.NoError(err, "os.Pipe")
	orig := os.Stdout
	os.Stdout = w
	fnErr := fn()
	os.Stdout = orig
	c.NoError(w.Close(), "close pipe writer")
	out, err := io.ReadAll(r)
	c.NoError(err, "read pipe")
	return string(out), fnErr
}

// writeCorpusTranscript writes a minimal, well-formed insights.Transcript
// corpus file — the same shape pkg/agentcli/local's own corpus tests use — so
// --corpus runs have something real to compact/detect against.
func writeCorpusTranscript(t *testing.T, dir, name, convID string) {
	t.Helper()
	c := assert.NewAborting(t)
	textContent := func(s string) json.RawMessage {
		b, err := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		c.NoError(err)
		return b
	}
	tr := map[string]any{
		"conversation_id": convID,
		"owner":           "brent",
		"persona":         "diagnose",
		"source":          "corpus",
		"turns": []map[string]any{
			{"ordinal": 0, "role": "user", "content": textContent("why is replica X lagging?")},
			{"ordinal": 1, "role": "assistant", "content": textContent("investigating..."), "model": "claude-haiku-4-5", "input_tokens": 10, "output_tokens": 5},
		},
	}
	raw, err := json.Marshal(tr)
	c.NoError(err)
	c.NoError(os.WriteFile(filepath.Join(dir, name), raw, 0o644))
}

// canonReportFindings is a single report_findings tool_use response with one
// skill-gap finding — draft-eligible, so a full-pipeline run also drafts.
const canonReportFindings = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
	`"content":[{"type":"tool_use","id":"tu_1","name":"report_findings","input":{` +
	`"outcome":"agent invented a bespoke pgbouncer restart runbook from scratch",` +
	`"verdicts":{"skill-gap":"finding","knowledge-to-persist":"ok","grind":"ok"},` +
	`"findings":[{"axis":"skill-gap","title":"missing pgbouncer restart runbook",` +
	`"topic_key":"pgbouncer-restart-runbook","evidence":[{"ordinal":1,"quote":"hi"}],` +
	`"recommendation":{"kind":"new-skill","skill_name":"pgbouncer-restart","summary":"codify the restart steps"},` +
	`"confidence":0.8}]}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":50}}`

// canonProposeSkillEdit is the propose_skill_edit tool_use response Draft
// consumes after canonReportFindings ranks its skill-gap finding.
const canonProposeSkillEdit = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
	`"content":[{"type":"tool_use","id":"tu_2","name":"propose_skill_edit","input":{` +
	`"files":[{"path":"skills/pgbouncer-restart/SKILL.md","content":"# PgBouncer Restart\n"}],` +
	`"rationale":"codify the steps"}}],"stop_reason":"tool_use","usage":{"input_tokens":40,"output_tokens":77}}`

func TestParseAnalyzeArgsStageFlags(t *testing.T) {
	got, err := parseAnalyzeArgs([]string{"--detect", "019f-aaaa"})
	assert.NewAborting(t).False(err != nil || got.StopAfter != "detect" || len(got.ConversationIDs) != 1, "parse = %+v, %v", got, err)
	if _, err := parseAnalyzeArgs([]string{"--detect", "--rank", "x"}); err == nil {
		t.Fatal("stage flags must be mutually exclusive")
	}
	if _, err := parseAnalyzeArgs([]string{"--corpus", "/tmp/x", "019f-aaaa"}); err == nil {
		t.Fatal("--corpus and conversation ids are mutually exclusive")
	}
	if _, err := parseAnalyzeArgs(nil); err == nil {
		t.Fatal("analyze requires ids or --corpus")
	}
}

func TestParseAnalyzeArgsCompareSplitsAndTrims(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := parseAnalyzeArgs([]string{"--corpus", "/tmp/x", "--compare", "a,b , c"})
	c.Require().NoError(err, "parse")
	want := []string{"a", "b", "c"}
	c.Require().Len(got.Compare, len(want), "Compare = %+v, want %+v", got.Compare, want)
	for i, m := range want {
		c.Eq(m, got.Compare[i], "Compare[%d] = %q, want", i, got.Compare[i])
	}
}

func TestParseAnalyzeArgsCompareRequiresCorpus(t *testing.T) {
	_, err := parseAnalyzeArgs([]string{"--compare", "a,b", "019f-aaaa"})
	assert.NewAborting(t).Error(err, "--compare without --corpus must be rejected")
}

func TestParseAnalyzeArgsCorpusNoDSN(t *testing.T) {
	// Corpus mode should parse successfully without a DSN
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	c := assert.NewAborting(t)
	got, err := parseAnalyzeArgs([]string{"--corpus", "/tmp/transcripts"})
	c.NoError(err, "--corpus without DSN should parse")
	c.Eq("/tmp/transcripts", got.CorpusDir, "corpus dir")
	c.Eq("", got.DB, "DB should be empty, got")

	// But conversation ids still require a DSN
	dsn := "postgres://localhost/test"
	got, err = parseAnalyzeArgs([]string{"--db", dsn, "019f-aaaa"})
	c.NoError(err, "parse with DSN")
	c.Eq(dsn, got.DB, "DB")
}

func TestResolveProfileFromAnalyzerDir(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "profiles.yaml"), []byte("default:\n  detector_model: claude-haiku-4-5\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "detector.md"), []byte("BASE DETECTOR"), 0o644)
	p, err := resolveProfile(dir, "", "", true)
	c.Require().NoError(err)
	c.Require().Eq("claude-haiku-4-5", p.DetectorModel, "model =")
	c.StrContains(p.EffectiveDetectorPrompt(analyze.BuiltinDetectorPrompt()), "BASE DETECTOR", "analyzer-dir base prompt must be attached to the profile")
	if _, err := resolveProfile(dir, "nope", "", true); err == nil {
		t.Fatal("unknown profile name must error")
	}
	// No --analyzer-dir no longer means "no config": it auto-seeds and loads
	// ~/.config/rafiki/profiles/<name>/analyzer/ from the embedded default,
	// which names a real model.
	auto, err := resolveProfile("", "", "", true)
	c.Require().NoError(err, "no analyzer dir must fall back to the auto-seeded default, got")
	c.Require().NotEq("", auto.DetectorModel, "auto-seeded default profile must name a detector model")
}

func TestDirectUpstreamRejectsSlashModel(t *testing.T) {
	c := assert.NewAborting(t)
	p := &analyze.Profile{DetectorModel: "deepseek/deepseek-v4-pro"}
	c.Error(checkModelServable(p, false), "slash ids cannot be served direct-to-Anthropic; must fail fast") /* proxied */
	c.NoError(checkModelServable(p, true), "proxied slash id must be allowed")
}

// TestResolveProfileCompactNeedsNoModel: the compact stage makes no LLM
// call, so it must resolve without a model — the zero-credential dev loop.
func TestResolveProfileCompactNeedsNoModel(t *testing.T) {
	c := assert.NewCollecting(t)
	p, err := resolveProfile("", "", "", false)
	c.Require().NoError(err, "compact-stage profile resolution must not require a model")
	c.NotEq(0, p.Compact.MaxToolResultBytes, "Defaults() must still apply so Compact has a policy")
}

// Fix 7: --analyzer-dir with no --profile and no "default" profile must
// error, listing the available profile names, rather than silently running
// with a zero-value profile — even when --model is also given, since
// --model only overrides the three model fields.
func TestResolveProfileNoDefaultEnumeratesAvailable(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "profiles.yaml"), []byte("prod:\n  detector_model: claude-haiku-4-5\nstaging:\n  detector_model: claude-haiku-4-5\n"), 0o644))

	_, err := resolveProfile(dir, "", "", true)
	c.Require().Error(err, "no --profile and no default profile must error")
	c.False(!strings.Contains(err.Error(), "prod") || !strings.Contains(err.Error(), "staging"), "error must enumerate available profiles, got: %v", err)

	// Passing --model must not paper over the missing default: --model only
	// overrides detector/rank/draft, not filters/compact policy/prompt bases.
	if _, err := resolveProfile(dir, "", "some-model", true); err == nil {
		t.Fatal("no default profile must still error even with --model set")
	}
}

// Fix 1 (security): the proxy branch of resolveUpstream must never leak
// ANTHROPIC_API_KEY to --proxy-url. anthropic.NewClient prepends
// option.DefaultClientOptions(), which turns a set ANTHROPIC_API_KEY env var
// into an implicit X-Api-Key header; without the WithAPIKey("") override,
// every proxied request would carry the developer's real key to whatever
// host --proxy-url points at, alongside the intended proxy bearer token.
func TestResolveUpstreamProxyDoesNotLeakAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "developers-real-anthropic-key")
	c := assert.NewCollecting(t)

	var gotAPIKey, gotAuth string
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canonReportFindings))
	}))
	defer srv.Close()

	client, proxied, err := resolveUpstream(srv.URL, "proxy-bearer-token")
	c.Require().NoError(err, "resolveUpstream")
	c.Require().True(proxied, "resolveUpstream with --proxy-url must report proxied=true")

	// Actually issue a call through the resolved client so the SDK's real
	// composed request options run header-by-header, not just an inspection
	// of the option list.
	_, _ = client.SendParams(t.Context(), llm.SendMeta{}, anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	c.Require().Eq(1, requests, "proxy server saw")
	c.Eq("", gotAPIKey, "X-Api-Key leaked to the proxy")
	c.Eq("Bearer proxy-bearer-token", gotAuth, "Authorization")
}

// Fix 2: the compact stage must print the compacted transcript to stdout
// when --out is absent, in both human and JSON mode — previously it was
// silently dropped.
func TestAgentAnalyzeCompactNoOutPrintsStdout(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	out, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{"--corpus", dir, "--compact"})
	})
	c.Require().NoError(err, "agentAnalyzeCmd --compact")
	c.Require().NotEq("", strings.TrimSpace(out), "--compact with no --out must print the compacted transcript, got empty stdout")

	jsonOut, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{"--corpus", dir, "--compact", "-J"})
	})
	c.Require().NoError(err, "agentAnalyzeCmd --compact -J")
	c.Require().NotEq("", strings.TrimSpace(jsonOut), "--compact -J with no --out must print JSON, got empty stdout")
	var parsed analyzeResultJSON
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("--compact -J output must be valid JSON: %v\noutput: %s", err, jsonOut)
	}
	c.Len(parsed.Payloads, 1, "payloads = %d, want 1 compacted transcript", len(parsed.Payloads))
}

// Fix 2: the detect stage must also print its per-conversation analysis to
// stdout when --out is absent — via a fake proxy upstream so the test needs
// no live credentials or network.
func TestAgentAnalyzeDetectNoOutPrintsStdout(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	c := assert.NewAborting(t)
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canonReportFindings))
	}))
	defer srv.Close()

	out, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{
			"--corpus", dir, "--detect",
			"--proxy-url", srv.URL, "--proxy-token", "tok",
			"--model", "claude-haiku-4-5",
		})
	})
	c.NoError(err, "agentAnalyzeCmd --detect")
	c.NotEq("", strings.TrimSpace(out), "--detect with no --out must print the analysis, got empty stdout")
}

// Fix 3: a drafted skill edit carried on a Summary's ranked finding must be
// written to disk under --out via agentcli.WriteSkillEdits, and its path
// reported on stdout.
func TestAgentAnalyzeWithOutWritesSkillEdits(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")
	outDir := t.TempDir()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(canonReportFindings))
			return
		}
		_, _ = w.Write([]byte(canonProposeSkillEdit))
	}))
	defer srv.Close()

	out, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{
			"--corpus", dir,
			"--proxy-url", srv.URL, "--proxy-token", "tok",
			"--model", "claude-haiku-4-5",
			"--out", outDir,
		})
	})
	c.Require().NoError(err, "agentAnalyzeCmd")

	written := filepath.Join(outDir, "skills", "pgbouncer-restart", "SKILL.md")
	content, err := os.ReadFile(written)
	c.Require().NoError(err, "drafted skill file not written to disk")
	c.StrContains(string(content), "PgBouncer Restart", "written skill file content = %q, want it to contain the drafted content", content)
	c.StrContains(out, written, "stdout must report the written skill file path")
}

// Fix 4: `agent findings dismiss/action` must dispatch regardless of whether
// a flag precedes the verb — dispatching off the raw pre-parse args[0] (the
// bug this replaces) only worked when the verb happened to come first.
func TestAgentFindingsDismissDispatchesRegardlessOfFlagOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"verb first", []string{"dismiss", "--db", "", "deadbeef"}},
		{"flag first", []string{"--db", "", "dismiss", "deadbeef"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := agentFindingsCmd(tc.args)
			c.Require().Error(err, "dismiss with an empty --db must error attempting the mutation, not succeed")
			c.StrContains(err.Error(), "--db", "error must be connectPool's DSN-required error (proving dismiss was reached), got: %v", err)
		})
	}
}

// Fix 4: the list path must reject leftover positional args, mirroring
// agentExportCmd's fixed-arity check, and an unknown verb must error rather
// than silently falling through to the list path.
func TestAgentFindingsRejectsLeftoverArgsAndUnknownVerb(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Require().Error(agentFindingsCmd([]string{"dismiss", "--db", "", "id1", "extra"}), "dismiss with more than one id must error")
	err := agentFindingsCmd([]string{"bogus"})
	c.Require().Error(err, "an unknown first positional argument must error, not silently list")
	c.StrContains(err.Error(), "unknown", "error should name the command unknown, got: %v", err)
}

// Fix 5: --compare must honor -j, marshaling the sweep's runs as JSON
// instead of silently ignoring the flag and rendering the human table.
func TestAgentAnalyzeCompareHonorsJSONFlag(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(canonReportFindings))
	}))
	defer srv.Close()

	out, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{
			"--corpus", dir, "--detect", "--compare", "model-a,model-b",
			"--proxy-url", srv.URL, "--proxy-token", "tok",
			"-J",
		})
	})
	c.Require().NoError(err, "agentAnalyzeCmd --compare -J")
	var runs []compareRunJSON
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		t.Fatalf("--compare -J output must be valid JSON: %v\noutput: %s", err, out)
	}
	c.Require().Len(runs, 2, "runs = %d, want 2 (one per swept model)", len(runs))
	c.False(runs[0].Model != "model-a" || runs[1].Model != "model-b", "runs = %+v, want model-a then model-b", runs)
}

// Fix 6: --compare must preflight the profile's draft model, not just the
// swept detector models — a full-pipeline compare run with no draft model
// configured must error naming draft_model/--model, before any network call.
func TestAgentAnalyzeCompareRequiresDraftModel(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	// The auto-seeded default profile (no --analyzer-dir) now always names a
	// draft_model, so reconstructing "no draft model configured" needs an
	// explicit analyzer dir whose profile sets only a detector model.
	analyzerDir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(analyzerDir, "profiles.yaml"),
		[]byte("default:\n  detector_model: claude-haiku-4-5\n"), 0o644))

	err := agentAnalyzeCmd([]string{
		"--corpus", dir, "--compare", "model-a", "--analyzer-dir", analyzerDir,
		"--proxy-url", "http://127.0.0.1:0", "--proxy-token", "tok",
		// No --model, no --draft/--detect/--rank: full pipeline, no draft model configured.
	})
	c.Require().Error(err, "--compare through the draft stage with no draft model must error")
	c.False(!strings.Contains(err.Error(), "draft_model") && !strings.Contains(err.Error(), "--model"), "error must name draft_model/--model, got: %v", err)
}

// Fix 6: --compare must also reject an unservable model in the sweep when
// direct-to-Anthropic (no --proxy-url) can't serve a slash/tilde id.
func TestAgentAnalyzeCompareRejectsUnservableModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake-direct-key")
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	err := agentAnalyzeCmd([]string{
		"--corpus", dir, "--detect", "--compare", "deepseek/deepseek-v4-pro",
	})
	assert.NewAborting(t).Error(err, "a slash-id model direct-to-Anthropic can't serve must be rejected before any per-conversation work")
}

// sampleQueryResult is one result exercising every cell shape insights.Entry
// carries: a string, a bare int, and floats in all three render formats.
func sampleQueryResult() insights.QueryResult {
	return insights.QueryResult{
		Columns: []insights.Column{
			{Name: "tool", Kind: insights.ColString},
			{Name: "calls", Kind: insights.ColInt},
			{Name: "cost", Kind: insights.ColFloat, Format: "usd"},
			{Name: "hit", Kind: insights.ColFloat, Format: "pct"},
			{Name: "ratio", Kind: insights.ColFloat},
		},
		Rows: [][]insights.Entry{
			{
				insights.StringEntry("bash"), insights.IntEntry(606),
				insights.FloatEntry(0.0042), insights.FloatEntry(0.5), insights.FloatEntry(0.727),
			},
		},
	}
}

func TestRenderQueryResultTable(t *testing.T) {
	c := assert.NewCollecting(t)
	var got bytes.Buffer
	c.Require().NoError(renderQueryResult(&got, agentcli.ModeTable, sampleQueryResult()))
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
// Both JSON modes carry the same typed shape; only the indentation differs.
func TestRenderQueryResultJSONTypedValues(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		mode         agentcli.Mode
		wantIndented bool
	}{
		{agentcli.ModeJSON, true},
		{agentcli.ModeJSONCompact, false},
	} {
		var got bytes.Buffer
		c.Require().NoError(renderQueryResult(&got, tc.mode, sampleQueryResult()), "mode %v", tc.mode)
		c.Eq(tc.wantIndented, strings.Contains(got.String(), "\n  "), "mode %v: indentation wrong:\n%s", tc.mode, got.String())

		var back struct {
			Columns []string `json:"columns"`
			Rows    [][]any  `json:"rows"`
		}
		err := json.Unmarshal(got.Bytes(), &back)
		c.Require().NoError(err, "mode %v: output is not valid JSON: %v\n%s", tc.mode, err, got.String())
		c.False(len(back.Columns) != 5 || back.Columns[2] != "cost", "mode %v: columns did not survive: %+v", tc.mode, back.Columns)
		row := back.Rows[0]
		c.Require().Len(row, 5, "mode %v: row width %d, want 5", tc.mode, len(row))
		if s, ok := row[0].(string); !ok || s != "bash" {
			t.Errorf("mode %v: cell 0: got %#v, want string \"bash\"", tc.mode, row[0])
		}
		if _, ok := row[1].(float64); !ok {
			t.Errorf("mode %v: cell 1 (int): got %#v, want a number, not a string", tc.mode, row[1])
		}
		for i := 2; i < 5; i++ {
			if _, ok := row[i].(float64); !ok {
				t.Errorf("mode %v: cell %d (float): got %#v, want a number, not a string", tc.mode, i, row[i])
			}
		}
	}
}

// insights.Entry's own doc names the pkg/table formatting switch as one of the
// sites every concrete type must be handled at; renderQueryResult holds BOTH
// switches (JSON and table), so a cell that is none of the three types — or a
// nil Entry, which no current producer can emit — must fail loud with the same
// wording as the two adapters, never silently shorten a JSON row or render an
// empty cell.
func TestRenderQueryResultUnknownEntryFails(t *testing.T) {
	c := assert.NewAborting(t)
	res := sampleQueryResult()
	res.Rows = [][]insights.Entry{{nil, insights.IntEntry(1)}}
	for _, tc := range []struct {
		mode agentcli.Mode
		name string
	}{
		{agentcli.ModeJSON, "json"},
		{agentcli.ModeJSONCompact, "json compact"},
		{agentcli.ModeTable, "table"},
	} {
		var got bytes.Buffer
		err := renderQueryResult(&got, tc.mode, res)
		c.Error(err, "%s: want an error for a nil Entry, got none (output:\n%s)", tc.name, got.String())
		c.StrContains(err.Error(), "agent_cli: unhandled insights.Entry type", "%s: error %q does not name the site and type", tc.name, err)
	}
}
