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
		"path, which may be a directory or a single file. Honours .gitignore, " +
		"so ignored files are not searched; hidden files are included, but .git " +
		"itself is not. Optionally restrict the search to files matching a glob. " +
		"Output is one \"path:line:text\" line per match, capped at 100 matches " +
		"by default."
)

func init() { DefaultBlueprint.Register(&GrepBlueprint{}) }

// GrepBlueprint is the static metadata for the grep tool.
type GrepBlueprint struct{}

func (GrepBlueprint) Name() string        { return "grep" }
func (GrepBlueprint) Description() string { return grepDescription }
func (GrepBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "pattern", Type: "string", Description: "Regular expression (RE2 syntax) to search for."},
			{Name: "path", Type: "string", Description: "File or directory to search (absolute or relative to the working directory). Required; must not be the filesystem root (\"/\")."},
			{Name: "glob", Type: "string", Description: "Optional glob pattern to restrict which files are searched, matched against each file's path relative to path. A pattern with no / (e.g. \"*.go\") matches by file name at any depth, like ripgrep's -g."},
			{Name: "max_matches", Type: "integer", Description: "Maximum number of matches to return. Defaults to 100."},
		},
		Required: []string{"pattern", "path"},
	}
}
func (GrepBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (GrepBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &grepTool{GrepBlueprint: GrepBlueprint{}, cwd: opts.Cwd}, nil
}

type grepTool struct {
	GrepBlueprint
	cwd string
}

// Execute searches file contents for a regular expression.
func (gt *grepTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
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
	// resolveToolPath, not filepath.Abs: Abs joins a relative path against
	// the *daemon's* process cwd, but the agent's cwd comes from the spawn
	// request, so the two are routinely different directories. Every other
	// file tool resolves through this helper.
	base, err := resolveToolPath(in.Path, "", gt.cwd)
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
