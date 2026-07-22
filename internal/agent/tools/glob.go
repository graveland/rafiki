package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	// maxGlobResults caps how many matches glob returns; beyond this a
	// "[+N more]" trailer reports the overflow instead of flooding the model.
	maxGlobResults = 200

	globDescription = "Find files by glob pattern (doublestar syntax: * ? [...] and ** for " +
		"recursive matching), rooted at path (defaults to the current working " +
		"directory). Results are sorted by modification time, newest first, and " +
		"capped at 200 matches."
	globSchema = `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern (doublestar syntax, supports **) to match file paths against. Relative to path — use \"**/*.go\", not an absolute path."},
			"path": {"type": "string", "description": "Base directory to search from. Defaults to the current working directory."}
		},
		"required": ["pattern"]
	}`
)

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// newGlobTool builds the glob ToolFunc. It has no shared state, so unlike
// read/write/edit it takes no FileTracker.
func newGlobTool() ToolFunc {
	return func(_ context.Context, input json.RawMessage) (string, error) {
		var in globInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("glob: invalid input: %w", err)
		}
		if in.Pattern == "" {
			return "", fmt.Errorf("glob: pattern is required")
		}

		base := in.Path
		if base == "" {
			wd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("glob: %w", err)
			}
			base = wd
		}
		base, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
		baseInfo, err := os.Stat(base)
		if err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
		if !baseInfo.IsDir() {
			return "", fmt.Errorf("glob: %q is not a directory", base)
		}

		// The pattern is matched against an fs rooted at base, so an absolute
		// pattern matches nothing and would return a cheerful "no files
		// matched". read/write/edit all *require* absolute paths, so the tool
		// surface actively trains the model to pass one here: rebase it onto
		// base when it lands inside, and say so plainly when it doesn't.
		pattern := in.Pattern
		if filepath.IsAbs(pattern) {
			rel, relErr := filepath.Rel(base, filepath.Clean(pattern))
			if relErr != nil {
				return "", fmt.Errorf("glob: pattern %q is absolute and could not be interpreted relative to path %s: %w; pattern is relative to path", in.Pattern, base, relErr)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("glob: pattern %q is absolute and falls outside path %s; pattern is relative to path — pass the containing directory as path and a relative pattern such as \"**/*.go\"", in.Pattern, base)
			}
			pattern = rel
		}

		matches, err := doublestar.Glob(os.DirFS(base), pattern)
		if err != nil {
			return "", fmt.Errorf("glob: invalid pattern %q: %w", in.Pattern, err)
		}
		if len(matches) == 0 {
			return "no files matched", nil
		}

		type entry struct {
			path  string
			mtime time.Time
		}
		entries := make([]entry, 0, len(matches))
		for _, m := range matches {
			full := filepath.Join(base, m)
			info, err := os.Stat(full)
			if err != nil {
				// Raced away between Glob and Stat (deleted/renamed
				// concurrently) — not a tool failure, just one fewer result.
				continue
			}
			entries = append(entries, entry{path: full, mtime: info.ModTime()})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.After(entries[j].mtime) })

		truncated := len(entries) > maxGlobResults
		total := len(entries)
		if truncated {
			entries = entries[:maxGlobResults]
		}

		var out strings.Builder
		for _, e := range entries {
			out.WriteString(e.path)
			out.WriteByte('\n')
		}
		if truncated {
			fmt.Fprintf(&out, "[+%d more]\n", total-maxGlobResults)
		}
		return out.String(), nil
	}
}
