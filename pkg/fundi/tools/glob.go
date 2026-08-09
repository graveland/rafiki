package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// maxGlobResults caps how many matches glob returns; beyond this a
	// trailer reports the overflow instead of flooding the model.
	maxGlobResults = 200

	// globSortPool is how many matches are collected before the mtime sort.
	//
	// Collecting only maxGlobResults+1 made "newest first" a lie: those are
	// the first 201 paths in ripgrep's traversal order, so in a repo with
	// 5000 .go files the actually-newest file was usually not among them and
	// the tool confidently returned an arbitrary prefix sorted by mtime.
	// Sorting a larger pool and then taking the top 200 restores the
	// documented behaviour at bounded cost — the extra work is one stat per
	// candidate, not a second walk.
	globSortPool = 5000

	globDescription = "Find files by glob pattern (doublestar syntax: * ? [...] and ** for " +
		"recursive matching), rooted at path (defaults to the current working " +
		"directory). Results are sorted by modification time, newest first, and " +
		"capped at 200 matches. Honours .gitignore; hidden files are included, " +
		"but .git itself is not searched."
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

	// Collect a large pool so the mtime sort is meaningful; see globSortPool.
	paths, poolFull, err := DiscoverFiles(ctx, FileQuery{Root: base, Glob: pattern, Limit: globSortPool})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ToolResult{}, ctxErr
		}
		return ToolResult{}, fmt.Errorf("glob: invalid pattern %q: %w", in.Pattern, err)
	}
	if len(paths) == 0 {
		return NewTextResult("no files matched"), nil
	}

	type entry struct {
		path  string
		mtime time.Time
	}
	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			slog.Debug("agent/tools: glob: skipping match that could not be stat'd", "path", p, "error", err)
			continue
		}
		entries = append(entries, entry{path: p, mtime: info.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.After(entries[j].mtime) })

	// Cut to the reported cap after sorting, so the 200 returned really are
	// the newest of everything found. The length guard matters because
	// entries can be shorter than paths when a match was deleted between
	// the walk and its stat: slicing to a larger length would panic, and
	// slicing within a larger cap would resurrect zero-valued entries.
	truncated := poolFull
	if len(entries) > maxGlobResults {
		entries = entries[:maxGlobResults]
		truncated = true
	}

	var out strings.Builder
	for _, e := range entries {
		out.WriteString(e.path)
		out.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&out, "[more matches omitted]\n")
	}
	return NewTextResult(out.String()), nil
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}
