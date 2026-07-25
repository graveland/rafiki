package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/rafiki/analyze"
	"git.graveland.dev/brent/rafiki/llm"
)

// captureStdout runs fn with os.Stdout redirected to an in-memory pipe and
// returns whatever fn wrote there, alongside fn's own error. Every agent
// subcommand writes directly to os.Stdout (no io.Writer injection point), so
// this is the only way to assert on their output from outside the package.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	fnErr := fn()
	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out), fnErr
}

// writeCorpusTranscript writes a minimal, well-formed insights.Transcript
// corpus file — the same shape agentcli/local's own corpus tests use — so
// --corpus runs have something real to compact/detect against.
func writeCorpusTranscript(t *testing.T, dir, name, convID string) {
	t.Helper()
	textContent := func(s string) json.RawMessage {
		b, err := json.Marshal([]map[string]any{{"type": "text", "text": s}})
		if err != nil {
			t.Fatal(err)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
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
	if err != nil || got.StopAfter != "detect" || len(got.ConversationIDs) != 1 {
		t.Fatalf("parse = %+v, %v", got, err)
	}
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
	got, err := parseAnalyzeArgs([]string{"--corpus", "/tmp/x", "--compare", "a,b , c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(got.Compare) != len(want) {
		t.Fatalf("Compare = %+v, want %+v", got.Compare, want)
	}
	for i, m := range want {
		if got.Compare[i] != m {
			t.Errorf("Compare[%d] = %q, want %q", i, got.Compare[i], m)
		}
	}
}

func TestParseAnalyzeArgsCompareRequiresCorpus(t *testing.T) {
	if _, err := parseAnalyzeArgs([]string{"--compare", "a,b", "019f-aaaa"}); err == nil {
		t.Fatal("--compare without --corpus must be rejected")
	}
}

func TestParseAnalyzeArgsCorpusNoDSN(t *testing.T) {
	// Corpus mode should parse successfully without a DSN
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	got, err := parseAnalyzeArgs([]string{"--corpus", "/tmp/transcripts"})
	if err != nil {
		t.Fatalf("--corpus without DSN should parse: %v", err)
	}
	if got.CorpusDir != "/tmp/transcripts" {
		t.Fatalf("corpus dir = %q, want /tmp/transcripts", got.CorpusDir)
	}
	if got.DB != "" {
		t.Fatalf("DB should be empty, got %q", got.DB)
	}

	// But conversation ids still require a DSN
	dsn := "postgres://localhost/test"
	got, err = parseAnalyzeArgs([]string{"--db", dsn, "019f-aaaa"})
	if err != nil {
		t.Fatalf("parse with DSN: %v", err)
	}
	if got.DB != dsn {
		t.Fatalf("DB = %q, want %q", got.DB, dsn)
	}
}

func TestResolveProfileFromAnalyzerDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "profiles.yaml"), []byte("default:\n  detector_model: claude-haiku-4-5\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "detector.md"), []byte("BASE DETECTOR"), 0o644)
	p, err := resolveProfile(dir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.DetectorModel != "claude-haiku-4-5" {
		t.Fatalf("model = %q", p.DetectorModel)
	}
	if !strings.Contains(p.EffectiveDetectorPrompt(analyze.BuiltinDetectorPrompt()), "BASE DETECTOR") {
		t.Error("analyzer-dir base prompt must be attached to the profile")
	}
	if _, err := resolveProfile(dir, "nope", "", true); err == nil {
		t.Fatal("unknown profile name must error")
	}
	if _, err := resolveProfile("", "", "", true); err == nil {
		t.Fatal("no model and no analyzer dir must error")
	}
}

func TestDirectUpstreamRejectsSlashModel(t *testing.T) {
	p := &analyze.Profile{DetectorModel: "deepseek/deepseek-v4-pro"}
	if err := checkModelServable(p, false /* proxied */); err == nil {
		t.Fatal("slash ids cannot be served direct-to-Anthropic; must fail fast")
	}
	if err := checkModelServable(p, true); err != nil {
		t.Fatalf("proxied slash id must be allowed: %v", err)
	}
}

// TestResolveProfileCompactNeedsNoModel: the compact stage makes no LLM
// call, so it must resolve without a model — the zero-credential dev loop.
func TestResolveProfileCompactNeedsNoModel(t *testing.T) {
	p, err := resolveProfile("", "", "", false)
	if err != nil {
		t.Fatalf("compact-stage profile resolution must not require a model: %v", err)
	}
	if p.Compact.MaxToolResultBytes == 0 {
		t.Error("Defaults() must still apply so Compact has a policy")
	}
}

// Fix 7: --analyzer-dir with no --profile and no "default" profile must
// error, listing the available profile names, rather than silently running
// with a zero-value profile — even when --model is also given, since
// --model only overrides the three model fields.
func TestResolveProfileNoDefaultEnumeratesAvailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profiles.yaml"), []byte("prod:\n  detector_model: claude-haiku-4-5\nstaging:\n  detector_model: claude-haiku-4-5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProfile(dir, "", "", true)
	if err == nil {
		t.Fatal("no --profile and no default profile must error")
	}
	if !strings.Contains(err.Error(), "prod") || !strings.Contains(err.Error(), "staging") {
		t.Errorf("error must enumerate available profiles, got: %v", err)
	}

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
	if err != nil {
		t.Fatalf("resolveUpstream: %v", err)
	}
	if !proxied {
		t.Fatal("resolveUpstream with --proxy-url must report proxied=true")
	}

	// Actually issue a call through the resolved client so the SDK's real
	// composed request options run header-by-header, not just an inspection
	// of the option list.
	_, _ = client.SendParams(t.Context(), llm.SendMeta{}, anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if requests != 1 {
		t.Fatalf("proxy server saw %d requests, want 1", requests)
	}
	if gotAPIKey != "" {
		t.Errorf("X-Api-Key leaked to the proxy: %q (must be empty)", gotAPIKey)
	}
	if gotAuth != "Bearer proxy-bearer-token" {
		t.Errorf("Authorization = %q, want Bearer proxy-bearer-token", gotAuth)
	}
}

// Fix 2: the compact stage must print the compacted transcript to stdout
// when --out is absent, in both human and JSON mode — previously it was
// silently dropped.
func TestAgentAnalyzeCompactNoOutPrintsStdout(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	out, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{"--corpus", dir, "--compact"})
	})
	if err != nil {
		t.Fatalf("agentAnalyzeCmd --compact: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("--compact with no --out must print the compacted transcript, got empty stdout")
	}

	jsonOut, err := captureStdout(t, func() error {
		return agentAnalyzeCmd([]string{"--corpus", dir, "--compact", "-J"})
	})
	if err != nil {
		t.Fatalf("agentAnalyzeCmd --compact -J: %v", err)
	}
	if strings.TrimSpace(jsonOut) == "" {
		t.Fatal("--compact -J with no --out must print JSON, got empty stdout")
	}
	var parsed analyzeResultJSON
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("--compact -J output must be valid JSON: %v\noutput: %s", err, jsonOut)
	}
	if len(parsed.Payloads) != 1 {
		t.Errorf("payloads = %d, want 1 compacted transcript", len(parsed.Payloads))
	}
}

// Fix 2: the detect stage must also print its per-conversation analysis to
// stdout when --out is absent — via a fake proxy upstream so the test needs
// no live credentials or network.
func TestAgentAnalyzeDetectNoOutPrintsStdout(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
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
	if err != nil {
		t.Fatalf("agentAnalyzeCmd --detect: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("--detect with no --out must print the analysis, got empty stdout")
	}
}

// Fix 3: a drafted skill edit carried on a Summary's ranked finding must be
// written to disk under --out via agentcli.WriteSkillEdits, and its path
// reported on stdout.
func TestAgentAnalyzeWithOutWritesSkillEdits(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
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
	if err != nil {
		t.Fatalf("agentAnalyzeCmd: %v", err)
	}

	written := filepath.Join(outDir, "skills", "pgbouncer-restart", "SKILL.md")
	content, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("drafted skill file not written to disk: %v", err)
	}
	if !strings.Contains(string(content), "PgBouncer Restart") {
		t.Errorf("written skill file content = %q, want it to contain the drafted content", content)
	}
	if !strings.Contains(out, written) {
		t.Errorf("stdout must report the written skill file path %q, got: %s", written, out)
	}
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
			err := agentFindingsCmd(tc.args)
			if err == nil {
				t.Fatal("dismiss with an empty --db must error attempting the mutation, not succeed")
			}
			if !strings.Contains(err.Error(), "--db") {
				t.Errorf("error must be connectPool's DSN-required error (proving dismiss was reached), got: %v", err)
			}
		})
	}
}

// Fix 4: the list path must reject leftover positional args, mirroring
// agentExportCmd's fixed-arity check, and an unknown verb must error rather
// than silently falling through to the list path.
func TestAgentFindingsRejectsLeftoverArgsAndUnknownVerb(t *testing.T) {
	if err := agentFindingsCmd([]string{"dismiss", "--db", "", "id1", "extra"}); err == nil {
		t.Fatal("dismiss with more than one id must error")
	}
	err := agentFindingsCmd([]string{"bogus"})
	if err == nil {
		t.Fatal("an unknown first positional argument must error, not silently list")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should name the command unknown, got: %v", err)
	}
}

// Fix 5: --compare must honor -j, marshaling the sweep's runs as JSON
// instead of silently ignoring the flag and rendering the human table.
func TestAgentAnalyzeCompareHonorsJSONFlag(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	t.Setenv("RAFIKI_TEST_DSN", "")
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
	if err != nil {
		t.Fatalf("agentAnalyzeCmd --compare -J: %v", err)
	}
	var runs []compareRunJSON
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		t.Fatalf("--compare -J output must be valid JSON: %v\noutput: %s", err, out)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (one per swept model)", len(runs))
	}
	if runs[0].Model != "model-a" || runs[1].Model != "model-b" {
		t.Errorf("runs = %+v, want model-a then model-b", runs)
	}
}

// Fix 6: --compare must preflight the profile's draft model, not just the
// swept detector models — a full-pipeline compare run with no draft model
// configured must error naming draft_model/--model, before any network call.
func TestAgentAnalyzeCompareRequiresDraftModel(t *testing.T) {
	dir := t.TempDir()
	writeCorpusTranscript(t, dir, "conv-a.json", "corpus-conv-a")

	err := agentAnalyzeCmd([]string{
		"--corpus", dir, "--compare", "model-a",
		"--proxy-url", "http://127.0.0.1:0", "--proxy-token", "tok",
		// No --model, no --draft/--detect/--rank: full pipeline, no draft model configured.
	})
	if err == nil {
		t.Fatal("--compare through the draft stage with no draft model must error")
	}
	if !strings.Contains(err.Error(), "draft_model") && !strings.Contains(err.Error(), "--model") {
		t.Errorf("error must name draft_model/--model, got: %v", err)
	}
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
	if err == nil {
		t.Fatal("a slash-id model direct-to-Anthropic can't serve must be rejected before any per-conversation work")
	}
}
