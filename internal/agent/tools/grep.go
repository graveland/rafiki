package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	// defaultGrepMaxMatches caps how many matches grep returns by default;
	// beyond this a "[+N more]" trailer reports the overflow.
	defaultGrepMaxMatches = 100

	grepDescription = "Search file contents for a regular expression (RE2 syntax), walking " +
		"path (defaults to the current working directory). .git directories are " +
		"always excluded. Optionally restrict the search to files matching a glob. " +
		"Output is one \"path:line:text\" line per match, capped at 100 matches by " +
		"default."
	grepSchema = `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Regular expression (RE2 syntax) to search for."},
			"path": {"type": "string", "description": "Base directory to search from. Defaults to the current working directory."},
			"glob": {"type": "string", "description": "Optional glob pattern to restrict which files are searched, matched against each file's path relative to path. A pattern with no / (e.g. \"*.go\") matches by file name at any depth, like ripgrep's -g."},
			"max_matches": {"type": "integer", "description": "Maximum number of matches to return. Defaults to 100."}
		},
		"required": ["pattern"]
	}`
)

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxMatches int    `json:"max_matches"`
}

// newGrepTool builds the grep ToolFunc. It has no shared state, so unlike
// read/write/edit it takes no FileTracker.
func newGrepTool() ToolFunc {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		var in grepInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("grep: invalid input: %w", err)
		}
		if in.Pattern == "" {
			return "", fmt.Errorf("grep: pattern is required")
		}
		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return "", fmt.Errorf("grep: invalid pattern %q: %w", in.Pattern, err)
		}

		base := in.Path
		if base == "" {
			wd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("grep: %w", err)
			}
			base = wd
		}
		baseInfo, err := os.Stat(base)
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
		if !baseInfo.IsDir() {
			return "", fmt.Errorf("grep: %q is not a directory", base)
		}

		maxMatches := in.MaxMatches
		if maxMatches <= 0 {
			maxMatches = defaultGrepMaxMatches
		}

		var out strings.Builder
		shown := 0
		total := 0
		walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// A subtree we can't even stat (permission denied, race with
				// a concurrent delete) shouldn't fail the whole search — log
				// it and skip that subtree rather than aborting.
				slog.Warn("agent/tools: grep: skipping unreadable path", "path", p, "error", err)
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if in.Glob != "" {
				rel, relErr := filepath.Rel(base, p)
				if relErr != nil {
					rel = p
				}
				ok, mErr := doublestar.Match(in.Glob, rel)
				if mErr != nil {
					return fmt.Errorf("invalid glob %q: %w", in.Glob, mErr)
				}
				// doublestar's `*` does not cross a path separator, so a
				// bare "*.go" would match only top-level files and silently
				// under-report everything nested. The model's prior is
				// ripgrep's -g '*.go', which matches at any depth: treat a
				// separator-free pattern as a basename pattern.
				if !ok && !strings.ContainsRune(in.Glob, '/') {
					ok, mErr = doublestar.Match(in.Glob, filepath.Base(rel))
					if mErr != nil {
						return fmt.Errorf("invalid glob %q: %w", in.Glob, mErr)
					}
				}
				if !ok {
					return nil
				}
			}

			f, openErr := os.Open(p)
			if openErr != nil {
				// Unreadable single file (permissions, broken symlink) — log
				// and keep walking; one bad file shouldn't sink the search.
				slog.Debug("agent/tools: grep: skipping unreadable file", "path", p, "error", openErr)
				return nil
			}
			defer f.Close()

			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			lineNo := 0
			for scanner.Scan() {
				lineNo++
				line := scanner.Text()
				if re.MatchString(line) {
					total++
					if shown < maxMatches {
						fmt.Fprintf(&out, "%s:%d:%s\n", p, lineNo, line)
						shown++
					}
				}
			}
			if scanErr := scanner.Err(); scanErr != nil {
				slog.Debug("agent/tools: grep: stopped scanning file early", "path", p, "error", scanErr)
			}
			return nil
		})
		if walkErr != nil {
			return "", fmt.Errorf("grep: %w", walkErr)
		}

		if total == 0 {
			return "no matches", nil
		}
		if total > maxMatches {
			fmt.Fprintf(&out, "[+%d more]\n", total-maxMatches)
		}
		return out.String(), nil
	}
}
