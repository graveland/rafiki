package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
		"path. .git directories are always excluded. Optionally restrict the " +
		"search to files matching a glob. Output is one \"path:line:text\" line " +
		"per match, capped at 100 matches by default."
	grepSchema = `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Regular expression (RE2 syntax) to search for."},
			"path": {"type": "string", "description": "File or directory to search. Required; must not be the filesystem root (\"/\")."},
			"glob": {"type": "string", "description": "Optional glob pattern to restrict which files are searched, matched against each file's path relative to path. A pattern with no / (e.g. \"*.go\") matches by file name at any depth, like ripgrep's -g."},
			"max_matches": {"type": "integer", "description": "Maximum number of matches to return. Defaults to 100."}
		},
		"required": ["pattern", "path"]
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
	return func(ctx context.Context, input json.RawMessage) (string, error) {
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

		if in.Path == "" {
			return "", fmt.Errorf("grep: path is required")
		}
		// Absolutize before walking (glob does the same): every emitted match
		// is prefixed with the walked path, and read/edit both *reject* a
		// relative path — so a relative base would produce output the model
		// cannot feed back into any other tool.
		base, err := filepath.Abs(in.Path)
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
		if base == string(filepath.Separator) {
			return "", fmt.Errorf("grep: refusing to search the filesystem root (%s); pass a narrower path", base)
		}
		baseInfo, err := os.Stat(base)
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}

		// Accept both files and directories: a single file just searches
		// that one file; a directory walks the tree as before.
		var (
			paths   []string
			errList []error
		)

		if baseInfo.IsDir() {
			walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if err != nil {
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
						slog.Warn("agent/tools: grep: path is not relative to the search base, matching the glob against the full path", "base", base, "path", p, "error", relErr)
						rel = p
					}
					ok, mErr := doublestar.Match(in.Glob, rel)
					if mErr != nil {
						return fmt.Errorf("invalid glob %q: %w", in.Glob, mErr)
					}
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
				paths = append(paths, p)
				return nil
			})
			if walkErr != nil {
				errList = append(errList, walkErr)
			}
		} else {
			paths = []string{base}
		}

		maxMatches := in.MaxMatches
		if maxMatches <= 0 {
			maxMatches = defaultGrepMaxMatches
		}

		var out strings.Builder
		shown := 0
		total := 0

		for _, p := range paths {
			if ctxErr := ctx.Err(); ctxErr != nil {
				errList = append(errList, ctxErr)
				break
			}

			f, openErr := os.Open(p)
			if openErr != nil {
				slog.Debug("agent/tools: grep: skipping unreadable file", "path", p, "error", openErr)
				continue
			}

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
			f.Close()
			if scanErr := scanner.Err(); scanErr != nil {
				slog.Debug("agent/tools: grep: stopped scanning file early", "path", p, "error", scanErr)
			}
		}

		if len(errList) > 0 {
			return "", fmt.Errorf("grep: %w", errors.Join(errList...))
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
