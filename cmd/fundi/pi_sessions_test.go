package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── readPiSession tests ────────────────────────────────────────────────────

func writeSessionFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writeSessionFile: %v", err)
	}
	return path
}

func TestReadPiSession_WellFormed(t *testing.T) {
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"session","version":3,"id":"test-uuid-1","cwd":"/some/project","timestamp":"2026-01-01T00:00:00Z"}
{"type":"model_change","provider":"anthropic","modelId":"claude-haiku-4-5","timestamp":"2026-01-01T00:00:01Z"}
{"type":"thinking_level_change","thinkingLevel":"medium","timestamp":"2026-01-01T00:00:01Z"}
{"type":"message","message":{}}
`)
	info, err := readPiSession(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SessionID != "test-uuid-1" {
		t.Errorf("SessionID = %q, want test-uuid-1", info.SessionID)
	}
	if info.Cwd != "/some/project" {
		t.Errorf("Cwd = %q, want /some/project", info.Cwd)
	}
	if info.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", info.Provider)
	}
	if info.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", info.Model)
	}
	if info.Thinking != "medium" {
		t.Errorf("Thinking = %q, want medium", info.Thinking)
	}
	if info.Path != path {
		t.Errorf("Path = %q, want %q", info.Path, path)
	}
}

func TestReadPiSession_LastModelChangeWins(t *testing.T) {
	// Multiple model_change events: the last one should win.
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"session","id":"uuid-multi","cwd":"/tmp"}
{"type":"model_change","provider":"anthropic","modelId":"claude-haiku-4-5"}
{"type":"model_change","provider":"anthropic","modelId":"claude-opus-4-7"}
{"type":"thinking_level_change","thinkingLevel":"low"}
{"type":"model_change","provider":"openai","modelId":"gpt-4o"}
`)
	info, err := readPiSession(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Provider != "openai" {
		t.Errorf("Provider = %q, want openai (last wins)", info.Provider)
	}
	if info.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o (last wins)", info.Model)
	}
	// thinking_level_change after the first model_change; last value.
	if info.Thinking != "low" {
		t.Errorf("Thinking = %q, want low", info.Thinking)
	}
}

func TestReadPiSession_LastThinkingLevelWins(t *testing.T) {
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"session","id":"uuid-think","cwd":"/tmp"}
{"type":"model_change","provider":"anthropic","modelId":"claude-opus-4-7"}
{"type":"thinking_level_change","thinkingLevel":"low"}
{"type":"thinking_level_change","thinkingLevel":"xhigh"}
`)
	info, err := readPiSession(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Thinking != "xhigh" {
		t.Errorf("Thinking = %q, want xhigh (last wins)", info.Thinking)
	}
}

func TestReadPiSession_HeaderOnly(t *testing.T) {
	// A session with only the header record: no model_change → empty Provider/Model.
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"session","id":"uuid-hdr","cwd":"/only/header"}
`)
	info, err := readPiSession(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SessionID != "uuid-hdr" {
		t.Errorf("SessionID = %q, want uuid-hdr", info.SessionID)
	}
	if info.Cwd != "/only/header" {
		t.Errorf("Cwd = %q, want /only/header", info.Cwd)
	}
	if info.Provider != "" {
		t.Errorf("Provider = %q, want empty", info.Provider)
	}
	if info.Model != "" {
		t.Errorf("Model = %q, want empty", info.Model)
	}
	if info.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", info.Thinking)
	}
}

func TestReadPiSession_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"session","id":"uuid-bad","cwd":"/tmp"}
{not valid json}
`)
	_, err := readPiSession(path)
	if err == nil {
		t.Fatal("expected error for malformed jsonl, got nil")
	}
	if !strings.Contains(err.Error(), "invalid pi session file") {
		t.Errorf("error should contain 'invalid pi session file', got: %v", err)
	}
}

func TestReadPiSession_WrongFirstRecordType(t *testing.T) {
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"model_change","provider":"anthropic","modelId":"claude-opus-4-7"}
`)
	_, err := readPiSession(path)
	if err == nil {
		t.Fatal("expected error for wrong first record type, got nil")
	}
	if !strings.Contains(err.Error(), "not a pi session file") {
		t.Errorf("error should contain 'not a pi session file', got: %v", err)
	}
}

func TestReadPiSession_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", "")
	_, err := readPiSession(path)
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
	if !strings.Contains(err.Error(), "not a pi session file") {
		t.Errorf("error should contain 'not a pi session file', got: %v", err)
	}
}

func TestReadPiSession_FileNotFound(t *testing.T) {
	_, err := readPiSession("/nonexistent/path/to/session.jsonl")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "--pi-session") {
		t.Errorf("error should contain '--pi-session' prefix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

// ─── resolvePiSession tests ─────────────────────────────────────────────────

func TestResolvePiSession_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := writeSessionFile(t, dir, "session.jsonl", `{"type":"session","id":"abs-uuid","cwd":"/abs"}
`)
	info, err := resolvePiSession(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SessionID != "abs-uuid" {
		t.Errorf("SessionID = %q, want abs-uuid", info.SessionID)
	}
}

func TestResolvePiSession_TildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable:", err)
	}
	// Write a session file inside the home directory (a temp subdir).
	sub, err := os.MkdirTemp(home, "pi-sessions-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sub)

	path := writeSessionFile(t, sub, "session.jsonl", `{"type":"session","id":"tilde-uuid","cwd":"/tilde"}
`)
	// Build a tilde-relative path.
	rel, err := filepath.Rel(home, path)
	if err != nil {
		t.Fatal(err)
	}
	tildePath := "~/" + rel

	info, err := resolvePiSession(tildePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SessionID != "tilde-uuid" {
		t.Errorf("SessionID = %q, want tilde-uuid", info.SessionID)
	}
}

func TestResolvePiSession_UUID_OneMatch(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable:", err)
	}
	// Build the expected glob path: ~/.pi/agent/sessions/<dir>/TIMESTAMP_UUID.jsonl
	sessionsDir := filepath.Join(home, ".pi", "agent", "sessions", "test-cwd")
	if err := os.MkdirAll(sessionsDir, 0750); err != nil {
		t.Fatal(err)
	}
	uuid := "aabbccdd-1122-3344-5566-778899aabbcc"
	fname := "2026-01-01T00-00-00-000Z_" + uuid + ".jsonl"
	path := filepath.Join(sessionsDir, fname)
	content := `{"type":"session","id":"` + uuid + `","cwd":"/found"}
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	info, err := resolvePiSession(uuid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SessionID != uuid {
		t.Errorf("SessionID = %q, want %q", info.SessionID, uuid)
	}
	if info.Cwd != "/found" {
		t.Errorf("Cwd = %q, want /found", info.Cwd)
	}
}

func TestResolvePiSession_UUID_NoMatch(t *testing.T) {
	uuid := "00000000-0000-0000-0000-000000000000"
	_, err := resolvePiSession(uuid)
	if err == nil {
		t.Fatal("expected error for uuid with no matches, got nil")
	}
	if !strings.Contains(err.Error(), "no session found for uuid") {
		t.Errorf("error should contain 'no session found for uuid', got: %v", err)
	}
	if !strings.Contains(err.Error(), uuid) {
		t.Errorf("error should contain the uuid %q, got: %v", uuid, err)
	}
}

func TestResolvePiSession_UUID_MultipleMatches(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable:", err)
	}
	uuid := "ffee0011-ffee-0011-ffee-0011ffeeff00"

	// Create two matches in different session dirs.
	for _, subdir := range []string{"multi-cwd-a", "multi-cwd-b"} {
		dir := filepath.Join(home, ".pi", "agent", "sessions", subdir)
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
		fname := "2026-01-02T00-00-00-000Z_" + uuid + ".jsonl"
		path := filepath.Join(dir, fname)
		content := `{"type":"session","id":"` + uuid + `","cwd":"/multi"}
`
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(path)
	}

	_, err = resolvePiSession(uuid)
	if err == nil {
		t.Fatal("expected error for multiple matches, got nil")
	}
	if !strings.Contains(err.Error(), "multiple sessions match") {
		t.Errorf("error should contain 'multiple sessions match', got: %v", err)
	}
	if !strings.Contains(err.Error(), uuid) {
		t.Errorf("error should contain the uuid, got: %v", err)
	}
	// Error should list the paths so the user can pick one.
	if !strings.Contains(err.Error(), "multi-cwd-a") || !strings.Contains(err.Error(), "multi-cwd-b") {
		t.Errorf("error should list both matching paths, got: %v", err)
	}
}

// ─── isSessionPath tests ─────────────────────────────────────────────────────

func TestIsSessionPath(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"/absolute/path.jsonl", true},
		{"relative/path.jsonl", true},
		{"~/some/session.jsonl", true},
		{"session.jsonl", true},   // ends with .jsonl
		{"~session", true},        // starts with ~
		{"a/b", true},             // contains /
		{"bare-uuid-1234", false}, // plain uuid-like string
		{"aabbccdd-1122-3344-5566-778899aabbcc", false},
	}
	for _, tc := range cases {
		got := isSessionPath(tc.input)
		if got != tc.want {
			t.Errorf("isSessionPath(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// ─── expandHome tests ────────────────────────────────────────────────────────

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable:", err)
	}

	cases := []struct {
		input string
		want  string
	}{
		{"~/foo/bar", filepath.Join(home, "foo", "bar")},
		{"~", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}
	for _, tc := range cases {
		got := expandHome(tc.input)
		if got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ─── resume cmd flag tests ────────────────────────────────────────────────────

func TestResumeCmd_PiSessionAndPositionalArgAreMutuallyExclusive(t *testing.T) {
	cmd := newResumeCmd()
	err := executeWithFlags(cmd, "--pi-session", "/some/session.jsonl", "some-child-id")
	if err == nil {
		t.Fatal("expected error when both positional arg and --pi-session are set, got nil")
	}
	if !strings.Contains(err.Error(), "--pi-session") {
		t.Errorf("error should mention --pi-session, got: %v", err)
	}
}

func TestResumeCmd_KillAndKeepOnPiSessionAreMutuallyExclusive(t *testing.T) {
	cmd := newResumeCmd()
	err := executeWithFlags(cmd, "--kill-on-exit", "--keep-on-exit")
	if err == nil {
		t.Fatal("expected error when both --kill-on-exit and --keep-on-exit are set, got nil")
	}
	if !strings.Contains(err.Error(), "kill-on-exit") || !strings.Contains(err.Error(), "keep-on-exit") {
		t.Errorf("expected both flag names in error, got: %v", err)
	}
}
