package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// defaultGrepMaxMatches caps how many matches grep returns by default;
	// beyond this a trailer reports the overflow.
	defaultGrepMaxMatches = 100

	grepDescription = "Search file contents for a regular expression (RE2 syntax), walking " +
		"path. .git directories are always excluded. Optionally restrict the " +
		"search to files matching a glob. Output is one \"path:line:text\" line " +
		"per match, capped at 100 matches by default."
)

func init() { DefaultBlueprint.Register(&GrepTool{}) }

// GrepTool implements Tool for the grep file-content search tool.
type GrepTool struct{}

func (GrepTool) Name() string        { return "grep" }
func (GrepTool) Description() string { return grepDescription }
func (GrepTool) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "pattern", Type: "string", Description: "Regular expression (RE2 syntax) to search for."},
			{Name: "path", Type: "string", Description: "File or directory to search. Required; must not be the filesystem root (\"/\")."},
			{Name: "glob", Type: "string", Description: "Optional glob pattern to restrict which files are searched, matched against each file's path relative to path. A pattern with no / (e.g. \"*.go\") matches by file name at any depth, like ripgrep's -g."},
			{Name: "max_matches", Type: "integer", Description: "Maximum number of matches to return. Defaults to 100."},
		},
		Required: []string{"pattern", "path"},
	}
}

// Execute searches file contents for a regular expression.
func (GrepTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in grepInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("grep: invalid input: %w", err)
	}
	if in.Pattern == "" {
		return ToolResult{}, fmt.Errorf("grep: pattern is required")
	}

	if in.Path == "" {
		return ToolResult{}, fmt.Errorf("grep: path is required")
	}
	base, err := filepath.Abs(in.Path)
	if err != nil {
		return ToolResult{}, fmt.Errorf("grep: %w", err)
	}
	if base == string(filepath.Separator) {
		return ToolResult{}, fmt.Errorf("grep: refusing to search the filesystem root (%s); pass a narrower path", base)
	}
	_, err = os.Stat(base)
	if err != nil {
		return ToolResult{}, fmt.Errorf("grep: %w", err)
	}

	maxMatches := in.MaxMatches
	if maxMatches <= 0 {
		maxMatches = defaultGrepMaxMatches
	}

	// Request one more than the cap to detect overflow via SearchContent's
	// truncated return, so we can emit a "[more matches omitted]" trailer.
	matches, truncated, err := SearchContent(ctx, ContentQuery{
		Root:    base,
		Pattern: in.Pattern,
		Glob:    in.Glob,
		Limit:   maxMatches + 1,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("grep: %w", err)
	}

	if len(matches) == 0 {
		return NewTextResult("no matches"), nil
	}

	overflow := truncated
	if len(matches) > maxMatches {
		overflow = true
		matches = matches[:maxMatches]
	}

	var out strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&out, "%s:%d:%s\n", m.Path, m.Line, m.Text)
	}
	if overflow {
		fmt.Fprintf(&out, "[more matches omitted]\n")
	}

	return NewTextResult(out.String()), nil
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxMatches int    `json:"max_matches"`
}
