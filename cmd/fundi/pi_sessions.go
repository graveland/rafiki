package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PiSessionInfo is the subset of metadata we extract from a pi session.jsonl
// to build a SpawnRequest.
type PiSessionInfo struct {
	Path      string // absolute path to the .jsonl file
	SessionID string // pi's session uuid (from the session header)
	Cwd       string // working directory at the time of session creation
	Provider  string // most recent model_change.provider, or ""
	Model     string // most recent model_change.modelId, or ""
	Thinking  string // most recent thinking_level_change.thinkingLevel, or ""
}

// resolvePiSession turns the user's --pi-session argument into a PiSessionInfo.
// Accepts either an absolute/relative path (or ~-prefixed) to a .jsonl file,
// or a bare UUID which is resolved by globbing ~/.pi/agent/sessions/*/
func resolvePiSession(input string) (*PiSessionInfo, error) {
	if isSessionPath(input) {
		return readPiSession(expandHome(input))
	}
	// Treat as UUID: glob the sessions directory.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}
	pattern := filepath.Join(home, ".pi", "agent", "sessions", "*", "[0-9]*_"+input+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob sessions: %w", err)
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no session found for uuid %s", input)
	case 1:
		return readPiSession(matches[0])
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "multiple sessions match uuid %s; specify the full path:\n", input)
		for _, m := range matches {
			fmt.Fprintf(&b, "  %s\n", m)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
}

// isSessionPath reports whether the user's input should be treated as a file
// path rather than a bare UUID. Matches the spec: contains /, starts with ~,
// or ends with .jsonl.
func isSessionPath(input string) bool {
	return strings.Contains(input, "/") || strings.HasPrefix(input, "~") || strings.HasSuffix(input, ".jsonl")
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	// "~" alone → home; "~/..." → home + rest; "~user/..." is not supported
	rest := path[1:]
	if rest == "" {
		return home
	}
	return filepath.Join(home, rest)
}

// piSessionRecord is the minimal union of every jsonl line shape we care about.
// Fields we don't recognise are silently ignored by json.Unmarshal.
type piSessionRecord struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	Cwd           string `json:"cwd"`
	Provider      string `json:"provider"`
	ModelID       string `json:"modelId"`
	ThinkingLevel string `json:"thinkingLevel"`
}

// readPiSession opens path and walks the jsonl extracting fields. The file is
// streamed line by line so very long sessions are cheap to process. Only the
// session header (first record), model_change, and thinking_level_change
// records are examined; all others are skipped.
func readPiSession(path string) (*PiSessionInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("--pi-session: session file not found: %s", path)
		}
		return nil, fmt.Errorf("--pi-session: open session file: %w", err)
	}
	defer f.Close()

	info := &PiSessionInfo{Path: path}
	scanner := bufio.NewScanner(f)
	// Pi session files can have large messages; use a generous line buffer.
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	sawFirstRecord := false
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var rec piSessionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("invalid pi session file: %w (line %d)", err, lineNum)
		}

		if !sawFirstRecord {
			sawFirstRecord = true
			if rec.Type != "session" {
				return nil, fmt.Errorf("not a pi session file: %s", path)
			}
			info.SessionID = rec.ID
			info.Cwd = rec.Cwd
			continue
		}

		switch rec.Type {
		case "model_change":
			info.Provider = rec.Provider
			info.Model = rec.ModelID
		case "thinking_level_change":
			info.Thinking = rec.ThinkingLevel
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("invalid pi session file: %w", err)
	}

	// An empty file (or one with no non-blank lines) has no session header.
	if !sawFirstRecord {
		return nil, fmt.Errorf("not a pi session file: %s", path)
	}

	return info, nil
}
