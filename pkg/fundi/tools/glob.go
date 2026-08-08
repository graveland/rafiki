package tools

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
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
)

func init() { DefaultBlueprint.Register(&GlbTool{}) }

// GlbTool implements Tool for the glob filesystem search tool.
type GlbTool struct{}

func (GlbTool) Name() string        { return "glob" }
func (GlbTool) Description() string { return globDescription }
func (GlbTool) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "pattern", Type: "string", Description: "Glob pattern (doublestar syntax, supports **) to match file paths against. Relative to path — use \"**/*.go\", not an absolute path."},
			{Name: "path", Type: "string", Description: "Base directory to search from. Defaults to the current working directory."},
		},
		Required: []string{"pattern"},
	}
}

// Execute finds files matching a glob pattern rooted at the given path.
func (GlbTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in globInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("glob: invalid input: %w", err)
	}
	if in.Pattern == "" {
		return ToolResult{}, fmt.Errorf("glob: pattern is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	base := in.Path
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ToolResult{}, fmt.Errorf("glob: %w", err)
		}
		base = wd
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return ToolResult{}, fmt.Errorf("glob: %w", err)
	}
	baseInfo, err := os.Stat(base)
	if err != nil {
		return ToolResult{}, fmt.Errorf("glob: %w", err)
	}
	if !baseInfo.IsDir() {
		return ToolResult{}, fmt.Errorf("glob: %q is not a directory", base)
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
			return ToolResult{}, fmt.Errorf("glob: pattern %q is absolute and could not be interpreted relative to path %s: %w; pattern is relative to path", in.Pattern, base, relErr)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ToolResult{}, fmt.Errorf("glob: pattern %q is absolute and falls outside path %s; pattern is relative to path — pass the containing directory as path and a relative pattern such as \"**/*.go\"", in.Pattern, base)
		}
		pattern = rel
	}

	matches, err := doublestar.Glob(ctxFS{FS: os.DirFS(base), ctx: ctx}, pattern, doublestar.WithFailOnIOErrors())
	if err != nil {
		// A canceled turn surfaces as an I/O error from ctxFS deep inside
		// doublestar's walk (WithFailOnIOErrors makes that abort the walk
		// instead of being silently swallowed) — report the real cause
		// rather than a confusing "invalid pattern".
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ToolResult{}, ctxErr
		}
		return ToolResult{}, fmt.Errorf("glob: invalid pattern %q: %w", in.Pattern, err)
	}
	if len(matches) == 0 {
		return NewTextResult("no files matched"), nil
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
			// Still say so: a silently dropped match is indistinguishable
			// from a pattern that never matched.
			slog.Debug("agent/tools: glob: skipping match that could not be stat'd", "path", full, "error", err)
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
	return NewTextResult(out.String()), nil
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// ctxFS wraps an fs.FS and fails Open/ReadDir once ctx is done, so a
// doublestar walk over a large or slow (e.g. network-mounted) tree — driven
// entirely inside the library, with no callback of our own to check — aborts
// promptly instead of running to completion after the caller has moved on.
// Paired with doublestar.WithFailOnIOErrors, which is required for this
// synthetic I/O error to actually stop the walk rather than being silently
// skipped like a real permission error.
type ctxFS struct {
	fs.FS
	ctx context.Context
}

func (c ctxFS) Open(name string) (fs.File, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	return c.FS.Open(name)
}

func (c ctxFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	if rdfs, ok := c.FS.(fs.ReadDirFS); ok {
		return rdfs.ReadDir(name)
	}
	return fs.ReadDir(c.FS, name)
}
