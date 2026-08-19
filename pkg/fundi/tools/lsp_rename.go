package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const lspRenameDescription = `Rename a symbol across the entire workspace using the language server.
Input: "path" (file), "line" and "col" (1-based position of the symbol), and "new_name".
The language server finds all references and applies the rename safely.
IMPORTANT: This writes to disk. The rename is applied to ALL files in the workspace
that reference the symbol, including files the agent has not read. The FileTracker
is force-refreshed for every modified file so subsequent reads see the new state.
This weakens the read-before-edit invariant, but a rename is semantically safe
(unlike a blind text edit) and a 40-file rename cannot realistically pre-read all files.`

func init() { DefaultBlueprint.Register(&LSPRenameBlueprint{}) }

type LSPRenameBlueprint struct{}

func (LSPRenameBlueprint) Name() string        { return "lsp_rename" }
func (LSPRenameBlueprint) Description() string { return lspRenameDescription }
func (LSPRenameBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "File path (absolute or relative to cwd)"},
			{Name: "line", Type: "integer", Description: "1-based line number of the symbol"},
			{Name: "col", Type: "integer", Description: "1-based column number of the symbol"},
			{Name: "new_name", Type: "string", Description: "New name for the symbol"},
		},
		Required: []string{"path", "line", "col", "new_name"},
	}
}

func (LSPRenameBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPRenameBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if lspUnavailable(opts) {
		return nil, nil
	}
	return &lspRenameTool{LSPRenameBlueprint: LSPRenameBlueprint{}, lsp: opts.LSP, tr: opts.FileTracker, cwd: opts.Cwd}, nil
}

type lspRenameTool struct {
	LSPRenameBlueprint
	lsp LSPClient
	tr  *FileTracker
	cwd string
}

type lspRenameInput struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	NewName string `json:"new_name"`
}

func (lt *lspRenameTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspRenameInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_rename: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, err
	}

	modified, err := lt.lsp.Rename(ctx, absPath, in.Line-1, in.Col-1, in.NewName)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_rename: %w", err)
	}

	// Force-refresh FileTracker for every touched path so the
	// read-before-edit invariant is satisfied for subsequent operations.
	// This is a conscious weakening of the invariant — the agent cannot
	// realistically pre-read 40 files before a rename, and a language-server-
	// guided rename is semantically safe in a way a blind text edit is not.
	for _, p := range modified {
		info, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		lt.tr.RecordRead(p, info.ModTime())
	}

	if len(modified) == 0 {
		return NewTextResult("lsp_rename: no files were modified"), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Renamed symbol to %q in %d file(s):\n", in.NewName, len(modified))
	for _, p := range modified {
		rel := relativePath(lt.cwd, p)
		fmt.Fprintf(&sb, "  %s\n", rel)
	}
	return NewTextResult(strings.TrimRight(sb.String(), "\n")), nil
}
